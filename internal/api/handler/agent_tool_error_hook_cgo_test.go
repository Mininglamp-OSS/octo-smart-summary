//go:build cgo
// +build cgo

package handler

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// The failed marker used to be one-way: the model retried the tool, succeeded,
// and the run still reported FAILED because nothing ever cleared it. Under
// record-only semantics that is a user-visible FAILED on a good deliverable.
//
// The fix is not another transient string in the classifier — that list has been
// wrong in both directions on the same error — but an observation: a later
// success by the same tool proves the failure was recoverable.
func newHookTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil
	}
	if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummarySpec{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func hookTestRun(t *testing.T, db *gorm.DB, requestID string) *model.AgentSummaryRun {
	t.Helper()
	run, _, err := summaryrun.NewStore(db).CreateOrGetRun(context.Background(), "u1", "sess1", requestID, model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func runStatus(t *testing.T, db *gorm.DB, runID string) string {
	t.Helper()
	got, err := summaryrun.NewStore(db).GetByID(context.Background(), "u1", runID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	return got.Status
}

func TestToolErrorHookClearsFailedOnLaterSuccess(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	run := hookTestRun(t, db, "req-recover")
	h := &AgentChatHandler{runStore: summaryrun.NewStore(db)}
	runner := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runner, "u1", run.RunID)

	// A stale pooled connection kills a redundant second fetch_channel call.
	runner.OnToolError("fetch_channel", "", agent.ToolErrorEnvelope{Fatal: true, ErrorCode: "CRITICAL_TOOL_ERROR"})
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("status after fatal error = %q, want failed", got)
	}

	// The model retries it and it works.
	runner.OnToolSuccess("fetch_channel", "")
	if got := runStatus(t, db, run.RunID); got == model.RunStatusFailed {
		t.Fatal("a later success by the same tool must clear the failed marker")
	}
}

func TestToolErrorHookKeepsFailedWhileAnotherToolIsStillFatal(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	run := hookTestRun(t, db, "req-two-tools")
	h := &AgentChatHandler{runStore: summaryrun.NewStore(db)}
	runner := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runner, "u1", run.RunID)

	runner.OnToolError("fetch_channel", "", agent.ToolErrorEnvelope{Fatal: true})
	runner.OnToolError("summarize_chunk", "", agent.ToolErrorEnvelope{Fatal: true})

	// Recovering one tool is not recovering the run.
	runner.OnToolSuccess("fetch_channel", "")
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("status = %q while summarize_chunk is still fatal, want failed", got)
	}

	runner.OnToolSuccess("summarize_chunk", "")
	if got := runStatus(t, db, run.RunID); got == model.RunStatusFailed {
		t.Fatal("every fatal tool has since succeeded; the run must no longer be failed")
	}
}

func TestToolErrorHookIgnoresSuccessOfNeverFailedTool(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	run := hookTestRun(t, db, "req-unrelated")
	h := &AgentChatHandler{runStore: summaryrun.NewStore(db)}
	runner := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runner, "u1", run.RunID)

	runner.OnToolError("fetch_channel", "", agent.ToolErrorEnvelope{Fatal: true})
	// An unrelated tool succeeding says nothing about fetch_channel's failure.
	runner.OnToolSuccess("merge_summaries", "")
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("status = %q, want failed — a different tool's success is not recovery", got)
	}
}

func TestToolErrorHookIgnoresNonFatalErrors(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	run := hookTestRun(t, db, "req-nonfatal")
	h := &AgentChatHandler{runStore: summaryrun.NewStore(db)}
	runner := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runner, "u1", run.RunID)

	runner.OnToolError("fetch_channel", "", agent.ToolErrorEnvelope{Fatal: false, Retryable: true})
	if got := runStatus(t, db, run.RunID); got == model.RunStatusFailed {
		t.Fatal("a retryable error must not mark the run failed")
	}
}

// TestToolErrorHookKeyedByTarget pins the round-4 P1-3 (yujiawei): fetch_channel
// / summarize_chunk run once per channel / chunk through the worker pool, so the
// fatal set must be keyed on (tool, target). Before the fix a marker keyed on the
// tool name alone let chunk B's success clear chunk A's fatal marker in the same
// step, making the verdict depend on scheduling order.
func TestToolErrorHookKeyedByTarget(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	run := hookTestRun(t, db, "req-per-target")
	h := &AgentChatHandler{runStore: summaryrun.NewStore(db)}
	runner := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runner, "u1", run.RunID)

	// Chunk A fails fatally; chunk B (a DIFFERENT target of the same tool) succeeds.
	runner.OnToolError("summarize_chunk", "messages_handle=chunk-A", agent.ToolErrorEnvelope{Fatal: true})
	runner.OnToolSuccess("summarize_chunk", "messages_handle=chunk-B")
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("status = %q — a different chunk's success must NOT clear chunk A's fatal marker", got)
	}

	// A retry of chunk A itself (same target) is the genuine recovery signal.
	runner.OnToolSuccess("summarize_chunk", "messages_handle=chunk-A")
	if got := runStatus(t, db, run.RunID); got == model.RunStatusFailed {
		t.Fatal("the failing target itself succeeded on retry; the run must no longer be failed")
	}
}

