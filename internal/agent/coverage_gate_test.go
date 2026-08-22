package agent

// SS-12-b pre-freeze coverage gate — the pure, dependency-free half.
//
// The cgo-gated half (a real sqlite run row + spec, the freeze actually not
// happening, and the repaired channel's messages coming out CITABLE) lives in
// coverage_gate_cgo_test.go.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
)

// The cheap gates must return BEFORE GetSummaryDeps, which panics when unset.
// Summary deps are deliberately not set in this test, so a regression that moves
// the flag/run checks below the deps read fails loudly here instead of taking
// down the flag-off path in production.
func TestCheckCoverageBeforeFreeze_CheapGatesNeverTouchDeps(t *testing.T) {
	for name, tc := range map[string]struct {
		v2    string
		uid   string
		runID string
	}{
		"v2 off":         {"off", "u1", "run-1"},
		"no run id":      {"on", "u1", ""},
		"no uid":         {"on", "", "run-1"},
		"off and no run": {"off", "u1", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_SUMMARY_V2_MODE", tc.v2)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("gate reached GetSummaryDeps and panicked (%v) — the cheap gates must return first", r)
				}
			}()
			if err := checkCoverageBeforeFreeze(context.Background(), tc.uid, "sess", tc.runID); err != nil {
				t.Fatalf("err = %v, want nil (gate must be a no-op here)", err)
			}
		})
	}
}

// A gate error must come out RETRYABLE and NON-FATAL. summarize_chunk is in
// criticalTools, so the default arm of classifyToolError would mark it fatal and
// the run FAILED — trading "incomplete summary" for "no summary", which is the
// one trade this feature must never make.
func TestClassifyToolError_CoverageGateIsRetryableNonFatal(t *testing.T) {
	err := &CoverageGateError{
		Missing:     []summaryspec.Channel{{ChannelID: "c-1", Type: "group"}},
		Instruction: "覆盖检查未通过: channel_id=\"c-1\" channel_type=2",
	}
	env := classifyToolError("summarize_chunk", err)
	if !env.Retryable {
		t.Error("retryable = false; the SS-12 §5 prompt only tells the planner to retry on retryable=true")
	}
	if env.Fatal {
		t.Error("fatal = true; a coverage gap must not mark the run FAILED — it must be recoverable within the run")
	}
	if env.ErrorCode != "COVERAGE_INCOMPLETE" {
		t.Errorf("error_code = %q, want COVERAGE_INCOMPLETE", env.ErrorCode)
	}
}

// The instruction embeds user-controlled channel NAMES verbatim. Classification
// is keyed on the error TYPE, so a channel called "无权限组" / "timeout" cannot
// steer summarize_chunk into PERMISSION_DENIED (fatal, non-retryable) or
// TIMEOUT. Text-keyed classification would have made that a real, remotely
// triggerable way to fail a run.
func TestClassifyToolError_CoverageGateNotSteeredByChannelName(t *testing.T) {
	for _, name := range []string{"permission denied", "timeout", "unauthorized", "not accessible", "429 rate limit"} {
		err := &CoverageGateError{
			Missing:     []summaryspec.Channel{{ChannelID: "c-1", Name: name, Type: "group"}},
			Instruction: buildCoverageGateInstruction([]summaryspec.Channel{{ChannelID: "c-1", Name: name, Type: "group"}}, summaryspec.TimeRange{}),
		}
		env := classifyToolError("summarize_chunk", err)
		if env.ErrorCode != "COVERAGE_INCOMPLETE" || env.Fatal || !env.Retryable {
			t.Errorf("channel name %q steered classification to %+v", name, env)
		}
	}
}

