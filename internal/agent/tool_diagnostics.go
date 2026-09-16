package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// Diagnostic only: opt in to one exact run. Never record prompts, argument
// values, model-authored tool names, error text, user/channel names or tokens.
const toolDiagnosticRunEnv = "AGENT_TOOL_DIAGNOSTIC_RUN_ID"

type toolDiagnosticStepKey struct{}

var toolDiagnosticSequence atomic.Uint64

type toolDiagnostics struct {
	sequence uint64
	step     int
	allowed  map[string]bool
}

func toolDiagnosticsEnabled(ctx context.Context) bool {
	runID, _ := ctx.Value(ContextKeyRunID).(string)
	return runID != "" && runID == os.Getenv(toolDiagnosticRunEnv)
}

func beginToolDiagnostics(ctx context.Context, messages []Message, tools []Tool) *toolDiagnostics {
	if !toolDiagnosticsEnabled(ctx) {
		return nil
	}
	step, _ := ctx.Value(toolDiagnosticStepKey{}).(int)
	d := &toolDiagnostics{sequence: toolDiagnosticSequence.Add(1), step: step, allowed: make(map[string]bool)}
	for _, tool := range tools {
		d.allowed[tool.Function.Name] = true
	}
	log.Printf("[tool-diag] call=%d step=%d phase=request messages=%d tools=%d", d.sequence, d.step, len(messages), len(tools))
	for i, message := range messages {
		d.calls("request", i, message.ToolCalls)
	}
	return d
}

func (d *toolDiagnostics) calls(phase string, messageIndex int, calls []ToolCall) {
	if d == nil {
		return
	}
	for i, call := range calls {
		name := "unregistered"
		if d.allowed[call.Function.Name] {
			name = call.Function.Name
		}
		raw := []byte(call.Function.Arguments)
		digest := sha256.Sum256(raw)
		var value json.RawMessage
		err := json.Unmarshal(raw, &value)
		var offset int64
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			offset = syntax.Offset
		}
		kind := "invalid"
		if err == nil {
			switch strings.TrimSpace(string(raw))[0] {
			case '{':
				kind = "object"
			case '[':
				kind = "array"
			case '"':
				kind = "string"
			default:
				kind = "scalar"
			}
		}
		log.Printf("[tool-diag] call=%d step=%d phase=%s message=%d index=%d tool=%s bytes=%d sha256=%x json_valid=%t root=%s syntax_offset=%d",
			d.sequence, d.step, phase, messageIndex, i, name, len(raw), digest, err == nil, kind, offset)
	}
}

func (d *toolDiagnostics) httpStatus(status int) {
	if d != nil {
		log.Printf("[tool-diag] call=%d step=%d phase=http status=%d", d.sequence, d.step, status)
	}
}

func (d *toolDiagnostics) response(content string, calls []ToolCall, finishReason string, completionTokens int) {
	if d == nil {
		return
	}
	reason := "other"
	switch finishReason {
	case "stop", "length", "tool_calls", "content_filter":
		reason = finishReason
	}
	log.Printf("[tool-diag] call=%d step=%d phase=response content_bytes=%d calls=%d finish=%s completion_tokens=%d",
		d.sequence, d.step, len(content), len(calls), reason, completionTokens)
	d.calls("response", 0, calls)
}
