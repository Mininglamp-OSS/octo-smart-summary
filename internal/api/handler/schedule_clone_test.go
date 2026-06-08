package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupScheduleDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummarySchedule{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func setupScheduleRouter(h *ScheduleHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summary-schedules", h.CreateSchedule)
	r.PUT("/api/v1/summary-schedules/:id", h.UpdateSchedule)
	r.DELETE("/api/v1/summary-schedules/:id", h.DeleteSchedule)
	r.PUT("/api/v1/summary-schedules/:id/toggle", h.ToggleSchedule)
	return r
}

// seedSharedSchedule creates one schedule shared by two tasks.
func seedSharedSchedule(t *testing.T, db *gorm.DB) (schedID, taskA, taskB int64) {
	t.Helper()
	sched := model.SummarySchedule{
		SpaceID:      "space1",
		CreatorID:    "creator1",
		Title:        "Shared",
		SummaryMode:  model.ModeByPerson,
		IntervalDays: 1,
		RunTime:      "17:00",
		IsActive:     1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	now := time.Now().UTC()
	tA := model.SummaryTask{TaskNo: "A", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &sched.ID}
	tB := model.SummaryTask{TaskNo: "B", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &sched.ID}
	db.Create(&tA)
	db.Create(&tB)
	return sched.ID, tA.ID, tB.ID
}

func doUpdate(t *testing.T, r *gin.Engine, schedID int64, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(schedID), body)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	return resp.Data
}

func doScheduleJSONRequest(t *testing.T, r *gin.Engine, method, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	return doScheduleJSONRequestAsUser(t, r, "creator1", "space1", method, path, body)
}

func doScheduleJSONRequestAsUser(t *testing.T, r *gin.Engine, userID, spaceID, method, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Token", userID)
	req.Header.Set("X-Space-Id", spaceID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestUpdateSchedule_TaskScopeRejectsScheduleAlreadyBoundToAnotherTask(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	schedID, taskA, taskB := seedSharedSchedule(t, db)
	_ = taskB

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(schedID), map[string]interface{}{
		"scope":         "task",
		"task_id":       taskA,
		"run_time":      "09:30",
		"interval_days": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for shared schedule under one-to-one invariant, got %d: %s", w.Code, w.Body.String())
	}

	var orig model.SummarySchedule
	if err := db.First(&orig, schedID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if orig.RunTime != "17:00" {
		t.Errorf("schedule run_time changed to %q, want 17:00", orig.RunTime)
	}

	var ta, tb model.SummaryTask
	if err := db.First(&ta, taskA).Error; err != nil {
		t.Fatalf("reload taskA: %v", err)
	}
	if err := db.First(&tb, taskB).Error; err != nil {
		t.Fatalf("reload taskB: %v", err)
	}
	if ta.ScheduleID == nil || *ta.ScheduleID != schedID {
		t.Errorf("taskA schedule_id = %v, want original %d", ta.ScheduleID, schedID)
	}
	if tb.ScheduleID == nil || *tb.ScheduleID != schedID {
		t.Errorf("taskB schedule_id = %v, want original %d", tb.ScheduleID, schedID)
	}
}

// TestUpdateSchedule_SoleOwnerUpdatesInPlace verifies that scope=task always
// updates the task's exclusive schedule in place.
func TestUpdateSchedule_SoleOwnerUpdatesInPlace(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Solo", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", IsActive: 1}
	db.Create(&sched)
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "Solo", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &sched.ID}
	db.Create(&task)

	data := doUpdate(t, r, sched.ID, map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "08:15",
		"interval_days": 1,
	})

	if int64(data["schedule_id"].(float64)) != sched.ID {
		t.Fatalf("expected in-place update keeping schedule id %d", sched.ID)
	}
	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.RunTime != "08:15" {
		t.Errorf("run_time = %q, want 08:15", got.RunTime)
	}

	var count int64
	db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&count)
	if count != 1 {
		t.Errorf("expected in-place update only, live schedule count = %d", count)
	}
}

func TestCreateSchedule_RejectsNonTaskScope(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "No bind",
		"scope":           "list",
		"interval_days":   1,
		"run_time":        "09:00",
		"time_range_type": 2,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 40000 {
		t.Fatalf("code=%d want 40000 body=%s", resp.Code, w.Body.String())
	}

	var count int64
	if err := db.Model(&model.SummarySchedule{}).Count(&count).Error; err != nil {
		t.Fatalf("count schedules: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no schedule created, got %d", count)
	}
}

