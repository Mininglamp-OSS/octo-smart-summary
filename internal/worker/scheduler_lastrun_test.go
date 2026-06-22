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

// seedDueScheduleWithPolicy is seedDueSchedule with an explicit confirm_policy and
// participant_config, so the claim/requeue path can be exercised under AUTO vs
// CONFIRM. participantUserIDs are the NON-creator configured users (the creator u1
// is always implicitly materialized by buildScheduledTaskParticipants).
func seedDueScheduleWithPolicy(t *testing.T, db *gorm.DB, lastRunAt *time.Time, confirmPolicy int, participantUserIDs ...string) model.SummarySchedule {
	t.Helper()
	sched := seedDueSchedule(t, db, lastRunAt)
	updates := map[string]interface{}{"confirm_policy": confirmPolicy}
	if len(participantUserIDs) > 0 {
		cfg := "["
		for i, uid := range participantUserIDs {
			if i > 0 {
				cfg += ","
			}
			cfg += `{"user_id":"` + uid + `"}`
		}
		cfg += "]"
		updates["participant_config"] = model.JSON(cfg)
	}
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
		Updates(updates).Error; err != nil {
		t.Fatalf("set confirm_policy/participant_config: %v", err)
	}
	return reloadSchedule(t, db, sched.ID)
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
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, false)
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
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, false)
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
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, false)
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
	_, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, false)
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

// 🔴 REGRESSION (finding 2): the V5 OBJECT-form participant_config
// {"participants":[...],"confirm_gate_passed":...} must still be detected as
// multi-person at claim time. The old scheduleConfigMultiPerson Unmarshal'd into a
// bare array, FAILED on the object form, and returned false -> the
// FEATURE_TEAM_SCHEDULE-off multi-person guard was silently bypassed (a CONFIRM
// multi-person schedule was wrongly treated as single-person and kept running).
func TestClaim_MultiPersonV5ObjectConfig_FlagOff_StillDisables(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)
	// V5 object form: creator u1 + u2 -> multi-person.
	sched.ParticipantConfig = model.JSON(`{"participants":[{"user_id":"u1","confirmed":true},{"user_id":"u2","confirmed":true}],"confirm_gate_passed":true}`)
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
		Update("participant_config", sched.ParticipantConfig).Error; err != nil {
		t.Fatalf("set participant_config: %v", err)
	}
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	_, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, false /* flag off */)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true")
	}
	got := reloadSchedule(t, db, sched.ID)
	if got.IsActive != 0 {
		t.Errorf("V5 object-form multi-person config (flag off) must disable schedule, got is_active=%d", got.IsActive)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(old) {
		t.Errorf("last_run_at must be preserved (no run produced), got %v want %v", got.LastRunAt, old)
	}
}

// Counterpart: a V5 object-form config that is creator-only must NOT be treated
// as multi-person (single-person scheduled runs continue under flag off).
func TestClaim_CreatorOnlyV5ObjectConfig_FlagOff_NotDisabled(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)
	// V5 object form: only the creator u1.
	sched.ParticipantConfig = model.JSON(`{"participants":[{"user_id":"u1","confirmed":true}],"confirm_gate_passed":true}`)
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
		Update("participant_config", sched.ParticipantConfig).Error; err != nil {
		t.Fatalf("set participant_config: %v", err)
	}
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	_, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, false /* flag off */)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true")
	}
	got := reloadSchedule(t, db, sched.ID)
	if got.IsActive != 1 {
		t.Errorf("creator-only V5 object config must NOT disable schedule, got is_active=%d", got.IsActive)
	}
}

// With FEATURE_TEAM_SCHEDULE on, a multi-person schedule must NOT be disabled at
// claim time; it must produce a brand-new task whose subtables include every
// configured participant (all Accepted under AUTO policy=0) plus the creator.
func TestClaim_MultiPersonConfig_FlagOn_RunsMultiPerson(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)

	// creator u1 + u2 + u3 in participant_config -> 3 distinct users.
	sched.ParticipantConfig = model.JSON(`[{"user_id":"u2"},{"user_id":"u3"}]`)
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
		Update("participant_config", sched.ParticipantConfig).Error; err != nil {
		t.Fatalf("set participant_config: %v", err)
	}

	now := time.Now().UTC()
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, true /* featureTeamSchedule */)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || taskID == 0 {
		t.Fatalf("flag-on multi-person should create a task, got claimed=%v taskID=%d", claimed, taskID)
	}

	got := reloadSchedule(t, db, sched.ID)
	if got.IsActive != 1 {
		t.Errorf("flag-on multi-person must keep schedule active, got is_active=%d", got.IsActive)
	}

	// Subtables: 3 distinct participants (u1 creator + u2 + u3), all Accepted, each
	// with a Pending personal_result.
	var pCount, prCount, acceptedCount int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&pCount)
	db.Model(&model.PersonalResult{}).Where("task_id = ?", taskID).Count(&prCount)
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND status = ?", taskID, model.ParticipantAccepted).Count(&acceptedCount)
	if pCount != 3 || prCount != 3 {
		t.Errorf("multi-person subtables: participants=%d personal_results=%d, want 3/3", pCount, prCount)
	}
	if acceptedCount != 3 {
		t.Errorf("AUTO policy: all participants must be Accepted, got accepted=%d/3", acceptedCount)
	}

	// The created task must be a scheduled task.
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.TriggerType != model.TriggerScheduled {
		t.Errorf("new task trigger_type=%d, want TriggerScheduled", task.TriggerType)
	}
}

