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

// finalizeRunFor is a readability shim: build the handler and call finalizeRun.
type finalizeHelper struct{}

var h finalizeHelper

func (finalizeHelper) finalizeRunFor(t *testing.T, db *gorm.DB, ctx context.Context, uid, sess, req, content string, cits []model.Citation) (finishgate.Verdict, []finishgate.Gap) {
	t.Helper()
	return (&AgentSummaryHandler{db: db}).finalizeRun(ctx, uid, sess, req, content, cits)
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

// TestFinalizeRunMissingRunIsANoOp pins the round-5 fix: "no such run" and
// "could not read the run" are opposite situations and were reported identically.
//
// GetByRequest uses First(), so a request with no run row returns
// ErrRecordNotFound — the NORMAL path whenever V2 is off or run creation was
// skipped/failed. Reporting an unverified PARTIAL there put an "incomplete"
// warning on perfectly ordinary saves.
func TestFinalizeRunMissingRunIsANoOp(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	h := &AgentSummaryHandler{db: db}

	verdict, gaps := h.finalizeRun(context.Background(), "u1", "sess1", "req-unknown", "内容", nil)
	if verdict != "" || gaps != nil {
		t.Fatalf("a request with no run row has nothing to judge, got verdict=%s gaps=%v", verdict, gaps)
	}
}

// TestFinalizeRunLookupFailureReportsUnverified is the counterpart, and the
// reason the two must stay distinguishable: when the read genuinely FAILS the
// run may well exist and be incomplete, and we cannot tell. Degrading to "no
// warning" there is indistinguishable from a clean run.
//
// The failure is produced by dropping the table rather than by cancelling the
// context, so the error is a real query failure and not a ctx error that a
// future refactor might special-case.
func TestFinalizeRunLookupFailureReportsUnverified(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	if err := db.Migrator().DropTable(&model.AgentSummaryRun{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	h := &AgentSummaryHandler{db: db}

	verdict, gaps := h.finalizeRun(context.Background(), "u1", "sess1", "req-broken", "内容", nil)
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
// this run — the run is resolved by (user_id, session_id, request_id). The
// owner-scoped query simply finds nothing, which is a no-op rather than an
// unverified verdict.
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
	if verdict != "" || gaps != nil {
		t.Fatalf("another user's run must not resolve; got verdict=%s gaps=%v", verdict, gaps)
	}

	// And the owner still gets a verdict for the same request_id, so the no-op
	// above is scoping rather than the lookup being broken outright.
	if v, _ := h.finalizeRun(ctx, "u1", "sess1", "req-owner", "内容", nil); v == "" {
		t.Fatal("the owner must still resolve the run")
	}
}

// TestFinalizeRunCitationExemptionKeysOnTheFact pins round-7 P1-3 AT THE CALL
// SITE.
//
// A unit test on citationsValid passes the flag as a literal, so it cannot catch
// the wiring being reverted to run.FetchExpected — mutation-verifying that revert
// left every citationsValid test green. The defect is precisely which run column
// is handed in, so it has to be pinned here, where the row is real.
//
// The soft-rewrite state is reachable end to end inside this PR: fetch_expected=0
// is persisted while the fetch tools are KEPT, the model uses them
// (coverage_measured=1), the save-time citation build then fails and yields nil,
// and the content keeps its [1][2][3]. Keyed on the expectation that is COMPLETE;
// keyed on the fact it is a FAILED with a citation gap.
func TestFinalizeRunCitationExemptionKeysOnTheFact(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()

	// Soft rewrite: no fetch expected, but the tools were kept.
	run, _, err := store.CreateOrGetRunWithFetchExpectation(ctx, "u1", "sess1", "req-soft", model.ScopePolicyClosed, false)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// ... and the model went and fetched anyway. That is the FACT.
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Updates(map[string]any{
			"spec_id":            "spec-1",
			"coverage_measured":  true,
			"attempted_channels": `["c1"]`,
			"succeeded_channels": `["c1"]`,
		}).Error; err != nil {
		t.Fatalf("record the fetch: %v", err)
	}

	// The citation build failed → nil citations, markers still in the content.
	verdict, gaps := h.finalizeRunFor(t, db, ctx, "u1", "sess1", "req-soft", "结论一 [1]，结论二 [2][3]", nil)
	if verdict == finishgate.Complete {
		t.Fatalf("verdict = COMPLETE for a summary with dangling citation markers — the expectation overruled the fact (gaps=%v)", gaps)
	}
	if !hasGapKind(gaps, finishgate.GapCitation) {
		t.Fatalf("gaps = %v, want a citation gap naming the real defect", gaps)
	}

	// The genuine refine-borrowed turn — never fetched at all — stays COMPLETE.
	run2, _, err := store.CreateOrGetRunWithFetchExpectation(ctx, "u1", "sess1", "req-borrow", model.ScopePolicyClosed, false)
	if err != nil {
		t.Fatalf("create borrow run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run2.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}
	verdict2, gaps2 := h.finalizeRunFor(t, db, ctx, "u1", "sess1", "req-borrow", "重写后的总结 [1][2]", nil)
	if verdict2 != finishgate.Complete {
		t.Fatalf("verdict = %s, want COMPLETE — a turn that never fetched borrows its markers (gaps=%v)", verdict2, gaps2)
	}
}
