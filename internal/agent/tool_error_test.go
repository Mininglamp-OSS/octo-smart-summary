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
		{"panic", "fetch_channel", errors.New("tool fetch_channel panicked: kaboom"), "INTERNAL_ERROR", false, true},
		{"rate limited", "fetch_channel", errors.New("upstream status=429"), "RATE_LIMITED", true, false},
		{"http 5xx", "fetch_channel", errors.New("LLM API error: status=503 body=busy"), "UPSTREAM_ERROR", true, false},
		{"network", "fetch_channel", errors.New("network boom"), "TRANSIENT_TOOL_ERROR", true, false},
		{"db connection", "fetch_channel", errors.New("driver: bad connection"), "TRANSIENT_TOOL_ERROR", true, false},
		{"permission", "fetch_channel", errors.New("channel not accessible by user"), "PERMISSION_DENIED", false, true},
		{"identity", "summarize_chunk", errors.New("missing user identity in context"), "PERMISSION_DENIED", false, true},
		{"invalid args", "fetch_channel", errors.New("parse args: bad json"), "INVALID_ARGUMENT", true, false},
		{"empty time_start (issue C)", "fetch_channel", errors.New("parse time_start: parsing time \"\" as \"2006-01-02T15:04:05Z07:00\": cannot parse \"\" as \"2006\""), "INVALID_ARGUMENT", true, false},
		{"specific required arg", "fetch_channel", errors.New("channel_type is required (1=DM, 2=Group, 5=Thread)"), "INVALID_ARGUMENT", true, false},
		{"bare invalid does not classify as argument", "fetch_channel", errors.New("upstream returned invalid response shape"), "CRITICAL_TOOL_ERROR", false, true},
		{"bare required does not classify as argument", "fetch_channel", errors.New("required backend unavailable"), "CRITICAL_TOOL_ERROR", false, true},
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
		r.OnToolError = func(_, _ string, env ToolErrorEnvelope) { got = append(got, env) }
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

// TestClassifyToolErrorTransientConnectionShapes pins the round-2 P1. Removing
// the bare "invalid" match correctly stopped mysql's ErrInvalidConn being
// swallowed as a bad ARGUMENT, but it then fell to the critical-tool default and
// became FATAL — strictly worse than the previous head for that string, because
// a fatal marker makes the whole run report FAILED.
//
// These are all stale-pooled-connection or interrupted-stream shapes: one class,
// one branch. Splitting them is how `driver: bad connection` ended up retryable
// while `invalid connection` — the same situation — did not.
func TestClassifyToolErrorTransientConnectionShapes(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	for _, msg := range []string{
		"get user channels: invalid connection",
		"fetch messages: mysql: invalid connection",
		"get user channels: sql: connection is already closed",
		"fetch messages: broken pipe",
		`merge summaries: call LLM: Post "https://llm/v1": unexpected EOF`,
		"fetch messages: Error 1213: Deadlock found when trying to get lock",
		"fetch messages: Error 1205: Lock wait timeout exceeded",
	} {
		env := classifyToolError("fetch_channel", errors.New(msg))
		if env.Fatal || !env.Retryable {
			t.Errorf("classifyToolError(%q) = %s retryable=%t fatal=%t, want a retryable non-fatal transient error",
				msg, env.ErrorCode, env.Retryable, env.Fatal)
		}
	}
}

// TestClassifyToolErrorAnchorsHTTPStatuses pins that incidental digits are not
// read as HTTP statuses. Channel and user ids are interpolated into error text,
// so an unanchored "429" match reported a permission failure as a throttle and
// the model retried a call that can never succeed until MaxSteps ran out.
func TestClassifyToolErrorAnchorsHTTPStatuses(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	env := classifyToolError("fetch_channel", errors.New("channel 1234297 not accessible by user u-88"))
	if env.ErrorCode != "PERMISSION_DENIED" {
		t.Errorf("channel id containing 429 = %s, want PERMISSION_DENIED", env.ErrorCode)
	}

	env = classifyToolError("fetch_channel", errors.New("some error with status 5000 in it"))
	if env.ErrorCode == "UPSTREAM_ERROR" {
		t.Error("status 5000 is not a 5xx: the code must not be part of a longer number")
	}

	// Real statuses must still classify.
	for _, msg := range []string{"LLM API error: status=503 body=upstream busy", "http 429 too many"} {
		env = classifyToolError("merge_summaries", errors.New(msg))
		if env.Fatal || !env.Retryable {
			t.Errorf("classifyToolError(%q) = %s fatal=%t, want retryable", msg, env.ErrorCode, env.Fatal)
		}
	}
}

