package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

const MapFailedMarker = "总结失败"

const kimiRequiredTemperature = 0.6

const maxLLMErrorBodyBytes = 4096

// ErrReasoningBudgetExhausted marks a response whose reasoning consumed the
// output budget before producing user-visible content.
var ErrReasoningBudgetExhausted = errors.New("LLM returned empty content: reasoning consumed entire max_tokens budget")

// ErrTokenLimitExhausted marks an empty response terminated by the token cap.
var ErrTokenLimitExhausted = errors.New("LLM returned empty content due to token limit")

type tokenLimitError struct {
	message string
}

func (e *tokenLimitError) Error() string { return e.message }

// Is preserves the token-limit identity introduced on main while allowing
// callers to distinguish streaming from non-streaming truncation.
func (e *tokenLimitError) Is(target error) bool { return target == ErrTokenLimitExhausted }

var (
	ErrOutputTruncated       = &tokenLimitError{message: "LLM response truncated due to token limit"}
	ErrStreamOutputTruncated = &tokenLimitError{message: "LLM streamed response truncated due to token limit"}
)

// LLMClient handles calls to a chat-completions-compatible LLM API.
type LLMClient struct {
	apiURL          string
	apiKey          string
	model           string
	fallbackModels  []string
	timeout         time.Duration
	toolCallTimeout time.Duration
	maxTokens       int
	enableThinking  bool
	client          *http.Client
}

// NewLLMClient creates a new LLM client.
//
// fallbackModels (typically sourced from LLM_FALLBACK_MODELS) are tried in
// order when the primary model fails in a way a different model/provider may
// survive — 429/5xx (after retries) and, critically, 403 account-level denials
// such as a Bedrock SCP explicit-deny, which the gateway can route to
// a different provider (issue #211). A nil/empty slice preserves single-model
// behaviour. The primary model, empty entries and duplicates are dropped.
func NewLLMClient(apiURL, apiKey, model string, timeoutSec, maxTokens int, enableThinking bool, toolCallTimeoutSec int, fallbackModels []string) *LLMClient {
	fallbacks := make([]string, 0, len(fallbackModels))
	seen := map[string]bool{model: true}
	for _, m := range fallbackModels {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		fallbacks = append(fallbacks, m)
	}
	return &LLMClient{
		apiURL:          strings.TrimRight(apiURL, "/"),
		apiKey:          apiKey,
		model:           model,
		fallbackModels:  fallbacks,
		timeout:         time.Duration(timeoutSec) * time.Second,
		toolCallTimeout: time.Duration(toolCallTimeoutSec) * time.Second,
		maxTokens:       maxTokens,
		enableThinking:  enableThinking,
		client:          &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
	}
}

// models returns the ordered model list (primary first, then fallbacks) used
// by every fallback-aware call path.
func (c *LLMClient) models() []string {
	return append([]string{c.model}, c.fallbackModels...)
}

// ChatMessage represents a single message in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToolFunction describes an OpenAI function calling tool definition.
type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// Tool wraps ToolFunction in the OpenAI tool format.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolChoice forces the LLM to call a specific function.
type ToolChoice struct {
	Type     string             `json:"type"`
	Function ToolChoiceFunction `json:"function"`
}

// ToolChoiceFunction specifies the function name for tool_choice.
type ToolChoiceFunction struct {
	Name string `json:"name"`
}

// ThinkingParam controls the thinking/reasoning behavior for supported models.
type ThinkingParam struct {
	Type string `json:"type"` // "enabled" or "disabled"
}

type chatRequestWithTools struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Tools       []Tool        `json:"tools"`
	// ToolChoice controls function calling behavior.
	// For Kimi models: string "auto" (Kimi does not support forced function calling).
	// For other models: ToolChoice struct with Type="function" and Function specification.
	ToolChoice         interface{}            `json:"tool_choice"`
	ChatTemplateKwargs map[string]interface{} `json:"chat_template_kwargs,omitempty"`
	Thinking           *ThinkingParam         `json:"thinking,omitempty"`
	Stream             bool                   `json:"stream,omitempty"`
}

type chatResponseWithTools struct {
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequest struct {
	Model              string                 `json:"model"`
	Messages           []ChatMessage          `json:"messages"`
	Temperature        float64                `json:"temperature"`
	MaxTokens          int                    `json:"max_tokens"`
	ChatTemplateKwargs map[string]interface{} `json:"chat_template_kwargs,omitempty"`
	Thinking           *ThinkingParam         `json:"thinking,omitempty"`
	Stream             bool                   `json:"stream,omitempty"`
	StreamOptions      *streamOptions         `json:"stream_options,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		TotalTokens      int `json:"total_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// buildThinkingConfig returns model-specific thinking parameters.
// For Kimi: top-level thinking field. For Qwen/DeepSeek: chat_template_kwargs.
func (c *LLMClient) buildThinkingConfig(model string) (*ThinkingParam, map[string]interface{}) {
	if c.enableThinking {
		return nil, nil
	}
	if config.IsKimiModel(model) {
		return &ThinkingParam{Type: "disabled"}, nil
	}
	if config.IsQwenOrDeepSeekModel(model) {
		return nil, map[string]interface{}{"enable_thinking": false}
	}
	return nil, nil
}

func readErrorBody(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, maxLLMErrorBodyBytes))
	return llmfallback.SafeTextForLog(string(b), 200)
}

