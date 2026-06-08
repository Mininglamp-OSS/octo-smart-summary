package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupSharedScheduleDBs(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), time.Now().UnixNano())
	open := func() *gorm.DB {
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		return db
	}
	db := open()
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummarySchedule{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, open()
}

func TestPR62Round13_DeleteScheduleConcurrentDeleteReturns40916(t *testing.T) {
	db, concurrentDB := setupSharedScheduleDBs(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID:      "space1",
		CreatorID:    "creator1",
		Title:        "Delete",
		SummaryMode:  model.ModeByPerson,
		IntervalDays: 1,
		RunTime:      "17:00",
		IsActive:     1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "DELETE-SCHED-R13",
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

	var scheduleQueryCount int32
	var callbackErr atomic.Value
	callbackName := "pr62_round13_delete_schedule_conflict"
	if err := db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "summary_schedule" {
			return
		}
		if atomic.AddInt32(&scheduleQueryCount, 1) != 2 {
			return
		}
		deletedAt := time.Now().UTC()
		if err := concurrentDB.Model(&model.SummarySchedule{}).
			Where("id = ?", sched.ID).
			Update("deleted_at", &deletedAt).Error; err != nil {
			callbackErr.Store(err)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	defer db.Callback().Query().Remove(callbackName)

	w := doScheduleJSONRequest(t, r, http.MethodDelete, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{})
	if err, _ := callbackErr.Load().(error); err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 40916 {
		t.Fatalf("code=%d want 40916", resp.Code)
	}

	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.ScheduleID == nil || *gotTask.ScheduleID != sched.ID {
		t.Fatalf("task schedule_id=%v want still bound to %d", gotTask.ScheduleID, sched.ID)
	}
}
