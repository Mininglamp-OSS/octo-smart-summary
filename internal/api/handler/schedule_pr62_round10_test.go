package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

func TestPR62Round10_UpdateMonthlyRecomputeUsesStoredAnchorDOM(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := timezone.Now()
	if now.Hour() == 23 && now.Minute() >= 58 {
		t.Skip("too close to midnight for stable monthly run_time assertions")
	}

	nextRun := now.Add(24 * time.Hour)
	sched := model.SummarySchedule{
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "monthly-update-anchor",
		SummaryMode:    model.ModeByPerson,
		IntervalMonths: 1,
		RunTime:        "09:00",
		DayOfMonth:     0,
		AnchorDOM:      30,
		TimeRangeType:  2,
		IsActive:       1,
		NextRunAt:      &nextRun,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	seedScheduleTask(t, db, "round10-update-monthly", &sched.ID)

	runTime := "23:59"
	want := expectedMonthlyNextRun(t, now, runTime, 30)

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"run_time": runTime,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	assertMonthlyNextRun(t, got.NextRunAt, want, 30)
	if got.AnchorDOM != 30 {
		t.Fatalf("anchor_dom=%d want 30", got.AnchorDOM)
	}
}

func TestPR62Round10_CreateReuseMonthlyRecomputeUsesExistingAnchorDOM(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := timezone.Now()
	if now.Hour() == 23 && now.Minute() >= 58 {
		t.Skip("too close to midnight for stable monthly run_time assertions")
	}

	nextRun := now.Add(24 * time.Hour)
	sched := model.SummarySchedule{
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "monthly-reuse-anchor",
		SummaryMode:    model.ModeByPerson,
		IntervalMonths: 1,
		RunTime:        "09:00",
		DayOfMonth:     0,
		AnchorDOM:      30,
		TimeRangeType:  2,
		IsActive:       0,
		NextRunAt:      &nextRun,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	task := seedScheduleTask(t, db, "round10-reuse-monthly", &sched.ID)

	runTime := "23:59"
	want := expectedMonthlyNextRun(t, now, runTime, 30)

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "reuse-monthly",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_months": 1,
		"day_of_month":    0,
		"run_time":        runTime,
		"time_range_type": 2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	assertMonthlyNextRun(t, got.NextRunAt, want, 30)
	if got.AnchorDOM != 30 {
		t.Fatalf("anchor_dom=%d want 30", got.AnchorDOM)
	}
	if got.IsActive != 1 {
		t.Fatalf("is_active=%d want 1", got.IsActive)
	}
}

func TestPR62Round10_ToggleMonthlyReenableUsesStoredAnchorDOM(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := timezone.Now()
	if now.Hour() == 23 && now.Minute() >= 58 {
		t.Skip("too close to midnight for stable monthly run_time assertions")
	}

	runTime := now.Add(2 * time.Minute).Format("15:04")
	want := expectedMonthlyNextRun(t, now, runTime, 30)

	nextRun := now.Add(-24 * time.Hour)
	sched := model.SummarySchedule{
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "monthly-toggle-anchor",
		SummaryMode:    model.ModeByPerson,
		IntervalMonths: 1,
		RunTime:        runTime,
		DayOfMonth:     0,
		AnchorDOM:      30,
		TimeRangeType:  2,
		IsActive:       0,
		NextRunAt:      &nextRun,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	db.Model(&sched).Update("is_active", 0)
	seedScheduleTask(t, db, "round10-toggle-monthly", &sched.ID)

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID)+"/toggle", map[string]interface{}{
		"is_active": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	assertMonthlyNextRun(t, got.NextRunAt, want, 30)
	if got.AnchorDOM != 30 {
		t.Fatalf("anchor_dom=%d want 30", got.AnchorDOM)
	}
}

func TestPR62Round10_CreateMonthlyDOMZeroUsesFreshAnchorDOMForFirstRun(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := timezone.Now()
	if now.Hour() == 23 && now.Minute() >= 58 {
		t.Skip("too close to midnight for stable monthly run_time assertions")
	}

	task := seedScheduleTask(t, db, "round10-create-monthly", nil)
	runTime := "23:59"
	wantAnchorDOM := now.Day()
	want := expectedMonthlyNextRun(t, now, runTime, wantAnchorDOM)

	w := doScheduleJSONRequest(t, r, http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"title":           "new-monthly",
		"scope":           "task",
		"task_id":         task.ID,
		"interval_months": 1,
		"day_of_month":    0,
		"run_time":        runTime,
		"time_range_type": 2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var reloadedTask model.SummaryTask
	if err := db.First(&reloadedTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloadedTask.ScheduleID == nil {
		t.Fatalf("task schedule_id is nil after create")
	}

	var got model.SummarySchedule
	if err := db.First(&got, *reloadedTask.ScheduleID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if got.AnchorDOM != wantAnchorDOM {
		t.Fatalf("anchor_dom=%d want %d", got.AnchorDOM, wantAnchorDOM)
	}
	assertMonthlyNextRun(t, got.NextRunAt, want, wantAnchorDOM)
}

func TestPR62Round10_UpdateDayModeIgnoresAnchorDOMDuringRecompute(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	now := timezone.Now()
	if now.Hour() == 23 && now.Minute() >= 58 {
		t.Skip("too close to midnight for stable daily run_time assertions")
	}

	nextRun := now.Add(24 * time.Hour)
	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "daily-anchor-ignored",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		DayOfMonth:    0,
		AnchorDOM:     30,
		TimeRangeType: 2,
		IsActive:      1,
		NextRunAt:     &nextRun,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	seedScheduleTask(t, db, "round10-update-daily", &sched.ID)

	runTime := "23:59"
	want, err := service.NextRunInitial("", 1, 0, runTime, 0, 0, now)
	if err != nil {
		t.Fatalf("want next_run: %v", err)
	}

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID), map[string]interface{}{
		"run_time": runTime,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at=%v want %v", got.NextRunAt, want)
	}
}

func seedScheduleTask(t *testing.T, db *gorm.DB, taskNo string, scheduleID *int64) model.SummaryTask {
	t.Helper()

	taskNow := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         taskNo,
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: taskNow,
		TimeRangeEnd:   taskNow,
		ScheduleID:     scheduleID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummaryParticipant{
		TaskID: task.ID,
		UserID: "creator1",
		Status: model.ParticipantAccepted,
	}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	return task
}

func expectedMonthlyNextRun(t *testing.T, now time.Time, runTime string, dom int) time.Time {
	t.Helper()

	candidate := monthlyCandidate(t, now, runTime, dom)
	if candidate.After(now) {
		return candidate
	}
	return monthlyCandidate(t, now.AddDate(0, 1, 0), runTime, dom)
}

func monthlyCandidate(t *testing.T, base time.Time, runTime string, dom int) time.Time {
	t.Helper()

	parsed, err := time.ParseInLocation("15:04", runTime, base.Location())
	if err != nil {
		t.Fatalf("parse run_time %q: %v", runTime, err)
	}
	day := clampDOM(base.Year(), base.Month(), dom, base.Location())
	return time.Date(base.Year(), base.Month(), day, parsed.Hour(), parsed.Minute(), 0, 0, base.Location())
}

func clampDOM(year int, month time.Month, dom int, loc *time.Location) int {
	if dom < 1 {
		return dom
	}
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
	if dom > last {
		return last
	}
	return dom
}

func assertMonthlyNextRun(t *testing.T, got *time.Time, want time.Time, anchorDOM int) {
	t.Helper()

	if got == nil {
		t.Fatalf("next_run_at is nil, want %v", want)
	}
	wantDay := clampDOM(want.Year(), want.Month(), anchorDOM, want.Location())
	if got.Day() != wantDay {
		t.Fatalf("next_run_at day=%d want %d (next_run_at=%v)", got.Day(), wantDay, got)
	}
	if !got.Equal(want) {
		t.Fatalf("next_run_at=%v want %v", got, want)
	}
}