// TruncationNotice is the single disclosure wording used whenever a degraded but
// usable length-truncated result is handed to the user instead of being
// discarded. Streaming and non-streaming paths share one convention.
const TruncationNotice = "\n\n> 输出因长度限制被截断，请缩小范围或降低详细程度后重试。"

type truncationPolicy int

const (
	truncateAllow truncationPolicy = iota
	truncateReject
	truncateDisclose
)

// Call makes a general chat completion request. A non-empty content response
// remains usable when the provider reports finish_reason=length, preserving the
// pre-existing behavior for refine and topic-narrowing callers.
func (c *LLMClient) Call(ctx context.Context, messages []ChatMessage, temperature float64) (string, int, error) {
	content, _, tokens, _, err := c.callWithPolicyAndModel(ctx, messages, temperature, truncateAllow)
	return content, tokens, err
}

// CallStrict rejects any length-truncated response. Map/Reduce callers use this
// path because a partial intermediate result can silently drop coverage.
func (c *LLMClient) CallStrict(ctx context.Context, messages []ChatMessage, temperature float64) (string, int, error) {
	content, _, tokens, _, err := c.callWithPolicyAndModel(ctx, messages, temperature, truncateReject)
	return content, tokens, err
}

// CallDisclosingTruncation keeps a usable terminal result and marks it as
// degraded; empty truncation still fails because there is nothing to deliver.
func (c *LLMClient) CallDisclosingTruncation(ctx context.Context, messages []ChatMessage, temperature float64) (content string, truncated bool, tokens int, err error) {
	content, truncated, tokens, _, err = c.callWithPolicyAndModel(ctx, messages, temperature, truncateDisclose)
	return content, truncated, tokens, err
}

// CallWithModel makes a chat completion request and also returns the model
// that produced the response. Callers that persist model attribution should
// use this method instead of ModelVersion.
func (c *LLMClient) CallWithModel(ctx context.Context, messages []ChatMessage, temperature float64) (string, int, string, error) {
	content, _, tokens, usedModel, err := c.callWithPolicyAndModel(ctx, messages, temperature, truncateAllow)
	return content, tokens, usedModel, err
}

func (c *LLMClient) callWithPolicyAndModel(ctx context.Context, messages []ChatMessage, temperature float64, policy truncationPolicy) (string, bool, int, string, error) {
	type result struct {
		content   string
		truncated bool
		tokens    int
	}
	// Keep retry ownership in the shared runner: exhaust transient failures on
	// one model before switching to the next model.
	res, usedModel, err := llmfallback.Run(ctx, llmfallback.Config{
		Models:      c.models(),
		MaxAttempts: 3,
	}, func(ctx context.Context, model string) (result, llmfallback.Outcome, error) {
		temp := temperature
		if config.IsKimiModel(model) {
			temp = kimiRequiredTemperature
		}
		log.Printf("[llm] calling model=%s temperature=%.2f max_tokens=%d", model, temp, c.maxTokens)
		reqBody := chatRequest{
			Model:       model,
			Messages:    messages,
			Temperature: temp,
			MaxTokens:   c.maxTokens,
		}
		thinking, kwargs := c.buildThinkingConfig(model)
		reqBody.Thinking = thinking
		reqBody.ChatTemplateKwargs = kwargs

		body, err := json.Marshal(reqBody)
		if err != nil {
			return result{}, llmfallback.Terminal, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return result{}, llmfallback.Terminal, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return result{}, llmfallback.Terminal, ctx.Err()
			}
			return result{}, llmfallback.RetrySameModel, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return result{}, llmfallback.ClassifyNonOKStatus(resp.StatusCode),
				fmt.Errorf("LLM API error: status=%d body=%s", resp.StatusCode, readErrorBody(resp.Body))
		}
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			if ctx.Err() != nil {
				return result{}, llmfallback.Terminal, ctx.Err()
			}
			return result{}, llmfallback.RetrySameModel, err
		}
		var chatResp chatResponse
		if err := json.Unmarshal(respBody, &chatResp); err != nil {
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("unmarshal LLM response: %w", err)
		}
		if len(chatResp.Choices) == 0 {
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("LLM returned no choices")
		}
		content := chatResp.Choices[0].Message.Content
		reasoningPresent := chatResp.Choices[0].Message.ReasoningContent != "" || chatResp.Choices[0].Message.Reasoning != ""
		if strings.TrimSpace(content) == "" && reasoningPresent {
			reasoningLen := len(chatResp.Choices[0].Message.ReasoningContent) + len(chatResp.Choices[0].Message.Reasoning)
			log.Printf("[llm] WARNING: content is empty but reasoning present (%d chars), finish_reason=%s, completion_tokens=%d. Reasoning consumed entire budget.",
				reasoningLen, chatResp.Choices[0].FinishReason, chatResp.Usage.CompletionTokens)
			return result{tokens: chatResp.Usage.TotalTokens}, llmfallback.Terminal, ErrReasoningBudgetExhausted
		}
		if chatResp.Choices[0].FinishReason == "length" {
			log.Printf("[llm] WARNING: response truncated with finish_reason=length, completion_tokens=%d", chatResp.Usage.CompletionTokens)
			if strings.TrimSpace(content) == "" || policy == truncateReject {
				return result{tokens: chatResp.Usage.TotalTokens}, llmfallback.Terminal, ErrOutputTruncated
			}
			return result{content: content, truncated: true, tokens: chatResp.Usage.TotalTokens}, llmfallback.Success, nil
		}
		return result{content: content, tokens: chatResp.Usage.TotalTokens}, llmfallback.Success, nil
	})
	return res.content, res.truncated, res.tokens, usedModel, err
}

