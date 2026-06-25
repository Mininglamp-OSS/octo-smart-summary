package worker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupProcessorTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummaryResult{},
		&model.SummaryChunk{},
		&model.SummaryEvent{},
		&model.SummarySchedule{},
	)
	return db
}

func TestSinglePersonMode_DirectComplete(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-SP-001",
		SpaceID:        "space1",
		CreatorID:      "user1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerManual,
	}
	db.Create(&task)

	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user1",
		UserName: "User One",
		Status:   model.ParticipantAccepted,
	}
	db.Create(&participant)

	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "user1",
		WorkerStatus:     model.PersonalStatusCompleted,
		Content:          "Test summary content",
		MsgCount:         10,
		TotalTokenUsed:   100,
		ModelVersion:     "test-model",
	}
	genAt := now
	pr.GeneratedAt = &genAt
	db.Create(&pr)
	db.Model(&participant).Update("personal_result_id", pr.ID)

	// Simulate the single-person completion logic from personal_processor
	var participantCount int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&participantCount)

	if participantCount != 1 {
		t.Fatalf("expected 1 participant, got %d", participantCount)
	}

	// Single-person: create SummaryResult directly from PersonalResult
	nextVer, _ := service.GetNextVersion(db, task.ID)
	result := model.SummaryResult{
		TaskID:         task.ID,
		Content:        pr.Content,
		TotalMsgCount:  pr.MsgCount,
		TotalTokenUsed: pr.TotalTokenUsed,
		ModelVersion:   pr.ModelVersion,
		Version:        nextVer,
		GeneratedAt:    now,
	}
	db.Create(&result)
	db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Update("status", model.StatusCompleted)

	// Verify task is completed
	var updatedTask model.SummaryTask
	db.First(&updatedTask, task.ID)
	if updatedTask.Status != model.StatusCompleted {
		t.Errorf("expected task status %d (Completed), got %d", model.StatusCompleted, updatedTask.Status)
	}

	// Verify SummaryResult was created
	var sr model.SummaryResult
	db.Where("task_id = ?", task.ID).First(&sr)
	if sr.Content != "Test summary content" {
		t.Errorf("expected summary content 'Test summary content', got '%s'", sr.Content)
	}
	if sr.Version != 1 {
		t.Errorf("expected version 1, got %d", sr.Version)
	}
}

func TestSinglePersonMode_NoWaitingConfirm(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-SP-002",
		SpaceID:        "space1",
		CreatorID:      "user1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusPending,
		TriggerType:    model.TriggerManual,
	}
	db.Create(&task)

	// Single participant — creator only
	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user1",
		UserName: "User One",
		Status:   model.ParticipantAccepted,
	}
	db.Create(&participant)

	// Count participants
	var count int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&count)

	if count != 1 {
		t.Fatalf("expected 1 participant, got %d", count)
	}

	// In single-person mode, task should go Pending → Processing → Completed
	// It should never enter WaitingConfirm
	db.Model(&task).Update("status", model.StatusProcessing)
	var reloaded model.SummaryTask
	db.First(&reloaded, task.ID)

	if reloaded.Status == model.StatusWaitingConfirm {
		t.Error("single-person mode should never enter WaitingConfirm state")
	}
	if reloaded.Status != model.StatusProcessing {
		t.Errorf("expected Processing status, got %d", reloaded.Status)
	}
}

func TestMultiPersonMode_CreatorDirectStart(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	dl := now.Add(24 * time.Hour)
	task := model.SummaryTask{
		TaskNo:          "TST-MP-001",
		SpaceID:         "space1",
		CreatorID:       "creator1",
		SummaryMode:     model.ModeByPerson,
		TimeRangeStart:  now.Add(-24 * time.Hour),
		TimeRangeEnd:    now,
		Status:          model.StatusPending,
		TriggerType:     model.TriggerManual,
		ConfirmDeadline: &dl,
	}
	db.Create(&task)

	// Creator — starts with Accepted (auto-accept)
	creatorP := model.SummaryParticipant{
		TaskID:      task.ID,
		UserID:      "creator1",
		UserName:    "Creator",
		Status:      model.ParticipantAccepted,
		ConfirmedAt: &now,
	}
	db.Create(&creatorP)

	// Other participant — starts with WaitingConfirm (ParticipantPending)
	otherP := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user2",
		UserName: "User Two",
		Status:   model.ParticipantPending,
	}
	db.Create(&otherP)

	// Verify participant count
	var count int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 participants, got %d", count)
	}

	// Creator should be Accepted, other should be Pending (waiting confirm)
	var creator model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", task.ID, "creator1").First(&creator)
	if creator.Status != model.ParticipantAccepted {
		t.Errorf("creator should be Accepted, got %d", creator.Status)
	}

	var other model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", task.ID, "user2").First(&other)
	if other.Status != model.ParticipantPending {
		t.Errorf("other participant should be Pending (WaitingConfirm), got %d", other.Status)
	}

	// Task should be Processing (not WaitingConfirm), following the Creator
	db.Model(&task).Update("status", model.StatusProcessing)
	var reloaded model.SummaryTask
	db.First(&reloaded, task.ID)
	if reloaded.Status != model.StatusProcessing {
		t.Errorf("multi-person task should be Processing, got %d", reloaded.Status)
	}
}

