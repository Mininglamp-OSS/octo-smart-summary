package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const validPreviewTerminalArgs = `{"result_type":"agent_preview","reply":"预览已生成。","execution_target":"agent_preview","preview":{"content":"# 正文","version":1}}`

func terminalRegistry(handler TerminalHandler) *Registry {
	reg := NewRegistry()
	schema, _ := EmitSummaryResponseTool()
	reg.RegisterTerminal(schema, handler)
	return reg
}

func defaultTerminalHandler() TerminalHandler {
	_, handler := EmitSummaryResponseTool()
	return handler
}

func terminalPolicy(maxSteps int) Policy {
	return Policy{MaxSteps: maxSteps, MaxTokens: 100000, StepTimeout: time.Second, TerminalTool: "emit_summary_response"}
}

func TestRunWithHistoryOutcomeLegacyFreeTextIsUnchanged(t *testing.T) {
	client := &fakeClient{turns: []AssistantTurn{{Content: "legacy answer"}}}
	runner := newTestRunner(client, NewRegistry(), Policy{MaxSteps: 1, MaxTokens: 1000, StepTimeout: time.Second})

	result, msgs, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "hello")
	if err != nil {
		t.Fatalf("RunWithHistoryOutcome: %v", err)
	}
	if result.Reply != "legacy answer" || result.Terminal != nil {
		t.Fatalf("result = %+v", result)
	}
	if got := msgs[len(msgs)-1]; got.Role != "assistant" || got.Content != "legacy answer" {
		t.Fatalf("legacy final message = %+v", got)
	}
}

func TestTerminalPolicyRejectsFreeTextThenAcceptsEmit(t *testing.T) {
	client := &fakeClient{turns: []AssistantTurn{
		{Content: "wrong free-text draft"},
		{ToolCalls: []ToolCall{mkToolCall("emit-1", "emit_summary_response", validPreviewTerminalArgs)}},
	}}
	runner := newTestRunner(client, terminalRegistry(defaultTerminalHandler()), terminalPolicy(2))

	result, msgs, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "summarize")
	if err != nil {
		t.Fatalf("RunWithHistoryOutcome: %v", err)
	}
	if result.Terminal == nil || result.Terminal.ResultType != SummaryResultAgentPreview || result.Reply != "预览已生成。" {
		t.Fatalf("result = %+v", result)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" {
		t.Fatalf("durable terminal transcript = %+v, want user + synthetic assistant only", msgs)
	}
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "wrong free-text draft") || strings.Contains(msg.Content, "请仅通过 emit_summary_response") {
			t.Fatalf("rejected free text or runtime nudge was persisted: %+v", msgs)
		}
		for _, call := range msg.ToolCalls {
			if call.Function.Name == "emit_summary_response" || strings.Contains(call.Function.Arguments, "# 正文") {
				t.Fatalf("terminal arguments were persisted: %+v", msgs)
			}
		}
		if msg.Role == "tool" && msg.Name == "emit_summary_response" {
			t.Fatalf("terminal tool result was persisted: %+v", msgs)
		}
	}
	if got := msgs[len(msgs)-1]; got.Role != "assistant" || got.Content != "预览已生成。" || len(got.ToolCalls) != 0 {
		t.Fatalf("synthetic final assistant = %+v", got)
	}
}

func TestTerminalMixedWithOrdinaryToolIsRejectedAndRetried(t *testing.T) {
	terminalCalls := 0
	baseHandler := defaultTerminalHandler()
	reg := terminalRegistry(func(ctx context.Context, args json.RawMessage) (TerminalOutcome, error) {
		terminalCalls++
		return baseHandler(ctx, args)
	})
	echoCalls := 0
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "echo"}}, func(context.Context, json.RawMessage) (string, error) {
		echoCalls++
		return "echo-ok", nil
	})
	client := &fakeClient{turns: []AssistantTurn{
		{Content: "# mixed draft that must not persist", ToolCalls: []ToolCall{
			mkToolCall("emit-mixed", "emit_summary_response", validPreviewTerminalArgs),
			mkToolCall("echo-1", "echo", `{}`),
		}},
		{ToolCalls: []ToolCall{mkToolCall("emit-only", "emit_summary_response", validPreviewTerminalArgs)}},
	}}
	runner := newTestRunner(client, reg, terminalPolicy(2))

	result, msgs, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "summarize")
	if err != nil {
		t.Fatalf("RunWithHistoryOutcome: %v", err)
	}
	if result.Terminal == nil || terminalCalls != 1 || echoCalls != 1 {
		t.Fatalf("terminal=%+v terminalCalls=%d echoCalls=%d", result.Terminal, terminalCalls, echoCalls)
	}
	var sawMixedRejection bool
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "mixed draft") {
			t.Fatalf("mixed terminal assistant content leaked into durable messages: %+v", msgs)
		}
		if msg.Role == "tool" && msg.ToolCallID == "emit-mixed" && strings.Contains(msg.Content, "only tool call") {
			sawMixedRejection = true
		}
		for _, call := range msg.ToolCalls {
			if call.Function.Name == "emit_summary_response" {
				t.Fatalf("mixed terminal call leaked into durable messages: %+v", msgs)
			}
		}
	}
	if sawMixedRejection {
		t.Fatalf("mixed terminal rejection leaked into durable transcript: %+v", msgs)
	}
	var transientRejection bool
	for _, msg := range client.lastMsgs {
		if msg.Role == "tool" && msg.ToolCallID == "emit-mixed" && strings.Contains(msg.Content, "only tool call") {
			transientRejection = true
		}
	}
	if !transientRejection {
		t.Fatalf("mixed terminal rejection was not available to the next planner turn: %+v", client.lastMsgs)
	}
}