type chatStreamResponse struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		TotalTokens      int `json:"total_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// CallStream makes a general streaming request and preserves the historical
// behavior for non-empty length-truncated content: what was already delivered
// remains the returned/persisted source of truth.
func (c *LLMClient) CallStream(ctx context.Context, messages []ChatMessage, temperature float64, onDelta func(string) error) (string, int, error) {
	content, tokens, _, err := c.callStreamWithModel(ctx, messages, temperature, onDelta, false)
	return content, tokens, err
}

// callStreamWithTruncationNotice is used by user-visible Map/Reduce streams.
// Once deltas have been emitted they cannot be retracted, so a usable partial
// result is completed with an explicit notice instead of returning an error
// after the client has already rendered content.
func (c *LLMClient) callStreamWithTruncationNotice(ctx context.Context, messages []ChatMessage, temperature float64, onDelta func(string) error) (string, int, error) {
	content, tokens, _, err := c.callStreamWithModel(ctx, messages, temperature, onDelta, true)
	return content, tokens, err
}

// CallStreamWithModel is CallStream with the actual producing model included
// for persistence and audit attribution.
func (c *LLMClient) CallStreamWithModel(ctx context.Context, messages []ChatMessage, temperature float64, onDelta func(string) error) (string, int, string, error) {
	return c.callStreamWithModel(ctx, messages, temperature, onDelta, false)
}

func (c *LLMClient) callStreamWithModel(ctx context.Context, messages []ChatMessage, temperature float64, onDelta func(string) error, discloseTruncation bool) (string, int, string, error) {
	type result struct {
		content string
		tokens  int
	}
	// Cross-model fallback is only safe BEFORE the stream starts emitting: once
	// onDelta receives content we are committed to that model, because switching
	// would double-emit. Failures after HTTP 200 but before the first delivered
	// delta may still try the next model. Transient failures exhaust the current
	// model's retry budget before switching models.
	res, usedModel, err := llmfallback.Run(ctx, llmfallback.Config{
		Models:      c.models(),
		MaxAttempts: 3,
	}, func(ctx context.Context, model string) (result, llmfallback.Outcome, error) {
		temp := temperature
		if config.IsKimiModel(model) {
			temp = kimiRequiredTemperature
		}
		log.Printf("[llm] streaming model=%s temperature=%.2f max_tokens=%d", model, temp, c.maxTokens)

		reqBody := chatRequest{
			Model:         model,
			Messages:      messages,
			Temperature:   temp,
			MaxTokens:     c.maxTokens,
			Stream:        true,
			StreamOptions: &streamOptions{IncludeUsage: true},
		}
		thinking, kwargs := c.buildThinkingConfig(model)
		reqBody.Thinking = thinking
		reqBody.ChatTemplateKwargs = kwargs

		body, err := json.Marshal(reqBody)
		if err != nil {
			return result{}, llmfallback.Terminal, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return result{}, llmfallback.Terminal, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := c.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return result{}, llmfallback.Terminal, ctx.Err()
			}
			return result{}, llmfallback.RetrySameModel, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return result{}, llmfallback.ClassifyNonOKStatus(resp.StatusCode),
				fmt.Errorf("LLM stream API error: status=%d body=%s", resp.StatusCode, readErrorBody(resp.Body))
		}

		var sb strings.Builder
		var totalTokens int
		var reasoningLen int
		emitted := false
		terminalSeen := false
		finishReason := ""
		streamFailure := func(err error) (result, llmfallback.Outcome, error) {
			res := result{content: sb.String(), tokens: totalTokens}
			if emitted {
				return res, llmfallback.Terminal, err
			}
			return res, llmfallback.RetrySameModel, err
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				terminalSeen = true
				break
			}
			var chunk chatStreamResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return streamFailure(fmt.Errorf("unmarshal LLM stream chunk: %w", err))
			}
			if chunk.Usage.TotalTokens > 0 {
				totalTokens = chunk.Usage.TotalTokens
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			if chunk.Choices[0].FinishReason != "" {
				terminalSeen = true
				finishReason = chunk.Choices[0].FinishReason
			}
			delta := chunk.Choices[0].Delta.Content
			if delta == "" {
				reasoningLen += len(chunk.Choices[0].Delta.ReasoningContent) + len(chunk.Choices[0].Delta.Reasoning)
				continue
			}
			sb.WriteString(delta)
			if onDelta != nil {
				emitted = true
				if err := onDelta(delta); err != nil {
					return result{content: sb.String(), tokens: totalTokens}, llmfallback.Terminal, err
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return streamFailure(fmt.Errorf("read LLM stream: %w", err))
		}
		if !terminalSeen {
			return streamFailure(fmt.Errorf("LLM stream ended without terminal marker"))
		}
		content := sb.String()
		if strings.TrimSpace(content) == "" && reasoningLen > 0 {
			return result{tokens: totalTokens}, llmfallback.Terminal, ErrReasoningBudgetExhausted
		}
		if strings.TrimSpace(content) == "" {
			if finishReason == "length" {
				return result{tokens: totalTokens}, llmfallback.Terminal, ErrStreamOutputTruncated
			}
			return streamFailure(fmt.Errorf("LLM returned empty streamed content"))
		}
		if finishReason == "length" && discloseTruncation {
			if onDelta != nil {
				if err := onDelta(TruncationNotice); err != nil {
					return result{content: content, tokens: totalTokens}, llmfallback.Terminal, err
				}
			}
			content += TruncationNotice
		}
		return result{content: content, tokens: totalTokens}, llmfallback.Success, nil
	})
	return res.content, res.tokens, usedModel, err
}

// CallRaw is a simple single-turn call returning text only. Used for topic narrowing.
func (c *LLMClient) CallRaw(ctx context.Context, prompt string) (string, error) {
	content, _, err := c.Call(ctx, []ChatMessage{{Role: "user", Content: prompt}}, 0.3)
	if err != nil {
		log.Printf("[llm] CallRaw failed: %s", llmfallback.SafeErrorForLog(err, 200))
		return "[]", nil
	}
	return content, nil
}

// CallWithTools makes a chat completion request with function calling.
// Returns the raw JSON string from tool_calls[0].function.arguments and token count.
func (c *LLMClient) CallWithTools(ctx context.Context, messages []ChatMessage, tools []Tool, forceFn string, temperature float64) (string, int, error) {
	type result struct {
		args   string
		tokens int
	}
	start := time.Now()
	// MaxAttempts:3 preserves CallWithTools' original per-model retry budget;
	// the runner adds cross-model fallback once those are exhausted (or on a
	// 403 account denial, which escalates immediately — issue #211).
	res, _, err := llmfallback.Run(ctx, llmfallback.Config{
		Models:          c.models(),
		PerModelTimeout: c.toolCallTimeout,
		MaxAttempts:     3,
	}, func(ctx context.Context, model string) (result, llmfallback.Outcome, error) {
		temp := temperature
		if config.IsKimiModel(model) {
			temp = kimiRequiredTemperature
		}
		log.Printf("[llm] CallWithTools: tool=%s temperature=%.2f model=%s", forceFn, temp, model)

		var toolChoice interface{}
		if config.IsKimiModel(model) {
			toolChoice = "auto"
		} else {
			toolChoice = ToolChoice{Type: "function", Function: ToolChoiceFunction{Name: forceFn}}
		}
		reqBody := chatRequestWithTools{
			Model:       model,
			Messages:    messages,
			Temperature: temp,
			MaxTokens:   c.maxTokens,
			Tools:       tools,
			ToolChoice:  toolChoice,
		}
		thinking, kwargs := c.buildThinkingConfig(model)
		reqBody.Thinking = thinking
		reqBody.ChatTemplateKwargs = kwargs

		body, err := json.Marshal(reqBody)
		if err != nil {
			return result{}, llmfallback.Terminal, fmt.Errorf("marshal request: %w", err)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, c.toolCallTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return result{}, llmfallback.Terminal, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return result{}, llmfallback.Terminal, ctx.Err()
			}
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("network error: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			errBody := readErrorBody(resp.Body)
			resp.Body.Close()
			return result{}, llmfallback.ClassifyNonOKStatus(resp.StatusCode),
				fmt.Errorf("LLM API error: status=%d body=%s", resp.StatusCode, errBody)
		}
		respBody, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			if ctx.Err() != nil {
				return result{}, llmfallback.Terminal, ctx.Err()
			}
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("read response body: %w", rerr)
		}

		var chatResp chatResponseWithTools
		if err := json.Unmarshal(respBody, &chatResp); err != nil {
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("unmarshal response: %w", err)
		}
		if len(chatResp.Choices) == 0 {
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("LLM returned no choices")
		}
		if chatResp.Choices[0].FinishReason == "length" {
			return result{tokens: chatResp.Usage.TotalTokens}, llmfallback.Terminal,
				fmt.Errorf("LLM tool response truncated due to token limit")
		}
		if len(chatResp.Choices[0].Message.ToolCalls) == 0 {
			reasoningPresent := chatResp.Choices[0].Message.ReasoningContent != "" || chatResp.Choices[0].Message.Reasoning != ""
			if reasoningPresent {
				return result{tokens: chatResp.Usage.TotalTokens}, llmfallback.Terminal,
					fmt.Errorf("CallWithTools: reasoning consumed entire max_tokens budget, no tool_calls produced")
			}
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("LLM returned no tool_calls")
		}

		if config.IsKimiModel(model) {
			for _, tc := range chatResp.Choices[0].Message.ToolCalls {
				if tc.Function.Name == forceFn {
					if tc.Function.Arguments == "" {
						return result{}, llmfallback.RetrySameModel, fmt.Errorf("LLM returned empty arguments")
					}
					return result{args: tc.Function.Arguments, tokens: chatResp.Usage.TotalTokens}, llmfallback.Success, nil
				}
			}
			calledFn := chatResp.Choices[0].Message.ToolCalls[0].Function.Name
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("LLM called function %q instead of expected %q", calledFn, forceFn)
		}

		calledFn := chatResp.Choices[0].Message.ToolCalls[0].Function.Name
		if calledFn != forceFn {
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("LLM called function %q instead of expected %q", calledFn, forceFn)
		}
		args := chatResp.Choices[0].Message.ToolCalls[0].Function.Arguments
		if args == "" {
			return result{}, llmfallback.RetrySameModel, fmt.Errorf("LLM returned empty arguments")
		}
		return result{args: args, tokens: chatResp.Usage.TotalTokens}, llmfallback.Success, nil
	})
	if err != nil {
		log.Printf("[llm] CallWithTools: tool=%s took %dms error=%s", forceFn, time.Since(start).Milliseconds(), llmfallback.SafeErrorForLog(err, 200))
		return "", 0, fmt.Errorf("CallWithTools failed: %w", err)
	}
	log.Printf("[llm] CallWithTools: tool=%s took %dms tokens=%d", forceFn, time.Since(start).Milliseconds(), res.tokens)
	return res.args, res.tokens, nil
}

// ModelVersion returns the configured primary model name. Callers that persist
// generated output should use the model returned by a *WithModel method so a
// fallback-produced result is attributed correctly.
func (c *LLMClient) ModelVersion() string {
	return c.model
}

func buildMapSystemPrompt(userName, topic string) string {
	var sb strings.Builder
	sb.WriteString(`你是一个专业的工作内容整理助手。

