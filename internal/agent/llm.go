package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmclient"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

// Client adapts Agent's multi-turn domain types to the shared LLM pipeline.
type Client struct {
	apiURL         string
	apiKey         string
	model          string
	fallbackModels []string
	timeout        time.Duration
	maxTokens      int
	temperature    float64
	enableThinking bool
	http           *http.Client
}

// NewClient constructs an agent LLM client. When fallbackModels is non-empty
// (typically sourced from LLM_FALLBACK_MODELS), Chat exhausts the primary
// model's per-model retry budget before switching to each fallback in order.
// Passing a nil / empty slice preserves the single-model behavior. See
// issue #179 for motivation.
func NewClient(apiURL, apiKey, model string, timeoutSec, maxTokens int, fallbackModels []string) *Client {
	return NewClientWithModelConfig(apiURL, apiKey, model, timeoutSec, maxTokens, fallbackModels, 0.3, false)
}

// NewClientWithModelConfig constructs an agent client using the same generation
// settings and model-specific compatibility rules as the worker LLM client.
func NewClientWithModelConfig(apiURL, apiKey, model string, timeoutSec, maxTokens int, fallbackModels []string, temperature float64, enableThinking bool) *Client {
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
		apiURL:         strings.TrimRight(apiURL, "/"),
		apiKey:         apiKey,
		model:          model,
		fallbackModels: fallbacks,
		timeout:        time.Duration(timeoutSec) * time.Second,
		maxTokens:      maxTokens,
		temperature:    temperature,
		enableThinking: enableThinking,
		http:           &http.Client{},
	}
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
	// Defense in depth for legacy histories and callers outside Runner.
	for _, message := range msgs {
		if invalid := invalidToolArguments(message.ToolCalls); invalid != nil {
			return AssistantTurn{}, invalid
		}
	}
	diagnostics := beginToolDiagnostics(ctx, msgs, tools)
	request := llmclient.Request{
		Messages:       toLLMMessages(msgs),
		Tools:          toLLMTools(tools),
		Temperature:    c.temperature,
		MaxTokens:      c.maxTokens,
		EnableThinking: c.enableThinking,
	}
	if len(tools) > 0 {
		request.ToolChoice = llmclient.ToolChoice{Mode: llmclient.ToolChoiceAuto}
	}
	shared := llmclient.New(c.apiURL, c.apiKey, c.model, c.fallbackModels, c.http)
	result, err := shared.Complete(ctx, request, llmclient.CallOptions{
		Path:                    llmfallback.PathAgentChat,
		AttemptTimeout:          c.timeout,
		PerModelTimeout:         c.timeout,
		MaxAttempts:             3,
		TerminalOnProtocolError: true,
		OnHTTPStatus:            diagnostics.httpStatus,
		ValidateResponse: func(_ string, response llmclient.Response) (llmfallback.Outcome, error) {
			choice := response.Choices[0]
			diagnostics.response(
				choice.Message.Content,
				fromLLMToolCalls(choice.Message.ToolCalls),
				choice.FinishReason,
				response.Usage.CompletionTokens,
			)
			if choice.FinishReason != "length" {
				return llmfallback.Success, nil
			}
			if len(choice.Message.ToolCalls) > 0 {
				return llmfallback.Terminal, fmt.Errorf("LLM tool response truncated: finish_reason=length")
			}
			if strings.TrimSpace(choice.Message.Content) == "" {
				return llmfallback.RetrySameModel, fmt.Errorf("LLM response truncated before producing content: finish_reason=length")
			}
			return llmfallback.Success, nil
		},
	})
	if err != nil {
		return AssistantTurn{}, err
	}
	choice := result.Response.Choices[0]
	toolCalls := fromLLMToolCalls(choice.Message.ToolCalls)
	// Set alongside the prose notice below, never instead of it: the notice tells
	// the reader where the text stops, the flag lets the runner record a fact the
	// model cannot edit away. See AssistantTurn.Truncated.
	truncated := false
	content := choice.Message.Content
	if choice.FinishReason == "length" {
		// A content-only turn is the user-facing answer. It is degraded but still
		// usable, so preserve it with an explicit disclosure instead of failing
		// the whole request and discarding everything the model produced.
		content += llmclient.TruncationNotice
		truncated = true
	}
	return AssistantTurn{
		Content:          content,
		ToolCalls:        toolCalls,
		Tokens:           result.Response.Usage.TotalTokens,
		CompletionTokens: result.Response.Usage.CompletionTokens,
		Truncated:        truncated,
	}, nil
}

func toLLMMessages(messages []Message) []llmclient.Message {
	converted := make([]llmclient.Message, len(messages))
	for i, message := range messages {
		converted[i] = llmclient.Message{
			Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID,
			Name: message.Name, ToolCalls: toLLMToolCalls(message.ToolCalls),
		}
	}
	return converted
}

func toLLMTools(tools []Tool) []llmclient.Tool {
	converted := make([]llmclient.Tool, len(tools))
	for i, tool := range tools {
		converted[i] = llmclient.Tool{Type: tool.Type, Function: llmclient.ToolFunction{
			Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters,
		}}
	}
	return converted
}

func toLLMToolCalls(calls []ToolCall) []llmclient.ToolCall {
	converted := make([]llmclient.ToolCall, len(calls))
	for i, call := range calls {
		converted[i].ID = call.ID
		converted[i].Type = call.Type
		converted[i].Function.Name = call.Function.Name
		converted[i].Function.Arguments = call.Function.Arguments
	}
	return converted
}

func fromLLMToolCalls(calls []llmclient.ToolCall) []ToolCall {
	converted := make([]ToolCall, len(calls))
	for i, call := range calls {
		converted[i].ID = call.ID
		converted[i].Type = call.Type
		converted[i].Function.Name = call.Function.Name
		converted[i].Function.Arguments = call.Function.Arguments
	}
	return converted
}