// 🔴 REAL-BUG REGRESSION (scope=task AUTO requeue path): a scheduled multi-person
// AUTO schedule that already has a prior bound task (the requeue path, NOT the
// first-run path) must, after claim, build EVERY participant as Accepted with a
// Pending personal_result, and the AUTO dispatch selector must then pick up the
// WHOLE roster. The e2e bug was that the claim/requeue path left participants in a
// non-Accepted state so scheduledAutoDispatchTargets selected nobody -> the run
// produced no personal_result. This test exercises the genuine
// claimAndCreateScheduledTask -> buildScheduledTaskChildren ->
// buildScheduledTaskParticipants -> scheduledAutoDispatchTargets chain end-to-end
// (no hand-seeded Accepted participants).
func TestClaim_AutoRequeue_AllParticipantsAccepted_DispatchSelectsAll(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	// AUTO (confirm_policy=0) + creator u1 + u2 + u3 configured.
	sched := seedDueScheduleWithPolicy(t, db, &old, model.SchedConfirmAuto, "u2", "u3")
	// Prior terminal bound task under the schedule => this is the REQUEUE path
	// (latest exists, terminal so no overlap skip), distinct from first-run.
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, true /* featureTeamSchedule */)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || taskID == 0 {
		t.Fatalf("AUTO requeue should create a task, got claimed=%v taskID=%d", claimed, taskID)
	}

	// New task must be a scheduled task (trigger_type=2).
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.TriggerType != model.TriggerScheduled {
		t.Errorf("requeued task trigger_type=%d, want TriggerScheduled(2)", task.TriggerType)
	}

	// All 3 participants Accepted, all 3 personal_result Pending.
	var total, accepted, prPending int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&total)
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND status = ?", taskID, model.ParticipantAccepted).Count(&accepted)
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND worker_status = ?", taskID, model.PersonalStatusPending).Count(&prPending)
	if total != 3 {
		t.Fatalf("want 3 participants, got %d", total)
	}
	if accepted != 3 {
		t.Errorf("AUTO requeue: all participants must be Accepted, got accepted=%d/3", accepted)
	}
	if prPending != 3 {
		t.Errorf("AUTO requeue: all personal_result must be Pending, got pending=%d/3", prPending)
	}

	// The whole point: AUTO dispatch must now select EVERY participant.
	targets, err := scheduledAutoDispatchTargets(db, taskID)
	if err != nil {
		t.Fatalf("dispatch targets: %v", err)
	}
	if len(targets) != 3 {
		t.Errorf("AUTO requeue dispatch must select all 3 participants, got %d: %v", len(targets), targets)
	}
}

// seedDueScheduleV5Confirm seeds a CONFIRM (confirm_policy=1) schedule with a V5
// object-form participant_config where `confirmedUserIDs` are pre-confirmed and
// every other configured user (plus the creator unless listed) is confirmed=false.
// `allUserIDs` is the configured participant roster (excluding the implicit creator).
func seedDueScheduleV5Confirm(t *testing.T, db *gorm.DB, lastRunAt *time.Time, allUserIDs []string, confirmedUserIDs ...string) model.SummarySchedule {
	t.Helper()
	sched := seedDueSchedule(t, db, lastRunAt) // creator is u1
	confirmedSet := map[string]struct{}{}
	for _, u := range confirmedUserIDs {
		confirmedSet[u] = struct{}{}
	}
	cfg := model.ScheduleParticipantConfig{Participants: []model.ScheduleParticipantEntry{}}
	add := func(uid string) {
		_, c := confirmedSet[uid]
		cfg.Participants = append(cfg.Participants, model.ScheduleParticipantEntry{UserID: uid, Confirmed: c})
	}
	add("u1") // creator always in roster (Q2)
	for _, u := range allUserIDs {
		if u == "u1" {
			continue
		}
		add(u)
	}
	cfg.RecomputeGate("u1")
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("marshal v5 cfg: %v", err)
	}
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
		Updates(map[string]interface{}{"confirm_policy": model.SchedConfirmRequire, "participant_config": raw}).Error; err != nil {
		t.Fatalf("set v5 cfg: %v", err)
	}
	return reloadSchedule(t, db, sched.ID)
}