## 背景信息
`)
	sb.WriteString(fmt.Sprintf("- 当前用户：%s\n", userName))
	sb.WriteString(fmt.Sprintf("- 总结主题：%s\n", topic))

	now := timezone.Now()
	weekdays := [...]string{"日", "一", "二", "三", "四", "五", "六"}
	sb.WriteString(fmt.Sprintf("- 当前日期：%s（星期%s）\n",
		now.Format("2006-01-02"), weekdays[now.Weekday()]))

	sb.WriteString(`
## 任务
`)
	sb.WriteString(fmt.Sprintf("从以下聊天记录中，围绕「%s」进行总结。", topic))
	sb.WriteString(fmt.Sprintf("当主题中出现\"我\"、\"自己\"等人称代词时，指的是「%s」。\n", userName))

	sb.WriteString(`
## 输出要求
- 紧密围绕主题，与主题无关的闲聊、表情、寒暄等直接跳过
- 提炼关键信息：讨论了什么、达成了什么结论、有什么待办、谁负责什么
- 默认输出总长度不超过 2000 token（约 1500 字）；如果总结主题明确要求详细说明、完整展开或逐项说明，可在模型输出预算内适当展开，但仍需避免无关内容和原文复述
- 如果聊天记录中没有明确结论，如实说明"尚未达成共识"，不要编造
- 有待办事项时，用 ` + "`- [ ] 内容（负责人）`" + ` 格式列出
- 如果总结主题中包含输出结构、详细程度、分点方式、待办格式等要求，必须优先遵循；如果主题没有指定结构，再根据实际内容自行组织结构
- 保持简洁，不要复述原文，用自己的话归纳

