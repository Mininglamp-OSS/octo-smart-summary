package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// Client 是 agent 自带的 LLM 客户端，独立于 service/llm.go，只依赖标准库。
type Client struct {
	apiURL    string
	apiKey    string
	model     string
	timeout   time.Duration
	maxTokens int
	http      *http.Client
}

func NewClient(apiURL, apiKey, model string, timeoutSec, maxTokens int) *Client {
	return &Client{
		apiURL:    apiURL,
		apiKey:    apiKey,
		model:     model,
		timeout:   time.Duration(timeoutSec) * time.Second,
		maxTokens: maxTokens,
		http:      &http.Client{},
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

// Chat 发起一次多轮回喂中的单跳请求。3 次指数退避重试：网络错/5xx/429 重试，
// 4xx(非429) 直接失败。每次尝试用带 c.timeout 的子 ctx 限时。
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error) {
	reqBody := chatRequest{
		Model:       c.model,
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
		return AssistantTurn{}, fmt.Errorf("marshal request: %w", err)
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避：1s, 2s；尊重外层 ctx 取消。
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			select {
			case <-ctx.Done():
				return AssistantTurn{}, ctx.Err()
			case <-time.After(backoff):
			}
		}

		turn, retry, err := c.doOnce(ctx, payload)
		if err == nil {
			return turn, nil
		}
		lastErr = err
		if !retry {
			return AssistantTurn{}, err
		}
	}
	return AssistantTurn{}, fmt.Errorf("chat failed after %d attempts: %w", maxAttempts, lastErr)
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