// V5 §4.3/方案乙: a CONFIRM round materializes ONLY confirmed members (creator
// included per Q2 — the creator is NOT auto-accepted). Here u1(creator)+u2 are
// confirmed and u3 is not, so the round must build exactly 2 Accepted participants
// (u1,u2) and skip u3 entirely (no participant, no personal_result).
func TestClaim_ConfirmV5_PartialConfirm_OnlyConfirmedMaterialized(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueScheduleV5Confirm(t, db, &old, []string{"u2", "u3"}, "u1", "u2")
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, true)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || taskID == 0 {
		t.Fatalf("partial-confirm round should create a task, got claimed=%v taskID=%d", claimed, taskID)
	}

	var total, accepted int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&total)
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND status = ?", taskID, model.ParticipantAccepted).Count(&accepted)
	if total != 2 || accepted != 2 {
		t.Fatalf("V5 partial confirm: want 2 Accepted participants (u1,u2), got total=%d accepted=%d", total, accepted)
	}
	// u3 (unconfirmed) must NOT exist this round (方案乙).
	var u3 int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "u3").Count(&u3)
	if u3 != 0 {
		t.Errorf("unconfirmed u3 must not be materialized this round, got %d rows", u3)
	}
	// personal_result count must equal accepted (2), one per confirmed member.
	var prs int64
	db.Model(&model.PersonalResult{}).Where("task_id = ?", taskID).Count(&prs)
	if prs != 2 {
		t.Errorf("want 2 personal_result rows (one per confirmed), got %d", prs)
	}
	// AUTO dispatch is gated to AUTO; CONFIRM uses the same Accepted selector via the
	// processor path. The two Accepted members are the dispatch roster.
	targets, err := scheduledAutoDispatchTargets(db, taskID)
	if err != nil {
		t.Fatalf("dispatch targets: %v", err)
	}
	if len(targets) != 2 {
		t.Errorf("V5 partial confirm: dispatch roster must be the 2 confirmed members, got %d: %v", len(targets), targets)
	}
}

// next_run_at must advance and last_run_at must be preserved on a zero-confirmed
// soft-deleted round (business skip, not a real run).
func TestClaim_ConfirmV5_ZeroConfirmed_NextRunAdvancesLastRunPreserved(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueScheduleV5Confirm(t, db, &old, []string{"u2", "u3"} /* none confirmed */)
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	_, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, true)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true (next_run_at advanced)")
	}
	got := reloadSchedule(t, db, sched.ID)
	if got.LastRunAt == nil || !got.LastRunAt.Equal(old) {
		t.Errorf("last_run_at must be preserved on zero-confirmed skip, got %v want %v", got.LastRunAt, old)
	}
	if got.NextRunAt == nil || !got.NextRunAt.After(now) {
		t.Errorf("next_run_at should advance on zero-confirmed skip, got %v", got.NextRunAt)
	}
}

