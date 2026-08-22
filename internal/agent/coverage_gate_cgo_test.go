//go:build cgo
// +build cgo

package agent

// SS-12-b pre-freeze coverage gate — end-to-end against a real run row, a real
// spec, and a real (in-memory) citation manifest.
//
// The load-bearing test here is
// TestCoverageGate_RepairedChannelIsCitable: it is the regression the reviewers
// asked for by name. Under the OLD post-answer loop the repaired channel's
// messages were fetched after the freeze, so applyFrozenManifest dropped every
// one of them, summarize_chunk returned 无可总结内容 + dropped_count, and the
// model told the user the channel was empty. Here the gate blocks BEFORE the
// freeze, the planner fetches, and the repaired channel's messages reach the
// model with real citation ordinals.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/artifact"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

// echoLLM stands in for the Map model and records the prompt it was fed, so a
// test can assert WHICH messages (and which [n] ordinals) actually reached the
// model rather than trusting the coverage counters alone.
type echoLLM struct {
	srv    *httptest.Server
	prompt string
}

func newEchoLLM(t *testing.T) *echoLLM {
	t.Helper()
	e := &echoLLM{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				e.prompt += m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"summary ok"}}],"usage":{"total_tokens":10}}`)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *echoLLM) cfg() config.Config {
	return config.Config{
		LLMApiURL:              e.srv.URL,
		LLMModel:               "test-model",
		LLMTimeout:             10,
		LLMMaxToken:            256,
		SummaryRepairMaxRounds: 2,
		CharsPerTokenCJK:       1,
		CharsPerTokenASCII:     4,
	}
}

// coverageGateFixture builds a closed-scope run whose spec expects two channels.
type coverageGateFixture struct {
	db        *gorm.DB
	runStore  *summaryrun.Store
	runID     string
	uid       string
	sessionID string
}

func newCoverageGateFixture(t *testing.T, fetchExpected bool, expected []summaryspec.Channel) *coverageGateFixture {
	t.Helper()
	db := newCoverageGateTestDB(t)
	if db == nil {
		return nil
	}
	const uid = "u-gate"
	sessionID := "sess-" + t.Name()
	ctx := context.Background()
	store := summaryrun.NewStore(db)

	run, _, err := store.CreateOrGetRunWithFetchExpectation(ctx, uid, sessionID, "req-"+t.Name(), model.ScopePolicyClosed, fetchExpected)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	objective := "总结这两个频道"
	spec, sources, err := summaryspec.Validate(summaryspec.Draft{
		Objective: &objective,
		Channels:  expected,
		TimeRange: &summaryspec.TimeRange{Start: 1755000000, End: 1755086400},
	}, summaryspec.Options{ProvidedSource: summaryspec.SourceUser, ChannelSource: summaryspec.SourceUI})
	if err != nil {
		t.Fatalf("validate spec: %v", err)
	}
	if _, err := store.SaveSpec(ctx, run, run.Version, spec, sources, objective); err != nil {
		t.Fatalf("save spec: %v", err)
	}
	t.Cleanup(func() { forgetCoverageGateRun(run.RunID) })
	return &coverageGateFixture{db: db, runStore: store, runID: run.RunID, uid: uid, sessionID: sessionID}
}

// seedFetched simulates fetch_channel having run for one channel: coverage
// recorded on the run row, messages cached, evidence persisted.
func (f *coverageGateFixture) seedFetched(t *testing.T, channelID string, msgs []pipeline.Message) string {
	t.Helper()
	if err := f.runStore.RecordChannelFetch(context.Background(), f.uid, f.runID, channelID, true, false); err != nil {
		t.Fatalf("record fetch %s: %v", channelID, err)
	}
	handle := messageCache.Store(msgs, f.uid)
	t.Cleanup(func() {
		messageCache.mu.Lock()
		delete(messageCache.store, handle)
		messageCache.mu.Unlock()
	})
	if err := seedEvidence(f.db, f.uid, f.sessionID, handle, msgs); err != nil {
		t.Fatalf("seed evidence %s: %v", channelID, err)
	}
	return handle
}

func (f *coverageGateFixture) toolCtx() context.Context {
	ctx := context.WithValue(context.Background(), ContextKeyUID, f.uid)
	ctx = context.WithValue(ctx, ContextKeySessionID, f.sessionID)
	return context.WithValue(ctx, ContextKeyRunID, f.runID)
}

func (f *coverageGateFixture) toolCtxAtStep(step int) context.Context {
	return withCoverageGateStep(f.toolCtx(), step, profiles["summary"].Policy.MaxSteps)
}

func (f *coverageGateFixture) frozen(t *testing.T) bool {
	t.Helper()
	_, _, found, err := artifact.NewStore(f.db).GetFrozenManifestByRun(context.Background(), f.uid, f.runID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return found
}

var twoChannelSpec = []summaryspec.Channel{
	{ChannelID: "ch-A", Name: "产品群", Type: "group"},
	{ChannelID: "ch-B", Name: "研发群", Type: "group"},
}

// newCoverageGateTestDB is newFirstTurnTestDB plus agent_summary_spec: the gate
// reads the run's persisted Spec to learn which channels were expected, so a
// fixture without that table cannot exercise it at all.
func newCoverageGateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newFirstTurnTestDB(t)
	if db == nil {
		return nil
	}
	if err := db.AutoMigrate(&model.AgentSummarySpec{}); err != nil {
		t.Fatalf("migrate spec table: %v", err)
	}
	return db
}

// A closed-scope run with an expected channel that was never fetched must NOT
// freeze the manifest. The error must name the channel with an INTEGER
// channel_type and be retryable + non-fatal.
func TestCoverageGate_BlocksFreezeAndNamesMissingChannel(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	f := newCoverageGateFixture(t, true, twoChannelSpec)
	if f == nil {
		return
	}
	llm := newEchoLLM(t)
	SetSummaryDeps(f.db, nil, nil, llm.cfg())
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	// Only ch-A was fetched. ch-B is expected but never attempted.
	handle := f.seedFetched(t, "ch-A", []pipeline.Message{
		{ChannelID: "ch-A", MessageSeq: 1, Timestamp: 1000, Content: "A one"},
	})

	_, h := SummarizeChunkTool()
	args, _ := json.Marshal(map[string]string{"messages_handle": handle})
	_, err := h(f.toolCtx(), args)
	if err == nil {
		t.Fatal("summarize_chunk succeeded with an expected channel unfetched — the manifest would freeze without ch-B and ch-B could never be cited")
	}

	var gate *CoverageGateError
	if !errors.As(err, &gate) {
		t.Fatalf("err = %v (%T), want *CoverageGateError", err, err)
	}
	if len(gate.Missing) != 1 || gate.Missing[0].ChannelID != "ch-B" {
		t.Fatalf("missing = %+v, want exactly ch-B", gate.Missing)
	}
	if !strings.Contains(gate.Instruction, `channel_id="ch-B"`) || !strings.Contains(gate.Instruction, "channel_type=2") {
		t.Fatalf("instruction must name ch-B with the INTEGER channel_type fetch_channel accepts:\n%s", gate.Instruction)
	}
	if strings.Contains(gate.Instruction, "channel_type=group") {
		t.Fatalf("instruction emits the STRING type, which fetch_channel rejects during arg decoding:\n%s", gate.Instruction)
	}

	// THE point of the placement: nothing froze.
	if f.frozen(t) {
		t.Fatal("citation manifest froze despite the gate — a later ch-B fetch would be uncitable, which is the whole defect")
	}
	// And the run must not have been marked as having dropped anything.
	run, _ := f.runStore.GetByID(context.Background(), f.uid, f.runID)
	if run.DroppedMessages != 0 {
		t.Fatalf("dropped_messages = %d, want 0 — blocking must not manufacture a dropped-messages gap", run.DroppedMessages)
	}

	env := classifyToolError("summarize_chunk", err)
	if !env.Retryable || env.Fatal {
		t.Fatalf("envelope = %+v, want retryable=true fatal=false (summarize_chunk is a criticalTool; fatal would mark the run FAILED)", env)
	}
}

// THE regression the reviewers asked for by name: "repair must still summarize
// and cite the repaired channel's messages".
//
// Under the old post-answer loop this was impossible — the repair fetch was
// post-freeze, every message missed the manifest, and the tool reported
// 无可总结内容. Here the planner obeys the gate, fetches ch-B, re-calls
// summarize_chunk, and ch-B's messages must reach the model with real ordinals.
func TestCoverageGate_RepairedChannelIsCitable(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	f := newCoverageGateFixture(t, true, twoChannelSpec)
	if f == nil {
		return
	}
	llm := newEchoLLM(t)
	SetSummaryDeps(f.db, nil, nil, llm.cfg())
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	_, h := SummarizeChunkTool()
	handleA := f.seedFetched(t, "ch-A", []pipeline.Message{
		{ChannelID: "ch-A", MessageSeq: 1, Timestamp: 1000, Content: "A one"},
	})

	// Round 1: blocked.
	argsA, _ := json.Marshal(map[string]string{"messages_handle": handleA})
	if _, err := h(f.toolCtxAtStep(1), argsA); err == nil {
		t.Fatal("round 1 must be blocked")
	}

	// The planner obeys: it fetches ch-B. This is the fetch the old design could
	// only ever make AFTER the freeze.
	handleB := f.seedFetched(t, "ch-B", []pipeline.Message{
		{ChannelID: "ch-B", MessageSeq: 7, Timestamp: 2000, Content: "B seven"},
		{ChannelID: "ch-B", MessageSeq: 8, Timestamp: 3000, Content: "B eight"},
	})

	// Round 2: coverage is complete, so the gate stands aside and the freeze
	// happens over a pool that INCLUDES ch-B.
	argsB, _ := json.Marshal(map[string]string{"messages_handle": handleB})
	out, err := h(f.toolCtxAtStep(2), argsB)
	if err != nil {
		t.Fatalf("round 2 summarize_chunk: %v", err)
	}
	var result struct {
		Summary        string `json:"summary"`
		InputCount     int    `json:"input_count"`
		ProcessedCount int    `json:"processed_count"`
		DroppedCount   int    `json:"dropped_count"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode result: %v; out=%s", err, out)
	}

	// The old failure mode, asserted directly.
	if result.Summary == "无可总结内容" {
		t.Fatal(`repaired channel summarized as "无可总结内容" — this is exactly the post-freeze defect: the model would then tell the user the channel was empty`)
	}
	if result.DroppedCount != 0 {
		t.Fatalf("dropped_count = %d, want 0 — the repaired channel's messages must be IN the manifest, not dropped by it", result.DroppedCount)
	}
	if result.InputCount != 2 || result.ProcessedCount != 2 {
		t.Fatalf("input=%d processed=%d, want 2/2 (both ch-B messages summarized)", result.InputCount, result.ProcessedCount)
	}

	// Citability, checked where it actually matters: what the model was fed.
	if !strings.Contains(llm.prompt, "B seven") || !strings.Contains(llm.prompt, "B eight") {
		t.Fatalf("ch-B's messages never reached the model:\n%s", llm.prompt)
	}
	if strings.Contains(llm.prompt, "[0]") {
		t.Fatalf("model was told to cite [0] — an uncitable message leaked into the chunk:\n%s", llm.prompt)
	}
	// ch-A sorts first by timestamp, so ch-B's messages own ordinals 2 and 3.
	for _, want := range []string{"[2]", "[3]"} {
		if !strings.Contains(llm.prompt, want) {
			t.Fatalf("expected citation ordinal %s in the prompt:\n%s", want, llm.prompt)
		}
	}
	if !f.frozen(t) {
		t.Fatal("manifest never froze on the complete-coverage round")
	}

	// And the frozen manifest itself contains ch-B — the durable proof that the
	// save-time citation pass can resolve it too.
	_, entries, _, err := artifact.NewStore(f.db).GetFrozenManifestByRun(context.Background(), f.uid, f.runID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	ord := artifact.OrdinalMap(entries)
	for _, key := range []string{"ch-B:7", "ch-B:8"} {
		if ord[key] < 1 {
			t.Fatalf("%s missing from the frozen manifest (ordinals=%v) — it would be uncitable at save time", key, ord)
		}
	}
}

