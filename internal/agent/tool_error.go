package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// ToolErrorEnvelope is the structured result a failed tool returns to the model
// (SS-07b), replacing the opaque "错误: <text>" string. It lets the planner
// reason about whether the failure is retryable or fatal instead of guessing
// from free text (defect #5), and lets the runner surface fatal failures to the
// finish gate.
type ToolErrorEnvelope struct {
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code"`
	Retryable bool   `json:"retryable"`
	Fatal     bool   `json:"fatal"`
	Message   string `json:"message"`
}

// criticalTools are the tools whose failure compromises data completeness; a
// fatal error from them must block a COMPLETE verdict.
var criticalTools = map[string]bool{
	"fetch_channel":   true,
	"search_messages": true,
	"filter_relevant": true,
	"summarize_chunk": true,
	"merge_summaries": true,
}

// classifyToolError maps a tool failure to a structured envelope. The rules are
// deliberately conservative and text-based (the underlying tools return plain
// errors); grow them as tools adopt typed errors.
//
//   - context deadline / canceled            → retryable, not fatal
//   - permission / access / identity / auth  → fatal (cannot be retried into success)
//   - invalid args / parse                    → not retryable, not fatal (planner bug)
//   - anything else on a critical tool        → fatal (data completeness at risk)
//   - anything else on a non-critical tool    → retryable
func classifyToolError(toolName string, err error) ToolErrorEnvelope {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	low := strings.ToLower(msg)
	env := ToolErrorEnvelope{OK: false, Message: msg}

	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(low, "deadline") || strings.Contains(low, "timeout"):
		env.ErrorCode, env.Retryable, env.Fatal = "TIMEOUT", true, false
	case errors.Is(err, context.Canceled) || strings.Contains(low, "canceled") || strings.Contains(low, "cancelled"):
		env.ErrorCode, env.Retryable, env.Fatal = "CANCELED", true, false
	case strings.Contains(low, "not accessible") || strings.Contains(low, "permission") ||
		strings.Contains(low, "access denied") || strings.Contains(low, "identity") ||
		strings.Contains(low, "unauthor") || strings.Contains(low, "forbidden"):
		env.ErrorCode, env.Retryable, env.Fatal = "PERMISSION_DENIED", false, true
	case strings.Contains(low, "parse args") || strings.Contains(low, "invalid") || strings.Contains(low, "required"):
		env.ErrorCode, env.Retryable, env.Fatal = "INVALID_ARGUMENT", false, false
	case strings.Contains(low, "persist evidence") || strings.Contains(low, "evidence"):
		env.ErrorCode, env.Retryable, env.Fatal = "EVIDENCE_WRITE_FAILED", false, true
	default:
		if criticalTools[toolName] {
			env.ErrorCode, env.Retryable, env.Fatal = "CRITICAL_TOOL_ERROR", false, true
		} else {
			env.ErrorCode, env.Retryable, env.Fatal = "TOOL_ERROR", true, false
		}
	}
	return env
}

// JSON renders the envelope as the tool result string fed back to the model.
func (e ToolErrorEnvelope) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"ok":false,"error_code":"TOOL_ERROR","retryable":true,"fatal":false,"message":"marshal error"}`
	}
	return string(b)
}
