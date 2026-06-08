package worker

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestPR62Round12_ScheduledEmptyWindowKeepsEditedResult(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := time.Now().UTC()
	editedAt := now.Add(-time.Hour)

	task := model.SummaryTask{
		TaskNo:         "task-r12-empty-window",
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
	oldResult := model.SummaryResult{
		TaskID:       task.ID,
		Content:      "hand edited summary",
		ModelVersion: "manual-edit",
		Version:      1,
		EditedAt:     &editedAt,
		GeneratedAt:  now.Add(-2 * time.Hour),
	}
	if err := db.Create(&oldResult).Error; err != nil {
		t.Fatalf("create old result: %v", err)
	}

	content := " \n" + noRelevantContentMessage + "\n"
	if !shouldSkipScheduledPlaceholderResult(task.TriggerType, content) {
		t.Fatalf("scheduled placeholder should trigger keep-previous path")
	}
	if shouldSkipScheduledPlaceholderResult(model.TriggerManual, content) {
		t.Fatalf("manual placeholder must not trigger scheduled keep-previous path")
	}
	if err := completeTaskWithoutNewResult(db, task.ID); err != nil {
		t.Fatalf("completeTaskWithoutNewResult: %v", err)
	}

	var results []model.SummaryResult
	if err := db.Where("task_id = ?", task.ID).Order("id ASC").Find(&results).Error; err != nil {
		t.Fatalf("load results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count=%d want 1", len(results))
	}
	if results[0].ID != oldResult.ID {
		t.Fatalf("kept result id=%d want %d", results[0].ID, oldResult.ID)
	}
	if results[0].Content != oldResult.Content {
		t.Fatalf("kept result content=%q want %q", results[0].Content, oldResult.Content)
	}
	if results[0].EditedAt == nil {
		t.Fatalf("edited result lost edited_at marker")
	}

	var reloadedTask model.SummaryTask
	if err := db.First(&reloadedTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloadedTask.Status != model.StatusCompleted {
		t.Fatalf("task status=%d want %d", reloadedTask.Status, model.StatusCompleted)
	}
	if reloadedTask.ProcessingDeadline != nil {
		t.Fatalf("processing_deadline=%v want nil", reloadedTask.ProcessingDeadline)
	}
}

func TestPR62Round12_ScheduledPruneKeepsEditedResults(t *testing.T) {
	db := setupSchedulerTestDB(t)
	now := time.Now().UTC()
	editedAt := now.Add(-2 * time.Hour)

	task := model.SummaryTask{
		TaskNo:         "task-r12-prune",
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
	editedResult := model.SummaryResult{
		TaskID:       task.ID,
		Content:      "hand edited summary",
		ModelVersion: "edited",
		Version:      1,
		EditedAt:     &editedAt,
		GeneratedAt:  now.Add(-3 * time.Hour),
	}
	if err := db.Create(&editedResult).Error; err != nil {
		t.Fatalf("create edited result: %v", err)
	}
	autoResult := model.SummaryResult{
		TaskID:       task.ID,
		Content:      "stale scheduled summary",
		ModelVersion: "auto-old",
		Version:      2,
		GeneratedAt:  now.Add(-time.Hour),
	}
	if err := db.Create(&autoResult).Error; err != nil {
		t.Fatalf("create auto result: %v", err)
	}

	newResult := model.SummaryResult{
		Content:      "fresh scheduled summary",
		ModelVersion: "auto-new",
		GeneratedAt:  now,
	}
	if err := saveLatestResultAndCompleteTask(db, task.ID, &newResult, true); err != nil {
		t.Fatalf("saveLatestResultAndCompleteTask: %v", err)
	}

	var results []model.SummaryResult
	if err := db.Where("task_id = ?", task.ID).Order("version ASC").Find(&results).Error; err != nil {
		t.Fatalf("load results: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("result count=%d want 2", len(results))
	}
	if results[0].ID != editedResult.ID {
		t.Fatalf("edited result id=%d want %d", results[0].ID, editedResult.ID)
	}
	if results[0].EditedAt == nil {
		t.Fatalf("edited result lost edited_at marker")
	}
	if results[1].ID != newResult.ID {
		t.Fatalf("new result id=%d want %d", results[1].ID, newResult.ID)
	}
	if results[1].Content != "fresh scheduled summary" {
		t.Fatalf("new result content=%q want fresh scheduled summary", results[1].Content)
	}
	for _, result := range results {
		if result.ID == autoResult.ID {
			t.Fatalf("stale auto-generated result id=%d should have been pruned", autoResult.ID)
		}
	}
}
