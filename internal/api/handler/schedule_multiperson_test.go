package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// --- 方案A (PR#62 yujiawei r3): API-layer fail-closed multi-person guard ------
// Scheduled summary is single-person only this version. The worker-layer Method
// A guard (internal/worker/scheduler.go) already skips multi-person bound tasks
// every cycle, but before this fix the API accepted multi-person input (200 +
// next_run_at) and the user waited forever with no error. These tests pin the
// new API-layer rejection: multi-person => HTTP 400 / code 40015, nothing
// persisted. The single-person happy path must keep working (regression).

const codeTeamScheduleNotSupported = 40015

func decodeCode(t *testing.T, w interface{ Bytes() []byte }) int {
	t.Helper()
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp.Code
}

//  1. CreateSchedule with > 1 participants must be rejected (400 / 40015) and
//     nothing must be written.
func TestCreateSchedule_MultiPersonRejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "PR62-R3-MP-CREATE",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "team schedule",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "23:30",
		"time_range_type": 2,
		"participants": []map[string]interface{}{
			{"user_id": "u1", "user_name": "A"},
			{"user_id": "u2", "user_name": "B"},
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if code := decodeCode(t, w.Body); code != codeTeamScheduleNotSupported {
		t.Fatalf("expected code %d, got %d body=%s", codeTeamScheduleNotSupported, code, w.Body.String())
	}
	// Nothing persisted.
	var cnt int64
	db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&cnt)
	if cnt != 0 {
		t.Fatalf("expected no schedule persisted, live count=%d", cnt)
	}
	// Task must remain unbound.
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID != nil {
		t.Fatalf("expected task unbound, got schedule_id=%v", *gotTask.ScheduleID)
	}
}

// 2) UpdateSchedule with > 1 participants must be rejected (400 / 40015).
func TestUpdateSchedule_MultiPersonRejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID: "space1", CreatorID: "creator1", Title: "single", SummaryMode: model.ModeByPerson,
		IntervalDays: 1, RunTime: "17:00", IsActive: 1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"run_time": "08:15",
		"participants": []map[string]interface{}{
			{"user_id": "u1", "user_name": "A"},
			{"user_id": "u2", "user_name": "B"},
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if code := decodeCode(t, w.Body); code != codeTeamScheduleNotSupported {
		t.Fatalf("expected code %d, got %d body=%s", codeTeamScheduleNotSupported, code, w.Body.String())
	}
	// participant_config must NOT have been written.
	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload sched: %v", err)
	}
	if len(got.ParticipantConfig) != 0 {
		t.Fatalf("expected participant_config untouched, got %s", string(got.ParticipantConfig))
	}
}

//  3. loadTaskForTaskScope: binding a schedule to a task that already has > 1
//     SummaryParticipant rows must be rejected (400 / 40015), same口径 as worker.
func TestLoadTaskForTaskScope_MultiPersonTaskRejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "PR62-R3-MP-BIND",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Two participants on the task -> multi-person by worker口径.
	for _, uid := range []string{"u1", "u2"} {
		if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: uid, UserName: uid}).Error; err != nil {
			t.Fatalf("create participant: %v", err)
		}
	}

	// Note: the create-schedule request carries NO participants array (so the
	// CreateSchedule input-level guard does not fire); the rejection must come
	// from loadTaskForTaskScope counting the task's bound participants.
	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "bind to team task",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "23:30",
		"time_range_type": 2,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if code := decodeCode(t, w.Body); code != codeTeamScheduleNotSupported {
		t.Fatalf("expected code %d, got %d body=%s", codeTeamScheduleNotSupported, code, w.Body.String())
	}
	// No schedule persisted, task still unbound.
	var cnt int64
	db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&cnt)
	if cnt != 0 {
		t.Fatalf("expected no schedule persisted, live count=%d", cnt)
	}
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID != nil {
		t.Fatalf("expected task unbound, got schedule_id=%v", *gotTask.ScheduleID)
	}
}