// Bounded: a channel the planner CANNOT fetch (no permission, deleted) must not
// block forever. After the cap the freeze proceeds and the run still delivers a
// summary — an incomplete summary is never traded for no summary. The finish
// gate then discloses the gap.
func TestCoverageGate_AfterCapFreezeProceedsAndRunStillSummarizes(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	f := newCoverageGateFixture(t, true, twoChannelSpec)
	if f == nil {
		return
	}
	llm := newEchoLLM(t)
	SetSummaryDeps(f.db, nil, nil, llm.cfg())
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	handle := f.seedFetched(t, "ch-A", []pipeline.Message{
		{ChannelID: "ch-A", MessageSeq: 1, Timestamp: 1000, Content: "A one"},
	})
	_, h := SummarizeChunkTool()
	args, _ := json.Marshal(map[string]string{"messages_handle": handle})

	// The planner keeps re-calling without ever fetching ch-B (it can't).
	var lastOut string
	var lastErr error
	calls := 0
	for i := 0; i < 40; i++ {
		calls++
		lastOut, lastErr = h(f.toolCtxAtStep(i+1), args)
		if lastErr == nil {
			break
		}
		var gate *CoverageGateError
		if !errors.As(lastErr, &gate) {
			t.Fatalf("call %d returned an unexpected error: %v", i+1, lastErr)
		}
	}
	if lastErr != nil {
		t.Fatalf("gate never gave up after %d calls — an unfetchable channel would loop forever and the user would get NO summary; last err: %v", calls, lastErr)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2: one blocked step followed by immediate no-progress release", calls)
	}

	var result struct {
		Summary      string `json:"summary"`
		DroppedCount int    `json:"dropped_count"`
	}
	if err := json.Unmarshal([]byte(lastOut), &result); err != nil {
		t.Fatalf("decode result: %v; out=%s", err, lastOut)
	}
	if result.Summary == "" || result.Summary == "无可总结内容" {
		t.Fatalf("after the cap the run must still produce a real summary, got %q", result.Summary)
	}
	if !f.frozen(t) {
		t.Fatal("freeze must proceed once the gate gives up")
	}

	// The gap is still honest and still PRECISE: ch-B was never attempted, so the
	// finish gate keeps its actionable GapChannel instead of the vague
	// dropped-messages gap the old loop produced.
	run, err := f.runStore.GetByID(context.Background(), f.uid, f.runID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	var attempted, succeeded []string
	_ = json.Unmarshal([]byte(run.AttemptedChannels), &attempted)
	_ = json.Unmarshal([]byte(run.SucceededChannels), &succeeded)
	for _, set := range [][]string{attempted, succeeded} {
		for _, ch := range set {
			if ch == "ch-B" {
				t.Fatal("ch-B must not appear in attempted/succeeded — it was never fetched, and claiming it flips an honest PARTIAL into a false COMPLETE")
			}
		}
	}
	if run.DroppedMessages != 0 {
		t.Fatalf("dropped_messages = %d, want 0 — the gate must not manufacture a dropped-messages gap in place of the precise channel gap", run.DroppedMessages)
	}
}

// FetchExpected=false is a turn that was never owed a fetch (SS-08b confident
// rewrite / a refine turn routed away from fetching). The finish gate skips its
// own absence audit there; the gate must stay out of the way too — and worse, a
// confident rewrite has its fetch tools physically stripped, so a demand it
// cannot satisfy would burn the whole step budget for nothing.
func TestCoverageGate_NeverFiresWhenFetchWasNotExpected(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	f := newCoverageGateFixture(t, false, twoChannelSpec)
	if f == nil {
		return
	}
	llm := newEchoLLM(t)
	SetSummaryDeps(f.db, nil, nil, llm.cfg())
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	handle := f.seedFetched(t, "ch-A", []pipeline.Message{
		{ChannelID: "ch-A", MessageSeq: 1, Timestamp: 1000, Content: "A one"},
	})
	_, h := SummarizeChunkTool()
	args, _ := json.Marshal(map[string]string{"messages_handle": handle})
	if _, err := h(f.toolCtx(), args); err != nil {
		t.Fatalf("gate fired on a turn that was never owed a fetch: %v", err)
	}
	if !f.frozen(t) {
		t.Fatal("freeze must proceed normally when no fetch was expected")
	}
}

// Open scope has no authoritative expected set, so nothing can be proven
// missing and the gate must never fire.
func TestCoverageGate_OpenScopeNeverFires(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	db := newCoverageGateTestDB(t)
	if db == nil {
		return
	}
	const uid = "u-open"
	const sessionID = "sess-open"
	ctx := context.Background()
	store := summaryrun.NewStore(db)
	run, _, err := store.CreateOrGetRun(ctx, uid, sessionID, "req-open", model.ScopePolicyOpen)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() { forgetCoverageGateRun(run.RunID) })

	objective := "总结我这周所有群"
	spec, sources, err := summaryspec.Validate(summaryspec.Draft{Objective: &objective, Channels: twoChannelSpec},
		summaryspec.Options{ProvidedSource: summaryspec.SourceUser})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := store.SaveSpec(ctx, run, run.Version, spec, sources, objective); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	llm := newEchoLLM(t)
	SetSummaryDeps(db, nil, nil, llm.cfg())
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	if err := checkCoverageBeforeFreeze(ctx, uid, sessionID, run.RunID); err != nil {
		t.Fatalf("gate fired on an open-scope run: %v", err)
	}
}

