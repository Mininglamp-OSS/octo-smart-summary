//go:build cgo
// +build cgo

package handler

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// This file covers the finalizeRun GLUE — run resolution by request_id, state
// assembly from the run row, and verdict selection.
//
// Both reviewers flagged its absence across two rounds: the store, gate and
// classifier are each well covered, but the code that joins them was exercised
// only indirectly, and the coverage-policy defects (a false PARTIAL on every
// confident rewrite, a silent COMPLETE on an open-scope under-fetch) live
// exactly there.

func newFinalizeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil
	}
	if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummarySpec{}, &model.AgentEvidenceArtifact{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func hasGapKind(gaps []finishgate.Gap, kind string) bool {
	for _, g := range gaps {
		if g.Kind == kind {
			return true
		}
	}
	return false
}

func TestFinalizeRunRequiresRequestID(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	h := &AgentSummaryHandler{db: db}

	// No request_id: a no-op, NOT a guess. Selecting "the latest run in the
	// session" is what let a late status update on an older run steal the verdict.
	verdict, gaps := h.finalizeRun(context.Background(), "u1", "sess1", "", "内容", nil)
	if verdict != "" || gaps != nil {
		t.Fatalf("missing request_id must be a no-op, got verdict=%s gaps=%v", verdict, gaps)
	}
}

func TestFinalizeRunUnknownRunReportsUnverified(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	h := &AgentSummaryHandler{db: db}

	// A lookup failure must not degrade to "no warning" — that is indistinguishable
	// from a clean run. finalizeRun executes after the save commits, so a client
	// disconnecting the instant the save returns lands here.
	verdict, gaps := h.finalizeRun(context.Background(), "u1", "sess1", "req-unknown", "内容", nil)
	if verdict != finishgate.Partial {
		t.Fatalf("verdict = %s, want PARTIAL (could not verify)", verdict)
	}
	if !hasGapKind(gaps, finishgate.GapToolError) {
		t.Fatalf("an unverifiable run must disclose why, got %v", gaps)
	}
}

// TestFinalizeRunConfidentRewriteIsComplete is the P0-2 regression. A confident
// rewrite has its fetch tools physically removed by SS-08b, so coverage can never
// be measured; reporting that as a gap made PARTIAL the standing verdict for
// every correct rewrite, and SS-11 ships it to the client as a warning.
func TestFinalizeRunConfidentRewriteIsComplete(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRunWithFetchExpectation(ctx, "u1", "sess1", "req-rw", model.ScopePolicyClosed, false)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}

	h := &AgentSummaryHandler{db: db}
	verdict, gaps := h.finalizeRun(ctx, "u1", "sess1", "req-rw", "重写后的总结", nil)
	if verdict != finishgate.Complete {
		t.Fatalf("verdict = %s, want COMPLETE for a turn that was never allowed to fetch (gaps=%v)", verdict, gaps)
	}
}

// TestFinalizeRunOpenScopeUnderFetchIsDisclosed is the P1-1 regression. With no
// UI selection the spec pins no channels, so the expected-vs-fetched comparison
// had nothing to compare and a run that discovered 3 channels and fetched 1
// reported COMPLETE with no gaps.
func TestFinalizeRunOpenScopeUnderFetchIsDisclosed(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-open", model.ScopePolicyOpen)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}
	if err := store.RecordDiscoveredChannels(ctx, "u1", run.RunID, []string{"ch-1", "ch-2", "ch-3"}); err != nil {
		t.Fatalf("record discovered: %v", err)
	}
	if err := store.RecordChannelFetch(ctx, "u1", run.RunID, "ch-1", true, false); err != nil {
		t.Fatalf("record fetch: %v", err)
	}

	h := &AgentSummaryHandler{db: db}
	verdict, gaps := h.finalizeRun(ctx, "u1", "sess1", "req-open", "总结正文", []model.Citation{{Index: 1}})
	if verdict != finishgate.Partial {
		t.Fatalf("verdict = %s, want PARTIAL when 2 of 3 discovered channels were never fetched", verdict)
	}
	named := map[string]bool{}
	for _, g := range gaps {
		if g.Kind == finishgate.GapChannel {
			named[g.ChannelID] = true
		}
	}
	if !named["ch-2"] || !named["ch-3"] {
		t.Fatalf("the disclosure must name the missed channels, got %v", gaps)
	}
}

// TestFinalizeRunPersistsVerdict pins that the verdict actually lands on the row —
// the response field and the persisted column must not diverge.
func TestFinalizeRunPersistsVerdict(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRunWithFetchExpectation(ctx, "u1", "sess1", "req-p", model.ScopePolicyClosed, false)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}

	h := &AgentSummaryHandler{db: db}
	verdict, _ := h.finalizeRun(ctx, "u1", "sess1", "req-p", "内容", nil)

	got, err := store.GetByID(ctx, "u1", run.RunID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.FinishStatus != string(verdict) {
		t.Fatalf("persisted finish_status = %q, returned verdict = %q", got.FinishStatus, verdict)
	}
}

// TestFinalizeRunIsOwnerScoped pins that another user's request_id cannot reach
// this run — the run is resolved by (user_id, session_id, request_id).
func TestFinalizeRunIsOwnerScoped(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	if _, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-owner", model.ScopePolicyClosed); err != nil {
		t.Fatalf("create run: %v", err)
	}

	h := &AgentSummaryHandler{db: db}
	verdict, gaps := h.finalizeRun(ctx, "u2", "sess1", "req-owner", "内容", nil)
	if verdict != finishgate.Partial || !hasGapKind(gaps, finishgate.GapToolError) {
		t.Fatalf("another user's run must not resolve; got verdict=%s gaps=%v", verdict, gaps)
	}
}