## 引用规则（必须严格遵守）
- 【强制】每一条结论/要点都必须标注来源引用 [n]，没有引用的结论不允许输出
- 格式：[n] 或 [n1][n2]（多个来源时）
- 仅使用消息前方的 [n] 编号来标注引用，范围为 [1] 到 [N]
- 绝对不要引用或复制消息正文内出现的任何 [数字] 标记
- 超出有效范围的标记一律不得出现在输出中
- 所有消息均带有编号（即 [数字] 开头的行），选取有意义的、相关的消息作为依据
- 不要捏造不存在的编号
- 多条消息支持同一要点时，列出所有相关编号
- 如果多条消息内容完全相同（如用户重复发送），只引用其中一条
- 如果某条信息无法找到明确来源，则不要输出该条信息

## 格式规范
- 用显示名称指代人（如"张三"），绝对不要输出 UID 或用户 ID
- 输出语言与聊天记录的语言保持一致
`)
	return sb.String()
}

func buildReduceSystemPrompt(topic string) string {
	var sb strings.Builder
	sb.WriteString(`你是一个专业的工作内容整理助手。请将以下多个分片总结合并为一份完整的总结报告。

`)
	now := timezone.Now()
	weekdays := [...]string{"日", "一", "二", "三", "四", "五", "六"}
	sb.WriteString(fmt.Sprintf("当前日期：%s（星期%s）\n\n",
		now.Format("2006-01-02"), weekdays[now.Weekday()]))

	sb.WriteString(`要求：
