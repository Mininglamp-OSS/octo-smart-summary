package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeClient 是可编程的 chatter 替身：每次 Chat 弹出一个预设 turn，
// 并记录最后一次收到的 msgs 供断言回喂内容。
type fakeClient struct {
	turns    []AssistantTurn
	idx      int
	lastMsgs []Message
	calls    int
}

func (f *fakeClient) Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error) {
	f.calls++
	f.lastMsgs = append([]Message(nil), msgs...)
	if f.idx >= len(f.turns) {
		// 用尽后一律收敛，避免测试无限循环。
		return AssistantTurn{Content: "done"}, nil
	}
	tr := f.turns[f.idx]
	f.idx++
	return tr, nil
}

func mkToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func newTestRunner(fc *fakeClient, reg *Registry, policy Policy) *Runner {
	return NewRunner(fc, reg, NewPool(4), policy)
}

func regWithEcho(names ...string) *Registry {
	reg := NewRegistry()
	for _, n := range names {
		n := n
		reg.Register(Tool{Type: "function", Function: ToolFunction{Name: n}},
			func(ctx context.Context, args json.RawMessage) (string, error) {
				return "R:" + n, nil
			})
	}
	return reg
}

func TestRunner_Run(t *testing.T) {
	policy := Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second}

	tests := []struct {
		name      string
		turns     []AssistantTurn
		reg       *Registry
		policy    Policy
		wantOut   string
		wantErr   bool
		errSubstr string
		checkMsgs func(t *testing.T, fc *fakeClient)
	}{
		{
			name:    "converge immediately",
			turns:   []AssistantTurn{{Content: "hello", Tokens: 10}},
			reg:     regWithEcho(),
			policy:  policy,
			wantOut: "hello",
		},
		{
			name: "two parallel tools then converge",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{
					mkToolCall("c1", "alpha", `{}`),
					mkToolCall("c2", "beta", `{}`),
				}, Tokens: 20},
				{Content: "final", Tokens: 5},
			},
			reg:     regWithEcho("alpha", "beta"),
			policy:  policy,
			wantOut: "final",
			checkMsgs: func(t *testing.T, fc *fakeClient) {
				// 第二轮请求的 msgs 应含: system,user,assistant(tool_calls),tool(c1),tool(c2)
				m := fc.lastMsgs
				if len(m) != 5 {
					t.Fatalf("msgs len = %d, want 5: %+v", len(m), m)
				}
				if m[2].Role != "assistant" || len(m[2].ToolCalls) != 2 {
					t.Fatalf("assistant msg wrong: %+v", m[2])
				}
				// 顺序稳定：c1 对应 alpha、c2 对应 beta。
				if m[3].ToolCallID != "c1" || m[3].Name != "alpha" || m[3].Content != "R:alpha" {
					t.Fatalf("tool msg 0 wrong: %+v", m[3])
				}
				if m[4].ToolCallID != "c2" || m[4].Name != "beta" || m[4].Content != "R:beta" {
					t.Fatalf("tool msg 1 wrong: %+v", m[4])
				}
			},
		},
		{
			name: "max steps exceeded",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{mkToolCall("c1", "alpha", `{}`)}, Tokens: 1},
				{ToolCalls: []ToolCall{mkToolCall("c2", "alpha", `{}`)}, Tokens: 1},
			},
			reg:       regWithEcho("alpha"),
			policy:    Policy{MaxSteps: 2, MaxTokens: 100000, StepTimeout: time.Second},
			wantErr:   true,
			errSubstr: "max steps exceeded",
		},
		{
			name: "token budget injects wrap-up",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{mkToolCall("c1", "alpha", `{}`)}, Tokens: 999},
				{Content: "wrapped", Tokens: 1},
			},
			reg:     regWithEcho("alpha"),
			policy:  Policy{MaxSteps: 5, MaxTokens: 500, StepTimeout: time.Second},
			wantOut: "wrapped",
			checkMsgs: func(t *testing.T, fc *fakeClient) {
				m := fc.lastMsgs
				last := m[len(m)-1]
				if last.Role != "user" || !strings.Contains(last.Content, "token预算") {
					t.Fatalf("expected wrap-up user msg, got %+v", last)
				}
			},
		},
		{
			name: "unknown tool and handler error fed back",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{
					mkToolCall("c1", "ghost", `{}`),
					mkToolCall("c2", "explode", `{}`),
				}, Tokens: 1},
				{Content: "recovered", Tokens: 1},
			},
			reg: func() *Registry {
				reg := regWithEcho()
				reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "explode"}},
					func(ctx context.Context, args json.RawMessage) (string, error) {
						return "", context.Canceled
					})
				return reg
			}(),
			policy:  policy,
			wantOut: "recovered",
			checkMsgs: func(t *testing.T, fc *fakeClient) {
				m := fc.lastMsgs
				if len(m) != 5 {
					t.Fatalf("msgs len = %d, want 5", len(m))
				}
				if !strings.Contains(m[3].Content, "错误") || !strings.Contains(m[3].Content, "unknown tool") {
					t.Fatalf("unknown-tool result not fed back: %+v", m[3])
				}
				if !strings.Contains(m[4].Content, "错误") {
					t.Fatalf("handler error not fed back: %+v", m[4])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeClient{turns: tt.turns}
			r := newTestRunner(fc, tt.reg, tt.policy)
			out, err := r.Run(context.Background(), "sys", "usr")

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (out=%q)", out)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error %q missing substr %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out != tt.wantOut {
				t.Fatalf("out = %q, want %q", out, tt.wantOut)
			}
			if tt.checkMsgs != nil {
				tt.checkMsgs(t, fc)
			}
		})
	}
}