func TestCreateSchedule_TaskScopeUpdatesExistingScheduleInsteadOfCreatingSecond(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "Existing",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "17:00",
		TimeRangeType: 2,
		IsActive:      1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "CREATE-SCOPE-UPDATE",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "Updated by create fallback",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "08:30",
		"time_range_type": 2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if int64(resp.Data["schedule_id"].(float64)) != sched.ID {
		t.Fatalf("expected create fallback to reuse schedule %d", sched.ID)
	}

	var gotSched model.SummarySchedule
	if err := db.First(&gotSched, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if gotSched.Title != "Updated by create fallback" || gotSched.RunTime != "08:30" {
		t.Fatalf("schedule not updated in place: title=%q run_time=%q", gotSched.Title, gotSched.RunTime)
	}

	var count int64
	if err := db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&count).Error; err != nil {
		t.Fatalf("count schedules: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected no second schedule row, live count=%d", count)
	}
}

func TestCreateSchedule_TaskScopeBindsUnscheduledTask(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "CREATE-SCOPE-BIND",
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
		"title":           "Fresh bind",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "09:00",
		"time_range_type": 2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	newSchedID := int64(resp.Data["schedule_id"].(float64))

	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil || *gotTask.ScheduleID != newSchedID {
		t.Fatalf("expected task bound to new schedule %d, got %v", newSchedID, gotTask.ScheduleID)
	}
}

func TestUpdateSchedule_TaskScopeBindsUnscheduledTask(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Fresh", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", IsActive: 1}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "Unscheduled", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	data := doUpdate(t, r, sched.ID, map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "08:15",
		"interval_days": 1,
	})

	if int64(data["schedule_id"].(float64)) != sched.ID {
		t.Fatalf("expected update to keep schedule id %d", sched.ID)
	}

	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil || *gotTask.ScheduleID != sched.ID {
		t.Fatalf("expected task bound to schedule %d, got %v", sched.ID, gotTask.ScheduleID)
	}

	var gotSched model.SummarySchedule
	if err := db.First(&gotSched, sched.ID).Error; err != nil {
		t.Fatalf("reload sched: %v", err)
	}
	if gotSched.RunTime != "08:15" {
		t.Fatalf("run_time = %q, want 08:15", gotSched.RunTime)
	}
}

func TestUpdateSchedule_TaskScopeRebindsToNewSchedule(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	oldSched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Old", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", IsActive: 1}
	newSched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "New", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "11:00", IsActive: 1}
	if err := db.Create(&oldSched).Error; err != nil {
		t.Fatalf("create old sched: %v", err)
	}
	if err := db.Create(&newSched).Error; err != nil {
		t.Fatalf("create new sched: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "Rebind", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &oldSched.ID}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	data := doUpdate(t, r, newSched.ID, map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "08:15",
		"interval_days": 1,
	})

	if int64(data["schedule_id"].(float64)) != newSched.ID {
		t.Fatalf("expected rebind to keep target schedule id %d", newSched.ID)
	}

	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil || *gotTask.ScheduleID != newSched.ID {
		t.Fatalf("expected task rebound to schedule %d, got %v", newSched.ID, gotTask.ScheduleID)
	}

	var gotOld, gotNew model.SummarySchedule
	if err := db.First(&gotOld, oldSched.ID).Error; err != nil {
		t.Fatalf("reload old sched: %v", err)
	}
	if err := db.First(&gotNew, newSched.ID).Error; err != nil {
		t.Fatalf("reload new sched: %v", err)
	}
	if gotOld.RunTime != "17:00" {
		t.Fatalf("old schedule run_time = %q, want unchanged 17:00", gotOld.RunTime)
	}
	if gotOld.DeletedAt == nil {
		t.Fatalf("expected old schedule soft deleted after rebind")
	}
	if gotNew.RunTime != "08:15" {
		t.Fatalf("new schedule run_time = %q, want 08:15", gotNew.RunTime)
	}
}

func TestUpdateSchedule_TaskScopeRejectsTargetScheduleBoundToAnotherTask(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	oldSched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Old", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", IsActive: 1}
	targetSched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Target", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "11:00", IsActive: 1}
	if err := db.Create(&oldSched).Error; err != nil {
		t.Fatalf("create old sched: %v", err)
	}
	if err := db.Create(&targetSched).Error; err != nil {
		t.Fatalf("create target sched: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "Rebind", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &oldSched.ID}
	other := model.SummaryTask{TaskNo: "Other", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &targetSched.ID}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("create other task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(targetSched.ID), map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "08:15",
		"interval_days": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when target schedule is already bound, got %d: %s", w.Code, w.Body.String())
	}

	var gotTask, gotOther model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if err := db.First(&gotOther, other.ID).Error; err != nil {
		t.Fatalf("reload other task: %v", err)
	}
	if gotTask.ScheduleID == nil || *gotTask.ScheduleID != oldSched.ID {
		t.Fatalf("expected task to stay on old schedule %d, got %v", oldSched.ID, gotTask.ScheduleID)
	}
	if gotOther.ScheduleID == nil || *gotOther.ScheduleID != targetSched.ID {
		t.Fatalf("expected other task stay on original target %d, got %v", targetSched.ID, gotOther.ScheduleID)
	}

	var gotTarget model.SummarySchedule
	if err := db.First(&gotTarget, targetSched.ID).Error; err != nil {
		t.Fatalf("reload target sched: %v", err)
	}
	if gotTarget.RunTime != "11:00" {
		t.Fatalf("target schedule run_time = %q, want unchanged 11:00", gotTarget.RunTime)
	}

	var count int64
	if err := db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&count).Error; err != nil {
		t.Fatalf("count schedules: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected no clone created, live schedule count=%d", count)
	}
}