// Regression: single-person create path still succeeds (1 participant, and a
// task with exactly 1 bound participant binds fine).
func TestCreateSchedule_SinglePersonStillAllowed(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "PR62-R3-SP-OK",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "creator1"}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "solo schedule",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "23:30",
		"time_range_type": 2,
		"participants": []map[string]interface{}{
			{"user_id": "creator1", "user_name": "creator1"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil {
		t.Fatalf("expected task bound to a schedule")
	}
}

// --- r4 Bug1 (PR#62, three reviewers): the "1 non-creator participant" hole ----
// The worker's syncScheduledTaskParticipants always prepends task.CreatorID, so
// a request carrying exactly ONE participant that is NOT the creator would slip
// past the old `count > 1` guard and be inflated to 2 rows by the worker,
// stranding the task in the unsupported team branch. These tests pin the
// upgraded口径: participants must be a subset of {task.CreatorID}.

//  r4-1) CreateSchedule with exactly ONE non-creator participant must be
//        rejected (400 / 40015) and nothing persisted.
func TestCreateSchedule_SingleNonCreatorParticipantRejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "PR62-R4-MP-1NONCREATOR",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "sneaky single non-creator",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "23:30",
		"time_range_type": 2,
		// Exactly ONE participant, and it is NOT the task creator.
		"participants": []map[string]interface{}{
			{"user_id": "someone_else", "user_name": "X"},
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if code := decodeCode(t, w.Body); code != codeTeamScheduleNotSupported {
		t.Fatalf("expected code %d, got %d body=%s", codeTeamScheduleNotSupported, code, w.Body.String())
	}
	var cnt int64
	db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&cnt)
	if cnt != 0 {
		t.Fatalf("expected no schedule persisted, live count=%d", cnt)
	}
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID != nil {
		t.Fatalf("expected task unbound, got schedule_id=%v", *gotTask.ScheduleID)
	}
}

//  r4-2) UpdateSchedule with exactly ONE non-creator participant must be
//        rejected (400 / 40015); participant_config untouched.
func TestUpdateSchedule_SingleNonCreatorParticipantRejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID: "space1", CreatorID: "creator1", Title: "single", SummaryMode: model.ModeByPerson,
		IntervalDays: 1, RunTime: "17:00", IsActive: 1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"run_time": "08:15",
		"participants": []map[string]interface{}{
			{"user_id": "someone_else", "user_name": "X"},
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if code := decodeCode(t, w.Body); code != codeTeamScheduleNotSupported {
		t.Fatalf("expected code %d, got %d body=%s", codeTeamScheduleNotSupported, code, w.Body.String())
	}
	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload sched: %v", err)
	}
	if len(got.ParticipantConfig) != 0 {
		t.Fatalf("expected participant_config untouched, got %s", string(got.ParticipantConfig))
	}
}

//  r4-3) CreateSchedule with ZERO participants still succeeds (subset of
//        {creator} vacuously). The worker will prepend the creator -> 1 person.
func TestCreateSchedule_ZeroParticipantsStillAllowed(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "PR62-R4-ZEROP",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "zero participants",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "23:30",
		"time_range_type": 2,
		// no participants key at all
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil {
		t.Fatalf("expected task bound to a schedule")
	}
}

//  r4-4) CreateSchedule with exactly the creator as the only participant still
//        succeeds (subset of {creator}).
func TestCreateSchedule_OnlyCreatorParticipantStillAllowed(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "PR62-R4-ONLYCREATOR",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "only creator",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "23:30",
		"time_range_type": 2,
		"participants": []map[string]interface{}{
			{"user_id": "creator1", "user_name": "creator1"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil {
		t.Fatalf("expected task bound to a schedule")
	}
}

// r4 unit: participantsSubsetOfCreator truth table.
func TestParticipantsSubsetOfCreator(t *testing.T) {
	cases := []struct {
		name    string
		ps      []participantReq
		creator string
		want    bool
	}{
		{"empty", nil, "c1", true},
		{"only creator", []participantReq{{UserID: "c1"}}, "c1", true},
		{"creator dup", []participantReq{{UserID: "c1"}, {UserID: "c1"}}, "c1", true},
		{"empty userid skipped", []participantReq{{UserID: ""}}, "c1", true},
		{"single non-creator", []participantReq{{UserID: "x"}}, "c1", false},
		{"creator plus other", []participantReq{{UserID: "c1"}, {UserID: "x"}}, "c1", false},
		{"two others", []participantReq{{UserID: "a"}, {UserID: "b"}}, "c1", false},
	}
	for _, tc := range cases {
		if got := participantsSubsetOfCreator(tc.ps, tc.creator); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
