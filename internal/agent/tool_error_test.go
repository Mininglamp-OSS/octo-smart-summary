package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
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
		{"outside selected scope", "fetch_channel", &ErrChannelOutsideSelectedScope{ChannelID: "group-2", ChannelType: 2}, "CHANNEL_OUTSIDE_SELECTED_SCOPE", false, false},
		{"channel-scoped permission", "fetch_channel", errors.New("channel not accessible by user"), "PERMISSION_DENIED", false, false},
		{"identity", "summarize_chunk", errors.New("missing user identity in context"), "PERMISSION_DENIED", false, true},
		{"invalid args", "fetch_channel", errors.New("parse args: bad json"), "INVALID_ARGUMENT", true, false},
		{"merge invalid args is completeness-fatal", "merge_summaries", errors.New("parse args: unexpected end of JSON input"), "INVALID_ARGUMENT", true, true},
		{"merge timeout is completeness-fatal", "merge_summaries", context.DeadlineExceeded, "TIMEOUT", true, true},
		{"empty time_start (issue C)", "fetch_channel", errors.New("parse time_start: parsing time \"\" as \"2006-01-02T15:04:05Z07:00\": cannot parse \"\" as \"2006\""), "INVALID_ARGUMENT", true, false},
		{"specific required arg", "fetch_channel", errors.New("channel_type is required (1=DM, 2=Group, 5=Thread)"), "INVALID_ARGUMENT", true, false},
		// The critical-tool default is fatal AND retryable. The two fields answer
		// different questions: fatal = "completeness is compromised if this is never
		// recovered", retryable = "a retry might recover it". An UNRECOGNISED error
		// is not known to be permanent, and this arm's own mitigation (the marker
		// clears when the same tool later succeeds) REQUIRES the retry.
		{"bare invalid does not classify as argument", "fetch_channel", errors.New("upstream returned invalid response shape"), "CRITICAL_TOOL_ERROR", true, true},
		{"bare required does not classify as argument", "fetch_channel", errors.New("required backend unavailable"), "CRITICAL_TOOL_ERROR", true, true},
		{"evidence", "fetch_channel", errors.New("persist evidence: db down"), "EVIDENCE_WRITE_FAILED", false, true},
		{"critical default", "summarize_chunk", errors.New("something odd"), "CRITICAL_TOOL_ERROR", true, true},
		// PR #208 round-5 P2-4. These are per-request store caps: the store only
		// grows within a request, so a retry hits the identical cap. Classified
		// retryable (via the critical-tool default, because the message spells
		// "summary handle" with a space and missed the "summary_handle" branch),
		// they made the runner nudge an identical retry every step until MaxSteps
		// and end the request with ZERO output.
		{
			"store handle-count cap is a deterministic dead end",
			"summarize_chunk",
			fmt.Errorf("store summary result: %w", fmt.Errorf("%w: too many summary handles in one request (max 128)", ErrSummaryHandleCapacity)),
			"RESOURCE_EXHAUSTED", false, true,
		},
		{
			"store text-size cap is a deterministic dead end",
			"summarize_chunk",
			fmt.Errorf("store summary result: %w", fmt.Errorf("%w: summary handle text exceeds per-request limit (8388608 bytes)", ErrSummaryHandleCapacity)),
			"RESOURCE_EXHAUSTED", false, true,
		},
		{
			"store cap on a non-critical tool is not fatal",
			"peek_channel",
			fmt.Errorf("%w: too many summary handles in one request (max 128)", ErrSummaryHandleCapacity),
			"RESOURCE_EXHAUSTED", false, false,
		},
		// The model passing more handles than can exist is NOT a capacity dead
		// end: it can fix its own arguments, so it keeps the retryable handle
		// classification.
		{
			"too many handles in the ARGUMENTS stays retryable",
			"merge_summaries",
			errors.New("too many summary_handles (max 128)"),
			"INVALID_ARGUMENT", true, true,
		},
		{"noncritical default", "get_current_time", errors.New("something odd"), "TOOL_ERROR", true, false},
		// A permission denial is fatal only where it costs the deliverable something.
		{"permission on a non-critical tool is not fatal", "peek_channel", errors.New("channel 12345 not accessible by user u-88"), "PERMISSION_DENIED", false, false},
		{"permission on list_channels is not fatal", "list_channels", errors.New("channel 12345 not accessible by user u-88"), "PERMISSION_DENIED", false, false},
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
				return "", errors.New("persist evidence: db down")
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
		if len(*got) != 1 || !(*got)[0].Fatal || (*got)[0].ErrorCode != "EVIDENCE_WRITE_FAILED" {
			t.Fatalf("OnToolError not fired with fatal evidence-write error: %+v", *got)
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
		env = classifyToolError("fetch_channel", errors.New(msg))
		if env.Fatal || !env.Retryable {
			t.Errorf("classifyToolError(%q) = %s fatal=%t, want retryable", msg, env.ErrorCode, env.Fatal)
		}
	}
}