// The blocking review item: the instruction must carry the INTEGER channel_type
// fetch_channel declares, not the Spec's STRING type. `{"channel_type":"group"}`
// fails argument decoding BEFORE recordFetch runs, so the channel stays "never
// attempted" and the gate repeats the identical instruction forever.
func TestBuildCoverageGateInstruction_IntegerChannelType(t *testing.T) {
	got := buildCoverageGateInstruction([]summaryspec.Channel{
		{ChannelID: "c-dm", Name: "老板", Type: "direct"},
		{ChannelID: "c-grp", Name: "产品群", Type: "group"},
		{ChannelID: "c-thr", Name: "子区", Type: "thread"},
	}, summaryspec.TimeRange{Start: 1755000000, End: 1755086400})

	for _, want := range []string{
		`channel_id="c-dm" channel_type=1`,
		`channel_id="c-grp" channel_type=2`,
		`channel_id="c-thr" channel_type=5`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction missing %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"channel_type=group", "channel_type=direct", "channel_type=thread"} {
		if strings.Contains(got, bad) {
			t.Errorf("instruction emits the STRING channel type %q — fetch_channel rejects it:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "time_start=") || !strings.Contains(got, time.Unix(1755000000, 0).Format(time.RFC3339)) {
		t.Errorf("instruction must carry an RFC3339 window:\n%s", got)
	}
	if !strings.Contains(got, "不要编造") {
		t.Errorf("instruction must forbid fabrication for a genuinely unreachable channel:\n%s", got)
	}
}

// An unmappable type must never be printed as channel_type=0: fetch_channel
// rejects 0 with "channel_type is required", so an instruction the tool refuses
// is worse than no instruction at all.
func TestBuildCoverageGateInstruction_NeverEmitsZeroType(t *testing.T) {
	got := buildCoverageGateInstruction([]summaryspec.Channel{{ChannelID: "c-1"}}, summaryspec.TimeRange{})
	if strings.Contains(got, "channel_type=0") {
		t.Fatalf("emitted channel_type=0, which fetch_channel always rejects:\n%s", got)
	}
	if strings.Contains(got, "1970") {
		t.Fatalf("an empty TimeRange must not render as epoch zero:\n%s", got)
	}
	if !strings.Contains(got, "沿用") {
		t.Fatalf("without a window the instruction must tell the planner to reuse its own:\n%s", got)
	}
}

// One mapping, one definition. The handler's selected-channels prompt and this
// gate print the same value into the same fetch_channel argument.
func TestToolChannelType(t *testing.T) {
	for in, want := range map[string]int{
		"direct": 1, "group": 2, "thread": 5,
		"GROUP": 2, " thread ": 5,
		"": 0, "wat": 0,
	} {
		if got := ToolChannelType(in); got != want {
			t.Errorf("ToolChannelType(%q) = %d, want %d", in, got, want)
		}
	}
}

// Termination, the property that makes this safe to put in front of the freeze.
// A planner that cannot fetch the channel re-calls summarize_chunk unchanged;
// the signature does not move, so the fan-out budget drains and the gate stops
// blocking. The freeze then proceeds and the run still produces a summary.
func TestAdmitCoverageBlock_TerminatesWhenPlannerMakesNoProgress(t *testing.T) {
	runID := "run-no-progress"
	forgetCoverageGateRun(runID)
	t.Cleanup(func() { forgetCoverageGateRun(runID) })

	blocked := 0
	for i := 0; i < 100; i++ {
		block, _ := admitCoverageBlock(runID, "same-signature", 2)
		if !block {
			break
		}
		blocked++
	}
	if blocked == 0 {
		t.Fatal("never blocked once — the gate would never fire at all")
	}
	if blocked > maxBlocksPerCoverageRound {
		t.Fatalf("blocked %d times at one signature, want at most %d", blocked, maxBlocksPerCoverageRound)
	}
	if block, _ := admitCoverageBlock(runID, "same-signature", 2); block {
		t.Fatal("still blocking after the budget drained — the freeze must be allowed to proceed")
	}
}

// A round is keyed on coverage state, so ONE planner step that fans out N
// parallel summarize_chunk calls costs ONE round, not N. Without this a 3-way
// fan-out would exhaust the default 2-round budget in a single step and the gate
// would effectively never fire.
func TestAdmitCoverageBlock_FanOutInOneStepCostsOneRound(t *testing.T) {
	runID := "run-fan-out"
	forgetCoverageGateRun(runID)
	t.Cleanup(func() { forgetCoverageGateRun(runID) })

	for i := 0; i < 3; i++ {
		block, round := admitCoverageBlock(runID, "sig-A", 2)
		if !block {
			t.Fatalf("parallel call %d was not blocked", i)
		}
		if round != 1 {
			t.Fatalf("parallel call %d reported round %d, want 1 — a fan-out is one round", i, round)
		}
	}
	// The planner fetched something: new coverage state, new round.
	block, round := admitCoverageBlock(runID, "sig-B", 2)
	if !block || round != 2 {
		t.Fatalf("after progress: block=%v round=%d, want true/2", block, round)
	}
	// Budget exhausted: a third distinct state must NOT block.
	if block, _ := admitCoverageBlock(runID, "sig-C", 2); block {
		t.Fatal("blocked a 3rd round with maxRounds=2 — the cap is not enforced")
	}
}

// maxRounds<=0 is the documented kill switch and must never block.
func TestAdmitCoverageBlock_ZeroRoundsDisables(t *testing.T) {
	if block, _ := admitCoverageBlock("run-x", "sig", 0); block {
		t.Fatal("maxRounds=0 must disable the gate entirely")
	}
	if block, _ := admitCoverageBlock("", "sig", 2); block {
		t.Fatal("an empty run id must never block")
	}
}

// attempted_channels order reflects fetch scheduling, not coverage. Two parallel
// calls that observed the same set in different orders must agree they are the
// same round, or a fan-out silently costs several rounds.
func TestCoverageSignature_OrderIndependent(t *testing.T) {
	a := coverageSignature([]string{"c1", "c2", "c3"}, []string{"c9", "c8"})
	b := coverageSignature([]string{"c3", "c1", "c2"}, []string{"c8", "c9"})
	if a != b {
		t.Fatalf("signature is order-dependent: %q vs %q", a, b)
	}
	if a == coverageSignature([]string{"c1", "c2"}, []string{"c9", "c8"}) {
		t.Fatal("signature must change when the attempted set changes — progress has to be observable")
	}
}

// A corrupted attempted_channels JSON must not be silently read as "nothing was
// fetched": that is exactly the state that makes the gate block, so the one log
// line that explains a mysterious block has to exist. (Behaviourally it still
// degrades to nil; this pins the decode contract.)
func TestDecodeAttemptedChannels(t *testing.T) {
	if got := decodeAttemptedChannels(`["c1","c2"]`, "run"); len(got) != 2 || got[0] != "c1" {
		t.Fatalf("decode = %v, want [c1 c2]", got)
	}
	if got := decodeAttemptedChannels("", "run"); got != nil {
		t.Fatalf("empty = %v, want nil", got)
	}
	if got := decodeAttemptedChannels("{not json", "run"); got != nil {
		t.Fatalf("corrupt = %v, want nil", got)
	}
}

// Unknown ids must degrade to a bare ChannelID rather than vanish: a channel we
// cannot describe is still a channel the run owes.
func TestChannelsForIDsPreservesOrderAndUnknowns(t *testing.T) {
	all := []summaryspec.Channel{
		{ChannelID: "a", Name: "A", Type: "group"},
		{ChannelID: "b", Name: "B", Type: "direct"},
	}
	got := channelsForIDs(all, []string{"b", "zzz", "a"})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (an unknown id must not be dropped)", len(got))
	}
	if got[0].ChannelID != "b" || got[0].Name != "B" {
		t.Fatalf("got[0] = %+v, want the full record for b, in ids order", got[0])
	}
	if got[1].ChannelID != "zzz" || got[1].Name != "" {
		t.Fatalf("got[1] = %+v, want a bare ChannelID for the unknown id", got[1])
	}
	if got[2].ChannelID != "a" {
		t.Fatalf("got[2] = %+v, want ids order preserved", got[2])
	}
}

// Stale run state must not accumulate forever in a long-lived process.
func TestCoverageGateStateExpires(t *testing.T) {
	runID := "run-ttl"
	forgetCoverageGateRun(runID)
	t.Cleanup(func() {
		coverageGateClock = time.Now
		forgetCoverageGateRun(runID)
	})

	base := time.Now()
	coverageGateClock = func() time.Time { return base }
	if block, _ := admitCoverageBlock(runID, "sig", 1); !block {
		t.Fatal("first call must block")
	}
	coverageGateClock = func() time.Time { return base.Add(coverageGateTTL + time.Minute) }
	// Sweep happens on the next admit; a fresh unrelated run triggers it.
	admitCoverageBlock("run-other", "sig", 1)
	t.Cleanup(func() { forgetCoverageGateRun("run-other") })

	coverageGateMu.Lock()
	_, still := coverageGateRuns[runID]
	coverageGateMu.Unlock()
	if still {
		t.Fatal("expired run state was not swept — state grows unbounded in a long-lived process")
	}
}

// The bound is not decorative: exceeding the runner's MaxSteps makes
// RunWithHistory return "max steps exceeded", i.e. NO summary at all — strictly
// worse than the incomplete-but-disclosed summary the gate is protecting. The
// serial worst case (maxRounds × fan-out width) must therefore fit inside the
// summary profile's step budget with room for the run's real work.
func TestCoverageGateWorstCaseFitsStepBudget(t *testing.T) {
	const defaultMaxRounds = 2 // SUMMARY_REPAIR_MAX_ROUNDS default
	const summaryProfileMaxSteps = 15
	if worst := defaultMaxRounds * maxBlocksPerCoverageRound; worst >= summaryProfileMaxSteps {
		t.Fatalf("worst-case blocked calls = %d, which can exhaust the summary profile's MaxSteps (%d) and end the run with no summary at all", worst, summaryProfileMaxSteps)
	}
}

// P1-1 regression, pinned at the runner rather than asserted by inspection.
//
// The post-answer repair loop injected its instruction through
// RunWithHistory's userMessage parameter, which the runner records as
// Message{Role: "user"} and returns in newMsgs. The handler persists newMsgs
// verbatim, and History() renders every non-empty user/assistant message as a
// chat bubble — so an internal control prompt appeared in the transcript AS
// SOMETHING THE USER SAID, and poisoned LoadHistory for every later turn.
//
// The pre-freeze gate carries the identical text, but as a TOOL ERROR. This
// test drives a failing tool through the real runner and asserts the instruction
// comes back only on role="tool" — the role History() skips. If anyone ever
// re-routes gate text through a user turn, this fails.
func TestCoverageGateInstructionNeverBecomesAUserMessage(t *testing.T) {
	const instruction = "覆盖检查未通过:用户明确选定的以下频道还没有被 fetch_channel 抓取过"

	reg := NewRegistry()
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "summarize_chunk"}},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			return "", &CoverageGateError{
				Missing:     []summaryspec.Channel{{ChannelID: "ch-B", Type: "group"}},
				Instruction: instruction,
			}
		})

	fc := &fakeClient{turns: []AssistantTurn{
		{ToolCalls: []ToolCall{mkToolCall("call-1", "summarize_chunk", `{"messages_handle":"h1"}`)}},
		{Content: "final answer"},
	}}
	r := newTestRunner(fc, reg, Policy{MaxSteps: 5, MaxTokens: 10000, StepTimeout: 5 * time.Second})

	_, newMsgs, err := r.RunWithHistory(context.Background(), "system", nil, "总结这些频道")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	sawOnTool := false
	for _, m := range newMsgs {
		if !strings.Contains(m.Content, instruction) {
			continue
		}
		// This is the assertion that matters: History() bubbles exactly the
		// user/assistant roles, so the gate text must never wear one.
		if m.Role == "user" || m.Role == "assistant" {
			t.Fatalf("gate instruction surfaced as role=%q — it would render in the user's transcript as their own message and poison LoadHistory:\n%s",
				m.Role, m.Content)
		}
		if m.Role == "tool" {
			sawOnTool = true
		}
	}
	if !sawOnTool {
		t.Fatal("gate instruction never reached the planner at all — the retryable contract depends on the model seeing it as a tool result")
	}

	// The user's own turn is persisted, and it is theirs alone.
	userTurns := 0
	for _, m := range newMsgs {
		if m.Role == "user" {
			userTurns++
			if m.Content != "总结这些频道" {
				t.Fatalf("user turn content = %q, want the user's actual message", m.Content)
			}
		}
	}
	if userTurns != 1 {
		t.Fatalf("user turns = %d, want exactly 1 (the gate must not manufacture extra user turns)", userTurns)
	}
}
