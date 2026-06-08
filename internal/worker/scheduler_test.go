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
	if resultCount != 1 {
		t.Fatalf("summaryResultCount=%d want 1", resultCount)
	}

	var chunkCount int64
	db.Model(&model.SummaryChunk{}).Where("task_id = ?", task.ID).Count(&chunkCount)
	if chunkCount != 1 {
		t.Fatalf("summaryChunkCount=%d want 1", chunkCount)
	}
}

func TestClaimAndCreateScheduledTask_AppliesScheduleConfigs(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()
	due := now.Add(-time.Minute)

	sourceConfig := model.JSON(`[{"source_type":3,"source_id":"dm-user-2","source_name":"User Two(私聊)"}]`)
	participantConfig := model.JSON(`[{"user_id":"user2","user_name":"User Two"}]`)
	sched := model.SummarySchedule{
		SpaceID:           "space1",
		CreatorID:         "creator1",
		Title:             "Daily",
		SummaryMode:       model.ModeByPerson,
		IntervalDays:      1,
		RunTime:           "09:00",
		TimeRangeType:     2,
		SourceConfig:      sourceConfig,
		ParticipantConfig: participantConfig,
		IsActive:          1,
		NextRunAt:         &due,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	task := model.SummaryTask{
		TaskNo:         "task-config-sync",
		SpaceID:        sched.SpaceID,
		CreatorID:      sched.CreatorID,
		Title:          "Daily",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-48 * time.Hour),
		TimeRangeEnd:   now.Add(-24 * time.Hour),
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerManual,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummarySource{
		TaskID:     task.ID,
		SourceType: model.SourceGroup,
		SourceID:   "group-legacy",
		SourceName: "Legacy Group",
	}).Error; err != nil {
		t.Fatalf("create legacy source: %v", err)
	}
	legacyParticipant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "legacy-user",
		UserName: "Legacy User",
		Status:   model.ParticipantCompleted,
	}
	if err := db.Create(&legacyParticipant).Error; err != nil {
		t.Fatalf("create legacy participant: %v", err)
	}
	if err := db.Create(&model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: legacyParticipant.ID,
		UserID:           legacyParticipant.UserID,
		WorkerStatus:     model.PersonalStatusCompleted,
	}).Error; err != nil {
		t.Fatalf("create legacy personal result: %v", err)
	}

	taskID, claimed, err := claimAndCreateScheduledTask(db, sched, now)
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	if !claimed || taskID != task.ID {
		t.Fatalf("claimed=%v taskID=%d want claimed=true taskID=%d", claimed, taskID, task.ID)
	}

	var sources []model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Order("id ASC").Find(&sources).Error; err != nil {
		t.Fatalf("load sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("source count=%d want 1", len(sources))
	}
	if sources[0].SourceID != "dm-user-2" || sources[0].SourceType != 3 {
		t.Fatalf("unexpected source after sync: %+v", sources[0])
	}

	var participants []model.SummaryParticipant
	if err := db.Where("task_id = ?", task.ID).Order("id ASC").Find(&participants).Error; err != nil {
		t.Fatalf("load participants: %v", err)
	}
	if len(participants) != 2 {
		t.Fatalf("participant count=%d want 2", len(participants))
	}
	if participants[0].UserID != "creator1" {
		t.Fatalf("expected creator first, got %+v", participants[0])
	}
	for _, participant := range participants {
		if participant.Status != model.ParticipantAccepted {
			t.Fatalf("participant status=%d want %d", participant.Status, model.ParticipantAccepted)
		}
		if participant.ConfirmedAt == nil {
			t.Fatalf("participant %s should be auto-confirmed", participant.UserID)
		}
		if participant.PersonalResultID == nil {
			t.Fatalf("participant %s should have personal_result_id", participant.UserID)
		}
	}

	var personalResultCount int64
	if err := db.Model(&model.PersonalResult{}).Where("task_id = ?", task.ID).Count(&personalResultCount).Error; err != nil {
		t.Fatalf("count personal results: %v", err)
	}
	if personalResultCount != 2 {
		t.Fatalf("personalResultCount=%d want 2", personalResultCount)
	}
}

