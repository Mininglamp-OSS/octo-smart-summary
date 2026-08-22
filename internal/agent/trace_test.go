package agent

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(old)
		log.SetFlags(flags)
	})
	fn()
	return buf.String()
}

func TestTraceDisabledByDefault(t *testing.T) {
	t.Setenv(TraceEnvVar, "")
	if TraceEnabled() {
		t.Fatal("tracing must be off by default")
	}
	ctx, tr := StartTrace(context.Background(), "sess-1")
	if tr != nil {
		t.Fatal("StartTrace returned a trace while disabled")
	}
	if TraceFromContext(ctx) != nil {
		t.Fatal("a disabled trace must not be attached to ctx")
	}
	// Every method must be a safe no-op on the nil trace.
	out := captureLog(t, func() {
		tr.AddStep(1, 10, 100, 2, 3)
		tr.CloseStep(1, 5, []string{"fetch_channel"})
		tr.AddTool("fetch_channel", 5)
		tr.AddSubPhase("map", 5)
		tr.AddCitationCap(10, 3, 2, 5, 100, 40, 9)
		tr.Report("ok")
	})
	if out != "" {
		t.Fatalf("disabled trace logged %q", out)
	}
	if tr.Active() {
		t.Fatal("nil trace reported Active")
	}
}

func TestTraceEnabledSpellings(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "t"} {
		t.Setenv(TraceEnvVar, v)
		if !TraceEnabled() {
			t.Errorf("TraceEnabled() = false for %q", v)
		}
	}
	for _, v := range []string{"", "0", "false", "nope", "  "} {
		t.Setenv(TraceEnvVar, v)
		if TraceEnabled() {
			t.Errorf("TraceEnabled() = true for %q", v)
		}
	}
}

func TestTraceReportsPhaseSplit(t *testing.T) {
	t.Setenv(TraceEnvVar, "1")
	ctx, tr := StartTrace(context.Background(), "sess-abc")
	if tr == nil {
		t.Fatal("StartTrace returned nil while enabled")
	}
	if TraceFromContext(ctx) != tr {
		t.Fatal("trace not retrievable from ctx")
	}

	tr.AddStep(1, 120, 4096, 3, 40)
	tr.CloseStep(1, 300, []string{"fetch_channel", "peek_channel"})
	tr.AddStep(2, 90, 99336, 9, 55)
	tr.AddTool("fetch_channel", 250)
	tr.AddTool("peek_channel", 40)
	tr.AddSubPhase("map(4chunks)", 210)
	tr.AddCitationCap(1227, 325, 301, 601, 11314, 6927, 211)

	out := captureLog(t, func() { tr.Report("ok") })

	for _, want := range []string{
		"[agent-trace] session=sess-abc",
		"outcome=ok",
		"steps=2",
		"max_prompt=99336chars",
		"step=1 planning=120ms prompt=4096chars/3msgs",
		"fetch_channel,peek_channel",
		"step=2 planning=90ms prompt=99336chars/9msgs",
		"tools(slowest first): fetch_channel=250ms peek_channel=40ms",
		"sub-phases: map(4chunks)=210ms",
		"citations: map_calls=1 markers=1227->325 (dedup -301, cap -601)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\ngot:\n%s", want, out)
		}
	}
	// A few dozen lines per run, not hundreds.
	if n := strings.Count(out, "\n"); n > 12 {
		t.Errorf("report emitted %d lines; keep it small enough for production", n)
	}
}

// Hard privacy line: the trace records sizes, counts, durations, step names
// and tool names — never content. This test feeds distinctive content into
// every field that could plausibly leak and asserts none of it reaches the log.
func TestTraceNeverLogsContent(t *testing.T) {
	t.Setenv(TraceEnvVar, "1")
	const (
		secretBody = "SECRET_MESSAGE_BODY_张三的私密聊天内容"
		secretArgs = `{"channel_id":"SECRET_CHANNEL_ID","keyword":"SECRET_KEYWORD"}`
		secretUser = "SECRET_USER_NAME"
	)

	msgs := []Message{
		{Role: "system", Content: "你是 Octo 智能总结 Agent " + secretUser},
		{Role: "user", Content: secretBody},
		{Role: "assistant", ToolCalls: []ToolCall{toolCallFixture("call_1", "search_messages", secretArgs)}},
		{Role: "tool", Name: "search_messages", Content: secretBody + secretBody},
	}

	// measurePrompt must return only sizes, and those sizes must account for
	// tool-call arguments (they are part of the serialised request).
	chars, count := measurePrompt(msgs)
	if count != 4 {
		t.Fatalf("measurePrompt count = %d, want 4", count)
	}
	if chars <= len(secretBody) {
		t.Fatalf("measurePrompt chars = %d, suspiciously small", chars)
	}
	// The tool-call arguments must be counted (they are part of the
	// serialised request) — sizes in, text out.
	if chars < len(secretArgs) {
		t.Fatalf("measurePrompt chars = %d, did not account for tool-call arguments", chars)
	}

	ctx, tr := StartTrace(context.Background(), "sess-priv")
	_ = ctx
	tr.AddStep(1, 100, chars, count, 42)
	tr.CloseStep(1, 200, []string{"search_messages"})
	tr.AddTool("search_messages", 200)
	tr.AddSubPhase("map(2chunks)", 150)
	tr.AddCitationCap(50, 12, 8, 30, 4000, 1200, 20)

	out := captureLog(t, func() { tr.Report("ok") })

	for _, secret := range []string{secretBody, secretArgs, secretUser, "SECRET_CHANNEL_ID", "SECRET_KEYWORD", "私密"} {
		if strings.Contains(out, secret) {
			t.Errorf("PRIVACY REGRESSION: trace log leaked %q\nlog:\n%s", secret, out)
		}
	}
	// The size derived from that content is fine and must be present.
	if !strings.Contains(out, "prompt=") {
		t.Errorf("trace dropped the prompt size it is supposed to report:\n%s", out)
	}
}

func TestTraceConcurrentToolSpans(t *testing.T) {
	t.Setenv(TraceEnvVar, "1")
	_, tr := StartTrace(context.Background(), "sess-race")
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			tr.AddTool("fetch_channel", int64(i))
			tr.AddSubPhase("map", int64(i))
			tr.AddCitationCap(10, 3, 2, 5, 100, 40, i)
		}(i)
	}
	for i := 0; i < 16; i++ {
		<-done
	}
	out := captureLog(t, func() { tr.Report("ok") })
	if !strings.Contains(out, "map_calls=16") {
		t.Errorf("citation spans lost under concurrency:\n%s", out)
	}
}

func TestTraceToolListIsCapped(t *testing.T) {
	t.Setenv(TraceEnvVar, "1")
	_, tr := StartTrace(context.Background(), "sess-wide")
	for i := 0; i < 40; i++ {
		tr.AddTool("fetch_channel", int64(i))
	}
	out := captureLog(t, func() { tr.Report("ok") })
	if !strings.Contains(out, "more)") {
		t.Errorf("wide fan-out not truncated:\n%s", out)
	}
	if n := strings.Count(out, "fetch_channel="); n > 8 {
		t.Errorf("logged %d tool spans, want at most 8", n)
	}
}

// toolCallFixture builds a ToolCall; ToolCall.Function is an anonymous struct,
// so it cannot be written as a composite literal at the call site.
func toolCallFixture(id, name, args string) ToolCall {
	var tc ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}
