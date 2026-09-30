package agent

// chatRequest is the test-side view used to inspect requests emitted by the
// shared LLM client without coupling production Agent code to wire DTOs.
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}