// TestToolErrorHookIsConcurrencySafe guards the shared fatal set: the hooks are
// called from the 4-wide tool worker pool.
func TestToolErrorHookIsConcurrencySafe(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	run := hookTestRun(t, db, "req-concurrent")
	h := &AgentChatHandler{runStore: summaryrun.NewStore(db)}
	runner := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runner, "u1", run.RunID)

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			tool := []string{"fetch_channel", "summarize_chunk"}[i%2]
			runner.OnToolError(tool, "", agent.ToolErrorEnvelope{Fatal: true})
			runner.OnToolSuccess(tool, "")
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	// Whatever the interleaving, the run must not be left failed: every tool that
	// failed also succeeded afterwards.
	if got := runStatus(t, db, run.RunID); got == model.RunStatusFailed {
		t.Fatalf("status = %q after every tool recovered", got)
	}
}

// TestReplayClearsStaleFailedStatus pins the round-10 P1 (mochashanyao P1 /
// yujiawei P1-2): a stale status=failed must not latch across a same-request_id
// replay.
//
// The fatal marker lives in the per-runner hook closure, so attempt B's
// OnToolSuccess can only clear a marker attempt B itself set. Before the fix, an
// SSE downgrade that reused request_id resolved to the SAME run row — still
// carrying attempt A's failure — built a fresh runner with an empty fatal set,
// ran cleanly, and finalizeRun still disclosed FAILED for a good deliverable.
// The run row is the only state shared across attempts, so the reset belongs
// there, at the moment the replay is recognised.
func TestReplayClearsStaleFailedStatus(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()

	// Attempt A: fatal tool error latches status=failed on the run row.
	run := hookTestRun(t, db, "req-replay")
	h := &AgentChatHandler{runStore: store}
	runnerA := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runnerA, "u1", run.RunID)
	runnerA.OnToolError("fetch_channel", "channel_id=ch-1", agent.ToolErrorEnvelope{Fatal: true})
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("attempt A status = %q, want failed", got)
	}

	// Attempt B: the client retries with the SAME request_id. CreateOrGetRun
	// resolves the existing row (created=false) — this is the replay branch that
	// maybePersistSummaryRun takes.
	replay, created, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-replay", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("replay CreateOrGetRun: %v", err)
	}
	if created {
		t.Fatal("same request_id must resolve to the existing run, not a new one")
	}
	if replay.RunID != run.RunID {
		t.Fatalf("replay run_id = %q, want %q", replay.RunID, run.RunID)
	}

	if err := store.ClearFailedStatusForReplay(ctx, "u1", replay.RunID); err != nil {
		t.Fatalf("ClearFailedStatusForReplay: %v", err)
	}
	if got := runStatus(t, db, run.RunID); got == model.RunStatusFailed {
		t.Fatal("a replay must start from a clean slate; attempt A's failure must not latch")
	}

	// And the new attempt's own fatal errors still mark the run, as usual.
	runnerB := agent.NewRunner(nil, nil, nil, agent.Policy{})
	h.attachToolErrorHook(runnerB, "u1", run.RunID)
	runnerB.OnToolError("fetch_channel", "channel_id=ch-9", agent.ToolErrorEnvelope{Fatal: true})
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("status = %q — attempt B's OWN fatal error must still mark the run failed", got)
	}
}

// TestClearFailedStatusForReplayIsScoped pins the two limits of the reset: it
// only ever moves failed → running, and it is owner-scoped.
func TestClearFailedStatusForReplayIsScoped(t *testing.T) {
	db := newHookTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()

	// A finished run is not a failed run; the reset must leave it alone.
	finished := hookTestRun(t, db, "req-finished")
	if err := store.SetStatus(ctx, "u1", finished.RunID, model.RunStatusFinished); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := store.ClearFailedStatusForReplay(ctx, "u1", finished.RunID); err != nil {
		t.Fatalf("ClearFailedStatusForReplay: %v", err)
	}
	if got := runStatus(t, db, finished.RunID); got != model.RunStatusFinished {
		t.Fatalf("finished run status = %q, want untouched %q", got, model.RunStatusFinished)
	}

	// Another user's run_id must not be resettable.
	owned := hookTestRun(t, db, "req-owned")
	if err := store.SetStatus(ctx, "u1", owned.RunID, model.RunStatusFailed); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := store.ClearFailedStatusForReplay(ctx, "someone-else", owned.RunID); err != nil {
		t.Fatalf("ClearFailedStatusForReplay (foreign uid): %v", err)
	}
	if got := runStatus(t, db, owned.RunID); got != model.RunStatusFailed {
		t.Fatalf("status = %q — a foreign uid must not reset another user's run", got)
	}
}
