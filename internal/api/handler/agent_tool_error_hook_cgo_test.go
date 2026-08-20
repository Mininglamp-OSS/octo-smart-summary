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
	runner.OnToolError("fetch_channel", agent.ToolErrorEnvelope{Fatal: true, ErrorCode: "CRITICAL_TOOL_ERROR"})
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("status after fatal error = %q, want failed", got)
	}

	// The model retries it and it works.
	runner.OnToolSuccess("fetch_channel")
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

	runner.OnToolError("fetch_channel", agent.ToolErrorEnvelope{Fatal: true})
	runner.OnToolError("summarize_chunk", agent.ToolErrorEnvelope{Fatal: true})

	// Recovering one tool is not recovering the run.
	runner.OnToolSuccess("fetch_channel")
	if got := runStatus(t, db, run.RunID); got != model.RunStatusFailed {
		t.Fatalf("status = %q while summarize_chunk is still fatal, want failed", got)
	}

	runner.OnToolSuccess("summarize_chunk")
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

	runner.OnToolError("fetch_channel", agent.ToolErrorEnvelope{Fatal: true})
	// An unrelated tool succeeding says nothing about fetch_channel's failure.
	runner.OnToolSuccess("merge_summaries")
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

	runner.OnToolError("fetch_channel", agent.ToolErrorEnvelope{Fatal: false, Retryable: true})
	if got := runStatus(t, db, run.RunID); got == model.RunStatusFailed {
		t.Fatal("a retryable error must not mark the run failed")
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
			runner.OnToolError(tool, agent.ToolErrorEnvelope{Fatal: true})
			runner.OnToolSuccess(tool)
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