func TestUpdateSchedule_ListPageUpdatesInPlace(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "List", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", IsActive: 1}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	data := doUpdate(t, r, sched.ID, map[string]interface{}{
		"run_time":      "10:00",
		"interval_days": 1,
	})
	if int64(data["schedule_id"].(float64)) != sched.ID {
		t.Fatalf("list-page update should keep original schedule id %d", sched.ID)
	}
	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.RunTime != "10:00" {
		t.Errorf("run_time = %q, want 10:00", got.RunTime)
	}
	var count int64
	db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&count)
	if count != 1 {
		t.Errorf("expected no extra schedule row, count = %d", count)
	}
}

func TestUpdateSchedule_TaskScopeRejectsInvalidTaskID(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	schedID, _, _ := seedSharedSchedule(t, db)

	cases := []struct {
		name   string
		body   map[string]interface{}
		status int
	}{
		{
			name:   "missing task_id",
			body:   map[string]interface{}{"scope": "task", "run_time": "09:30", "interval_days": 1},
			status: http.StatusBadRequest,
		},
		{
			name:   "unknown task_id",
			body:   map[string]interface{}{"scope": "task", "task_id": 999999, "run_time": "09:30", "interval_days": 1},
			status: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(schedID), tc.body)
		if w.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
	}
}

func TestUpdateSchedule_TaskScopeRejectsCrossSpaceTask(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Shared", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", IsActive: 1}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "space2-task", SpaceID: "space2", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "09:30",
		"interval_days": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateSchedule_TaskScopeRejectsOtherUsersTask(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "OTHER-TASK-CREATE",
		SpaceID:        "space1",
		CreatorID:      "other-user",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "bind other task",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_days":   1,
		"run_time":        "09:30",
		"time_range_type": 2,
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 40004 {
		t.Fatalf("code=%d want 40004 body=%s", resp.Code, w.Body.String())
	}
}

func TestUpdateSchedule_TaskScopeRejectsOtherUsersTask(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID:      "space1",
		CreatorID:    "creator1",
		Title:        "Owned",
		SummaryMode:  model.ModeByPerson,
		IntervalDays: 1,
		RunTime:      "17:00",
		IsActive:     1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "OTHER-TASK-UPDATE",
		SpaceID:        "space1",
		CreatorID:      "other-user",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"scope":         "task",
		"task_id":       task.ID,
		"run_time":      "09:30",
		"interval_days": 1,
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 40004 {
		t.Fatalf("code=%d want 40004 body=%s", resp.Code, w.Body.String())
	}
}

func TestCreateSchedule_RejectsMismatchedAnchors(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "bad",
		"interval_days":   1,
		"run_time":        "09:00",
		"day_of_week":     1,
		"time_range_type": 2,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateSchedule_RejectsMismatchedAnchors(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "monthly",
		SummaryMode:    model.ModeByPerson,
		IntervalMonths: 1,
		RunTime:        "09:00",
		DayOfMonth:     1,
		TimeRangeType:  2,
		IsActive:       1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"day_of_week": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDeleteSchedule_UnbindsTasks(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Delete", SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "17:00", IsActive: 1}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "DELETE-SCHED", SpaceID: "space1", CreatorID: "creator1", SummaryMode: model.ModeByPerson, TimeRangeStart: now, TimeRangeEnd: now, ScheduleID: &sched.ID}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodDelete, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var gotTask model.SummaryTask
	db.First(&gotTask, task.ID)
	if gotTask.ScheduleID != nil {
		t.Fatalf("expected task unbound after delete, got %v", gotTask.ScheduleID)
	}
}

func TestToggleSchedule_EnableFailsOnInvalidRecurrence(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "bad",
		SummaryMode:    model.ModeByPerson,
		IntervalDays:   0,
		IntervalMonths: 0,
		IsActive:       0,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	if err := db.Model(&sched).Update("is_active", 0).Error; err != nil {
		t.Fatalf("disable sched: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID)+"/toggle", map[string]interface{}{
		"is_active": true,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.IsActive != 0 {
		t.Fatalf("expected schedule stay inactive, got %d", got.IsActive)
	}
}