// SUMMARY_REPAIR_MAX_ROUNDS=0 is the kill switch: the gate must not fire at all.
func TestCoverageGate_ZeroRoundsDisablesTheGate(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	f := newCoverageGateFixture(t, true, twoChannelSpec)
	if f == nil {
		return
	}
	llm := newEchoLLM(t)
	cfg := llm.cfg()
	cfg.SummaryRepairMaxRounds = 0
	SetSummaryDeps(f.db, nil, nil, cfg)
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	if err := checkCoverageBeforeFreeze(f.toolCtx(), f.uid, f.sessionID, f.runID); err != nil {
		t.Fatalf("gate fired with SummaryRepairMaxRounds=0: %v", err)
	}
}

// Once the manifest IS frozen the moment has passed: blocking could only cost
// turns to fetch messages the manifest can no longer make citable. This is also
// the idempotent-replay shape (a replay reuses the original run's manifest).
func TestCoverageGate_DoesNotBlockAfterManifestAlreadyFrozen(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	f := newCoverageGateFixture(t, true, twoChannelSpec)
	if f == nil {
		return
	}
	llm := newEchoLLM(t)
	SetSummaryDeps(f.db, nil, nil, llm.cfg())
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	if _, _, _, err := artifact.NewStore(f.db).FreezeFromPool(context.Background(), f.runID, f.uid, f.sessionID,
		[]pipeline.Message{{ChannelID: "ch-A", MessageSeq: 1, Timestamp: 1000, CitationIndex: 1}}, artifact.FreezeMeta{}); err != nil {
		t.Fatalf("pre-freeze: %v", err)
	}
	if err := checkCoverageBeforeFreeze(f.toolCtx(), f.uid, f.sessionID, f.runID); err != nil {
		t.Fatalf("gate blocked after the manifest was already frozen: %v", err)
	}
}
