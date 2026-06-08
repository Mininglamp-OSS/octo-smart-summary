package handler

import (
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

func setupDeleteCascadeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySchedule{},
		&model.SummaryParticipant{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func setupDeleteCascadeRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.DELETE("/api/v1/summaries/:id", h.DeleteSummary)
	return r
}

// seedTaskWithSchedule creates a task (creator=taskCreator) bound to a schedule
// (creator=schedCreator), with an extra participant.
func seedTaskWithSchedule(t *testing.T, db *gorm.DB, taskCreator, schedCreator, participant string) (taskID, schedID int64) {
	t.Helper()
	sched := model.SummarySchedule{
		SpaceID:      "space1",
		CreatorID:    schedCreator,
		Title:        "Sched",
		SummaryMode:  model.ModeByPerson,
		IntervalDays: 1,
		RunTime:      "09:00",
		IsActive:     1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "T-cascade",
		SpaceID:        "space1",
		CreatorID:      taskCreator,
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// participant must be able to access (pass authorizeTaskAccess)
	if err := db.Create(&model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   participant,
		UserName: participant,
		Status:   model.ParticipantAccepted,
	}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	return task.ID, sched.ID
}

func doDelete(t *testing.T, r *gin.Engine, taskID int64, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/summaries/"+strconv.FormatInt(taskID, 10), nil)
	req.Header.Set("Token", token)
	req.Header.Set("X-Space-Id", "space1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestDeleteSummary_ParticipantCannotCascadeDeleteOthersSchedule verifies that a
// task participant who is NOT the schedule creator cannot take down another
// user's schedule by deleting the task. The schedule must survive (only unbound).
func TestDeleteSummary_ParticipantCannotCascadeDeleteOthersSchedule(t *testing.T) {
	db := setupDeleteCascadeDB(t)
	h := NewTaskHandler(db, db, "")
	r := setupDeleteCascadeRouter(h)

	// task creator = creator1, schedule creator = creator1, participant = victimUser
	taskID, schedID := seedTaskWithSchedule(t, db, "creator1", "creator1", "user2")

	// user2 (participant, not schedule creator) deletes the task.
	w := doDelete(t, r, taskID, "user2")
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}

	// Task is soft-deleted.
	var task model.SummaryTask
	if err := db.Unscoped().First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.DeletedAt == nil {
		t.Fatalf("task should be soft-deleted")
	}
	// Task must be unbound from schedule (not creator => no cascade).
	if task.ScheduleID != nil {
		t.Fatalf("task should be unbound from schedule, got schedule_id=%v", *task.ScheduleID)
	}

	// Schedule MUST survive (not the caller's schedule).
	var sched model.SummarySchedule
	if err := db.Where("id = ? AND deleted_at IS NULL", schedID).First(&sched).Error; err != nil {
		t.Fatalf("schedule should NOT be deleted by a non-creator participant: %v", err)
	}
}

// TestDeleteSummary_CreatorCascadeDeletesOwnSchedule verifies the happy path:
// when the caller is both the task creator and the schedule creator, deleting
// the task cascades the soft-delete to the bound schedule (legacy behaviour).
func TestDeleteSummary_CreatorCascadeDeletesOwnSchedule(t *testing.T) {
	db := setupDeleteCascadeDB(t)
	h := NewTaskHandler(db, db, "")
	r := setupDeleteCascadeRouter(h)

	taskID, schedID := seedTaskWithSchedule(t, db, "creator1", "creator1", "user2")

	// creator1 (task creator AND schedule creator) deletes the task.
	w := doDelete(t, r, taskID, "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}

	// Schedule MUST be cascade soft-deleted.
	var cnt int64
	db.Model(&model.SummarySchedule{}).Where("id = ? AND deleted_at IS NULL", schedID).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("schedule should be cascade soft-deleted by its creator, still active count=%d", cnt)
	}
}