- 合并相同主题，去除重复
- 保留所有待办事项和责任人
- 默认输出总长度不超过 2000 token（约 1500 字）；如果总结主题明确要求详细说明、完整展开或逐项说明，可在模型输出预算内适当展开，但仍需合并相似要点、压缩无关细节
- 如有冲突信息，保留最新的
- 保留所有 [n] 引用标记，不要删除或修改
- 合并相同要点时，合并其引用编号
- 如果总结主题中包含输出结构、详细程度、分点方式、待办格式等要求，必须优先遵循；如果主题没有指定结构，再根据实际内容自行组织结构
- 用显示名称指代人，绝对不要输出 UID 或用户 ID
- 输出语言与输入语言保持一致

引用规则：
- 仅使用分片总结中已有的 [n] 编号，不要引入新编号
- 绝对不要引用或复制正文内出现的任何 [数字] 标记
- 超出有效范围的标记一律不得出现在输出中
`)
	if topic != "" {
		sb.WriteString(fmt.Sprintf("\n重要：总结主题是「%s」，请只保留与该主题相关的条目，移除不相关内容。\n", topic))
	}
	return sb.String()
}

// CallMap runs the Map phase for a message chunk.
func (c *LLMClient) CallMap(ctx context.Context, formattedMessages string, sourceName string, chunkIndex int, msgCount int, timeStart, timeEnd string, topic string, userName string) (string, int, error) {
	content, tokens, _, err := c.CallMapWithModel(ctx, formattedMessages, sourceName, chunkIndex, msgCount, timeStart, timeEnd, topic, userName)
	return content, tokens, err
}

// CallMapWithModel is CallMap with the actual producing model included.
func (c *LLMClient) CallMapWithModel(ctx context.Context, formattedMessages string, sourceName string, chunkIndex int, msgCount int, timeStart, timeEnd string, topic string, userName string) (string, int, string, error) {
	if strings.TrimSpace(formattedMessages) == "" {
		return "(该时段无文本消息)", 0, c.model, nil
	}

	systemPrompt := buildMapSystemPrompt(userName, topic)

	userPrompt := fmt.Sprintf("来源：%s\n时间范围：%s ~ %s\n消息数：%d 条\n\n聊天记录：\n%s",
		sourceName, timeStart, timeEnd, msgCount, formattedMessages)

	content, _, tokens, usedModel, err := c.callWithPolicyAndModel(ctx, []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, 0.1, truncateReject)
	if err == nil {
		return content, tokens, usedModel, nil
	}
	log.Printf("[llm] Map chunk %d failed: %s", chunkIndex, llmfallback.SafeErrorForLog(err, 200))
	if errors.Is(err, ErrOutputTruncated) {
		return "", tokens, usedModel, fmt.Errorf("output truncated on chunk %d: %w", chunkIndex, err)
	}
	if errors.Is(err, ErrReasoningBudgetExhausted) {
		return "", tokens, usedModel, fmt.Errorf("reasoning budget exhausted on chunk %d: %w", chunkIndex, err)
	}
	return fmt.Sprintf("(分片 %d %s)", chunkIndex, MapFailedMarker), 0, c.model, nil
}

// CallReduce runs the Reduce phase to merge chunk summaries.
func (c *LLMClient) CallReduce(ctx context.Context, chunkSummaries []string, sourceNames string, startTime, endTime string, totalMsgCount int, topic string) (string, int, error) {
	content, tokens, _, err := c.CallReduceWithModel(ctx, chunkSummaries, sourceNames, startTime, endTime, totalMsgCount, topic)
	return content, tokens, err
}

// CallReduceWithModel is CallReduce with the actual producing model included.
func (c *LLMClient) CallReduceWithModel(ctx context.Context, chunkSummaries []string, sourceNames string, startTime, endTime string, totalMsgCount int, topic string) (string, int, string, error) {
	if len(chunkSummaries) == 1 {
		return chunkSummaries[0], 0, c.model, nil
	}

	var parts []string
	for i, s := range chunkSummaries {
		parts = append(parts, fmt.Sprintf("【分片 %d】\n%s", i+1, s))
	}
	summariesText := strings.Join(parts, "\n\n---\n\n")
	system := buildReduceSystemPrompt(topic)

	userPrompt := fmt.Sprintf("信息来源：%s\n时间范围：%s ~ %s\n消息总量：%d 条\n\n以下是各分片的总结，请合并：\n\n%s",
		sourceNames, startTime, endTime, totalMsgCount, summariesText)

	return c.callDisclosingTerminalReduceWithModel(ctx, []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: userPrompt},
	})
}

func (c *LLMClient) callDisclosingTerminalReduceWithModel(ctx context.Context, msgs []ChatMessage) (string, int, string, error) {
	content, truncated, tokens, usedModel, err := c.callWithPolicyAndModel(ctx, msgs, 0.1, truncateDisclose)
	if err != nil {
		return "", tokens, usedModel, err
	}
	if truncated {
		content += TruncationNotice
	}
	return content, tokens, usedModel, nil
}

// CallMapStream runs the Map phase for a user-visible single chunk and streams
// the generated summary deltas.
func (c *LLMClient) CallMapStream(ctx context.Context, formattedMessages string, sourceName string, chunkIndex int, msgCount int, timeStart, timeEnd string, topic string, userName string, onDelta func(string) error) (string, int, error) {
	content, tokens, _, err := c.CallMapStreamWithModel(ctx, formattedMessages, sourceName, chunkIndex, msgCount, timeStart, timeEnd, topic, userName, onDelta)
	return content, tokens, err
}

// CallMapStreamWithModel is CallMapStream with the actual producing model included.
func (c *LLMClient) CallMapStreamWithModel(ctx context.Context, formattedMessages string, sourceName string, chunkIndex int, msgCount int, timeStart, timeEnd string, topic string, userName string, onDelta func(string) error) (string, int, string, error) {
	if strings.TrimSpace(formattedMessages) == "" {
		return "(该时段无文本消息)", 0, c.model, nil
	}
	systemPrompt := buildMapSystemPrompt(userName, topic)
	userPrompt := fmt.Sprintf("来源：%s\n时间范围：%s ~ %s\n消息数：%d 条\n\n聊天记录：\n%s",
		sourceName, timeStart, timeEnd, msgCount, formattedMessages)

	var emitted bool
	wrappedDelta := func(delta string) error {
		emitted = true
		if onDelta == nil {
			return nil
		}
		return onDelta(delta)
	}
	content, tokens, usedModel, err := c.callStreamWithModel(ctx, []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, 0.1, wrappedDelta, true)
	if err == nil {
		return content, tokens, usedModel, nil
	}
	log.Printf("[llm] Stream Map chunk %d failed: %s", chunkIndex, llmfallback.SafeErrorForLog(err, 200))
	if emitted {
		return content, tokens, usedModel, err
	}
	if errors.Is(err, ErrStreamOutputTruncated) {
		return "", tokens, usedModel, fmt.Errorf("output truncated on chunk %d: %w", chunkIndex, err)
	}
	if errors.Is(err, ErrReasoningBudgetExhausted) {
		return "", tokens, usedModel, fmt.Errorf("reasoning budget exhausted on chunk %d: %w", chunkIndex, err)
	}
	return fmt.Sprintf("(分片 %d %s)", chunkIndex, MapFailedMarker), 0, c.model, nil
}

// CallReduceStream merges chunk summaries and streams the final user-visible
// reduced summary.
func (c *LLMClient) CallReduceStream(ctx context.Context, chunkSummaries []string, sourceNames string, startTime, endTime string, totalMsgCount int, topic string, onDelta func(string) error) (string, int, error) {
	content, tokens, _, err := c.CallReduceStreamWithModel(ctx, chunkSummaries, sourceNames, startTime, endTime, totalMsgCount, topic, onDelta)
	return content, tokens, err
}

// CallReduceStreamWithModel is CallReduceStream with the actual producing model included.
func (c *LLMClient) CallReduceStreamWithModel(ctx context.Context, chunkSummaries []string, sourceNames string, startTime, endTime string, totalMsgCount int, topic string, onDelta func(string) error) (string, int, string, error) {
	if len(chunkSummaries) == 1 {
		if onDelta != nil && chunkSummaries[0] != "" {
			_ = onDelta(chunkSummaries[0])
		}
		return chunkSummaries[0], 0, c.model, nil
	}

	var parts []string
	for i, s := range chunkSummaries {
		parts = append(parts, fmt.Sprintf("【分片 %d】\n%s", i+1, s))
	}
	summariesText := strings.Join(parts, "\n\n---\n\n")
	system := buildReduceSystemPrompt(topic)
	userPrompt := fmt.Sprintf("信息来源：%s\n时间范围：%s ~ %s\n消息总量：%d 条\n\n以下是各分片的总结，请合并：\n\n%s",
		sourceNames, startTime, endTime, totalMsgCount, summariesText)

	return c.callStreamWithModel(ctx, []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: userPrompt},
	}, 0.1, onDelta, true)
}

func buildReduceByPersonMessages(participantSummaries []struct{ Name, Summary string }, startTime, endTime string, topic string) []ChatMessage {
	var parts []string
	for i, ps := range participantSummaries {
		parts = append(parts, fmt.Sprintf("[P%d]【%s 的工作总结】\n%s", i+1, ps.Name, ps.Summary))
	}
	text := strings.Join(parts, "\n\n---\n\n")

	system := `你是专业的工作汇报整理助手，请将多位成员的工作总结合并为团队整体总结报告。

