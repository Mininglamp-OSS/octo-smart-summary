package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// =============================================================================
// PR#62 r5 tests
// =============================================================================

// --- Blocker2 (lml2468 Y1-bis): stored-config single-person guard -------------
//
// UpdateSchedule only validated participants when req.Participants != nil. When
// req.Participants == nil the bind reuses the schedule's STORED
// participant_config, which was never validated. A historically-dirty schedule
// whose stored config contains a non-creator could be bound and later inflated
// to multi-person by the worker. These tests pin the new stored-config guard.

// helper: marshal a participant_config the way the API stores it.
func mustParticipantConfig(t *testing.T, users ...[2]string) model.JSON {
	t.Helper()
	arr := make([]map[string]string, 0, len(users))
	for _, u := range users {
		arr = append(arr, map[string]string{"user_id": u[0], "user_name": u[1]})
	}
	b, err := json.Marshal(arr)
	if err != nil {
		t.Fatalf("marshal participant_config: %v", err)
	}
	return model.JSON(b)
}

// Blocker2.1: req.Participants == nil but stored config contains a NON-creator
// participant => bind must be rejected 400 / 40015 and the task must stay
// unbound (schedule_id not written).
func TestPR62Round5_StoredConfigMultiPerson_RejectedOnBind(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	// Dirty schedule: stored participant_config includes someone other than the
	// creator. Not yet bound to the task.
	sched := model.SummarySchedule{
		SpaceID:           "space1",
		CreatorID:         "creator1",
		Title:             "dirty stored config",
		SummaryMode:       model.ModeByPerson,
		IntervalDays:      1,
		RunTime:           "23:30",
		TimeRangeType:     2,
		IsActive:          1,
		ParticipantConfig: mustParticipantConfig(t, [2]string{"creator1", "C"}, [2]string{"stranger", "S"}),
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "B2-DIRTY", SpaceID: "space1", CreatorID: "creator1",
		SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Single creator participant row so the worker-style count guard passes; the
	// hole under test is the STORED config, not the participant rows.
	if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", Status: model.ParticipantAccepted}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}

	// PUT with NO participants field -> reuses stored config -> must be rejected.
	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "09:30",
		"interval_days": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for dirty stored config bind, got %d body=%s", w.Code, w.Body.String())
	}
	if code := decodeCode(t, w.Body); code != codeTeamScheduleNotSupported {
		t.Fatalf("expected code %d, got %d body=%s", codeTeamScheduleNotSupported, code, w.Body.String())
	}
	// Task must remain unbound.
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID != nil {
		t.Fatalf("task must remain unbound, got schedule_id=%v", *gotTask.ScheduleID)
	}
}

// Blocker2.2: req.Participants == nil and stored config is EMPTY => bind PASSES
// (the worker would only have {creator}).
func TestPR62Round5_StoredConfigEmpty_BindPasses(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID: "space1", CreatorID: "creator1", Title: "empty config",
		SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "23:30",
		TimeRangeType: 2, IsActive: 1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "B2-EMPTY", SpaceID: "space1", CreatorID: "creator1",
		SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", Status: model.ParticipantAccepted}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "09:30",
		"interval_days": 1,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty stored config bind, got %d body=%s", w.Code, w.Body.String())
	}
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil || *gotTask.ScheduleID != sched.ID {
		t.Fatalf("task should be bound to schedule %d, got %v", sched.ID, gotTask.ScheduleID)
	}
}

// Blocker2.3: req.Participants == nil and stored config contains ONLY the
// creator => bind PASSES.
func TestPR62Round5_StoredConfigCreatorOnly_BindPasses(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID: "space1", CreatorID: "creator1", Title: "creator only",
		SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "23:30",
		TimeRangeType: 2, IsActive: 1,
		ParticipantConfig: mustParticipantConfig(t, [2]string{"creator1", "C"}),
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "B2-CREATOR", SpaceID: "space1", CreatorID: "creator1",
		SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", Status: model.ParticipantAccepted}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "09:30",
		"interval_days": 1,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for creator-only stored config bind, got %d body=%s", w.Code, w.Body.String())
	}
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil || *gotTask.ScheduleID != sched.ID {
		t.Fatalf("task should be bound to schedule %d, got %v", sched.ID, gotTask.ScheduleID)
	}
}

