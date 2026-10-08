package llmclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmcompat"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

const (
	DefaultMaxResponseBytes int64 = 16 << 20
	MaxErrorBodyBytes       int64 = 4 << 10
	MaxRequestBodyBytes           = 16 << 20
	TruncationNotice              = "\n\n> 输出因长度限制被截断，请缩小范围或降低详细程度后重试。"
)

var ErrRequestTooLarge = errors.New("LLM request body exceeds the size guard")

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type ToolChoiceMode int

const (
	ToolChoiceNone ToolChoiceMode = iota
	ToolChoiceAuto
	ToolChoiceForced
)

type ToolChoice struct {
	Mode         ToolChoiceMode
	FunctionName string
}

type Request struct {
	Messages       []Message
	Tools          []Tool
	ToolChoice     ToolChoice
	Temperature    float64
	MaxTokens      int
	EnableThinking bool
}

type Response struct {
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Message      ResponseMessage `json:"message"`
	Delta        ResponseMessage `json:"delta"`
	FinishReason string          `json:"finish_reason"`
}

type ResponseMessage struct {
	Content          string     `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls"`
	ReasoningContent string     `json:"reasoning_content"`
	Reasoning        string     `json:"reasoning"`
}

type Usage struct {
	TotalTokens      int `json:"total_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type Result struct {
	Response Response
	Model    string
}

type StreamResult struct {
	Content       string
	Usage         Usage
	FinishReason  string
	ReasoningSize int
	Emitted       bool
	Model         string
}

type ResponseValidator func(model string, response Response) (llmfallback.Outcome, error)
type StreamValidator func(model string, result StreamResult) (llmfallback.Outcome, error)

type CallOptions struct {
	Path                    llmfallback.Path
	AttemptTimeout          time.Duration
	PerModelTimeout         time.Duration
	MaxAttempts             int
	MaxResponseBytes        int64
	OnHTTPStatus            func(int)
	ValidateResponse        ResponseValidator
	ValidateStream          StreamValidator
	Backoff                 func(int) time.Duration
	TerminalOnProtocolError bool
}

type requestBody struct {
	Model              string                   `json:"model"`
	Messages           []Message                `json:"messages"`
	Temperature        float64                  `json:"temperature"`
	MaxTokens          int                      `json:"max_tokens"`
	Tools              []Tool                   `json:"tools,omitempty"`
	ToolChoice         any                      `json:"tool_choice,omitempty"`
	ChatTemplateKwargs map[string]interface{}   `json:"chat_template_kwargs,omitempty"`
	Thinking           *llmcompat.ThinkingParam `json:"thinking,omitempty"`
	Stream             bool                     `json:"stream,omitempty"`
	StreamOptions      *streamOptions           `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type forcedToolChoice struct {
	Type     string                   `json:"type"`
	Function forcedToolChoiceFunction `json:"function"`
}

type forcedToolChoiceFunction struct {
	Name string `json:"name"`
}

type responseTooLargeError struct {
	Limit int64
}

func (e *responseTooLargeError) Error() string {
	return fmt.Sprintf("LLM response exceeds configured size limit of %d bytes", e.Limit)
}

type requestBuildError struct {
	err error
}

func (e *requestBuildError) Error() string { return fmt.Sprintf("build LLM request: %v", e.err) }
func (e *requestBuildError) Unwrap() error { return e.err }

// MarshalRequest is exported for contract tests and diagnostics. Production
// calls use the same builder immediately before every primary/fallback attempt.
func MarshalRequest(model string, req Request, stream bool) ([]byte, error) {
	body := buildRequest(model, req, stream)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}
