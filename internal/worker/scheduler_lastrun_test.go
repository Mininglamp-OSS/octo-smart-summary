package worker

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newSchedulerTestDB builds an in-memory DB with the tables claimAndCreateScheduledTask touches.
func newSchedulerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummarySchedule{},
		&model.SummaryTask{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummarySource{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedDueSchedule creates an active schedule that is due now (next_run_at in the
// past) using a daily cadence (interval_days=1) and type=4 incremental window.
func seedDueSchedule(t *testing.T, db *gorm.DB, lastRunAt *time.Time) model.SummarySchedule {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	sched := model.SummarySchedule{
		SpaceID:       "sp",
		CreatorID:     "u1",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		TimeRangeType: 4,
		IsActive:      1,
		NextRunAt:     &past,
		LastRunAt:     lastRunAt,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	return sched
}

func seedBoundTask(t *testing.T, db *gorm.DB, scheduleID int64, status int, participants int) model.SummaryTask {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: status, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &scheduleID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	for i := 0; i < participants; i++ {
		p := model.SummaryParticipant{TaskID: task.ID, UserID: userIDForIdx(i)}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("seed participant: %v", err)
		}
	}
	return task
}

func userIDForIdx(i int) string {
	return []string{"u1", "u2", "u3"}[i%3]
}

func reloadSchedule(t *testing.T, db *gorm.DB, id int64) model.SummarySchedule {
	t.Helper()
	// Always read into a FRESH struct: GORM will not overwrite a non-nil pointer
	// field with a NULL column, so reusing a struct can mask a NULL last_run_at.
	var s model.SummarySchedule
	if err := db.First(&s, id).Error; err != nil {
		t.Fatalf("reload schedule %d: %v", id, err)
	}
	return s
}

// On a real requeue, last_run_at must advance to ~now and next_run_at must move forward.
func TestClaim_RequeueAdvancesLastRunAt(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || taskID == 0 {
		t.Fatalf("expected a real requeue, got claimed=%v taskID=%d", claimed, taskID)
	}
	got := reloadSchedule(t, db, sched.ID)
	if got.LastRunAt == nil || got.LastRunAt.Before(now.Add(-time.Second)) {
		t.Errorf("last_run_at should advance to ~now on requeue, got %v", got.LastRunAt)
	}
	if got.NextRunAt == nil || !got.NextRunAt.After(now) {
		t.Errorf("next_run_at should move forward, got %v", got.NextRunAt)
	}
}

// Overlap skip (task still Processing): next_run_at advances but last_run_at is preserved.
func TestClaim_OverlapSkipPreservesLastRunAt(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)
	seedBoundTask(t, db, sched.ID, model.StatusProcessing, 1)

	now := time.Now().UTC()
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true (next_run_at advanced)")
	}
	if taskID != 0 {
		t.Fatalf("overlap should not requeue (taskID=0), got %d", taskID)
	}
	got := reloadSchedule(t, db, sched.ID)
	if got.LastRunAt == nil || !got.LastRunAt.Equal(old) {
		t.Errorf("last_run_at must be preserved on overlap skip, got %v want %v", got.LastRunAt, old)
	}
	if got.NextRunAt == nil || !got.NextRunAt.After(now) {
		t.Errorf("next_run_at should still advance on overlap skip, got %v", got.NextRunAt)
	}
	if got.IsActive != 1 {
		t.Errorf("overlap is transient; schedule must stay active, got is_active=%d", got.IsActive)
	}
}

// No prior task under the schedule (1->N first run, or after a group delete):
// this is NORMAL, not a broken binding. claim must CREATE a brand-new task,
// advance last_run_at, and keep the schedule active.
func TestClaim_NoPriorTaskCreatesFirstTask(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)
	// no prior task seeded

	now := time.Now().UTC()
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || taskID == 0 {
		t.Fatalf("first run should create a task, got claimed=%v taskID=%d", claimed, taskID)
	}
	got := reloadSchedule(t, db, sched.ID)
	if got.IsActive != 1 {
		t.Errorf("first run must keep schedule active, got is_active=%d", got.IsActive)
	}
	if got.LastRunAt == nil || got.LastRunAt.Before(now.Add(-time.Second)) {
		t.Errorf("last_run_at should advance to ~now on first run, got %v", got.LastRunAt)
	}
	// The new task must have its creator participant + personal_result rebuilt from
	// schedule config (single-person: just the creator).
	var pCount, prCount int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&pCount)
	db.Model(&model.PersonalResult{}).Where("task_id = ?", taskID).Count(&prCount)
	if pCount != 1 || prCount != 1 {
		t.Errorf("new task subtables not rebuilt: participants=%d personal_results=%d, want 1/1", pCount, prCount)
	}
}

// A schedule whose participant_config resolves to multiple distinct users is
// structurally unsupported for scheduling (single-person this version): disable
// the schedule and preserve last_run_at.
func TestClaim_MultiPersonConfigDisablesSchedule(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)
	// creator u1 + another user u2 in participant_config -> multi-person.
	sched.ParticipantConfig = model.JSON(`[{"user_id":"u2"}]`)
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
		Update("participant_config", sched.ParticipantConfig).Error; err != nil {
		t.Fatalf("set participant_config: %v", err)
	}
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	_, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true")
	}
	got := reloadSchedule(t, db, sched.ID)
	if got.IsActive != 0 {
		t.Errorf("multi-person config should disable schedule, got is_active=%d", got.IsActive)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(old) {
		t.Errorf("last_run_at must be preserved (no run produced), got %v want %v", got.LastRunAt, old)
	}
}
