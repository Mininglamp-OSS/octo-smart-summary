package worker

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestPR62Round13_ScheduledEmptyWindowKeepsPreviousPersonalResult(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := time.Now().UTC()
	genAt := now.Add(-time.Hour)

	task := model.SummaryTask{
		TaskNo:         "task-r13-personal-empty-window",
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
	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "creator1",
		UserName: "Creator",
		Status:   model.ParticipantProcessing,
	}
	if err := db.Create(&participant).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "creator1",
		Content:          "previous personal summary",
		CitationsJSON:    `[{"index":1,"sender":"A","content":"old","sent_at":"2026-06-08T11:00:00Z","source":"group","channel_id":"g1","channel_type":1,"message_seq":1}]`,
		MsgCount:         8,
		TotalTokenUsed:   50,
		ModelVersion:     "old-model",
		WorkerStatus:     model.PersonalStatusProcessing,
		GeneratedAt:      &genAt,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}

	updates := completedPersonalResultUpdates(
		pr,
		noRelevantContentMessage,
		nil,
		0,
		0,
		"new-model",
		now,
		true,
	)
	if err := db.Model(&pr).Updates(updates).Error; err != nil {
		t.Fatalf("apply personal result updates: %v", err)
	}
	if err := db.Model(&participant).Update("status", model.ParticipantCompleted).Error; err != nil {
		t.Fatalf("complete participant: %v", err)
	}
	if err := completeTaskWithoutNewResult(db, task.ID); err != nil {
		t.Fatalf("completeTaskWithoutNewResult: %v", err)
	}

	var gotPR model.PersonalResult
	if err := db.First(&gotPR, pr.ID).Error; err != nil {
		t.Fatalf("reload personal result: %v", err)
	}
	if gotPR.Content != "previous personal summary" {
		t.Fatalf("personal content=%q want previous content", gotPR.Content)
	}
	if gotPR.CitationsJSON != pr.CitationsJSON {
		t.Fatalf("citations_json=%q want %q", gotPR.CitationsJSON, pr.CitationsJSON)
	}
	if gotPR.MsgCount != pr.MsgCount {
		t.Fatalf("msg_count=%d want %d", gotPR.MsgCount, pr.MsgCount)
	}
	if gotPR.TotalTokenUsed != pr.TotalTokenUsed {
		t.Fatalf("total_token_used=%d want %d", gotPR.TotalTokenUsed, pr.TotalTokenUsed)
	}
	if gotPR.ModelVersion != pr.ModelVersion {
		t.Fatalf("model_version=%q want %q", gotPR.ModelVersion, pr.ModelVersion)
	}
	if gotPR.GeneratedAt == nil || !gotPR.GeneratedAt.Equal(genAt) {
		t.Fatalf("generated_at=%v want %v", gotPR.GeneratedAt, genAt)
	}
	if gotPR.WorkerStatus != model.PersonalStatusCompleted {
		t.Fatalf("worker_status=%d want %d", gotPR.WorkerStatus, model.PersonalStatusCompleted)
	}

	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.Status != model.StatusCompleted {
		t.Fatalf("task status=%d want %d", gotTask.Status, model.StatusCompleted)
	}

	var gotParticipant model.SummaryParticipant
	if err := db.First(&gotParticipant, participant.ID).Error; err != nil {
		t.Fatalf("reload participant: %v", err)
	}
	if gotParticipant.Status != model.ParticipantCompleted {
		t.Fatalf("participant status=%d want %d", gotParticipant.Status, model.ParticipantCompleted)
	}
}