// V5 §4.3/Q2 (soft-delete behavior): when NOBODY (creator included) has confirmed,
// the whole round is skipped. The freshly-created placeholder task is SOFT-DELETED
// (deleted_at set), NOT left as a Cancelled shell -- so it never surfaces in any
// deleted_at IS NULL query (list/detail). No participants/personal_results are
// built and no result is produced. (creator is no longer auto-accepted.)
func TestClaim_ConfirmV5_ZeroConfirmed_RoundSkipped(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	// u1(creator)+u2+u3 all unconfirmed.
	sched := seedDueScheduleV5Confirm(t, db, &old, []string{"u2", "u3"} /* none confirmed */)
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	now := time.Now().UTC()
	taskID, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, true)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The round is claimed (next_run_at advances) but produces no runnable task.
	if !claimed {
		t.Fatalf("zero-confirmed round should still claim (advance next_run), got claimed=%v", claimed)
	}
	if taskID != 0 {
		t.Fatalf("zero-confirmed round must report taskID=0 (skipped), got %d", taskID)
	}
	// The placeholder task must NOT appear in a deleted_at IS NULL query (list/detail
	// filter): it has been soft-deleted, so the schedule has zero LIVE tasks beyond
	// the prior terminal one. Specifically the round's new task must be unfindable
	// when filtering deleted_at IS NULL.
	var liveLatest model.SummaryTask
	err = db.Where("schedule_id = ? AND deleted_at IS NULL AND trigger_type = ?",
		sched.ID, model.TriggerScheduled).Order("id DESC").First(&liveLatest).Error
	if err != gorm.ErrRecordNotFound {
		t.Fatalf("zero-confirmed placeholder must be soft-deleted (unfindable under deleted_at IS NULL), got err=%v task=%+v", err, liveLatest)
	}
	// And the row physically still exists but with deleted_at set (Unscoped read).
	var task model.SummaryTask
	if err := db.Where("schedule_id = ?", sched.ID).Order("id DESC").First(&task).Error; err != nil {
		t.Fatalf("load latest task (incl soft-deleted): %v", err)
	}
	if task.DeletedAt == nil {
		t.Errorf("zero-confirmed placeholder must have deleted_at set (soft-deleted), got nil")
	}
	// No children built for the skipped round.
	var parts, prs int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&parts)
	db.Model(&model.PersonalResult{}).Where("task_id = ?", task.ID).Count(&prs)
	if parts != 0 || prs != 0 {
		t.Errorf("zero-confirmed round must build no children, got participants=%d personal_results=%d", parts, prs)
	}
	// summary_source rows are built BEFORE the accepted-count gate, so the
	// zero-confirm branch must physically clean them up; otherwise they leak as
	// orphans (parent task only soft-deleted) every skipped round.
	var srcs int64
	db.Model(&model.SummarySource{}).Where("task_id = ?", task.ID).Count(&srcs)
	if srcs != 0 {
		t.Errorf("zero-confirmed round must leave no orphan summary_source rows, got %d", srcs)
	}
}

// V5 §4.1: a fully-confirmed schedule (gate passed) runs every round WITHOUT any
// re-confirmation — the confirm state is read straight off the schedule, so two
// consecutive claims both materialize the full Accepted roster (一次性确认后多轮免确认).
func TestClaim_ConfirmV5_AllConfirmed_MultiRoundNoReconfirm(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	sched := seedDueScheduleV5Confirm(t, db, &old, []string{"u2"}, "u1", "u2") // all confirmed
	seedBoundTask(t, db, sched.ID, model.StatusCompleted, 1)

	runRound := func(label string) int64 {
		now := time.Now().UTC()
		taskID, claimed, err := claimAndCreateScheduledTask(db, nil, reloadSchedule(t, db, sched.ID), now, 30, true)
		if err != nil {
			t.Fatalf("%s claim: %v", label, err)
		}
		if !claimed || taskID == 0 {
			t.Fatalf("%s: all-confirmed round should create a task, got claimed=%v taskID=%d", label, claimed, taskID)
		}
		var accepted int64
		db.Model(&model.SummaryParticipant{}).
			Where("task_id = ? AND status = ?", taskID, model.ParticipantAccepted).Count(&accepted)
		if accepted != 2 {
			t.Errorf("%s: want 2 Accepted (u1,u2) without re-confirm, got %d", label, accepted)
		}
		// Mark the task terminal so the next round is not skipped by the overlap guard.
		db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("status", model.StatusCompleted)
		// Force the schedule due again for the next round.
		past := time.Now().UTC().Add(-48 * time.Hour)
		db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).Update("next_run_at", past)
		return taskID
	}

	t1 := runRound("round1")
	t2 := runRound("round2")
	if t1 == t2 {
		t.Fatalf("each round must create a distinct task, got %d twice", t1)
	}
}

// With FEATURE_TEAM_SCHEDULE off the legacy guard still disables a multi-person
// schedule (kept as a regression guard alongside TestClaim_MultiPersonConfigDisablesSchedule).
func TestClaim_MultiPersonConfig_FlagOff_StillDisables(t *testing.T) {
	db := newSchedulerTestDB(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	sched := seedDueSchedule(t, db, &old)
	sched.ParticipantConfig = model.JSON(`[{"user_id":"u2"}]`)
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
		Update("participant_config", sched.ParticipantConfig).Error; err != nil {
		t.Fatalf("set participant_config: %v", err)
	}

	now := time.Now().UTC()
	_, claimed, err := claimAndCreateScheduledTask(db, nil, sched, now, 30, false /* flag off */)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true")
	}
	if got := reloadSchedule(t, db, sched.ID); got.IsActive != 0 {
		t.Errorf("flag-off multi-person must disable schedule, got is_active=%d", got.IsActive)
	}
}