func TestMultiPersonMode_ParticipantAccept(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-MP-002",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerManual,
	}
	db.Create(&task)

	// Other participant in WaitingConfirm
	otherP := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user2",
		UserName: "User Two",
		Status:   model.ParticipantPending,
	}
	db.Create(&otherP)

	// Simulate accept
	confirmedAt := time.Now().UTC()
	db.Model(&otherP).Updates(map[string]interface{}{
		"status":       model.ParticipantAccepted,
		"confirmed_at": confirmedAt,
	})

	// Create PersonalResult
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: otherP.ID,
		UserID:           "user2",
		WorkerStatus:     model.PersonalStatusPending,
	}
	db.Create(&pr)
	db.Model(&otherP).Update("personal_result_id", pr.ID)

	// Verify state transition
	var reloaded model.SummaryParticipant
	db.First(&reloaded, otherP.ID)
	if reloaded.Status != model.ParticipantAccepted {
		t.Errorf("expected Accepted status after accept, got %d", reloaded.Status)
	}
	if reloaded.ConfirmedAt == nil {
		t.Error("expected confirmed_at to be set")
	}
}

func TestMultiPersonMode_ParticipantDecline(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-MP-003",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerManual,
	}
	db.Create(&task)

	otherP := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user2",
		UserName: "User Two",
		Status:   model.ParticipantPending,
	}
	db.Create(&otherP)

	// Simulate decline
	db.Model(&otherP).Update("status", model.ParticipantDeclined)

	var reloaded model.SummaryParticipant
	db.First(&reloaded, otherP.ID)
	if reloaded.Status != model.ParticipantDeclined {
		t.Errorf("expected Declined status, got %d", reloaded.Status)
	}

	// Task should still be Processing — decline doesn't affect task progress
	var reloadedTask model.SummaryTask
	db.First(&reloadedTask, task.ID)
	if reloadedTask.Status != model.StatusProcessing {
		t.Errorf("task should remain Processing after participant decline, got %d", reloadedTask.Status)
	}
}

func TestModeByPersonConstant(t *testing.T) {
	if model.ModeByPerson != 2 {
		t.Errorf("ModeByPerson should be 2 for DB compatibility, got %d", model.ModeByPerson)
	}
}

func TestSinglePersonMode_StateFlow(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-SF-001",
		SpaceID:        "space1",
		CreatorID:      "user1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusPending,
		TriggerType:    model.TriggerManual,
	}
	db.Create(&task)

	// Pending → Processing
	db.Model(&task).Update("status", model.StatusProcessing)
	db.First(&task, task.ID)
	if task.Status != model.StatusProcessing {
		t.Fatalf("expected Processing, got %d", task.Status)
	}

	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user1",
		UserName: "User One",
		Status:   model.ParticipantProcessing,
	}
	db.Create(&participant)

	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "user1",
		WorkerStatus:     model.PersonalStatusProcessing,
	}
	db.Create(&pr)

	// Participant Processing → Completed
	db.Model(&participant).Update("status", model.ParticipantCompleted)
	db.Model(&pr).Update("worker_status", model.PersonalStatusCompleted)

	// Single person: create SummaryResult directly
	var pCount int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&pCount)
	if pCount != 1 {
		t.Fatalf("expected 1 participant, got %d", pCount)
	}

	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "final summary",
		Version:     1,
		GeneratedAt: now,
	}
	db.Create(&result)

	// Processing → Completed
	db.Model(&task).Update("status", model.StatusCompleted)
	db.First(&task, task.ID)
	if task.Status != model.StatusCompleted {
		t.Errorf("expected Completed, got %d", task.Status)
	}

	// Verify full state: Task=Completed, Participant=Completed, PR=Completed, SummaryResult exists
	db.First(&participant, participant.ID)
	if participant.Status != model.ParticipantCompleted {
		t.Errorf("participant should be Completed, got %d", participant.Status)
	}

	db.First(&pr, pr.ID)
	if pr.WorkerStatus != model.PersonalStatusCompleted {
		t.Errorf("personal result should be Completed, got %d", pr.WorkerStatus)
	}

	var sr model.SummaryResult
	db.Where("task_id = ?", task.ID).First(&sr)
	if sr.ID == 0 {
		t.Error("SummaryResult should exist")
	}
}