type failingTerminalThenSuccessClient struct {
	calls          int
	sawFailurePair bool
}

func (c *failingTerminalThenSuccessClient) Chat(_ context.Context, msgs []Message, _ []Tool) (AssistantTurn, error) {
	c.calls++
	if c.calls == 2 {
		for _, msg := range msgs {
			if msg.Role == "tool" && msg.Name == "emit_summary_response" && strings.Contains(msg.Content, "invalid terminal payload") {
				c.sawFailurePair = true
			}
		}
	}
	return AssistantTurn{ToolCalls: []ToolCall{
		mkToolCall("emit", "emit_summary_response", validPreviewTerminalArgs),
	}}, nil
}

func TestFailedTerminalAttemptIsTransientOnly(t *testing.T) {
	client := &failingTerminalThenSuccessClient{}
	attempts := 0
	reg := terminalRegistry(func(ctx context.Context, args json.RawMessage) (TerminalOutcome, error) {
		attempts++
		if attempts == 1 {
			return TerminalOutcome{}, errors.New("invalid terminal payload")
		}
		return defaultTerminalHandler()(ctx, args)
	})
	runner := NewRunner(client, reg, NewPool(2), terminalPolicy(2))

	result, msgs, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "summarize")
	if err != nil {
		t.Fatalf("RunWithHistoryOutcome: %v", err)
	}
	if result.Terminal == nil || !client.sawFailurePair || attempts != 2 {
		t.Fatalf("terminal retry = result:%+v transient:%t attempts:%d", result, client.sawFailurePair, attempts)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[1].Content != "预览已生成。" {
		t.Fatalf("failed terminal attempt entered durable messages: %+v", msgs)
	}
}

type pendingMapTerminalClient struct {
	calls      int
	sawBlocked bool
}

func (c *pendingMapTerminalClient) Chat(_ context.Context, msgs []Message, _ []Tool) (AssistantTurn, error) {
	c.calls++
	for _, msg := range msgs {
		if msg.Role == "tool" && msg.Name == "emit_summary_response" && strings.Contains(msg.Content, "Map/Reduce") {
			c.sawBlocked = true
		}
	}
	switch c.calls {
	case 1:
		return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("map-fail", "summarize_chunk", `{"messages_handle":"messages-1"}`)}}, nil
	case 2:
		return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("emit-blocked", "emit_summary_response", validPreviewTerminalArgs)}}, nil
	case 3:
		return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("map-ok", "summarize_chunk", `{"messages_handle":"messages-1"}`)}}, nil
	case 4:
		for _, msg := range msgs {
			if msg.Role != "tool" || msg.Name != "summarize_chunk" {
				continue
			}
			var result struct {
				SummaryHandle string `json:"summary_handle"`
			}
			if json.Unmarshal([]byte(msg.Content), &result) == nil && result.SummaryHandle != "" {
				args, _ := json.Marshal(map[string]interface{}{"summary_handles": []string{result.SummaryHandle}})
				return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("reduce", "merge_summaries", string(args))}}, nil
			}
		}
		return AssistantTurn{}, errors.New("successful Map handle not found")
	default:
		return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("emit-final", "emit_summary_response", validPreviewTerminalArgs)}}, nil
	}
}

