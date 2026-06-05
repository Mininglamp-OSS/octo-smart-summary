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
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummarySchedule{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func setupScheduleRouter(h *ScheduleHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.PUT("/api/v1/summary-schedules/:id", h.UpdateSchedule)
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
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/summary-schedules/"+itoa(schedID), bytes.NewReader(b))
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
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

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// TestUpdateSchedule_SharedClonesForTaskScope verifies Plan A1: when a schedule
// is shared by >1 task and the detail page sends scope=task, the handler clones
// a new schedule for THIS task and leaves the original (and the sibling task)
// untouched.
func TestUpdateSchedule_SharedClonesForTaskScope(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	schedID, taskA, taskB := seedSharedSchedule(t, db)

	data := doUpdate(t, r, schedID, map[string]interface{}{
		"scope":     "task",
		"task_id":   taskA,
		"run_time":  "09:30",
		"interval_days": 1,
	})

	newSchedID := int64(data["schedule_id"].(float64))
	if newSchedID == schedID {
		t.Fatalf("expected a cloned schedule id different from original %d, got %d", schedID, newSchedID)
	}

	// Original schedule unchanged.
	var orig model.SummarySchedule
	db.First(&orig, schedID)
	if orig.RunTime != "17:00" {
		t.Errorf("original schedule run_time changed to %q, want 17:00", orig.RunTime)
	}

	// Clone has the new run_time.
	var clone model.SummarySchedule
	db.First(&clone, newSchedID)
	if clone.RunTime != "09:30" {
		t.Errorf("clone run_time = %q, want 09:30", clone.RunTime)
	}
	if clone.SpaceID != "space1" || clone.IsActive != 1 {
		t.Errorf("clone metadata wrong: space=%q active=%d", clone.SpaceID, clone.IsActive)
	}
	if clone.NextRunAt == nil {
		t.Errorf("clone next_run_at not computed")
	}

	// taskA now points at the clone; taskB still points at the original.
	var ta, tb model.SummaryTask
	db.First(&ta, taskA)
	db.First(&tb, taskB)
	if ta.ScheduleID == nil || *ta.ScheduleID != newSchedID {
		t.Errorf("taskA schedule_id = %v, want clone %d", ta.ScheduleID, newSchedID)
	}
	if tb.ScheduleID == nil || *tb.ScheduleID != schedID {
		t.Errorf("taskB schedule_id = %v, want original %d (must be untouched)", tb.ScheduleID, schedID)
	}
}

// TestUpdateSchedule_SoleOwnerUpdatesInPlace verifies that when the schedule is
// referenced by exactly one task, scope=task updates in place (no clone).
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
	db.Model(&model.SummarySchedule{}).Count(&count)
	if count != 1 {
		t.Errorf("expected no clone created, schedule count = %d", count)
	}
}

// TestUpdateSchedule_ListPageSharedUpdatesInPlace verifies the schedule list
// page behaviour is preserved: without scope=task the shared row is updated in
// place (managing the template directly).
func TestUpdateSchedule_ListPageSharedUpdatesInPlace(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	schedID, _, _ := seedSharedSchedule(t, db)

	data := doUpdate(t, r, schedID, map[string]interface{}{
		"run_time":      "10:00",
		"interval_days": 1,
	})
	if int64(data["schedule_id"].(float64)) != schedID {
		t.Fatalf("list-page update should keep original schedule id %d", schedID)
	}
	var got model.SummarySchedule
	db.First(&got, schedID)
	if got.RunTime != "10:00" {
		t.Errorf("run_time = %q, want 10:00 (in-place template edit)", got.RunTime)
	}
	var count int64
	db.Model(&model.SummarySchedule{}).Count(&count)
	if count != 1 {
		t.Errorf("expected no clone for list-page edit, count = %d", count)
	}
}
