package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"time"
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
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Chat 发起一次多轮回喂中的单跳请求。
//
// Retry / fallback policy:
//   - Per-model: up to 3 attempts with exponential backoff (1s, 2s). Network
//     errors, HTTP 5xx and 429 are retryable; 4xx (non-429) and JSON decode
//     errors are terminal for that model.
//   - Across models: when the primary model exhausts its retry budget with a
//     retryable error, and Client has fallbackModels configured, the request
//     is replayed against each fallback in order (with a fresh 3-attempt
//     budget). A terminal error (4xx / decode / empty choices) never triggers
//     fallback — those signal a caller-side or contract problem the next
//     model would hit too.
//
// A log line is emitted on every model switch to make silent quality drift
// observable during incidents.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error) {
	models := append([]string{c.model}, c.fallbackModels...)

	var lastErr error
	for modelIdx, model := range models {
		if modelIdx > 0 {
			log.Printf("[llm] falling back from %q to %q after retries exhausted: %v", models[modelIdx-1], model, lastErr)
		}

		turn, retry, err := c.chatOneModel(ctx, model, msgs, tools)
		if err == nil {
			return turn, nil
		}
		lastErr = err

		// Terminal errors (non-retryable at the model layer) also skip
		// cross-model fallback: a 400 / decode error / empty choices from
		// model A almost certainly indicates a caller-side problem (bad
		// payload, contract mismatch) that model B would hit too. Only
		// escalate to the next model when the current one signalled that
		// its own budget was worth retrying.
		if !retry {
			return AssistantTurn{}, err
		}
	}
	return AssistantTurn{}, fmt.Errorf("chat failed after exhausting %d model(s): %w", len(models), lastErr)
}

// chatOneModel runs the per-model retry loop for a single model identifier.
// Returns (turn, retry, err). `retry` mirrors doOnce's semantic and is true
// only when the loop exhausted its budget on retryable failures — signalling
// the caller that switching to a fallback model is meaningful. It is false
// on both success and terminal (non-retryable) failures.
func (c *Client) chatOneModel(ctx context.Context, model string, msgs []Message, tools []Tool) (AssistantTurn, bool, error) {
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
		return AssistantTurn{}, false, fmt.Errorf("marshal request: %w", err)
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避：1s, 2s；尊重外层 ctx 取消。
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			select {
			case <-ctx.Done():
				return AssistantTurn{}, false, ctx.Err()
			case <-time.After(backoff):
			}
		}

		turn, retry, err := c.doOnce(ctx, payload)
		if err == nil {
			return turn, false, nil
		}
		lastErr = err
		if !retry {
			// Terminal for this model; signal caller not to try fallbacks.
			return AssistantTurn{}, false, err
		}
	}
	// Exhausted the per-model retry budget on retryable errors: signal caller
	// (Chat) that a fallback model is worth attempting.
	return AssistantTurn{}, true, fmt.Errorf("chat failed after %d attempts on model %q: %w", maxAttempts, model, lastErr)
}

// doOnce 执行单次请求，返回是否值得重试。
func (c *Client) doOnce(ctx context.Context, payload []byte) (AssistantTurn, bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return AssistantTurn{}, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		// 网络层错误值得重试。
		return AssistantTurn{}, true, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		retry := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return AssistantTurn{}, retry, fmt.Errorf("http status %d: %s", resp.StatusCode, string(body))
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return AssistantTurn{}, false, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return AssistantTurn{}, false, fmt.Errorf("empty choices in response")
	}
	msg := cr.Choices[0].Message
	return AssistantTurn{
		Content:   msg.Content,
		ToolCalls: msg.ToolCalls,
		Tokens:    cr.Usage.TotalTokens,
	}, false, nil
}