要求：
- 合并相同主题，去除重复
- 保留所有待办事项和责任人
- 每个要点末尾必须标注来源成员编号，格式为 [Pn]，例如 [P1]、[P2]
- 多位成员贡献同一要点时，列出所有编号，如 [P1][P3]
- 只引用真实存在的编号，不要捏造
- 如果总结主题中包含输出结构、详细程度、分点方式、待办格式等要求，必须优先遵循；如果主题没有指定结构，再根据实际内容自行组织结构
- 用显示名称指代人，绝对不要输出 UID 或用户 ID`

	return []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: fmt.Sprintf("总结主题：%s\n时间范围：%s ~ %s\n\n%s", topic, startTime, endTime, text)},
	}
}

// CallReduceByPerson merges participant-level summaries.
// Each participant is assigned a [Pn] tag that the LLM should reference in the output.
func (c *LLMClient) CallReduceByPerson(ctx context.Context, participantSummaries []struct{ Name, Summary string }, startTime, endTime string, topic string) (string, int, error) {
	content, tokens, _, err := c.callDisclosingTerminalReduceWithModel(ctx, buildReduceByPersonMessages(participantSummaries, startTime, endTime, topic))
	return content, tokens, err
}

// CallReduceByPersonWithModel is CallReduceByPerson with the actual producing model included.
func (c *LLMClient) CallReduceByPersonWithModel(ctx context.Context, participantSummaries []struct{ Name, Summary string }, startTime, endTime string, topic string) (string, int, string, error) {
	return c.callDisclosingTerminalReduceWithModel(ctx, buildReduceByPersonMessages(participantSummaries, startTime, endTime, topic))
}

// CallReduceByPersonStream merges participant-level summaries and streams the
// final team summary. Each participant is assigned a [Pn] tag that the LLM should
// reference in the output.
func (c *LLMClient) CallReduceByPersonStream(ctx context.Context, participantSummaries []struct{ Name, Summary string }, startTime, endTime string, topic string, onDelta func(string) error) (string, int, error) {
	return c.callStreamWithTruncationNotice(ctx, buildReduceByPersonMessages(participantSummaries, startTime, endTime, topic), 0.1, onDelta)
}

// CallReduceByPersonStreamWithModel is CallReduceByPersonStream with the actual producing model included.
func (c *LLMClient) CallReduceByPersonStreamWithModel(ctx context.Context, participantSummaries []struct{ Name, Summary string }, startTime, endTime string, topic string, onDelta func(string) error) (string, int, string, error) {
	return c.callStreamWithModel(ctx, buildReduceByPersonMessages(participantSummaries, startTime, endTime, topic), 0.1, onDelta, true)
}
