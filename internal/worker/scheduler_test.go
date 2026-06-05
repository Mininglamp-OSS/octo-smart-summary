package worker

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupSchedulerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf(
		"file:%s-%d?mode=memory&cache=shared",
		strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()),
		time.Now().UnixNano(),
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec("PRAGMA busy_timeout = 5000").Error; err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummarySchedule{},
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummaryResult{},
		&model.SummaryChunk{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestClaimAndCreateScheduledTask_ConcurrentClaimReusesOneTask(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()
	due := now.Add(-time.Minute)

	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "Daily",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		TimeRangeType: 2,
		IsActive:      1,
		NextRunAt:     &due,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	oldStart := now.Add(-48 * time.Hour)
	oldEnd := now.Add(-24 * time.Hour)
	task := model.SummaryTask{
		TaskNo:         "task-reuse-1",
		SpaceID:        sched.SpaceID,
		CreatorID:      sched.CreatorID,
		Title:          "Daily",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: oldStart,
		TimeRangeEnd:   oldEnd,
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerManual,
		RetryCount:     2,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	source := model.SummarySource{
		TaskID:     task.ID,
		SourceType: model.SourceGroup,
		SourceID:   "group-1",
		SourceName: "Group 1",
	}
	if err := db.Create(&source).Error; err != nil {
		t.Fatalf("create source: %v", err)
	}
	participant := model.SummaryParticipant{
		TaskID:      task.ID,
		UserID:      sched.CreatorID,
		UserName:    "Creator",
		Status:      model.ParticipantCompleted,
		ConfirmedAt: &now,
	}
	if err := db.Create(&participant).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           sched.CreatorID,
		Content:          "old personal",
		MsgCount:         12,
		TotalTokenUsed:   34,
		ModelVersion:     "v-old",
		WorkerStatus:     model.PersonalStatusCompleted,
		GeneratedAt:      &now,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}
	if err := db.Model(&participant).Update("personal_result_id", pr.ID).Error; err != nil {
		t.Fatalf("bind personal result: %v", err)
	}

	snapshot := sched
	start := make(chan struct{})
	results := make(chan struct {
		taskID  int64
		claimed bool
		err     error
	}, 2)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			taskID, claimed, err := claimAndCreateScheduledTask(db, snapshot, now)
			results <- struct {
				taskID  int64
				claimed bool
				err     error
			}{taskID: taskID, claimed: claimed, err: err}
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	var claimedCount int
	for res := range results {
		if res.err != nil {
			t.Fatalf("claim err: %v", res.err)
		}
		if res.claimed {
			claimedCount++
		}
	}
	if claimedCount != 1 {
		t.Fatalf("claimedCount=%d want 1", claimedCount)
	}

	var taskCount int64
	db.Model(&model.SummaryTask{}).Where("schedule_id = ?", sched.ID).Count(&taskCount)
	if taskCount != 1 {
		t.Fatalf("taskCount=%d want 1", taskCount)
	}

	var updatedTask model.SummaryTask
	if err := db.First(&updatedTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	wantStart, wantEnd := service.ComputeTimeRange(sched.TimeRangeType, now)
	if updatedTask.ID != task.ID {
		t.Fatalf("task id changed: got %d want %d", updatedTask.ID, task.ID)
	}
	if updatedTask.Status != model.StatusPending {
		t.Fatalf("task status=%d want %d", updatedTask.Status, model.StatusPending)
	}
	if updatedTask.TriggerType != model.TriggerScheduled {
		t.Fatalf("trigger type=%d want %d", updatedTask.TriggerType, model.TriggerScheduled)
	}
	if updatedTask.RetryCount != 0 {
		t.Fatalf("retry_count=%d want 0", updatedTask.RetryCount)
	}
	if updatedTask.ErrorMessage != nil {
		t.Fatalf("error_message should be nil, got %v", *updatedTask.ErrorMessage)
	}
	if updatedTask.ProcessingDeadline != nil {
		t.Fatalf("processing_deadline should be nil")
	}
	if !updatedTask.TimeRangeStart.Equal(wantStart) || !updatedTask.TimeRangeEnd.Equal(wantEnd) {
		t.Fatalf("time range not refreshed: got [%v,%v] want [%v,%v]",
			updatedTask.TimeRangeStart, updatedTask.TimeRangeEnd, wantStart, wantEnd)
	}

	var participantCount int64
	db.Model(&model.SummaryParticipant{}).Count(&participantCount)
	if participantCount != 1 {
		t.Fatalf("participantCount=%d want 1", participantCount)
	}

	var prCount int64
	db.Model(&model.PersonalResult{}).Count(&prCount)
	if prCount != 1 {
		t.Fatalf("personalResultCount=%d want 1", prCount)
	}

	var updatedPR model.PersonalResult
	if err := db.First(&updatedPR, pr.ID).Error; err != nil {
		t.Fatalf("reload personal result: %v", err)
	}
	if updatedPR.WorkerStatus != model.PersonalStatusPending {
		t.Fatalf("worker_status=%d want %d", updatedPR.WorkerStatus, model.PersonalStatusPending)
	}
	if updatedPR.Content != "" || updatedPR.MsgCount != 0 || updatedPR.TotalTokenUsed != 0 {
		t.Fatalf("personal result not cleared: %+v", updatedPR)
	}
	if updatedPR.GeneratedAt != nil || updatedPR.ErrorMessage != nil || updatedPR.SubmittedAt != nil {
		t.Fatalf("personal result timestamps/errors should be cleared: %+v", updatedPR)
	}

	var updatedParticipant model.SummaryParticipant
	if err := db.First(&updatedParticipant, participant.ID).Error; err != nil {
		t.Fatalf("reload participant: %v", err)
	}
	if updatedParticipant.Status != model.ParticipantAccepted {
		t.Fatalf("participant status=%d want %d", updatedParticipant.Status, model.ParticipantAccepted)
	}
	if updatedParticipant.ConfirmedAt == nil {
		t.Fatalf("participant confirmed_at should be set")
	}
	if updatedParticipant.WorkerStartedAt != nil {
		t.Fatalf("participant worker_started_at should be nil")
	}

	var updated model.SummarySchedule
	if err := db.First(&updated, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if updated.LastRunAt == nil {
		t.Fatalf("expected last_run_at updated")
	}
	if updated.NextRunAt == nil || !updated.NextRunAt.After(due) {
		t.Fatalf("expected next_run_at advanced, got %v", updated.NextRunAt)
	}
}

func TestClaimAndCreateScheduledTask_ReusesExistingTaskWithoutNewRows(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()
	due := now.Add(-time.Minute)

	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "Daily",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		TimeRangeType: 2,
		IsActive:      1,
		NextRunAt:     &due,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	oldStart := now.Add(-72 * time.Hour)
	oldEnd := now.Add(-48 * time.Hour)
	task := model.SummaryTask{
		TaskNo:         "task-reset-1",
		SpaceID:        sched.SpaceID,
		CreatorID:      sched.CreatorID,
		Title:          "Weekly Summary",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: oldStart,
		TimeRangeEnd:   oldEnd,
		Status:         model.StatusFailed,
		TriggerType:    model.TriggerManual,
		RetryCount:     3,
		ScheduleID:     &sched.ID,
	}
	errMsg := "old failure"
	deadline := now.Add(time.Hour)
	task.ErrorMessage = &errMsg
	task.ProcessingDeadline = &deadline
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	source := model.SummarySource{
		TaskID:     task.ID,
		SourceType: model.SourceGroup,
		SourceID:   "group-1",
		SourceName: "Group 1",
	}
	if err := db.Create(&source).Error; err != nil {
		t.Fatalf("create source: %v", err)
	}
	confirmedAt := now.Add(-time.Hour)
	workerStarted := now.Add(-30 * time.Minute)
	participant := model.SummaryParticipant{
		TaskID:          task.ID,
		UserID:          sched.CreatorID,
		UserName:        "Creator",
		Status:          model.ParticipantCompleted,
		ConfirmedAt:     &confirmedAt,
		WorkerStartedAt: &workerStarted,
	}
	if err := db.Create(&participant).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	genAt := now.Add(-20 * time.Minute)
	submittedAt := now.Add(-10 * time.Minute)
	editAt := now.Add(-5 * time.Minute)
	prErr := "old personal failure"
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           sched.CreatorID,
		Content:          "old personal",
		MsgCount:         12,
		TotalTokenUsed:   34,
		ModelVersion:     "v-old",
		WorkerStatus:     model.PersonalStatusFailed,
		ErrorMessage:     &prErr,
		SubmittedAt:      &submittedAt,
		GeneratedAt:      &genAt,
		EditedAt:         &editAt,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}
	if err := db.Model(&participant).Update("personal_result_id", pr.ID).Error; err != nil {
		t.Fatalf("bind personal result: %v", err)
	}
	result := model.SummaryResult{
		TaskID:         task.ID,
		Content:        "old summary",
		TotalMsgCount:  20,
		TotalTokenUsed: 50,
		ModelVersion:   "old-model",
		Version:        3,
		GeneratedAt:    now.Add(-15 * time.Minute),
	}
	if err := db.Create(&result).Error; err != nil {
		t.Fatalf("create summary result: %v", err)
	}
	chunk := model.SummaryChunk{
		TaskID:       task.ID,
		ChunkIndex:   1,
		MsgCount:     5,
		ChunkSummary: "old chunk",
	}
	if err := db.Create(&chunk).Error; err != nil {
		t.Fatalf("create summary chunk: %v", err)
	}

	taskID, claimed, err := claimAndCreateScheduledTask(db, sched, now)
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true")
	}
	if taskID != task.ID {
		t.Fatalf("taskID=%d want %d", taskID, task.ID)
	}

	var taskCount int64
	db.Model(&model.SummaryTask{}).Where("schedule_id = ?", sched.ID).Count(&taskCount)
	if taskCount != 1 {
		t.Fatalf("taskCount=%d want 1", taskCount)
	}

	var sourceCount int64
	db.Model(&model.SummarySource{}).Where("task_id = ?", task.ID).Count(&sourceCount)
	if sourceCount != 1 {
		t.Fatalf("sourceCount=%d want 1", sourceCount)
	}

	var participantCount int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&participantCount)
	if participantCount != 1 {
		t.Fatalf("participantCount=%d want 1", participantCount)
	}

	var prCount int64
	db.Model(&model.PersonalResult{}).Where("task_id = ?", task.ID).Count(&prCount)
	if prCount != 1 {
		t.Fatalf("personalResultCount=%d want 1", prCount)
	}

	var resultCount int64
	db.Model(&model.SummaryResult{}).Where("task_id = ?", task.ID).Count(&resultCount)
	if resultCount != 0 {
		t.Fatalf("summaryResultCount=%d want 0", resultCount)
	}

	var chunkCount int64
	db.Model(&model.SummaryChunk{}).Where("task_id = ?", task.ID).Count(&chunkCount)
	if chunkCount != 0 {
		t.Fatalf("summaryChunkCount=%d want 0", chunkCount)
	}
}

func TestClaimAndCreateScheduledTask_MissingBoundTaskSkipsWithoutCreating(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()
	due := now.Add(-time.Minute)

	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "Daily",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		TimeRangeType: 2,
		IsActive:      1,
		NextRunAt:     &due,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	taskID, claimed, err := claimAndCreateScheduledTask(db, sched, now)
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true")
	}
	if taskID != 0 {
		t.Fatalf("taskID=%d want 0", taskID)
	}

	var taskCount int64
	db.Model(&model.SummaryTask{}).Where("schedule_id = ?", sched.ID).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("taskCount=%d want 0", taskCount)
	}

	var updated model.SummarySchedule
	if err := db.First(&updated, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if updated.LastRunAt == nil {
		t.Fatalf("expected last_run_at updated")
	}
	if updated.NextRunAt == nil || !updated.NextRunAt.After(due) {
		t.Fatalf("expected next_run_at advanced, got %v", updated.NextRunAt)
	}
}