// TestRunner_ParallelToolsRace 专门给 -race 用：一跳内多工具高并发无竞争、顺序稳定。
func TestRunner_ParallelToolsRace(t *testing.T) {
	const n = 12
	calls := make([]ToolCall, n)
	names := make([]string, n)
	for i := 0; i < n; i++ {
		name := "t" + string(rune('a'+i))
		names[i] = name
		calls[i] = mkToolCall("id"+name, name, `{}`)
	}
	reg := regWithEcho(names...)
	fc := &fakeClient{turns: []AssistantTurn{
		{ToolCalls: calls, Tokens: 1},
		{Content: "ok", Tokens: 1},
	}}
	r := newTestRunner(fc, reg, Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second})

	out, err := r.Run(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want ok", out)
	}
	// 校验回喂顺序与原 tool_calls 一致。
	m := fc.lastMsgs
	toolMsgs := m[3:]
	if len(toolMsgs) != n {
		t.Fatalf("tool msgs = %d, want %d", len(toolMsgs), n)
	}
	for i := 0; i < n; i++ {
		if toolMsgs[i].Content != "R:"+names[i] {
			t.Fatalf("order broken at %d: got %q want %q", i, toolMsgs[i].Content, "R:"+names[i])
		}
	}
}

// TestRunner_RunWithHistory 校验：history 被拼进上下文（system+history+user），
// 且返回的 newMsgs 完整（user+assistant，含 tool 轮）、不含 system、不含传入的 history。
func TestRunner_RunWithHistory(t *testing.T) {
	policy := Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second}

	t.Run("history spliced in, newMsgs exclude system+history", func(t *testing.T) {
		history := []Message{
			{Role: "user", Content: "H-u1"},
			{Role: "assistant", Content: "H-a1"},
		}
		fc := &fakeClient{turns: []AssistantTurn{{Content: "final", Tokens: 1}}}
		r := newTestRunner(fc, regWithEcho(), policy)

		reply, newMsgs, err := r.RunWithHistory(context.Background(), "sys", history, "now-user")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if reply != "final" {
			t.Fatalf("reply = %q, want final", reply)
		}
		// 发给 LLM 的上下文首帧应为 system+history+当前 user。
		m := fc.lastMsgs
		if len(m) != 4 {
			t.Fatalf("ctx msgs len = %d, want 4: %+v", len(m), m)
		}
		if m[0].Role != "system" || m[0].Content != "sys" {
			t.Fatalf("msg[0] not system: %+v", m[0])
		}
		if m[1].Content != "H-u1" || m[2].Content != "H-a1" {
			t.Fatalf("history not spliced: %+v", m[1:3])
		}
		if m[3].Role != "user" || m[3].Content != "now-user" {
			t.Fatalf("current user not appended: %+v", m[3])
		}
		// newMsgs 只含本回合：user + assistant，无 system、无 history。
		if len(newMsgs) != 2 {
			t.Fatalf("newMsgs len = %d, want 2: %+v", len(newMsgs), newMsgs)
		}
		if newMsgs[0].Role != "user" || newMsgs[0].Content != "now-user" {
			t.Fatalf("newMsgs[0] wrong: %+v", newMsgs[0])
		}
		if newMsgs[1].Role != "assistant" || newMsgs[1].Content != "final" {
			t.Fatalf("newMsgs[1] wrong: %+v", newMsgs[1])
		}
		for _, nm := range newMsgs {
			if nm.Role == "system" {
				t.Fatalf("newMsgs must not contain system: %+v", newMsgs)
			}
		}
	})

	t.Run("newMsgs capture assistant tool_calls + tool results", func(t *testing.T) {
		fc := &fakeClient{turns: []AssistantTurn{
			{ToolCalls: []ToolCall{mkToolCall("c1", "alpha", `{}`)}, Tokens: 1},
			{Content: "done", Tokens: 1},
		}}
		r := newTestRunner(fc, regWithEcho("alpha"), policy)

		_, newMsgs, err := r.RunWithHistory(context.Background(), "sys", nil, "u")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// 期望本回合新消息: user, assistant(tool_calls), tool(c1), assistant(final)
		if len(newMsgs) != 4 {
			t.Fatalf("newMsgs len = %d, want 4: %+v", len(newMsgs), newMsgs)
		}
		if newMsgs[1].Role != "assistant" || len(newMsgs[1].ToolCalls) != 1 {
			t.Fatalf("assistant tool_calls not captured: %+v", newMsgs[1])
		}
		if newMsgs[2].Role != "tool" || newMsgs[2].ToolCallID != "c1" || newMsgs[2].Content != "R:alpha" {
			t.Fatalf("tool result not captured: %+v", newMsgs[2])
		}
		if newMsgs[3].Role != "assistant" || newMsgs[3].Content != "done" {
			t.Fatalf("final assistant not captured: %+v", newMsgs[3])
		}
	})

	t.Run("Run delegates to RunWithHistory (zero regression)", func(t *testing.T) {
		fc := &fakeClient{turns: []AssistantTurn{{Content: "hi", Tokens: 1}}}
		r := newTestRunner(fc, regWithEcho(), policy)
		out, err := r.Run(context.Background(), "sys", "u")
		if err != nil || out != "hi" {
			t.Fatalf("Run delegate broken: out=%q err=%v", out, err)
		}
		// 无 history 时上下文仅 system+user。
		if len(fc.lastMsgs) != 2 {
			t.Fatalf("Run ctx len = %d, want 2", len(fc.lastMsgs))
		}
	})
}

