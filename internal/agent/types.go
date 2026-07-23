package agent

import "context"

// 自定义线格式类型：贴 OpenAI chat/completions，刻意不复用 internal/service，
// 以保证 internal/agent 零侵入、零本项目依赖。

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters 直接透传 JSON Schema，用 any 以免绑死结构。
	Parameters any `json:"parameters"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls 仅 assistant 轮次携带；tool_call_id/name 仅 role:"tool" 结果消息携带。
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// AssistantTurn 是单轮 LLM 结果的归一化视图：内容 + 全部工具调用 + 本轮消耗 token。
type AssistantTurn struct {
	Content   string
	ToolCalls []ToolCall
	Tokens    int
}

// ContextKeyUID is the context key for storing user ID in request context.
type contextKeyUID struct{}

// ContextKeyUID is exported for use by handler to inject uid into context.
var ContextKeyUID = contextKeyUID{}

// ContextKeySessionID is the context key for storing session ID in request context.
type contextKeySessionID struct{}

// ContextKeySessionID is exported for use by handler to inject session_id into context.
var ContextKeySessionID = contextKeySessionID{}

// ContextKeyAllowedArchivedChannels carries the archived thread IDs explicitly
// selected by the user for this request. Tool handlers still resolve channel
// membership through GetUserChannels; this value only scopes which archived
// threads may pass the otherwise-active-only discovery filter.
type contextKeyAllowedArchivedChannels struct{}

var ContextKeyAllowedArchivedChannels = contextKeyAllowedArchivedChannels{}

// SelectedArchivedChannelIDs returns the request-scoped archived thread IDs.
// A copy is returned so tool handlers cannot mutate the context value.
func SelectedArchivedChannelIDs(ctx context.Context) []string {
	allowed, _ := ctx.Value(ContextKeyAllowedArchivedChannels).(map[string]bool)
	ids := make([]string, 0, len(allowed))
	for id, ok := range allowed {
		if ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
