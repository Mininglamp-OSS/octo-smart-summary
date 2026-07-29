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

// truncateForLog caps a string to n runes and appends an ellipsis marker when
// truncated. Used to keep upstream error bodies out of long-lived logs while
// still surfacing enough context to diagnose retry-vs-fallback decisions.
// See issue #179's leakage criterion.
func truncateForLog(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "...(truncated)"
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
//   - Deadline-aware escalation: before each per-model attempt, chatOneModel
//     checks how much of the parent step deadline remains. If less than one
//     full c.timeout remains AND there are still untried fallback models,
//     the loop bails out early so the fallback gets a real chance instead
//     of being cut off by an already-dead parent context (issue #179 P1).
//
// A log line is emitted on every model switch to make silent quality drift
// observable during incidents. Upstream response bodies are truncated to
// avoid leaking gateway payloads into stdout on the success path.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error) {
	models := append([]string{c.model}, c.fallbackModels...)

	var lastErr error
	for modelIdx, model := range models {
		// Respect caller cancellation before opening the next model's request
		// budget. Without this, a deadline expiring mid-loop would spend one
		// extra doomed round-trip on the fallback and produce a misleading
		// "falling back ... after retries exhausted" log line (issue #179 P2).
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return AssistantTurn{}, fmt.Errorf("%w (last: %v)", err, lastErr)
			}
			return AssistantTurn{}, err
		}

		if modelIdx > 0 {
			log.Printf("[llm] falling back from %q to %q after retries exhausted: %s",
				models[modelIdx-1], model, truncateForLog(lastErr.Error(), 200))
		}

		hasFallback := modelIdx < len(models)-1
		turn, retry, err := c.chatOneModel(ctx, model, msgs, tools, hasFallback)
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

	// Preserve the single-model error contract when no fallback is configured
	// (log alerts / dashboards that pattern-match on "chat failed after N
	// attempts" continue to work). Only add the multi-model wrapper when
	// there actually was a cross-model attempt to describe.
	if len(models) == 1 {
		return AssistantTurn{}, lastErr
	}
	return AssistantTurn{}, fmt.Errorf("chat failed after exhausting %d model(s): %w", len(models), lastErr)
}

// chatOneModel runs the per-model retry loop for a single model identifier.
// Returns (turn, retry, err). `retry` mirrors doOnce's semantic and is true
// only when the loop exhausted its budget on retryable failures — signalling
// the caller that switching to a fallback model is meaningful. It is false
// on both success and terminal (non-retryable) failures.
//
// When hasFallback is true, the loop watches the parent context's deadline
// and bails out early (with retry=true) as soon as the remaining budget can
// no longer accommodate a full c.timeout attempt. Without this early exit,
// under the shipped defaults (AGENT_STEP_TIMEOUT=240s, LLM_TIMEOUT=180s) a
// slow primary would consume the entire step budget across 2 attempts and
// leave zero budget for the fallback — the exact "hanging upstream" failure
// mode LLM_FALLBACK_MODELS was introduced to mitigate (issue #179 P1).
func (c *Client) chatOneModel(ctx context.Context, model string, msgs []Message, tools []Tool, hasFallback bool) (AssistantTurn, bool, error) {
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
				// Parent cancelled during backoff. Signal retry=true so
				// Chat's outer loop still tries the fallback model (which
				// will re-check ctx and bail cleanly if the deadline is
				// truly exhausted). Distinguishes "we gave up on this
				// model" from "caller-side cancellation".
				return AssistantTurn{}, true, ctx.Err()
			case <-time.After(backoff):
			}

			// Deadline-aware early escalation, checked before every RETRY
			// (never the first attempt — the caller expects at least one
			// good-faith try on the primary). If we know we cannot fit
			// another full per-attempt c.timeout before the parent context
			// expires, and there is a fallback model waiting, stop
			// retrying this model now so the fallback gets a real budget.
			// This is the fix for the P1 identified in issue #179 review:
			// under 240s step / 180s per-attempt defaults, a hanging
			// primary would otherwise starve every fallback of budget.
			if hasFallback {
				if deadline, ok := ctx.Deadline(); ok {
					if time.Until(deadline) < c.timeout {
						if lastErr == nil {
							lastErr = fmt.Errorf("insufficient budget for another %s attempt on model %q", c.timeout, model)
						}
						return AssistantTurn{}, true, lastErr
					}
				}
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
		// Distinguish parent-context cancellation from a genuine transport
		// error. Parent cancellation means the caller is done — signal
		// non-retryable so Chat's outer loop can surface the cancel
		// verbatim instead of masquerading as an upstream retry (issue
		// #179 P2). Local timeouts (c.timeout on the child ctx) still
		// count as retryable transport failures.
		if ctx.Err() != nil {
			return AssistantTurn{}, false, ctx.Err()
		}
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