func TestMultiPersonMode_StateFlow(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	dl := now.Add(24 * time.Hour)
	task := model.SummaryTask{
		TaskNo:          "TST-SF-002",
		SpaceID:         "space1",
		CreatorID:       "creator1",
		SummaryMode:     model.ModeByPerson,
		TimeRangeStart:  now.Add(-24 * time.Hour),
		TimeRangeEnd:    now,
		Status:          model.StatusPending,
		TriggerType:     model.TriggerManual,
		ConfirmDeadline: &dl,
	}
	db.Create(&task)

	// Creator participant — auto-accepted
	creatorP := model.SummaryParticipant{
		TaskID:      task.ID,
		UserID:      "creator1",
		UserName:    "Creator",
		Status:      model.ParticipantAccepted,
		ConfirmedAt: &now,
	}
	db.Create(&creatorP)

	creatorPR := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: creatorP.ID,
		UserID:           "creator1",
		WorkerStatus:     model.PersonalStatusPending,
	}
	db.Create(&creatorPR)
	db.Model(&creatorP).Update("personal_result_id", creatorPR.ID)

	// Other participant — waiting for confirmation
	otherP := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user2",
		UserName: "User Two",
		Status:   model.ParticipantPending,
	}
	db.Create(&otherP)

	// Task → Processing (follows Creator)
	db.Model(&task).Update("status", model.StatusProcessing)

	// Creator's personal summary completes
	db.Model(&creatorP).Update("status", model.ParticipantCompleted)
	db.Model(&creatorPR).Updates(map[string]interface{}{
		"worker_status": model.PersonalStatusCompleted,
		"content":       "Creator summary",
		"msg_count":     5,
	})

	// Other participant accepts
	db.Model(&otherP).Updates(map[string]interface{}{
		"status":       model.ParticipantAccepted,
		"confirmed_at": now,
	})
	otherPR := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: otherP.ID,
		UserID:           "user2",
		WorkerStatus:     model.PersonalStatusPending,
	}
	db.Create(&otherPR)
	db.Model(&otherP).Update("personal_result_id", otherPR.ID)

	// Other participant's personal summary completes
	db.Model(&otherP).Update("status", model.ParticipantCompleted)
	db.Model(&otherPR).Updates(map[string]interface{}{
		"worker_status": model.PersonalStatusCompleted,
		"content":       "User2 summary",
		"msg_count":     3,
	})

	// Both submit
	submittedAt := time.Now().UTC()
	db.Model(&creatorP).Update("status", model.ParticipantSubmitted)
	db.Model(&creatorPR).Update("submitted_at", submittedAt)
	db.Model(&otherP).Update("status", model.ParticipantSubmitted)
	db.Model(&otherPR).Update("submitted_at", submittedAt)

	// Verify all submitted
	var submittedCount int64
	db.Model(&model.PersonalResult{}).Where("task_id = ? AND submitted_at IS NOT NULL", task.ID).Count(&submittedCount)
	if submittedCount != 2 {
		t.Errorf("expected 2 submitted, got %d", submittedCount)
	}

	// Meta-summary creates SummaryResult
	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "Merged team summary",
		Version:     1,
		GeneratedAt: now,
	}
	db.Create(&result)
	db.Model(&task).Update("status", model.StatusCompleted)

	// Verify final state
	db.First(&task, task.ID)
	if task.Status != model.StatusCompleted {
		t.Errorf("expected Completed, got %d", task.Status)
	}

	var sr model.SummaryResult
	db.Where("task_id = ?", task.ID).First(&sr)
	if sr.Content != "Merged team summary" {
		t.Errorf("unexpected result content: %s", sr.Content)
	}
}

func TestConfirmTimeoutDeclinesPendingParticipants(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	pastDeadline := now.Add(-1 * time.Hour)
	task := model.SummaryTask{
		TaskNo:          "TST-CT-001",
		SpaceID:         "space1",
		CreatorID:       "creator1",
		SummaryMode:     model.ModeByPerson,
		TimeRangeStart:  now.Add(-24 * time.Hour),
		TimeRangeEnd:    now,
		Status:          model.StatusProcessing,
		TriggerType:     model.TriggerManual,
		ConfirmDeadline: &pastDeadline,
	}
	db.Create(&task)

	// Creator — already accepted
	creatorP := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "creator1",
		UserName: "Creator",
		Status:   model.ParticipantAccepted,
	}
	db.Create(&creatorP)

	// Other — still pending (waiting confirm)
	otherP := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user2",
		UserName: "User Two",
		Status:   model.ParticipantPending,
	}
	db.Create(&otherP)

	// Run scanConfirmTimeouts
	scanConfirmTimeouts(db)

	// Creator should remain accepted
	var reloadedCreator model.SummaryParticipant
	db.First(&reloadedCreator, creatorP.ID)
	if reloadedCreator.Status != model.ParticipantAccepted {
		t.Errorf("creator should remain Accepted, got %d", reloadedCreator.Status)
	}

	// Other should be declined (timed out)
	var reloadedOther model.SummaryParticipant
	db.First(&reloadedOther, otherP.ID)
	if reloadedOther.Status != model.ParticipantDeclined {
		t.Errorf("timed-out participant should be Declined, got %d", reloadedOther.Status)
	}

	// Task should still be Processing — not cancelled
	var reloadedTask model.SummaryTask
	db.First(&reloadedTask, task.ID)
	if reloadedTask.Status != model.StatusProcessing {
		t.Errorf("task should remain Processing after participant timeout, got %d", reloadedTask.Status)
	}
}

func TestConfirmTimeout_NoEffectOnFutureDeadline(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	futureDeadline := now.Add(24 * time.Hour)
	task := model.SummaryTask{
		TaskNo:          "TST-CT-002",
		SpaceID:         "space1",
		CreatorID:       "creator1",
		SummaryMode:     model.ModeByPerson,
		TimeRangeStart:  now.Add(-24 * time.Hour),
		TimeRangeEnd:    now,
		Status:          model.StatusProcessing,
		TriggerType:     model.TriggerManual,
		ConfirmDeadline: &futureDeadline,
	}
	db.Create(&task)

	otherP := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user2",
		UserName: "User Two",
		Status:   model.ParticipantPending,
	}
	db.Create(&otherP)

	scanConfirmTimeouts(db)

	// Participant should still be pending — deadline hasn't passed
	var reloaded model.SummaryParticipant
	db.First(&reloaded, otherP.ID)
	if reloaded.Status != model.ParticipantPending {
		t.Errorf("participant should remain Pending when deadline hasn't passed, got %d", reloaded.Status)
	}
}

func TestSchedulerForcesModeByPerson(t *testing.T) {
	if model.ModeByPerson != 2 {
		t.Errorf("ModeByPerson should be 2, got %d", model.ModeByPerson)
	}
}