func TestMergeSummariesErrorsAreAlwaysFatalUntilRecovered(t *testing.T) {
	for _, err := range []error{
		errors.New("parse args: unexpected end of JSON input"),
		context.DeadlineExceeded,
		errors.New("LLM API error: status=503 body=busy"),
		errors.New("invalid or expired summary_handle: map_old_1"),
	} {
		env := classifyToolError("merge_summaries", err)
		if !env.Fatal {
			t.Errorf("merge_summaries error %q must be fatal until a successful retry: %+v", err, env)
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

// TestClassifyToolErrorPermissionAndIdentityTransientOrdering pins the branch
// ordering around permission vs weak transient terms, plus the round-9 P1-2 / P2-1
// carve-outs.
func TestClassifyToolErrorPermissionAndIdentityTransientOrdering(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	// Ordering: a permission-worded "try again" must classify as a non-retryable
	// PERMISSION_DENIED, not flip to a retryable transient. On a RUN-scoped critical
	// tool (no per-channel coverage recorder) it is also fatal.
	env := classifyToolError("summarize_chunk", errors.New("permission denied; please try again later"))
	if env.ErrorCode != "PERMISSION_DENIED" || !env.Fatal || env.Retryable {
		t.Errorf("run-scoped permission 'try again' = %s retryable=%t fatal=%t, want fatal PERMISSION_DENIED",
			env.ErrorCode, env.Retryable, env.Fatal)
	}

	// P1-2: the SAME wording on fetch_channel is channel-scoped — still a
	// non-retryable PERMISSION_DENIED, but NON-fatal (recorded as a failed target,
	// disclosed by the finish gate as PARTIAL + GapChannel).
	env = classifyToolError("fetch_channel", errors.New("permission denied; please try again later"))
	if env.ErrorCode != "PERMISSION_DENIED" || env.Fatal || env.Retryable {
		t.Errorf("channel-scoped permission 'try again' = %s retryable=%t fatal=%t, want non-fatal non-retryable PERMISSION_DENIED",
			env.ErrorCode, env.Retryable, env.Fatal)
	}

	// P2-1: a permanent permission denial that merely mentions identity + a weak
	// transient term must NOT be swallowed as a transient outage — it stays a
	// PERMISSION_DENIED (fatal on a run-scoped tool), or the model retries a denial
	// that can never succeed.
	env = classifyToolError("summarize_chunk", errors.New("identity: permission denied, please try again later"))
	if env.ErrorCode != "PERMISSION_DENIED" || !env.Fatal || env.Retryable {
		t.Errorf("identity+permission 'try again' = %s retryable=%t fatal=%t, want fatal PERMISSION_DENIED",
			env.ErrorCode, env.Retryable, env.Fatal)
	}

	// A genuine identity-service OUTAGE (no permission word) must be retryable and
	// non-fatal, whichever transient shape it takes.
	for _, msg := range []string{
		"backend service unavailable",
		"transient hiccup, try again",
		"identity service unavailable, retry later",
		"identity provider returned 503", // caught as a 5xx (returned NNN) → UPSTREAM_ERROR
	} {
		env := classifyToolError("fetch_channel", errors.New(msg))
		if env.Fatal || !env.Retryable {
			t.Errorf("classifyToolError(%q) = %s retryable=%t fatal=%t, want a retryable non-fatal transient",
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

// TestRunnerSameStepSuccessWinsRegardlessCompletionOrder pins PR #208 round-3
// B1. Duplicate merge_summaries calls share one (tool, target) fatal-marker key.
// A successful full Reduce in the same planner step must win over a failed
// sibling regardless of which worker returns last; otherwise callback scheduling
// can turn a saved deliverable into a false FAILED verdict.
func TestRunnerSameStepSuccessWinsRegardlessCompletionOrder(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	for _, first := range []string{"success", "failure"} {
		t.Run(first+" completes first", func(t *testing.T) {
			started := make(chan string, 2)
			finished := make(chan string, 2)
			release := map[string]chan struct{}{
				"success": make(chan struct{}),
				"failure": make(chan struct{}),
			}

			reg := NewRegistry()
			reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "merge_summaries"}},
				func(_ context.Context, args json.RawMessage) (string, error) {
					var req struct {
						Outcome string `json:"outcome"`
					}
					if err := json.Unmarshal(args, &req); err != nil {
						return "", err
					}
					started <- req.Outcome
					<-release[req.Outcome]
					finished <- req.Outcome
					if req.Outcome == "failure" {
						return "", errors.New("reduce failed")
					}
					return `{"merged_summary":"complete"}`, nil
				})

			r := NewRunner(nil, reg, NewPool(2), Policy{})
			var mu sync.Mutex
			failed := false
			errorsSeen := 0
			successesSeen := 0
			r.OnToolError = func(_, _ string, env ToolErrorEnvelope) {
				mu.Lock()
				defer mu.Unlock()
				errorsSeen++
				if env.Fatal {
					failed = true
				}
			}
			r.OnToolSuccess = func(_, _ string) {
				mu.Lock()
				defer mu.Unlock()
				successesSeen++
				failed = false
			}

			calls := []ToolCall{
				mkToolCall("reduce-success", "merge_summaries", `{"outcome":"success"}`),
				mkToolCall("reduce-failure", "merge_summaries", `{"outcome":"failure"}`),
			}
			done := make(chan struct{})
			go func() {
				r.runTools(context.Background(), calls, 1, 1)
				close(done)
			}()

			<-started
			<-started
			close(release[first])
			if got := <-finished; got != first {
				t.Fatalf("first completed worker = %q, want %q", got, first)
			}
			// Give the first Dispatch return and its callback time to complete before
			// releasing the sibling. This makes both historical callback orders
			// deterministic while the production fix itself does not rely on timing.
			time.Sleep(20 * time.Millisecond)
			second := "success"
			if first == "success" {
				second = "failure"
			}
			close(release[second])
			if got := <-finished; got != second {
				t.Fatalf("second completed worker = %q, want %q", got, second)
			}
			<-done

			mu.Lock()
			defer mu.Unlock()
			if errorsSeen != 1 || successesSeen != 1 {
				t.Fatalf("callbacks = errors:%d successes:%d, want 1 each", errorsSeen, successesSeen)
			}
			if failed {
				t.Fatal("same-step successful Reduce must clear its duplicate sibling failure")
			}
		})
	}
}

// TestFatalIsNotSelfSealing pins round-7 P1-4.
//
// The file's own design note argues the classifier need not be exact because "the
// run's fatal marker is cleared when the same tool later succeeds". That
// mitigation requires a retry — and every fatal envelope also set
// Retryable=false, which is the hint the model is given. Fatal + non-retryable is
// a closed loop, so any unlisted DB/RPC wording turned a transient blip into a
// permanent FAILED, the strongest label the contract has.
//
// The two fields answer different questions and must be allowed to disagree:
// fatal = "completeness is compromised if this is never recovered";
// retryable = "a retry might recover it". An UNRECOGNISED error is by definition
// not known to be permanent.
func TestFatalIsNotSelfSealing(t *testing.T) {
	t.Run("everyday transient wordings stay retryable", func(t *testing.T) {
		for _, tc := range []struct{ tool, msg string }{
			{"fetch_channel", "Error 1040: Too many connections"},
			{"fetch_channel", "im rpc: Unavailable desc = transport is closing"},
			{"fetch_channel", "server closed idle connection"},
			{"summarize_chunk", "unexpected end of JSON input"},
			{"summarize_chunk", "upstream returned 502"},
		} {
			env := classifyToolError(tc.tool, errors.New(tc.msg))
			if !env.Retryable {
				t.Errorf("classifyToolError(%q, %q) retryable=false — the fatal marker's own mitigation is the retry: %+v",
					tc.tool, tc.msg, env)
			}
		}
	})

	// A permission denial can never be retried into success, so it stays
	// non-retryable — and therefore its fatal marker is unclearable BY
	// CONSTRUCTION. A denial scoped to ONE channel must not be fatal; fetch_channel
	// records the failed target and the finish gate reports PARTIAL with a gap.
	t.Run("channel-scoped permission is not run-fatal", func(t *testing.T) {
		// fetch_channel is the critical tool whose every error path records the
		// failed channel, so its permission denial is channel-scoped; the rest are
		// non-critical, so permission is non-fatal for them anyway.
		for _, tool := range []string{"peek_channel", "list_channels", "narrow_channels_by_topic", "fetch_channel"} {
			env := classifyToolError(tool, errors.New("channel 12345 not accessible by user u-88"))
			if env.Fatal {
				t.Errorf("a channel-scoped permission denial on %s must not mark the whole run FAILED: %+v", tool, env)
			}
			if env.Retryable {
				t.Errorf("a permission denial is never retryable: %+v", env)
			}
		}

		// fetch_channel's OTHER per-channel permission paths (all call recordFetch)
		// are covered by the same tool-identity carve-out, not by matching each
		// spelling (round-9 P1-2, the under-inclusive half).
		for _, msg := range []string{
			"get user channels: permission denied for channel 12345",
			"fetch messages: forbidden",
		} {
			if env := classifyToolError("fetch_channel", errors.New(msg)); env.Fatal {
				t.Errorf("fetch_channel per-channel permission path must not be run-fatal: %q → %+v", msg, env)
			}
		}

		// search_messages has NO coverage recorder, so its ONE real permission-branch
		// string — a run-scoped identity failure — must stay fatal (round-9 P1-2, the
		// over-inclusive half: the previous arm pinned strings search_messages cannot
		// emit and would have let the gate report COMPLETE).
		if env := classifyToolError("search_messages", errors.New("missing user identity in context")); !env.Fatal {
			t.Errorf("search_messages run-scoped identity failure must stay fatal: %+v", env)
		}

		// A run-scoped identity/auth failure is still fatal on any critical tool.
		if env := classifyToolError("summarize_chunk", errors.New("missing user identity in context")); !env.Fatal {
			t.Errorf("run-scoped identity failure must stay fatal: %+v", env)
		}
	})
}
