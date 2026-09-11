package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

func TestToolArgumentRecoveryOverHTTPPreservesCompletedWork(t *testing.T) {
	requests, completedTools := 0, 0
	badArgs := "{\"preview\":{\"content\":\"private draft\nunescaped line\"}}"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid outer request")
			w.WriteHeader(400)
			return
		}
		requests++
		for _, message := range request.Messages {
			if !validToolArguments(message.ToolCalls) || strings.Contains(message.Content, "private draft") {
				t.Error("malformed call or rejected draft reached next model request")
				w.WriteHeader(400)
				return
			}
		}
		call := mkToolCall("fetch", "echo", `{}`)
		switch requests {
		case 2:
			call = mkToolCall("bad", "emit_summary_response", badArgs)
		case 3:
			call = mkToolCall("repaired", "emit_summary_response", validPreviewTerminalArgs)
			keptResult := false
			for _, message := range request.Messages {
				keptResult = keptResult || (message.Role == "tool" && message.Content == "completed evidence")
			}
			if !keptResult {
				t.Error("repair discarded previously completed work")
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{
				"finish_reason": "tool_calls",
				"message":       map[string]interface{}{"content": "", "tool_calls": []ToolCall{call}},
			}},
		})
	}))
	defer srv.Close()
	reg := terminalRegistry(defaultTerminalHandler())
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "echo"}}, func(context.Context, json.RawMessage) (string, error) {
		completedTools++
		return "completed evidence", nil
	})
	runner := NewRunner(NewClient(srv.URL, "test", "test-model", 5, 2048, nil), reg, NewPool(2), terminalPolicy(2))
	result, messages, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "summarize")
	if err != nil || result.Terminal == nil || requests != 3 || completedTools != 1 {
		t.Fatalf("repair failed: err=%v requests=%d tools=%d terminal=%t", err, requests, completedTools, result.Terminal != nil)
	}
	for _, message := range messages {
		if !validToolArguments(message.ToolCalls) || message.Content == toolArgumentRepairInstruction || strings.Contains(message.Content, "private draft") {
			t.Fatal("rejected output or recovery instruction entered durable history")
		}
	}
}

func TestToolArgumentRecoveryIsBoundedAndDoesNotDispatchMixedTurn(t *testing.T) {
	bad := AssistantTurn{ToolCalls: []ToolCall{
		mkToolCall("valid-sibling", "echo", `{}`),
		mkToolCall("bad", "emit_summary_response", `{"unfinished":`),
	}}
	client := &fakeClient{turns: []AssistantTurn{bad, bad, bad, {Content: "must not reach"}}}
	executed := 0
	reg := terminalRegistry(defaultTerminalHandler())
	reg.Register(Tool{Function: ToolFunction{Name: "echo"}}, func(context.Context, json.RawMessage) (string, error) {
		executed++
		return "unexpected", nil
	})
	runner := newTestRunner(client, reg, terminalPolicy(10))
	_, messages, err := runner.RunWithHistoryOutcome(context.Background(), "", nil, "summarize")
	var invalid *InvalidToolArgumentsError
	if !errors.As(err, &invalid) || client.calls != maxToolArgumentRepairs+1 || executed != 0 || len(messages) != 0 {
		t.Fatalf("err=%v calls=%d executed=%d messages=%d", err, client.calls, executed, len(messages))
	}
}

func TestToolArgumentRepairDoesNotRestartWorkAfterTokenBudget(t *testing.T) {
	client := &fakeClient{turns: []AssistantTurn{{ToolCalls: []ToolCall{mkToolCall("bad", "echo", "{")}, Tokens: 2}}}
	runner := newTestRunner(client, regWithEcho("echo"), Policy{MaxSteps: 10, MaxTokens: 1, StepTimeout: time.Second})
	_, _, err := runner.RunWithHistoryOutcome(context.Background(), "", nil, "request")
	var invalid *InvalidToolArgumentsError
	if !errors.As(err, &invalid) || client.calls != 1 {
		t.Fatalf("repair exceeded work budget: err=%v calls=%d", err, client.calls)
	}
}

func TestTerminalArgumentRepairAllowedAtFinalizationBudget(t *testing.T) {
	client := &fakeClient{turns: []AssistantTurn{
		{ToolCalls: []ToolCall{mkToolCall("bad", "emit_summary_response", "{")}, Tokens: 2},
		{ToolCalls: []ToolCall{mkToolCall("fixed", "emit_summary_response", validPreviewTerminalArgs)}, Tokens: 2},
	}}
	policy := terminalPolicy(1)
	policy.MaxTokens = 1
	runner := newTestRunner(client, terminalRegistry(defaultTerminalHandler()), policy)
	result, _, err := runner.RunWithHistoryOutcome(context.Background(), "", nil, "request")
	if err != nil || result.Terminal == nil || client.calls != 2 {
		t.Fatalf("final submission was not repaired: err=%v calls=%d", err, client.calls)
	}
}

func TestFinalizationRepairCannotReopenWork(t *testing.T) {
	for _, repaired := range []AssistantTurn{
		{ToolCalls: []ToolCall{mkToolCall("unexpected", "echo", "{}")}},
		{Content: "unexpected free text"},
	} {
		client := &fakeClient{turns: []AssistantTurn{
			{ToolCalls: []ToolCall{mkToolCall("bad", "emit_summary_response", "{")}, Tokens: 2},
			repaired,
		}}
		policy := terminalPolicy(1)
		policy.MaxTokens = 1
		executed := false
		reg := terminalRegistry(defaultTerminalHandler())
		reg.Register(Tool{Function: ToolFunction{Name: "echo"}}, func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "unexpected", nil
		})
		runner := newTestRunner(client, reg, policy)
		_, _, err := runner.RunWithHistoryOutcome(context.Background(), "", nil, "request")
		var invalid *InvalidToolArgumentsError
		if !errors.As(err, &invalid) || client.calls != 2 || executed {
			t.Fatalf("repair reopened work: err=%v calls=%d executed=%t", err, client.calls, executed)
		}
	}
}

func TestClientRejectsInvalidHistoryBeforeHTTP(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()
	for _, args := range []string{"", "{", "null", "[]", `"string"`, `{"text":"unclosed}`} {
		_, err := NewClient(srv.URL, "test", "test", 5, 256, nil).Chat(context.Background(),
			[]Message{{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("bad", "echo", args)}}}, nil)
		var invalid *InvalidToolArgumentsError
		if !errors.As(err, &invalid) {
			t.Fatalf("invalid history not rejected: %v", err)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid history reached provider %d times", requests)
	}
}

func TestClientPreservesHTTPStatusThroughFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid arguments", 400)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "test", "test", 5, 256, []string{"other"}).Chat(context.Background(), nil, nil)
	var upstream *llmfallback.HTTPError
	if !errors.As(err, &upstream) || upstream.StatusCode != 400 {
		t.Fatalf("lost deterministic upstream status: %v", err)
	}
}

func TestInvalidArgumentMetadataNeverContainsValues(t *testing.T) {
	for _, test := range []struct{ args, reason string }{
		{"", "empty"},
		{"null", "non_object"},
		{"[]", "non_object"},
		{"{\"private\":\"secret\nraw\"}", "invalid_json"},
	} {
		err := invalidToolArguments([]ToolCall{mkToolCall("call", "echo", test.args)})
		if err == nil || err.Reason != test.reason || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe or incorrect argument classification: %v", err)
		}
		if test.reason == "invalid_json" && err.SyntaxOffset == 0 {
			t.Fatal("missing syntax offset")
		}
	}
}
