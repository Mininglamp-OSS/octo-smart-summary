//go:build cgo
// +build cgo

package summaryrun

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func newStoreTestDB(t *testing.T) *gorm.DB {
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

func specDraft(t *testing.T) (summaryspec.Spec, summaryspec.FieldSources) {
	t.Helper()
	obj := "总结本周风险"
	spec, src, err := summaryspec.Validate(summaryspec.Draft{Objective: &obj}, summaryspec.Options{})
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	return spec, src
}

func TestCreateOrGetRunIdempotent(t *testing.T) {
	db := newStoreTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	run1, created1, err := s.CreateOrGetRun(ctx, "u1", "sess1", "req1", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if !created1 {
		t.Fatal("first call should create")
	}

	// Same idempotency tuple → reuse, not a new run (the SSE-downgrade replay).
	run2, created2, err := s.CreateOrGetRun(ctx, "u1", "sess1", "req1", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if created2 {
		t.Fatal("replay must not create a second run")
	}
	if run2.RunID != run1.RunID {
		t.Fatalf("replay returned different run: %s != %s", run2.RunID, run1.RunID)
	}

	var count int64
	db.Model(&model.AgentSummaryRun{}).Count(&count)
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (no duplicate run)", count)
	}

	// A different request_id is a distinct run.
	if _, created3, _ := s.CreateOrGetRun(ctx, "u1", "sess1", "req2", model.ScopePolicyClosed); !created3 {
		t.Fatal("distinct request_id should create a new run")
	}
}

func TestSaveSpecAdvancesRunAndCAS(t *testing.T) {
	db := newStoreTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	run, _, err := s.CreateOrGetRun(ctx, "u1", "sess1", "req1", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	spec, src := specDraft(t)

	specRow, err := s.SaveSpec(ctx, run, run.Version, spec, src, "总结本周风险")
	if err != nil {
		t.Fatalf("save spec: %v", err)
	}
	if specRow.Version != 1 {
		t.Fatalf("spec version = %d, want 1", specRow.Version)
	}
	if run.SpecID != specRow.SpecID || run.SpecVersion != 1 || run.Version != 1 {
		t.Fatalf("run not advanced: %+v", run)
	}
	if run.Status != model.RunStatusRunning {
		t.Fatalf("status = %q, want running", run.Status)
	}
	if specRow.SpecHash == "" {
		t.Fatal("spec hash not persisted")
	}

	// A stale expectedVersion (someone else advanced the run) must be rejected.
	if _, err := s.SaveSpec(ctx, run, 0 /* stale */, spec, src, "x"); !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("stale SaveSpec err = %v, want ErrConcurrentUpdate", err)
	}
}

func TestSaveSpecOwnerScoped(t *testing.T) {
	db := newStoreTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()
	run, _, err := s.CreateOrGetRun(ctx, "owner", "sess1", "req1", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	forged := *run
	forged.UserID = "attacker"
	spec, src := specDraft(t)
	if _, err := s.SaveSpec(ctx, &forged, forged.Version, spec, src, "x"); !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("cross-user SaveSpec err = %v, want ErrConcurrentUpdate", err)
	}
	var count int64
	if err := db.Model(&model.AgentSummarySpec{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rolled-back spec rows=%d err=%v, want 0", count, err)
	}
}

func TestUpdateStatusCAS(t *testing.T) {
	db := newStoreTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	run, _, _ := s.CreateOrGetRun(ctx, "u1", "sess1", "req1", model.ScopePolicyClosed)

	if err := s.UpdateStatusCAS(ctx, "u1", run.RunID, run.Version, model.RunStatusFinished); err != nil {
		t.Fatalf("cas update: %v", err)
	}
	// Same (now stale) version must fail — proves no lost-update.
	if err := s.UpdateStatusCAS(ctx, "u1", run.RunID, run.Version, model.RunStatusFailed); !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("stale cas err = %v, want ErrConcurrentUpdate", err)
	}
	// Owner scope: a guessed run_id from another user must not update.
	if err := s.UpdateStatusCAS(ctx, "attacker", run.RunID, run.Version, model.RunStatusFailed); err == nil {
		t.Fatal("cross-user UpdateStatusCAS must not succeed")
	}
}

func TestGetLatestSpec(t *testing.T) {
	db := newStoreTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	run, _, _ := s.CreateOrGetRun(ctx, "u1", "sess1", "req1", model.ScopePolicyClosed)

	// No spec yet → found=false, no error.
	if _, found, err := s.GetLatestSpec(ctx, "u1", run.RunID); err != nil || found {
		t.Fatalf("pre-save: found=%v err=%v, want false/nil", found, err)
	}

	spec, src := specDraft(t) // objective 总结本周风险
	if _, err := s.SaveSpec(ctx, run, run.Version, spec, src, "总结本周风险"); err != nil {
		t.Fatalf("save spec: %v", err)
	}

	got, found, err := s.GetLatestSpec(ctx, "u1", run.RunID)
	if err != nil || !found {
		t.Fatalf("post-save: found=%v err=%v", found, err)
	}
	if got.Objective != spec.Objective {
		t.Fatalf("objective = %q, want %q", got.Objective, spec.Objective)
	}
	// Owner-scoped: another user cannot read this run's spec.
	if _, found, _ := s.GetLatestSpec(ctx, "attacker", run.RunID); found {
		t.Fatal("cross-user GetLatestSpec should not find")
	}
}

func TestGetByIDOwnerScoped(t *testing.T) {
	db := newStoreTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	run, _, _ := s.CreateOrGetRun(ctx, "owner", "sess1", "req1", model.ScopePolicyClosed)

	if _, err := s.GetByID(ctx, "owner", run.RunID); err != nil {
		t.Fatalf("owner should read own run: %v", err)
	}
	// Another user guessing the run_id must get not-found.
	if _, err := s.GetByID(ctx, "attacker", run.RunID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("cross-user read err = %v, want RecordNotFound", err)
	}
}

func TestFinishStatusAndLatestRunBySession(t *testing.T) {
	db := newStoreTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	run, _, _ := s.CreateOrGetRun(ctx, "u1", "sess1", "req1", model.ScopePolicyClosed)

	got, found, err := s.GetLatestRunBySession(ctx, "u1", "sess1")
	if err != nil || !found || got.RunID != run.RunID {
		t.Fatalf("GetLatestRunBySession: found=%v err=%v id=%s", found, err, got.RunID)
	}
	if _, found, _ := s.GetLatestRunBySession(ctx, "attacker", "sess1"); found {
		t.Fatal("cross-user GetLatestRunBySession should not find")
	}

	if err := s.SetFinishStatus(ctx, run.RunID, model.FinishStatusPartial); err != nil {
		t.Fatalf("SetFinishStatus: %v", err)
	}
	reloaded, err := s.GetByID(ctx, "u1", run.RunID)
	if err != nil || reloaded.FinishStatus != model.FinishStatusPartial {
		t.Fatalf("finish_status = %q (err %v), want PARTIAL", reloaded.FinishStatus, err)
	}
}
