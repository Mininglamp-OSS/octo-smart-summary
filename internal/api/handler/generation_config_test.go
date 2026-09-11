//go:build cgo

package handler

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func seedAgentRegeneration(t *testing.T) (*gorm.DB, int64, int64) {
	t.Helper()
	db := setupRegenerateDB(t)
	id, _, prID := seedCompletedTask(t, db)
	now := time.Now().UTC()
	db.Model(&model.SummaryTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"trigger_type": model.TriggerAgent, "time_range_start": now, "time_range_end": now,
		"topic": "Display title", "title": "Display title",
	})
	return db, id, prID
}

func TestAgentRegenerateMissingConfigurationPreservesResult(t *testing.T) {
	db, id, prID := seedAgentRegeneration(t)
	r := setupRegenerateRouter(NewTaskHandler(db, nil, ""))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", id), bytes.NewBufferString(`{}`))
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected missing config, got %d: %s", w.Code, w.Body)
	}
	var pr model.PersonalResult
	db.First(&pr, prID)
	if pr.Content != "old personal content" {
		t.Fatal("missing configuration cleared previous result")
	}
	var task model.SummaryTask
	db.First(&task, id)
	if generationRequirement(db, task) != "" {
		t.Fatal("display title was treated as actual prompt")
	}
}

func TestAgentRegenerateSavesConfigAndBaselineWithoutChangingRecurrence(t *testing.T) {
	db, id, _ := seedAgentRegeneration(t)
	db.AutoMigrate(&model.SummarySchedule{})
	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Original", IsActive: 0,
		CronExpr: "0 9 * * 1"}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatal(err)
	}
	// GORM applies the model's default=1 to zero-valued creates.
	db.Model(&sched).Update("is_active", 0)
	db.Model(&model.SummaryTask{}).Where("id = ?", id).Update("schedule_id", sched.ID)
	r := setupRegenerateRouter(NewTaskHandler(db, nil, ""))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", id),
		bytes.NewBufferString(`{"topic":"Actual original request","time_range":{"start":"2026-09-01T00:00:00Z","end":"2026-09-07T23:59:59Z"}}`))
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	var task model.SummaryTask
	db.First(&task, id)
	if task.Title != "Display title" || generationRequirement(db, task) != "Actual original request" ||
		task.Status != model.StatusPending || !task.TimeRangeEnd.After(task.TimeRangeStart) {
		t.Fatalf("configuration not persisted independently: %+v", task)
	}
	var version model.PersonalResultVersion
	if err := db.Where("task_id = ?", id).First(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version.Content != "old personal content" || version.Version != 1 {
		t.Fatalf("baseline: %+v", version)
	}
	db.First(&sched, sched.ID)
	if sched.IsActive != 0 || sched.CronExpr != "0 9 * * 1" {
		t.Fatal("regeneration altered schedule recurrence")
	}
}

func TestSaveGenerationConfigDoesNotStartWorkflow(t *testing.T) {
	db, id, prID := seedAgentRegeneration(t)
	h := NewTaskHandler(db, nil, "")
	r := setupRegenerateRouter(h)
	r.PUT("/api/v1/summaries/:id/generation-config", h.SaveGenerationConfig)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/summaries/%d/generation-config", id),
		bytes.NewBufferString(`{"topic":"Saved instruction","time_range":{"start":"2026-09-01T00:00:00Z","end":"2026-09-07T23:59:59Z"}}`))
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	var task model.SummaryTask
	var pr model.PersonalResult
	db.First(&task, id)
	db.First(&pr, prID)
	if task.Status != model.StatusCompleted || task.ScheduleID != nil || pr.Content != "old personal content" {
		t.Fatal("saving configuration changed task, result or schedule")
	}
}
