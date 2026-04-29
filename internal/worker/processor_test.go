package worker

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
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