func TestTerminalCannotBypassPendingMapOrReduce(t *testing.T) {
	client := &pendingMapTerminalClient{}
	terminalCalls := 0
	baseHandler := defaultTerminalHandler()
	reg := terminalRegistry(func(ctx context.Context, args json.RawMessage) (TerminalOutcome, error) {
		terminalCalls++
		return baseHandler(ctx, args)
	})
	mapAttempts := 0
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "summarize_chunk"}}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		mapAttempts++
		if mapAttempts == 1 {
			return "", errors.New("transient map failure")
		}
		store, _ := summaryHandleStoreFromContext(ctx)
		handle, err := store.PutAtStep("map body", 1, summaryToolStepFromContext(ctx))
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(map[string]interface{}{"summary_handle": handle, "chunk_count": 1})
		return string(data), nil
	})
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "merge_summaries"}}, func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Handles []string `json:"summary_handles"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", err
		}
		store, _ := summaryHandleStoreFromContext(ctx)
		resolved, err := store.ResolveAllBefore(req.Handles, summaryToolStepFromContext(ctx))
		if err != nil {
			return "", err
		}
		store.MarkReduced(resolved.Generation)
		return `{"merged_summary":"done","chunk_count":1}`, nil
	})
	runner := NewRunner(client, reg, NewPool(4), terminalPolicy(5))

	result, msgs, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "summarize")
	if err != nil {
		t.Fatalf("RunWithHistoryOutcome: %v", err)
	}
	if result.Terminal == nil || terminalCalls != 1 {
		t.Fatalf("blocked terminal was dispatched: terminal=%+v calls=%d", result.Terminal, terminalCalls)
	}
	if !client.sawBlocked || mapAttempts != 2 {
		t.Fatalf("recovery not observed: sawBlocked=%t mapAttempts=%d", client.sawBlocked, mapAttempts)
	}
	for _, msg := range msgs {
		if msg.Name == "emit_summary_response" || msg.ToolCallID == "emit-blocked" {
			t.Fatalf("blocked terminal attempt entered durable messages: %+v", msgs)
		}
		for _, call := range msg.ToolCalls {
			if call.Function.Name == "emit_summary_response" {
				t.Fatalf("blocked terminal arguments entered durable messages: %+v", msgs)
			}
		}
	}
}

func TestTerminalSyntheticAssistantPreservesRunMetadata(t *testing.T) {
	client := &fakeClient{turns: []AssistantTurn{{ToolCalls: []ToolCall{
		mkToolCall("emit", "emit_summary_response", validPreviewTerminalArgs),
	}}}}
	runner := newTestRunner(client, terminalRegistry(defaultTerminalHandler()), terminalPolicy(1))
	ctx := context.WithValue(context.Background(), ContextKeyRunID, "run-current")
	history := []Message{{Role: "assistant", Content: "old partial", RunID: "run-current", OutputTruncated: true}}

	_, msgs, err := runner.RunWithHistoryOutcome(ctx, "system", history, "continue")
	if err != nil {
		t.Fatalf("RunWithHistoryOutcome: %v", err)
	}
	final := msgs[len(msgs)-1]
	if final.RunID != "run-current" || !final.OutputTruncated {
		t.Fatalf("final metadata = {run:%q truncated:%t}", final.RunID, final.OutputTruncated)
	}
}

func TestTerminalEmitsTraceAndProgressEvents(t *testing.T) {
	t.Setenv(TraceEnvVar, "1")
	client := &fakeClient{turns: []AssistantTurn{{ToolCalls: []ToolCall{
		mkToolCall("emit", "emit_summary_response", validPreviewTerminalArgs),
	}}}}
	runner := newTestRunner(client, terminalRegistry(defaultTerminalHandler()), terminalPolicy(1))
	var events []Event
	runner.OnEvent = func(event Event) { events = append(events, event) }
	ctx, trace := StartTrace(context.Background(), "sess-terminal")

	if _, _, err := runner.RunWithHistoryOutcome(ctx, "system", nil, "summarize"); err != nil {
		t.Fatalf("RunWithHistoryOutcome: %v", err)
	}
	var sawStart, sawEnd, sawStepEnd bool
	for _, event := range events {
		switch event.Type {
		case "tool_start":
			sawStart = event.Tool == "emit_summary_response"
		case "tool_end":
			sawEnd = event.Tool == "emit_summary_response"
		case "step_end":
			sawStepEnd = event.StepHasTools
		}
	}
	if !sawStart || !sawEnd || !sawStepEnd {
		t.Fatalf("terminal progress events = %+v", events)
	}
	logOutput := captureLog(t, func() { trace.Report("ok") })
	if !strings.Contains(logOutput, "emit_summary_response") {
		t.Fatalf("terminal tool missing from trace:\n%s", logOutput)
	}
}