// TestTruncateHistory 校验滑窗按"轮"截断，且绝不拆散 assistant(tool_calls) 与其 tool 结果。
func TestTruncateHistory(t *testing.T) {
	// 构造 3 轮，其中第 2 轮含一次 tool 调用（assistant.tool_calls + tool 结果）。
	full := []Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("t2", "alpha", `{}`)}},
		{Role: "tool", ToolCallID: "t2", Name: "alpha", Content: "R:alpha"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "a3"},
	}

	// assertPaired: 每个 assistant.tool_calls 的 id 都能在其后找到对应 tool 结果。
	assertPaired := func(t *testing.T, msgs []Message) {
		t.Helper()
		for i := range msgs {
			if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
				continue
			}
			for _, tc := range msgs[i].ToolCalls {
				found := false
				for j := i + 1; j < len(msgs); j++ {
					if msgs[j].Role == "tool" && msgs[j].ToolCallID == tc.ID {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("tool_call %q orphaned (no tool result after it): %+v", tc.ID, msgs)
				}
			}
		}
		// 反向：不应出现无主的 tool 结果（其对应 assistant 被截掉）。
		for i := range msgs {
			if msgs[i].Role != "tool" {
				continue
			}
			found := false
			for j := 0; j < i; j++ {
				if msgs[j].Role == "assistant" {
					for _, tc := range msgs[j].ToolCalls {
						if tc.ID == msgs[i].ToolCallID {
							found = true
						}
					}
				}
			}
			if !found {
				t.Fatalf("tool result %q orphaned (no owning assistant before it): %+v", msgs[i].ToolCallID, msgs)
			}
		}
	}

	t.Run("keep last 1 turn", func(t *testing.T) {
		got := TruncateHistory(full, 1)
		// 最近 1 轮 = u3, a3。
		if len(got) != 2 || got[0].Content != "u3" || got[1].Content != "a3" {
			t.Fatalf("want [u3,a3], got %+v", got)
		}
		assertPaired(t, got)
	})

	t.Run("keep last 2 turns keeps tool pair intact", func(t *testing.T) {
		got := TruncateHistory(full, 2)
		// 最近 2 轮 = 第2轮(u2,assistant(tc),tool,a2) + 第3轮(u3,a3)，起点是 u2。
		if got[0].Content != "u2" {
			t.Fatalf("expected window to start at u2, got %+v", got[0])
		}
		if len(got) != 6 {
			t.Fatalf("want 6 msgs (turn2+turn3), got %d: %+v", len(got), got)
		}
		assertPaired(t, got)
	})

	t.Run("n exceeds available -> keep all", func(t *testing.T) {
		got := TruncateHistory(full, 99)
		if len(got) != len(full) {
			t.Fatalf("want all %d, got %d", len(full), len(got))
		}
		assertPaired(t, got)
	})

	t.Run("empty history", func(t *testing.T) {
		if got := TruncateHistory(nil, 5); len(got) != 0 {
			t.Fatalf("want empty, got %+v", got)
		}
	})
}