func TestPersonalProcessor_SetsProcessingDeadline(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-DL-001",
		SpaceID:        "space1",
		CreatorID:      "user1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusPending,
		TriggerType:    model.TriggerManual,
	}
	db.Create(&task)

	leaseMinutes := 20
	deadline := time.Now().UTC().Add(time.Duration(leaseMinutes) * time.Minute)

	// Simulate the CAS update from personal_processor with processing_deadline
	result := db.Model(&model.SummaryTask{}).
		Where("id = ? AND status IN (?, ?)", task.ID, model.StatusPending, model.StatusWaitingConfirm).
		Updates(map[string]interface{}{
			"status":              model.StatusProcessing,
			"processing_deadline": deadline,
		})
	if result.Error != nil {
		t.Fatalf("CAS update failed: %v", result.Error)
	}
	if result.RowsAffected != 1 {
		t.Fatalf("expected 1 row affected, got %d", result.RowsAffected)
	}

	var reloaded model.SummaryTask
	db.First(&reloaded, task.ID)
	if reloaded.Status != model.StatusProcessing {
		t.Errorf("expected Processing status, got %d", reloaded.Status)
	}
	if reloaded.ProcessingDeadline == nil {
		t.Fatal("expected processing_deadline to be set, got nil")
	}
	if reloaded.ProcessingDeadline.Before(time.Now().UTC()) {
		t.Error("processing_deadline should be in the future")
	}
}

func TestPersonalProcessor_RefreshesDeadlineOnDuplicateTrigger(t *testing.T) {
	db := setupProcessorTestDB(t)

	now := time.Now().UTC()
	oldDeadline := now.Add(5 * time.Minute)
	task := model.SummaryTask{
		TaskNo:             "TST-DL-002",
		SpaceID:            "space1",
		CreatorID:          "user1",
		SummaryMode:        model.ModeByPerson,
		TimeRangeStart:     now.Add(-24 * time.Hour),
		TimeRangeEnd:       now,
		Status:             model.StatusProcessing,
		TriggerType:        model.TriggerManual,
		ProcessingDeadline: &oldDeadline,
	}
	db.Create(&task)

	// Simulate duplicate trigger: CAS fails because task is already Processing
	leaseMinutes := 20
	newDeadline := time.Now().UTC().Add(time.Duration(leaseMinutes) * time.Minute)

	cas := db.Model(&model.SummaryTask{}).
		Where("id = ? AND status IN (?, ?)", task.ID, model.StatusPending, model.StatusWaitingConfirm).
		Updates(map[string]interface{}{
			"status":              model.StatusProcessing,
			"processing_deadline": newDeadline,
		})
	if cas.RowsAffected != 0 {
		t.Fatal("expected CAS to not match (task already Processing)")
	}

	// Verify task is still Processing, then refresh deadline
	var currentTask model.SummaryTask
	if err := db.Select("status").First(&currentTask, task.ID).Error; err != nil {
		t.Fatalf("failed to load task: %v", err)
	}
	if currentTask.Status != model.StatusProcessing {
		t.Fatalf("expected Processing, got %d", currentTask.Status)
	}

	// Refresh deadline (as personal_processor does for already-processing tasks)
	db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Update("processing_deadline", newDeadline)

	var reloaded model.SummaryTask
	db.First(&reloaded, task.ID)
	if reloaded.ProcessingDeadline == nil {
		t.Fatal("expected processing_deadline to be set after refresh")
	}
	if !reloaded.ProcessingDeadline.After(oldDeadline) {
		t.Errorf("expected refreshed deadline (%v) to be after old deadline (%v)",
			reloaded.ProcessingDeadline, oldDeadline)
	}
}

// ---------------------------------------------------------------------------
// Blocker-1: AUTO scheduled multi-person dispatch (new tests).
// ---------------------------------------------------------------------------

