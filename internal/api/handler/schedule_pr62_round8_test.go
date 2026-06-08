package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func createBoundScheduleTask(t *testing.T, db *gorm.DB, scheduleCreator, taskCreator string) (model.SummarySchedule, model.SummaryTask) {
	t.Helper()

	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     scheduleCreator,
		Title:         "Bound",
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
		TaskNo:         "ROUND8-BOUND",
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
	return sched, task
}

func TestPR62Round8_UpdateWithoutTaskScope_LoadsBoundTaskAndSucceeds(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched, _ := createBoundScheduleTask(t, db, "creator1", "creator1")

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"title": "renamed in place",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if got.Title != "renamed in place" {
		t.Fatalf("title=%q want renamed in place", got.Title)
	}
}

func TestPR62Round8_UpdateWithoutTaskScope_RejectsOrphanedSchedule(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched, task := createBoundScheduleTask(t, db, "creator1", "creator1")
	deletedAt := time.Now().UTC()
	if err := db.Model(&task).Update("deleted_at", &deletedAt).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"title": "should fail",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 40008 {
		t.Fatalf("code=%d want 40008 body=%s", resp.Code, w.Body.String())
	}

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if got.Title != "Bound" {
		t.Fatalf("title changed on orphan reject: %q", got.Title)
	}
}

func TestPR62Round8_UpdateWithoutTaskScope_RejectsBoundTaskOwnerMismatch(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched, _ := createBoundScheduleTask(t, db, "creator1", "other-user")

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"title": "should fail",
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

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if got.Title != "Bound" {
		t.Fatalf("title changed on owner mismatch reject: %q", got.Title)
	}
}