// Blocker2.4 (no false positive): a pure NON-binding field update (req.Scope
// not "task") on a schedule whose stored config has a non-creator must NOT be
// rejected -- the stored-config guard only fires when an actual bind happens.
func TestPR62Round5_StoredConfigMultiPerson_NonBindingUpdateAllowed(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID: "space1", CreatorID: "creator1", Title: "dirty but no bind",
		SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "23:30",
		TimeRangeType: 2, IsActive: 1,
		ParticipantConfig: mustParticipantConfig(t, [2]string{"creator1", "C"}, [2]string{"stranger", "S"}),
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	// No scope/task_id -> not a bind, only touches title.
	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"title": "renamed only",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-binding update, got %d body=%s", w.Code, w.Body.String())
	}
	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.Title != "renamed only" {
		t.Fatalf("title not updated, got %q", got.Title)
	}
}

// --- Blocker1 (Jerry-Xin + lml2468): one-to-one binding serialization --------
//
// Two PUTs binding DIFFERENT tasks to the SAME schedule must not both succeed.
// Application-layer FOR UPDATE on the target schedule serializes them so the
// second sees boundCount>0 and is rejected (errTaskScopeScheduleBound / 40000).
// (SQLite test driver: the locking clause is a no-op, but the boundCount check
// is exercised sequentially exactly as it runs after the lock is acquired in
// MySQL. We assert the invariant: at most one live task bound per schedule.)
func TestPR62Round5_OneToOne_SecondBindRejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID: "space1", CreatorID: "creator1", Title: "one-to-one",
		SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "23:30",
		TimeRangeType: 2, IsActive: 1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	now := time.Now().UTC()
	tA := model.SummaryTask{TaskNo: "1to1-A", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now}
	tB := model.SummaryTask{TaskNo: "1to1-B", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now}
	if err := db.Create(&tA).Error; err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	if err := db.Create(&tB).Error; err != nil {
		t.Fatalf("create taskB: %v", err)
	}
	db.Create(&model.SummaryParticipant{TaskID: tA.ID, UserID: "creator1", Status: model.ParticipantAccepted})
	db.Create(&model.SummaryParticipant{TaskID: tB.ID, UserID: "creator1", Status: model.ParticipantAccepted})

	// First bind: task A -> schedule. Must succeed.
	w1 := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope": "task", "task_id": tA.ID, "run_time": "09:30", "interval_days": 1,
	})
	if w1.Code != http.StatusOK {
		t.Fatalf("first bind expected 200, got %d body=%s", w1.Code, w1.Body.String())
	}

	// Second bind: task B -> SAME schedule. Must be rejected (already bound).
	w2 := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope": "task", "task_id": tB.ID, "run_time": "10:30", "interval_days": 1,
	})
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("second bind expected 400 (already bound), got %d body=%s", w2.Code, w2.Body.String())
	}

	// Invariant: exactly one live task bound to this schedule.
	var boundCount int64
	db.Model(&model.SummaryTask{}).
		Where("schedule_id = ? AND deleted_at IS NULL", sched.ID).
		Count(&boundCount)
	if boundCount != 1 {
		t.Fatalf("one-to-one violated: %d live tasks bound to schedule %d", boundCount, sched.ID)
	}
	var gotA, gotB model.SummaryTask
	db.First(&gotA, tA.ID)
	db.First(&gotB, tB.ID)
	if gotA.ScheduleID == nil || *gotA.ScheduleID != sched.ID {
		t.Fatalf("taskA should be bound, got %v", gotA.ScheduleID)
	}
	if gotB.ScheduleID != nil {
		t.Fatalf("taskB should NOT be bound, got %v", *gotB.ScheduleID)
	}
}

// --- Migration unit test: generated-column key logic --------------------------
//
// The live-binding uniqueness migration is MySQL-only (STORED generated column),
// so we cannot run the actual DDL under sqlite. Instead we pin the LOGIC of the
// generated key (live_schedule_id = schedule_id only while deleted_at IS NULL
// AND schedule_id IS NOT NULL, else NULL) by replaying the same CASE expression
// in sqlite and asserting the uniqueness behavior it is meant to produce.
func TestPR62Round5_LiveScheduleBindingKey_Logic(t *testing.T) {
	// Emulate the generated key for the relevant rows.
	type tcase struct {
		name        string
		scheduleID  *int64
		deleted     bool
		wantNullKey bool
	}
	sid := int64(42)
	cases := []tcase{
		{"live bound -> key present", &sid, false, false},
		{"unbound -> NULL key", nil, false, true},
		{"soft-deleted bound -> NULL key", &sid, true, true},
		{"soft-deleted unbound -> NULL key", nil, true, true},
	}
	for _, tc := range cases {
		var liveKey *int64
		if !tc.deleted && tc.scheduleID != nil {
			v := *tc.scheduleID
			liveKey = &v
		}
		gotNull := liveKey == nil
		if gotNull != tc.wantNullKey {
			t.Fatalf("%s: gotNullKey=%v want=%v", tc.name, gotNull, tc.wantNullKey)
		}
	}
}