func TestClaimAndCreateScheduledTask_ReloadsLatestScheduleConfigBeforeClaim(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()
	due := now.Add(-2 * time.Hour)

	oldSourceConfig := model.JSON(`[{"source_type":1,"source_id":"group-old","source_name":"Old Group"}]`)
	newSourceConfig := model.JSON(`[{"source_type":3,"source_id":"dm-user-2","source_name":"User Two(私聊)"}]`)
	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "Drift",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		TimeRangeType: 1,
		SourceConfig:  oldSourceConfig,
		IsActive:      1,
		NextRunAt:     &due,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	task := model.SummaryTask{
		TaskNo:         "task-config-drift",
		SpaceID:        sched.SpaceID,
		CreatorID:      sched.CreatorID,
		Title:          "Drift",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-48 * time.Hour),
		TimeRangeEnd:   now.Add(-24 * time.Hour),
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerManual,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummarySource{
		TaskID:     task.ID,
		SourceType: model.SourceGroup,
		SourceID:   "group-old",
		SourceName: "Old Group",
	}).Error; err != nil {
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
	if err := db.Model(&model.SummarySchedule{}).
		Where("id = ?", sched.ID).
		Updates(map[string]interface{}{
			"interval_days":   7,
			"run_time":        "18:45",
			"time_range_type": 3,
			"source_config":   newSourceConfig,
		}).Error; err != nil {
		t.Fatalf("update schedule config before claim: %v", err)
	}

	taskID, claimed, err := claimAndCreateScheduledTask(db, snapshot, now)
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	if !claimed || taskID != task.ID {
		t.Fatalf("claimed=%v taskID=%d want claimed=true taskID=%d", claimed, taskID, task.ID)
	}

	wantNextRun, err := service.NextRunScheduledAdvance("", 7, 0, "18:45", 0, 0, due, now)
	if err != nil {
		t.Fatalf("want next run: %v", err)
	}
	var reloadedSched model.SummarySchedule
	if err := db.First(&reloadedSched, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if reloadedSched.NextRunAt == nil || !reloadedSched.NextRunAt.Equal(wantNextRun) {
		t.Fatalf("next_run_at=%v want %v", reloadedSched.NextRunAt, wantNextRun)
	}

	wantStart, wantEnd := service.ComputeTimeRange(3, now)
	var reloadedTask model.SummaryTask
	if err := db.First(&reloadedTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if !reloadedTask.TimeRangeStart.Equal(wantStart) || !reloadedTask.TimeRangeEnd.Equal(wantEnd) {
		t.Fatalf("time range not refreshed from latest config: got [%v,%v] want [%v,%v]",
			reloadedTask.TimeRangeStart, reloadedTask.TimeRangeEnd, wantStart, wantEnd)
	}

	var sources []model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Order("id ASC").Find(&sources).Error; err != nil {
		t.Fatalf("load sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("source count=%d want 1", len(sources))
	}
	if sources[0].SourceID != "dm-user-2" || sources[0].SourceType != 3 {
		t.Fatalf("expected latest source config applied, got %+v", sources[0])
	}
}

func TestSaveLatestResultAndCompleteTask_ReplacesOldArtifacts(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()

	task := model.SummaryTask{
		TaskNo:         "task-result-swap",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "Daily",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerScheduled,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	oldResult := model.SummaryResult{
		TaskID:         task.ID,
		Content:        "old summary",
		TotalMsgCount:  5,
		TotalTokenUsed: 10,
		ModelVersion:   "old-model",
		Version:        3,
		GeneratedAt:    now.Add(-time.Hour),
	}
	if err := db.Create(&oldResult).Error; err != nil {
		t.Fatalf("create old result: %v", err)
	}
	if err := db.Create(&model.SummaryChunk{
		TaskID:       task.ID,
		ChunkIndex:   1,
		MsgCount:     2,
		ChunkSummary: "old chunk",
	}).Error; err != nil {
		t.Fatalf("create old chunk: %v", err)
	}

	newResult := model.SummaryResult{
		Content:        "new summary",
		TotalMsgCount:  8,
		TotalTokenUsed: 16,
		ModelVersion:   "new-model",
		GeneratedAt:    now,
	}
	if err := saveLatestResultAndCompleteTask(db, task.ID, &newResult, true); err != nil {
		t.Fatalf("saveLatestResultAndCompleteTask: %v", err)
	}

	var results []model.SummaryResult
	if err := db.Where("task_id = ?", task.ID).Order("version DESC").Find(&results).Error; err != nil {
		t.Fatalf("load results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count=%d want 1", len(results))
	}
	if results[0].Content != "new summary" || results[0].Version != 4 {
		t.Fatalf("unexpected latest result: %+v", results[0])
	}

	var chunkCount int64
	if err := db.Model(&model.SummaryChunk{}).Where("task_id = ?", task.ID).Count(&chunkCount).Error; err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount != 0 {
		t.Fatalf("chunkCount=%d want 0", chunkCount)
	}

	var reloadedTask model.SummaryTask
	if err := db.First(&reloadedTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloadedTask.Status != model.StatusCompleted {
		t.Fatalf("task status=%d want %d", reloadedTask.Status, model.StatusCompleted)
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

// TestClaimAndCreateScheduledTask_MultiPersonTaskSkipped verifies the Method A
// guard: a bound task with >1 participant (a team task driven by the human meta
// flow) is NOT reset/overwritten by the scheduler. The claim CAS still advances
// next_run_at (expected re-skip next cycle), but task state, participants and
// existing results must be left untouched so the human meta link is not broken.
func TestClaimAndCreateScheduledTask_MultiPersonTaskSkipped(t *testing.T) {
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
	errMsg := "previous"
	task := model.SummaryTask{
		TaskNo:         "task-multi-1",
		SpaceID:        sched.SpaceID,
		CreatorID:      sched.CreatorID,
		Title:          "Team Summary",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: oldStart,
		TimeRangeEnd:   oldEnd,
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerManual,
		RetryCount:     2,
		ScheduleID:     &sched.ID,
	}
	task.ErrorMessage = &errMsg
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Two participants => team task.
	for i, uid := range []string{"creator1", "user2"} {
		p := model.SummaryParticipant{
			TaskID:      task.ID,
			UserID:      uid,
			UserName:    uid,
			Status:      model.ParticipantCompleted,
			ConfirmedAt: &now,
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("create participant %d: %v", i, err)
		}
		pr := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: p.ID,
			UserID:           uid,
			Content:          "old personal " + uid,
			WorkerStatus:     model.PersonalStatusCompleted,
			GeneratedAt:      &now,
		}
		if err := db.Create(&pr).Error; err != nil {
			t.Fatalf("create personal result %d: %v", i, err)
		}
	}

	result := model.SummaryResult{
		TaskID:       task.ID,
		Content:      "old team summary",
		ModelVersion: "old-model",
		Version:      1,
		GeneratedAt:  now.Add(-time.Hour),
	}
	if err := db.Create(&result).Error; err != nil {
		t.Fatalf("create summary result: %v", err)
	}

	taskID, claimed, err := claimAndCreateScheduledTask(db, sched, now)
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	// claimed=true (CAS advanced next_run_at) but no task overwrite => taskID==0.
	if !claimed {
		t.Fatalf("expected claimed=true (CAS should advance next_run_at)")
	}
	if taskID != 0 {
		t.Fatalf("taskID=%d want 0 (multi-person task must be skipped, not reset)", taskID)
	}

	// Task must be UNCHANGED: still Completed, same time range, retry/error intact.
	var reloaded model.SummaryTask
	if err := db.First(&reloaded, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloaded.Status != model.StatusCompleted {
		t.Fatalf("task status=%d want %d (must not be reset to pending)", reloaded.Status, model.StatusCompleted)
	}
	if !reloaded.TimeRangeStart.Equal(oldStart) || !reloaded.TimeRangeEnd.Equal(oldEnd) {
		t.Fatalf("time range changed: got [%v,%v] want [%v,%v]", reloaded.TimeRangeStart, reloaded.TimeRangeEnd, oldStart, oldEnd)
	}
	if reloaded.RetryCount != 2 {
		t.Fatalf("retry_count=%d want 2 (must be untouched)", reloaded.RetryCount)
	}

	// Personal results must NOT be cleared.
	var pr model.PersonalResult
	if err := db.Where("task_id = ? AND user_id = ?", task.ID, "user2").First(&pr).Error; err != nil {
		t.Fatalf("reload personal result: %v", err)
	}
	if pr.Content != "old personal user2" {
		t.Fatalf("personal result content cleared: %q", pr.Content)
	}

	// Existing summary result must survive.
	var resCount int64
	db.Model(&model.SummaryResult{}).Where("task_id = ?", task.ID).Count(&resCount)
	if resCount != 1 {
		t.Fatalf("summary result count=%d want 1 (must not be deleted)", resCount)
	}

	// next_run_at must still have been advanced by the claim CAS.
	var updated model.SummarySchedule
	if err := db.First(&updated, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if updated.NextRunAt == nil || !updated.NextRunAt.After(due) {
		t.Fatalf("expected next_run_at advanced, got %v", updated.NextRunAt)
	}
}

// ---------------------------------------------------------------------------
// Bug1: schedule-claim CAS and task reset are now in ONE transaction. A
// technical failure during the reset must ROLL BACK the next_run_at advance so
// the schedule stays due and is retried next cycle (no silently dropped cycle).
// ---------------------------------------------------------------------------
func TestClaimAndCreateScheduledTask_TechnicalFailureRollsBackNextRun(t *testing.T) {
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
	task := model.SummaryTask{
		TaskNo:         "task-rollback-1",
		SpaceID:        sched.SpaceID,
		CreatorID:      sched.CreatorID,
		Title:          "Daily",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-48 * time.Hour),
		TimeRangeEnd:   now.Add(-24 * time.Hour),
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerManual,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
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

	// Induce a technical failure INSIDE the reset path: drop personal_results so
	// syncScheduledTaskConfig's delete/insert errors. The claim CAS already ran
	// in the same tx; with the Bug1 fix it must roll back together.
	if err := db.Migrator().DropTable(&model.PersonalResult{}); err != nil {
		t.Fatalf("drop personal_results: %v", err)
	}

	taskID, claimed, err := claimAndCreateScheduledTask(db, sched, now)
	if err == nil {
		t.Fatalf("expected technical error, got nil (taskID=%d claimed=%v)", taskID, claimed)
	}
	if claimed {
		t.Fatalf("claimed should be false on rollback, got true")
	}

	// next_run_at MUST be unchanged (still the original due time) -> still due.
	var reloaded model.SummarySchedule
	if err := db.First(&reloaded, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if reloaded.NextRunAt == nil || !reloaded.NextRunAt.Equal(due) {
		t.Fatalf("next_run_at advanced despite rollback: got %v want %v", reloaded.NextRunAt, due)
	}
	if reloaded.LastRunAt != nil {
		t.Fatalf("last_run_at advanced despite rollback: %v", reloaded.LastRunAt)
	}
}

// Bug1: a BUSINESS skip (overlapping Processing task) must COMMIT the advanced
// next_run_at (not roll back), so we don't re-scan the same row forever.
func TestClaimAndCreateScheduledTask_BusinessSkipCommitsNextRun(t *testing.T) {
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
	// Bound task is STILL processing -> overlap protection skip.
	task := model.SummaryTask{
		TaskNo:         "task-overlap-1",
		SpaceID:        sched.SpaceID,
		CreatorID:      sched.CreatorID,
		Title:          "Daily",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-48 * time.Hour),
		TimeRangeEnd:   now.Add(-24 * time.Hour),
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerScheduled,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	taskID, claimed, err := claimAndCreateScheduledTask(db, sched, now)
	if err != nil {
		t.Fatalf("unexpected error on business skip: %v", err)
	}
	if !claimed {
		t.Fatalf("expected claimed=true for business skip")
	}
	if taskID != 0 {
		t.Fatalf("expected taskID=0 for skip, got %d", taskID)
	}

	// next_run_at MUST have advanced (committed) so the row isn't re-scanned.
	var reloaded model.SummarySchedule
	if err := db.First(&reloaded, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if reloaded.NextRunAt == nil || !reloaded.NextRunAt.After(due) {
		t.Fatalf("next_run_at should advance on business skip: got %v due %v", reloaded.NextRunAt, due)
	}

	// The processing task must NOT have been reset/overwritten.
	var reloadedTask model.SummaryTask
	if err := db.First(&reloadedTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloadedTask.Status != model.StatusProcessing {
		t.Fatalf("overlapping task got overwritten: status=%d", reloadedTask.Status)
	}
}

// ---------------------------------------------------------------------------
// Bug3: saveLatestResultAndCompleteTask version-retention scope.
//
//	isScheduled=false (manual/team) -> KEEP all prior versions.
//	isScheduled=true  (scheduled)   -> prune to the latest version only.
//
// ---------------------------------------------------------------------------
func TestSaveLatestResultAndCompleteTask_NonScheduledKeepsVersions(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()

	task := model.SummaryTask{
		TaskNo:         "task-keep-versions",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "Manual",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerManual,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	oldResult := model.SummaryResult{
		TaskID:       task.ID,
		Content:      "v1 manual",
		ModelVersion: "m1",
		Version:      1,
		GeneratedAt:  now.Add(-time.Hour),
	}
	if err := db.Create(&oldResult).Error; err != nil {
		t.Fatalf("create old result: %v", err)
	}

	newResult := model.SummaryResult{
		Content:      "v2 manual",
		ModelVersion: "m2",
		GeneratedAt:  now,
	}
	// isScheduled=false -> must KEEP the old version.
	if err := saveLatestResultAndCompleteTask(db, task.ID, &newResult, false); err != nil {
		t.Fatalf("saveLatestResultAndCompleteTask: %v", err)
	}

	var results []model.SummaryResult
	if err := db.Where("task_id = ?", task.ID).Order("version ASC").Find(&results).Error; err != nil {
		t.Fatalf("load results: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("non-scheduled result count=%d want 2 (history kept)", len(results))
	}
	// Version monotonicity: new version is strictly greater than the old.
	if results[1].Version <= results[0].Version {
		t.Fatalf("version not monotonic: %d then %d", results[0].Version, results[1].Version)
	}
	if results[1].Content != "v2 manual" {
		t.Fatalf("latest content=%q want v2 manual", results[1].Content)
	}

	var reloadedTask model.SummaryTask
	if err := db.First(&reloadedTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloadedTask.Status != model.StatusCompleted {
		t.Fatalf("task status=%d want completed", reloadedTask.Status)
	}
}

// Scheduled path still prunes to a single latest version (regression guard for
// the in-place overwrite semantics the scheduler relies on).
func TestSaveLatestResultAndCompleteTask_ScheduledPrunesOldVersions(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := timezone.Now()

	task := model.SummaryTask{
		TaskNo:         "task-prune-versions",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		Title:          "Scheduled",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerScheduled,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummaryResult{
		TaskID: task.ID, Content: "old cycle", Version: 1, GeneratedAt: now.Add(-time.Hour),
	}).Error; err != nil {
		t.Fatalf("create old result: %v", err)
	}
	if err := db.Create(&model.SummaryChunk{
		TaskID: task.ID, ChunkIndex: 1, MsgCount: 2, ChunkSummary: "old chunk",
	}).Error; err != nil {
		t.Fatalf("create old chunk: %v", err)
	}

	newResult := model.SummaryResult{Content: "new cycle", GeneratedAt: now}
	if err := saveLatestResultAndCompleteTask(db, task.ID, &newResult, true); err != nil {
		t.Fatalf("saveLatestResultAndCompleteTask: %v", err)
	}

	var results []model.SummaryResult
	if err := db.Where("task_id = ?", task.ID).Find(&results).Error; err != nil {
		t.Fatalf("load results: %v", err)
	}
	if len(results) != 1 || results[0].Content != "new cycle" {
		t.Fatalf("scheduled prune: got %d results %+v want 1 'new cycle'", len(results), results)
	}
	var chunkCount int64
	db.Model(&model.SummaryChunk{}).Where("task_id = ?", task.ID).Count(&chunkCount)
	if chunkCount != 0 {
		t.Fatalf("scheduled chunk cleanup: count=%d want 0", chunkCount)
	}
}
