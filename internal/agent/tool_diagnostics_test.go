package agent

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestToolDiagnosticsRedactsContentAndInvalidArguments(t *testing.T) {
	t.Setenv(toolDiagnosticRunEnv, "diagnostic-run")
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	ctx := context.WithValue(context.Background(), ContextKeyRunID, "diagnostic-run")
	ctx = context.WithValue(ctx, toolDiagnosticStepKey{}, 4)
	invalid := mkToolCall("private-call-id", "emit_summary_response", `{"body":"private-summary`)
	unknown := mkToolCall("other-private-id", "private-model-authored-name", `{"secret":"private-chat"}`)
	messages := []Message{{Role: "user", Content: "private-prompt"}, {Role: "assistant", ToolCalls: []ToolCall{invalid, unknown}}}
	d := beginToolDiagnostics(ctx, messages, []Tool{{Function: ToolFunction{Name: "emit_summary_response"}}})
	d.httpStatus(400)
	d.response("private-output", []ToolCall{invalid}, "private-finish", 12)
	got := output.String()
	for _, secret := range []string{"private-", "diagnostic-run", `"body"`, `"secret"`} {
		if strings.Contains(got, secret) {
			t.Fatalf("diagnostic exposed protected content: %q", secret)
		}
	}
	for _, expected := range []string{"step=4", "json_valid=false", "tool=emit_summary_response", "tool=unregistered", "status=400", "phase=response", "finish=other", "sha256="} {
		if !strings.Contains(got, expected) {
			t.Fatalf("missing safe diagnostic %s", expected)
		}
	}
}

func TestToolDiagnosticsRequiresExactRunOptIn(t *testing.T) {
	t.Setenv(toolDiagnosticRunEnv, "target-run")
	for _, run := range []string{"", "other-run"} {
		ctx := context.WithValue(context.Background(), ContextKeyRunID, run)
		if beginToolDiagnostics(ctx, nil, nil) != nil {
			t.Fatal("diagnostics enabled for another run")
		}
	}
	var disabled *toolDiagnostics
	disabled.httpStatus(200)
	disabled.response("", nil, "stop", 0)
}