// TestRunner_OnEvent_Nil verifies that when OnEvent is nil, behavior is unchanged.
func TestRunner_OnEvent_Nil(t *testing.T) {
	policy := Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second}
	fc := &fakeClient{
		turns: []AssistantTurn{
			{ToolCalls: []ToolCall{mkToolCall("c1", "alpha", `{}`)}, Tokens: 10},
			{Content: "done", Tokens: 5},
		},
	}
	runner := newTestRunner(fc, regWithEcho("alpha"), policy)

	// OnEvent is nil by default (not set)
	if runner.OnEvent != nil {
		t.Fatal("OnEvent should be nil by default")
	}

	reply, newMsgs, err := runner.RunWithHistory(context.Background(), "sys", nil, "user input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want %q", reply, "done")
	}
	if len(newMsgs) != 4 {
		t.Fatalf("newMsgs len = %d, want 4", len(newMsgs))
	}
}

// TestRunner_OnEvent_EmitsEvents verifies that OnEvent is called with correct events.
func TestRunner_OnEvent_EmitsEvents(t *testing.T) {
	policy := Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second}
	fc := &fakeClient{
		turns: []AssistantTurn{
			{ToolCalls: []ToolCall{mkToolCall("c1", "alpha", `{}`)}, Tokens: 10},
			{Content: "done", Tokens: 5},
		},
	}
	runner := newTestRunner(fc, regWithEcho("alpha"), policy)

	events := []Event{}
	runner.OnEvent = func(e Event) {
		events = append(events, e)
	}

	reply, _, err := runner.RunWithHistory(context.Background(), "sys", nil, "user input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want %q", reply, "done")
	}

	// Expected events: step_start(1), tool_start(alpha), tool_end(alpha), step_end(1), step_start(2), step_end(2)
	if len(events) < 4 {
		t.Fatalf("events len = %d, want at least 4, got: %+v", len(events), events)
	}

	// Verify step events have correct Step/OfSteps
	for _, e := range events {
		if strings.HasSuffix(e.Type, "_start") || strings.HasSuffix(e.Type, "_end") {
			if e.OfSteps != 5 {
				t.Errorf("event %v has OfSteps=%d, want 5", e.Type, e.OfSteps)
			}
			if e.Step < 1 || e.Step > 5 {
				t.Errorf("event %v has Step=%d, want 1-5", e.Type, e.Step)
			}
		}
		if e.Type == "tool_start" || e.Type == "tool_end" {
			if e.Tool != "alpha" {
				t.Errorf("tool event has Tool=%q, want %q", e.Tool, "alpha")
			}
		}
		if e.Type == "tool_end" || e.Type == "step_end" {
			if e.ElapsedMs < 0 {
				t.Errorf("event %v has negative ElapsedMs=%d", e.Type, e.ElapsedMs)
			}
		}
	}
}

