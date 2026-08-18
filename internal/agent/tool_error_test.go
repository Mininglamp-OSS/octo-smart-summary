package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestClassifyToolError(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		err       error
		errCode   string
		retryable bool
		fatal     bool
	}{
		{"timeout", "fetch_channel", context.DeadlineExceeded, "TIMEOUT", true, false},
		{"canceled", "fetch_channel", context.Canceled, "CANCELED", true, false},
		{"permission", "fetch_channel", errors.New("channel not accessible by user"), "PERMISSION_DENIED", false, true},
		{"identity", "summarize_chunk", errors.New("missing user identity in context"), "PERMISSION_DENIED", false, true},
		{"invalid args", "fetch_channel", errors.New("parse args: bad json"), "INVALID_ARGUMENT", false, false},
		{"evidence", "fetch_channel", errors.New("persist evidence: db down"), "EVIDENCE_WRITE_FAILED", false, true},
		{"critical default", "summarize_chunk", errors.New("something odd"), "CRITICAL_TOOL_ERROR", false, true},
		{"noncritical default", "get_current_time", errors.New("something odd"), "TOOL_ERROR", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := classifyToolError(c.tool, c.err)
			if env.OK {
				t.Fatal("error envelope must have ok=false")
			}
			if env.ErrorCode != c.errCode || env.Retryable != c.retryable || env.Fatal != c.fatal {
				t.Fatalf("got {code=%s retryable=%v fatal=%v}, want {code=%s retryable=%v fatal=%v}",
					env.ErrorCode, env.Retryable, env.Fatal, c.errCode, c.retryable, c.fatal)
			}
			// JSON is valid and round-trips ok=false.
			var back ToolErrorEnvelope
			if err := json.Unmarshal([]byte(env.JSON()), &back); err != nil || back.OK {
				t.Fatalf("envelope JSON invalid or ok!=false: %s (err %v)", env.JSON(), err)
			}
		})
	}
}

// TestRunnerToolErrorEnvelope verifies the runner emits the structured envelope
// and fires OnToolError only when V2 is on; off keeps the legacy "错误:" string
// and does not fire the hook.
func TestRunnerToolErrorEnvelope(t *testing.T) {
	buildRunner := func() (*Runner, *[]ToolErrorEnvelope) {
		reg := NewRegistry()
		reg.Register(
			Tool{Type: "function", Function: ToolFunction{Name: "fetch_channel"}},
			func(ctx context.Context, args json.RawMessage) (string, error) {
				return "", errors.New("channel not accessible by user")
			},
		)
		fc := &fakeClient{turns: []AssistantTurn{
			{ToolCalls: []ToolCall{mkToolCall("c1", "fetch_channel", `{}`)}, Tokens: 1},
			{Content: "final", Tokens: 1},
		}}
		r := NewRunner(fc, reg, NewPool(2), Policy{MaxSteps: 5, MaxTokens: 100000})
		var got []ToolErrorEnvelope
		r.OnToolError = func(_ string, env ToolErrorEnvelope) { got = append(got, env) }
		return r, &got
	}

	t.Run("v2 on → envelope + hook", func(t *testing.T) {
		t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
		r, got := buildRunner()
		if _, err := r.Run(context.Background(), "sys", "go"); err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(*got) != 1 || !(*got)[0].Fatal || (*got)[0].ErrorCode != "PERMISSION_DENIED" {
			t.Fatalf("OnToolError not fired with fatal permission error: %+v", *got)
		}
	})

	t.Run("v2 off → no hook (legacy path)", func(t *testing.T) {
		t.Setenv("AGENT_SUMMARY_V2_MODE", "off")
		r, got := buildRunner()
		if _, err := r.Run(context.Background(), "sys", "go"); err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(*got) != 0 {
			t.Fatalf("OnToolError must not fire when V2 is off, got %+v", *got)
		}
	})
}
