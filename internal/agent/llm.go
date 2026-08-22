package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// Client 是 agent 自带的 LLM 客户端，独立于 service/llm.go，只依赖标准库。
type Client struct {
	apiURL         string
	apiKey         string
	model          string
	fallbackModels []string
	timeout        time.Duration
	maxTokens      int
	http           *http.Client
}

// NewClient constructs an agent LLM client. When fallbackModels is non-empty
// (typically sourced from LLM_FALLBACK_MODELS), Chat exhausts the primary
// model's per-model retry budget before switching to each fallback in order.
// Passing a nil / empty slice preserves the single-model behavior. See
// issue #179 for motivation.
func NewClient(apiURL, apiKey, model string, timeoutSec, maxTokens int, fallbackModels []string) *Client {
	// Copy to isolate the caller's slice from mutation; also drop empty
	// entries and any entry that duplicates the primary model (would waste
	// the retry budget without gaining coverage).
	fallbacks := make([]string, 0, len(fallbackModels))
	seen := map[string]bool{model: true}
	for _, m := range fallbackModels {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		fallbacks = append(fallbacks, m)
	}
	return &Client{
		apiURL:         apiURL,
		apiKey:         apiKey,
		model:          model,
		fallbackModels: fallbacks,
		timeout:        time.Duration(timeoutSec) * time.Second,
		maxTokens:      maxTokens,
		http:           &http.Client{},
	}
}

// chatRequest / chatResponse 只描述我们真正会用到的字段。
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Chat 发起一次多轮回喂中的单跳请求。
//
// Retry / fallback policy:
//   - Per-model: up to 3 attempts with exponential backoff (1s, 2s). Network
//     errors, HTTP 5xx and 429 are retryable. HTTP 403 switches immediately
//     to the next model; 401, other 4xx and decode errors are terminal.
//   - Across models: when the primary model exhausts its retry budget with a
//     retryable error, and Client has fallbackModels configured, the request
//     is replayed against each fallback in order (with a fresh 3-attempt
//     budget). Terminal failures do not trigger fallback.
//   - Deadline-aware escalation: before a same-model backoff, the shared
//     runner reserves the backoff, pending retry and one full request timeout
//     for the next fallback so it is not starved by the primary (#179 P1).
//
// A bounded, single-line log is emitted on every model switch to make silent
// quality drift observable without dumping full upstream response bodies.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error) {
	models := append([]string{c.model}, c.fallbackModels...)
	turn, _, err := llmfallback.Run(ctx, llmfallback.Config{
		Models:          models,
		PerModelTimeout: c.timeout,
		MaxAttempts:     3,
	}, func(ctx context.Context, model string) (AssistantTurn, llmfallback.Outcome, error) {
		return c.attemptChat(ctx, model, msgs, tools)
	})
	return turn, err
}

// attemptChat performs one chat/completions request against `model` and maps
// the result to an llmfallback.Outcome. It owns only the single request +
// classification; retry / backoff / model-switch is driven by llmfallback.Run.
//
// Classification (shared policy — see llmfallback.ClassifyStatus):
//   - parent-context cancellation -> Terminal (surface the cancel; do not
//     spend a doomed round-trip on a fallback, issue #179 P2)
//   - transport error -> RetrySameModel (a local timeout is worth retrying)
//   - HTTP >=400 -> ClassifyStatus (429/5xx retry same model; 403 escalates to
//     the next model — the Bedrock SCP-deny case, #211; other 4xx terminal)
//   - decode error / empty choices -> Terminal
func (c *Client) attemptChat(ctx context.Context, model string, msgs []Message, tools []Tool) (AssistantTurn, llmfallback.Outcome, error) {
	reqBody := chatRequest{
		Model:       model,
		Messages:    msgs,
		Tools:       tools,
		MaxTokens:   c.maxTokens,
		Temperature: 0.3,
	}
	if len(tools) > 0 {
		reqBody.ToolChoice = "auto"
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return AssistantTurn{}, llmfallback.Terminal, fmt.Errorf("marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return AssistantTurn{}, llmfallback.Terminal, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return AssistantTurn{}, llmfallback.Terminal, ctx.Err()
		}
		return AssistantTurn{}, llmfallback.RetrySameModel, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return AssistantTurn{}, llmfallback.ClassifyNonOKStatus(resp.StatusCode),
			fmt.Errorf("http status %d: %s", resp.StatusCode, llmfallback.SafeTextForLog(string(body), 200))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return AssistantTurn{}, llmfallback.Terminal, ctx.Err()
		}
		return AssistantTurn{}, llmfallback.RetrySameModel, fmt.Errorf("read response: %w", err)
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return AssistantTurn{}, llmfallback.Terminal, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return AssistantTurn{}, llmfallback.Terminal, fmt.Errorf("empty choices in response")
	}
	choice := cr.Choices[0]
	msg := choice.Message
	// Set alongside the prose notice below, never instead of it: the notice tells
	// the reader where the text stops, the flag lets the runner record a fact the
	// model cannot edit away. See AssistantTurn.Truncated.
	truncated := false
	if choice.FinishReason == "length" {
		if len(msg.ToolCalls) > 0 {
			// Tool-call arguments can be syntactically present but cut off mid-JSON.
			// Never dispatch a truncated planner turn as if it were valid.
			return AssistantTurn{}, llmfallback.Terminal, fmt.Errorf("LLM tool response truncated: finish_reason=length")
		}
		if strings.TrimSpace(msg.Content) == "" {
			return AssistantTurn{}, llmfallback.RetrySameModel, fmt.Errorf("LLM response truncated before producing content: finish_reason=length")
		}
		// A content-only turn is the user-facing answer. It is degraded but still
		// usable, so preserve it with an explicit disclosure instead of failing
		// the whole request and discarding everything the model produced.
		msg.Content += service.TruncationNotice
		truncated = true
	}
	return AssistantTurn{
		Content:   msg.Content,
		ToolCalls: msg.ToolCalls,
		Tokens:    cr.Usage.TotalTokens,
		Truncated: truncated,
	}, llmfallback.Success, nil
}