// TestRunner_EmptyResponseNudge verifies the SUM-158 blocker follow-up:
// when the LLM returns content="" and tool_calls=[] mid-session, the runner
// nudges the model with a user message instead of silently returning empty
// success. Reproduces the "stream closed without done" symptom observed with
// kimi-k2.6 in production and locks in the recovery path.
func TestRunner_EmptyResponseNudge(t *testing.T) {
	fc := &fakeClient{turns: []AssistantTurn{
		// Step 1: a normal tool call so we're not on step 0
		{ToolCalls: []ToolCall{mkToolCall("id1", "alpha", "{}")}},
		// Step 2: EMPTY response — this used to terminate the runner silently
		{Content: "", ToolCalls: nil},
		// Step 3: after the nudge, model recovers and gives a real final answer
		{Content: "recovered answer", ToolCalls: nil},
	}}
	reg := regWithEcho("alpha")
	r := newTestRunner(fc, reg, Policy{MaxSteps: 5, MaxTokens: 10000, StepTimeout: 5 * time.Second})

	out, newMsgs, err := r.RunWithHistory(context.Background(), "system", nil, "hi")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if out != "recovered answer" {
		t.Errorf("out = %q, want %q", out, "recovered answer")
	}
	if fc.calls != 3 {
		t.Errorf("calls = %d, want 3 (tool call + empty + recovery)", fc.calls)
	}
	// The empty assistant + nudge must NOT be persisted to newMsgs (which is
	// what the caller writes to agent_message).
	for _, m := range newMsgs {
		if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
			t.Errorf("empty assistant should NOT be persisted to newMsgs; got %+v", m)
		}
		if m.Role == "user" && strings.Contains(m.Content, "请基于以上工具返回结果给出最终答案") {
			t.Errorf("nudge user message should NOT be persisted to newMsgs; got %+v", m)
		}
	}
	// But the nudge MUST reach the model on the next call — check via lastMsgs.
	foundNudge := false
	for _, m := range fc.lastMsgs {
		if m.Role == "user" && strings.Contains(m.Content, "请基于以上工具返回结果给出最终答案") {
			foundNudge = true
		}
	}
	if !foundNudge {
		t.Errorf("nudge user message should be in lastMsgs sent to LLM")
	}
}

// TestRunner_EmptyResponseAtFinalStepErrors verifies the guardrail: if the
// empty response happens at the very last step, we cannot nudge again — the
// runner must return a diagnostic error instead of a silent empty success.
func TestRunner_EmptyResponseAtFinalStepErrors(t *testing.T) {
	fc := &fakeClient{turns: []AssistantTurn{
		{Content: "", ToolCalls: nil}, // Step 1 = final (MaxSteps=1) and empty
	}}
	reg := NewRegistry()
	r := newTestRunner(fc, reg, Policy{MaxSteps: 1, MaxTokens: 10000, StepTimeout: 5 * time.Second})

	_, _, err := r.RunWithHistory(context.Background(), "system", nil, "hi")
	if err == nil {
		t.Fatal("expected error on empty final-step response, got nil")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention 'empty response'; got %v", err)
	}
}

// TestRunner_WhitespaceOnlyContentTreatedAsEmpty verifies that a whitespace-only
// content (e.g. "   \n\n") is treated the same as empty and triggers the nudge
// path, not a bogus "final answer".
func TestRunner_WhitespaceOnlyContentTreatedAsEmpty(t *testing.T) {
	fc := &fakeClient{turns: []AssistantTurn{
		{Content: "   \n\t\n  ", ToolCalls: nil}, // whitespace only
		{Content: "real answer", ToolCalls: nil}, // recovery
	}}
	reg := NewRegistry()
	r := newTestRunner(fc, reg, Policy{MaxSteps: 5, MaxTokens: 10000, StepTimeout: 5 * time.Second})

	out, _, err := r.RunWithHistory(context.Background(), "system", nil, "hi")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if out != "real answer" {
		t.Errorf("out = %q, want %q (whitespace-only should have triggered nudge)", out, "real answer")
	}
	if fc.calls != 2 {
		t.Errorf("calls = %d, want 2 (empty + recovery)", fc.calls)
	}
}