// TestClassifyToolErrorEvidenceReadIsNotAWriteFailure pins the narrowing of the
// bare "evidence" match: a READ failure ("query evidence rows: ...") was being
// reported as a fatal evidence-WRITE failure even when the underlying cause was a
// one-second connection blip.
func TestClassifyToolErrorEvidenceReadIsNotAWriteFailure(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	env := classifyToolError("summarize_chunk", errors.New("get session message pool: query evidence rows: invalid connection"))
	if env.ErrorCode == "EVIDENCE_WRITE_FAILED" {
		t.Error("an evidence READ blip must not be classified as a fatal write failure")
	}
	if env.Fatal {
		t.Errorf("evidence read blip = %s fatal=true, want non-fatal", env.ErrorCode)
	}

	// A genuine write failure stays fatal.
	env = classifyToolError("fetch_channel", errors.New("persist evidence: duplicate key"))
	if env.ErrorCode != "EVIDENCE_WRITE_FAILED" || !env.Fatal {
		t.Errorf("persist evidence failure = %s fatal=%t, want a fatal EVIDENCE_WRITE_FAILED", env.ErrorCode, env.Fatal)
	}
}

// TestClassifyToolErrorStaleHandleIsNotFatalPermission pins the round-4 P1
// (mochashanyao / yujiawei converged): a dropped message-cache handle surfaces as
// `invalid or expired messages_handle: h-123` on a plain cache miss from three
// critical tools. The old text embedded "access denied", so it hit the permission
// branch → fatal + non-retryable, which suppressed the very retry whose success
// would have cleared the marker and reported a good deliverable as FAILED. It must
// classify as a retryable, non-fatal bad argument, and must do so BEFORE the
// permission branch even for the legacy "or access denied" phrasing.
func TestClassifyToolErrorStaleHandleIsNotFatalPermission(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	for _, tool := range []string{"summarize_chunk", "filter_relevant", "search_messages"} {
		for _, msg := range []string{
			"invalid or expired messages_handle: h-123",
			"invalid messages_handle or access denied: h-456", // legacy phrasing must still not be fatal
		} {
			env := classifyToolError(tool, errors.New(msg))
			if env.ErrorCode != "INVALID_ARGUMENT" || env.Fatal || !env.Retryable {
				t.Errorf("classifyToolError(%s, %q) = %s retryable=%t fatal=%t, want retryable non-fatal INVALID_ARGUMENT",
					tool, msg, env.ErrorCode, env.Retryable, env.Fatal)
			}
		}
	}
}

// TestClassifyToolErrorPermissionBeatsWeakTransientTerms pins the round-4
// branch-ordering nit: `permission denied; please try again later` carries both a
// permission word and the weak transient term "try again". The permission branch
// must win — a stale permission is fatal and no amount of retrying clears it —
// while a genuine transient with no permission wording still classifies retryable.
func TestClassifyToolErrorPermissionBeatsWeakTransientTerms(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	env := classifyToolError("fetch_channel", errors.New("permission denied; please try again later"))
	if env.ErrorCode != "PERMISSION_DENIED" || !env.Fatal || env.Retryable {
		t.Errorf("permission-worded 'try again' = %s retryable=%t fatal=%t, want fatal PERMISSION_DENIED",
			env.ErrorCode, env.Retryable, env.Fatal)
	}

	for _, msg := range []string{"backend service unavailable", "transient hiccup, try again"} {
		env := classifyToolError("fetch_channel", errors.New(msg))
		if env.ErrorCode != "TRANSIENT_TOOL_ERROR" || env.Fatal || !env.Retryable {
			t.Errorf("classifyToolError(%q) = %s retryable=%t fatal=%t, want retryable TRANSIENT_TOOL_ERROR",
				msg, env.ErrorCode, env.Retryable, env.Fatal)
		}
	}
}

// TestRunnerReportsToolSuccess pins the mechanism the recoverable failed-marker
// rests on: a successful tool call must be observable, and only when V2 is on.
func TestRunnerReportsToolSuccess(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	reg := NewRegistry()
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "fetch_channel"}},
		func(_ context.Context, _ json.RawMessage) (string, error) { return "{}", nil })

	r := NewRunner(nil, reg, NewPool(1), Policy{})
	var succeeded []string
	r.OnToolSuccess = func(name, _ string) { succeeded = append(succeeded, name) }
	var call ToolCall
	call.Function.Name = "fetch_channel"
	call.Function.Arguments = "{}"
	r.runTools(context.Background(), []ToolCall{call}, 1, 1)

	if len(succeeded) != 1 || succeeded[0] != "fetch_channel" {
		t.Fatalf("OnToolSuccess not fired for a successful call, got %v", succeeded)
	}
}