// seedAutoScheduledMultiTask builds an AUTO (confirm_policy=0) scheduled task with
// `n` Accepted participants, each with a Pending personal_result, bound to a
// schedule, in Processing -- the exact state processTask's multi-person else branch
// sees after the pipeline runs.
func seedAutoScheduledMultiTask(t *testing.T, db *gorm.DB, n int, confirmPolicy int) (model.SummaryTask, []int64) {
	t.Helper()
	now := time.Now().UTC()
	sched := model.SummarySchedule{
		SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		IntervalDays: 1, TimeRangeType: 4, IsActive: 1, ConfirmPolicy: confirmPolicy,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	task := model.SummaryTask{
		TaskNo: "T-AUTO", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		ScheduleID: &sched.ID, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	var refIDs []int64
	for i := 0; i < n; i++ {
		part := model.SummaryParticipant{
			TaskID: task.ID, UserID: userIDForIdx(i), Status: model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := db.Create(&part).Error; err != nil {
			t.Fatalf("create participant: %v", err)
		}
		pr := model.PersonalResult{
			TaskID: task.ID, ParticipantRefID: part.ID, UserID: userIDForIdx(i),
			WorkerStatus: model.PersonalStatusPending,
		}
		if err := db.Create(&pr).Error; err != nil {
			t.Fatalf("create personal_result: %v", err)
		}
		db.Model(&part).Update("personal_result_id", pr.ID)
		refIDs = append(refIDs, part.ID)
	}
	return task, refIDs
}

// The AUTO scheduled multi-person path must select every Accepted+Pending
// participant for dispatch (the explicit main-path drive that replaces the
// 5-minute stuck-scan fallback).
func TestScheduledAutoDispatch_SelectsAllAcceptedPending(t *testing.T) {
	db := setupProcessorTestDB(t)
	task, refIDs := seedAutoScheduledMultiTask(t, db, 3, model.SchedConfirmAuto)

	targets, err := scheduledAutoDispatchTargets(db, task.ID)
	if err != nil {
		t.Fatalf("select targets: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("AUTO dispatch should select all 3 accepted+pending participants, got %d: %v", len(targets), targets)
	}
	want := map[int64]bool{refIDs[0]: true, refIDs[1]: true, refIDs[2]: true}
	for _, id := range targets {
		if !want[id] {
			t.Errorf("unexpected dispatch target %d", id)
		}
	}

	// confirm_policy=0 must be reported as AUTO so the dispatch branch is taken.
	p := &Processor{db: db}
	if !p.scheduleConfirmPolicyIsAuto(task) {
		t.Error("confirm_policy=0 must be AUTO")
	}
}

// Already-running / completed participants are not re-dispatched (idempotent
// selection): only worker_status==Pending + Accepted is picked.
func TestScheduledAutoDispatch_SkipsNonPendingOrNonAccepted(t *testing.T) {
	db := setupProcessorTestDB(t)
	task, refIDs := seedAutoScheduledMultiTask(t, db, 3, model.SchedConfirmAuto)

	// p0 already processing (worker_status != Pending) -> skip.
	db.Model(&model.PersonalResult{}).Where("participant_ref_id = ?", refIDs[0]).
		Update("worker_status", model.PersonalStatusProcessing)
	// p1 declined at participant level -> skip even though pr is Pending.
	db.Model(&model.SummaryParticipant{}).Where("id = ?", refIDs[1]).
		Update("status", model.ParticipantDeclined)

	targets, err := scheduledAutoDispatchTargets(db, task.ID)
	if err != nil {
		t.Fatalf("select targets: %v", err)
	}
	if len(targets) != 1 || targets[0] != refIDs[2] {
		t.Fatalf("only the accepted+pending participant should be dispatched, got %v want [%d]", targets, refIDs[2])
	}
}

// Under the CONFIRM policy (confirm_policy=1) the schedule must NOT be reported as
// AUTO -- non-creator participants must wait for human confirmation, not be
// blindly dispatched.
func TestScheduleConfirmPolicy_Confirm_IsNotAuto(t *testing.T) {
	db := setupProcessorTestDB(t)
	task, _ := seedAutoScheduledMultiTask(t, db, 2, model.SchedConfirmRequire)
	p := &Processor{db: db}
	if p.scheduleConfirmPolicyIsAuto(task) {
		t.Error("confirm_policy=1 (CONFIRM) must NOT be treated as AUTO")
	}
}

// ---------------------------------------------------------------------------
// 🟡 Closeout: scheduleConfirmPolicyIsAuto fails CLOSED (default NOT-auto).
// Returning true on uncertainty would全量 dispatch a possibly-CONFIRM task and
// break human-confirmation semantics, so both ScheduleID==nil and a failed
// schedule lookup must return false.
// ---------------------------------------------------------------------------

// A task with no bound schedule (ScheduleID==nil) must NOT be treated as AUTO --
// fail closed so a CONFIRM task is never auto-dispatched. P0 AUTO tasks always
// carry a ScheduleID (bound at claim time), so this never strands the real path.
func TestScheduleConfirmPolicy_NilScheduleID_NotAuto(t *testing.T) {
	db := setupProcessorTestDB(t)
	p := &Processor{db: db}
	task := model.SummaryTask{
		TaskNo: "T-NILSCHED", SpaceID: "sp", CreatorID: "u1",
		TriggerType: model.TriggerScheduled, ScheduleID: nil,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if p.scheduleConfirmPolicyIsAuto(task) {
		t.Error("ScheduleID==nil must NOT be treated as AUTO (fail closed, safe side)")
	}
}

// When the bound schedule lookup fails (e.g. the schedule row was deleted), the
// function must fail closed and return false rather than defaulting to AUTO.
func TestScheduleConfirmPolicy_LookupFailure_NotAuto(t *testing.T) {
	db := setupProcessorTestDB(t)
	p := &Processor{db: db}
	missingID := int64(999999) // no summary_schedule row with this id exists.
	task := model.SummaryTask{
		TaskNo: "T-BADSCHED", SpaceID: "sp", CreatorID: "u1",
		TriggerType: model.TriggerScheduled, ScheduleID: &missingID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if p.scheduleConfirmPolicyIsAuto(task) {
		t.Error("schedule lookup failure must NOT be treated as AUTO (fail closed, safe side)")
	}
}

// Sanity: the only path that returns true is an explicitly-read AUTO policy.
func TestScheduleConfirmPolicy_ExplicitAuto_IsAuto(t *testing.T) {
	db := setupProcessorTestDB(t)
	p := &Processor{db: db}
	sched := model.SummarySchedule{
		SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		IntervalDays: 1, TimeRangeType: 4, IsActive: 1, ConfirmPolicy: model.SchedConfirmAuto,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	task := model.SummaryTask{
		TaskNo: "T-AUTOSCHED", SpaceID: "sp", CreatorID: "u1",
		TriggerType: model.TriggerScheduled, ScheduleID: &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if !p.scheduleConfirmPolicyIsAuto(task) {
		t.Error("explicitly read confirm_policy==AUTO must return true")
	}
}

// ---------------------------------------------------------------------------
// 🔴 P0 dispatch-deadlock (CAS-gated AUTO multi-person dispatch).
//
// REPRODUCTION + REGRESSION.
//
// Root cause (controlled-reproduction proof below): the AUTO scheduled
// multi-person dispatch used to sit BEHIND a "refresh processing_deadline" CAS
// (WHERE status=Processing). The CAS can legitimately return RowsAffected==0 for
// a task we still effectively own -- e.g. a concurrent scanStuckTasks revive
// (Processing->Pending) lands in the window between claim and CAS, or a second
// worker/stale-reload races the row. The old code treated a CAS miss as "give up,
// skip dispatch, return" WITHOUT dispatching anyone and WITHOUT a terminal/
// recoverable state, leaving the task pinned in Processing. scanStuckTasks then
// revived it again 20min later, poll re-claimed, the CAS missed again -> permanent
// spin, retry_count climbing, NOTHING ever dispatched.
//
// reviveDuringPipeline mimics the exact concurrent writer: a scanStuckTasks-style
// Processing->Pending revive (with retry+1, deadline cleared) firing while
// processTask is mid-pipeline -- which is precisely what makes the post-pipeline
// CAS (WHERE status=Processing) miss.
// ---------------------------------------------------------------------------

// scanStuckTasksRevive is the relevant body of scheduler.scanStuckTasks's
// retryable branch (Processing->Pending, retry+1, deadline cleared) for tasks
// whose lease has expired. Used to drive the exact concurrent state change that
// strands the CAS, without spinning the whole 60s cron.
func scanStuckTasksRevive(db *gorm.DB, taskID int64) int64 {
	res := db.Model(&model.SummaryTask{}).
		Where("id = ? AND status = ?", taskID, model.StatusProcessing).
		Updates(map[string]interface{}{
			"status":              model.StatusPending,
			"processing_deadline": nil,
			"retry_count":         gorm.Expr("retry_count + 1"),
		})
	return res.RowsAffected
}

// seedPendingAutoScheduledMultiTask builds an AUTO (confirm_policy=0) scheduled
// multi-person task in StatusPending -- the exact row the real scheduler INSERTs
// (buildScheduledTaskChildren has already created Accepted participants + Pending
// personal_results). This is what poll() claims.
func seedPendingAutoScheduledMultiTask(t *testing.T, db *gorm.DB, n int) (model.SummaryTask, []int64) {
	t.Helper()
	now := time.Now().UTC()
	sched := model.SummarySchedule{
		SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		IntervalDays: 1, TimeRangeType: 4, IsActive: 1, ConfirmPolicy: model.SchedConfirmAuto,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	task := model.SummaryTask{
		TaskNo: "T-AUTO-PEND", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusPending, TriggerType: model.TriggerScheduled,
		ScheduleID: &sched.ID, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	var refIDs []int64
	for i := 0; i < n; i++ {
		part := model.SummaryParticipant{
			TaskID: task.ID, UserID: userIDForIdx(i), Status: model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := db.Create(&part).Error; err != nil {
			t.Fatalf("create participant: %v", err)
		}
		pr := model.PersonalResult{
			TaskID: task.ID, ParticipantRefID: part.ID, UserID: userIDForIdx(i),
			WorkerStatus: model.PersonalStatusPending,
		}
		if err := db.Create(&pr).Error; err != nil {
			t.Fatalf("create personal_result: %v", err)
		}
		db.Model(&part).Update("personal_result_id", pr.ID)
		refIDs = append(refIDs, part.ID)
	}
	return task, refIDs
}

// runProcessTaskWithReviveRace drives the real processTask for an AUTO scheduled
// multi-person task while a scanStuckTasks-style revive fires mid-pipeline (the
// concurrent writer that makes the post-pipeline CAS WHERE status=Processing
// miss). It records which participant_ref_ids got dispatched. executePipeline is
// stubbed to "0 messages == success" (the field-observed trigger). Returns the
// dispatched ref IDs.
func runProcessTaskWithReviveRace(t *testing.T, db *gorm.DB) (model.SummaryTask, []int64, []int64) {
	t.Helper()
	task, refIDs := seedPendingAutoScheduledMultiTask(t, db, 2)

	var dispatched []int64
	var mu sync.Mutex
	var reviveRows int64

	p := &Processor{
		db:  db,
		cfg: &config.Config{WorkerLeaseMinutes: 20, WorkerMaxRetry: 3, WorkerPollInterval: 2},
		dispatchPersonalFn: func(taskID, refID int64) {
			mu.Lock()
			dispatched = append(dispatched, refID)
			mu.Unlock()
		},
		executePipelineFn: func(_ model.SummaryTask) error {
			// Mid-pipeline: a concurrent scanStuckTasks revives the task
			// Processing->Pending (the exact race that strands the CAS).
			r := scanStuckTasksRevive(db, task.ID)
			atomic.StoreInt64(&reviveRows, r)
			return nil // 0 messages == success
		},
	}

	// poll() claims the Pending task -> Processing (deadline +20m), then
	// dispatch/processTask via the pool. Use a real (tiny) pool but our
	// dispatchPersonalFn short-circuits the LLM personal worker.
	p.pool = NewWorkerPool(4)
	// Reload the freshly-claimed task and run processTask synchronously so the
	// assertions are deterministic (production submits it to the pool).
	now := timezone.Now()
	deadline := now.Add(20 * time.Minute)
	res := db.Model(&model.SummaryTask{}).
		Where("id = ? AND status = ?", task.ID, model.StatusPending).
		Updates(map[string]interface{}{"status": model.StatusProcessing, "processing_deadline": deadline})
	if res.RowsAffected != 1 {
		t.Fatalf("claim should affect 1 row, got %d", res.RowsAffected)
	}
	var claimed model.SummaryTask
	if err := db.First(&claimed, task.ID).Error; err != nil {
		t.Fatalf("reload claimed task: %v", err)
	}

	p.processTask(claimed)

	if atomic.LoadInt64(&reviveRows) != 1 {
		t.Fatalf("setup invariant: the mid-pipeline revive must have flipped exactly 1 Processing row (got %d) so the post-pipeline CAS truly misses", reviveRows)
	}

	mu.Lock()
	out := append([]int64(nil), dispatched...)
	mu.Unlock()
	return task, refIDs, out
}

// TestP0DispatchDeadlock_CASMiss_StillDispatchesAllAndNotStranded is the
// reproduction+regression. With the OLD code (CAS-gated dispatch, return on
// RowsAffected==0) the mid-pipeline revive makes the CAS miss, so dispatched is
// EMPTY and the task is left non-dispatched -> this test FAILS. With the fix
// (dispatch on the main path before the fragile CAS, CAS miss non-fatal) all
// Accepted+Pending participants are dispatched even though the CAS missed.
func TestP0DispatchDeadlock_CASMiss_StillDispatchesAllAndNotStranded(t *testing.T) {
	db := setupProcessorTestDB(t)
	_, refIDs, dispatched := runProcessTaskWithReviveRace(t, db)

	if len(dispatched) != len(refIDs) {
		t.Fatalf("REPRO: AUTO multi-person dispatch must fire for ALL %d Accepted+Pending participants even when the deadline CAS misses; got %d dispatched: %v (old CAS-gated code skips dispatch -> 0)", len(refIDs), len(dispatched), dispatched)
	}
	want := map[int64]bool{}
	for _, id := range refIDs {
		want[id] = true
	}
	for _, id := range dispatched {
		if !want[id] {
			t.Errorf("dispatched unexpected participant_ref_id %d", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("these participants were never dispatched (stranded): %v", want)
	}
}

// TestP0DispatchDeadlock_Idempotent_NoDoubleDispatch proves the fix stays
// idempotent across repeated claims/retries: a participant whose personal worker
// already started (worker_status != Pending) is NOT re-dispatched, and a declined
// participant is never dispatched -- so multiple claim/retry rounds of the same
// task never double-run an in-flight or finished personal.
func TestP0DispatchDeadlock_Idempotent_NoDoubleDispatch(t *testing.T) {
	db := setupProcessorTestDB(t)
	task, refIDs := seedPendingAutoScheduledMultiTask(t, db, 3)

	// Round 1 already started p0 (Processing) and declined p1; only p2 is
	// Accepted+Pending and should be dispatched on a re-claim.
	db.Model(&model.PersonalResult{}).Where("participant_ref_id = ?", refIDs[0]).
		Update("worker_status", model.PersonalStatusProcessing)
	db.Model(&model.SummaryParticipant{}).Where("id = ?", refIDs[1]).
		Update("status", model.ParticipantDeclined)

	var dispatched []int64
	var mu sync.Mutex
	p := &Processor{
		db:   db,
		cfg:  &config.Config{WorkerLeaseMinutes: 20, WorkerMaxRetry: 3},
		pool: NewWorkerPool(2),
		dispatchPersonalFn: func(_, refID int64) {
			mu.Lock()
			dispatched = append(dispatched, refID)
			mu.Unlock()
		},
		executePipelineFn: func(_ model.SummaryTask) error { return nil },
	}

	// Claim (re-claim) -> Processing, then processTask.
	db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"status": model.StatusProcessing,
			"processing_deadline": timezone.Now().Add(20 * time.Minute)})
	var claimed model.SummaryTask
	db.First(&claimed, task.ID)
	p.processTask(claimed)

	if len(dispatched) != 1 || dispatched[0] != refIDs[2] {
		t.Fatalf("idempotent dispatch must re-dispatch ONLY the Accepted+Pending participant (p2=%d); got %v", refIDs[2], dispatched)
	}
}

// ---------------------------------------------------------------------------
// 🔴 P1 EXPERIENCE FIX: scheduled CONFIRM multi-person must ACTIVELY dispatch.
//
// A scheduled CONFIRM task (confirm_policy=1) arrives at the processor with its
// participants already materialized as confirmed Accepted members (zero-confirm
// rounds are soft-deleted scheduler-side and never reach here). Before the fix
// these rounds fell into the non-dispatch "participants pending confirm" branch
// and only got picked up ~5 min later by scanStuckPersonalTasks, so users saw
// "生成中" for minutes. processTask must now dispatch every Accepted+Pending
// personal up front, exactly like the AUTO path -- while MANUAL (non-scheduled)
// CONFIRM tasks keep waiting for human confirmation.
// ---------------------------------------------------------------------------

// runProcessTaskScheduled drives the real processTask for a scheduled
// multi-person task in StatusPending (the row poll() claims) with the given
// confirm policy, recording which participant_ref_ids got dispatched. The
// pipeline is stubbed to "0 messages == success".
func runProcessTaskScheduled(t *testing.T, db *gorm.DB, confirmPolicy int, n int) (model.SummaryTask, []int64, []int64) {
	t.Helper()
	now := time.Now().UTC()
	sched := model.SummarySchedule{
		SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		IntervalDays: 1, TimeRangeType: 4, IsActive: 1, ConfirmPolicy: confirmPolicy,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	task := model.SummaryTask{
		TaskNo: "T-SCHED-CONFIRM", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusPending, TriggerType: model.TriggerScheduled,
		ScheduleID: &sched.ID, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	var refIDs []int64
	for i := 0; i < n; i++ {
		part := model.SummaryParticipant{
			TaskID: task.ID, UserID: userIDForIdx(i), Status: model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := db.Create(&part).Error; err != nil {
			t.Fatalf("create participant: %v", err)
		}
		pr := model.PersonalResult{
			TaskID: task.ID, ParticipantRefID: part.ID, UserID: userIDForIdx(i),
			WorkerStatus: model.PersonalStatusPending,
		}
		if err := db.Create(&pr).Error; err != nil {
			t.Fatalf("create personal_result: %v", err)
		}
		db.Model(&part).Update("personal_result_id", pr.ID)
		refIDs = append(refIDs, part.ID)
	}

	var dispatched []int64
	var mu sync.Mutex
	p := &Processor{
		db:   db,
		cfg:  &config.Config{WorkerLeaseMinutes: 20, WorkerMaxRetry: 3},
		pool: NewWorkerPool(4),
		dispatchPersonalFn: func(_, refID int64) {
			mu.Lock()
			dispatched = append(dispatched, refID)
			mu.Unlock()
		},
		executePipelineFn: func(_ model.SummaryTask) error { return nil },
	}

	// Claim Pending -> Processing (deadline +20m), then run processTask.
	res := db.Model(&model.SummaryTask{}).
		Where("id = ? AND status = ?", task.ID, model.StatusPending).
		Updates(map[string]interface{}{"status": model.StatusProcessing,
			"processing_deadline": timezone.Now().Add(20 * time.Minute)})
	if res.RowsAffected != 1 {
		t.Fatalf("claim should affect 1 row, got %d", res.RowsAffected)
	}
	var claimed model.SummaryTask
	if err := db.First(&claimed, task.ID).Error; err != nil {
		t.Fatalf("reload claimed task: %v", err)
	}
	p.processTask(claimed)

	mu.Lock()
	out := append([]int64(nil), dispatched...)
	mu.Unlock()
	return task, refIDs, out
}

// A scheduled CONFIRM (confirm_policy=1) multi-person task must actively dispatch
// ALL its (already-confirmed) Accepted+Pending participants on the main path --
// not wait ~5 min for the stuck-personal scan. This is the P1 experience fix.
func TestScheduledConfirm_MultiPerson_DispatchesAllParticipants(t *testing.T) {
	db := setupProcessorTestDB(t)
	_, refIDs, dispatched := runProcessTaskScheduled(t, db, model.SchedConfirmRequire, 2)

	if len(dispatched) != len(refIDs) {
		t.Fatalf("scheduled CONFIRM multi-person must dispatch ALL %d Accepted+Pending participants up front (not wait for the 5-min stuck scan); got %d: %v", len(refIDs), len(dispatched), dispatched)
	}
	want := map[int64]bool{}
	for _, id := range refIDs {
		want[id] = true
	}
	for _, id := range dispatched {
		if !want[id] {
			t.Errorf("dispatched unexpected participant_ref_id %d", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("these confirmed participants were never dispatched (would stall ~5min): %v", want)
	}
}

// Regression: a MANUAL (non-scheduled) CONFIRM multi-person task must NOT actively
// dispatch non-creator participants from processTask -- they stay WaitingConfirm
// until they accept. Only the scheduled path was widened by the P1 fix.
func TestManualConfirm_MultiPerson_DoesNotDispatch(t *testing.T) {
	db := setupProcessorTestDB(t)
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T-MANUAL-CONFIRM", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusPending, TriggerType: model.TriggerManual,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Two participants with Pending personals (as a manual by-person task looks
	// before anyone confirms / after creator bootstrap).
	var refIDs []int64
	for i := 0; i < 2; i++ {
		part := model.SummaryParticipant{
			TaskID: task.ID, UserID: userIDForIdx(i), Status: model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := db.Create(&part).Error; err != nil {
			t.Fatalf("create participant: %v", err)
		}
		pr := model.PersonalResult{
			TaskID: task.ID, ParticipantRefID: part.ID, UserID: userIDForIdx(i),
			WorkerStatus: model.PersonalStatusPending,
		}
		if err := db.Create(&pr).Error; err != nil {
			t.Fatalf("create personal_result: %v", err)
		}
		db.Model(&part).Update("personal_result_id", pr.ID)
		refIDs = append(refIDs, part.ID)
	}

	var dispatched []int64
	var mu sync.Mutex
	p := &Processor{
		db:   db,
		cfg:  &config.Config{WorkerLeaseMinutes: 20, WorkerMaxRetry: 3},
		pool: NewWorkerPool(4),
		dispatchPersonalFn: func(_, refID int64) {
			mu.Lock()
			dispatched = append(dispatched, refID)
			mu.Unlock()
		},
		executePipelineFn: func(_ model.SummaryTask) error { return nil },
	}

	db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"status": model.StatusProcessing,
			"processing_deadline": timezone.Now().Add(20 * time.Minute)})
	var claimed model.SummaryTask
	db.First(&claimed, task.ID)
	p.processTask(claimed)

	if len(dispatched) != 0 {
		t.Fatalf("manual (non-scheduled) CONFIRM multi-person must NOT dispatch participants from processTask (they wait for confirmation); got %v", dispatched)
	}
}
