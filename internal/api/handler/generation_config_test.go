//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

func assertStoredLongRequirement(t *testing.T, task model.SummaryTask, requirement string) {
	t.Helper()
	if !utf8.ValidString(task.Topic) || utf8.RuneCountInString(task.Topic) > maxSummaryTopicRunes ||
		task.GenerationRequirement == nil || *task.GenerationRequirement != requirement || task.EffectiveTopic() != requirement {
		t.Fatal("full execution instruction or VARCHAR-safe compatibility topic lost")
	}
}

func TestAgentLongRequirementSaveAndRegenerate(t *testing.T) {
	requirement := strings.Repeat("需🙂", 4096)
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummarySpec{}); err != nil {
		t.Fatal(err)
	}
	// SQLite normally ignores VARCHAR(n). Explicitly enforce the production
	// sink limit so this test fails if unbounded text reaches task.topic.
	if err := db.Exec(`CREATE TRIGGER bounded_topic_insert BEFORE INSERT ON summary_task
		WHEN length(NEW.topic) > 2300 BEGIN SELECT RAISE(ABORT, 'topic too long'); END`).Error; err != nil {
		t.Fatal(err)
	}
	msg := seedAssistantMessage(t, db, "test-user", "long-request", "Summary body")
	run := model.AgentSummaryRun{RunID: "long-run", SpecID: "long-spec", UserID: "test-user", SessionID: "long-request", RequestID: "long-request-id"}
	spec := model.AgentSummarySpec{RunID: run.RunID, SpecID: run.SpecID, UserRequest: requirement}
	for _, row := range []interface{}{&run, &spec} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	db.Model(&msg).Update("run_id", run.RunID)
	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), map[string]interface{}{
		"session_id": msg.SessionID, "agent_message_id": msg.ID, "snapshot_version": 1,
		"origin_channel_id": "channel", "origin_channel_type": 1, "title": "Display title",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var saved model.SummaryTask
	db.First(&saved)
	assertStoredLongRequirement(t, saved, requirement)

	for _, sendTopic := range []bool{false, true} {
		db, id, _ := seedAgentRegeneration(t)
		db.Model(&model.SummaryTask{}).Where("id = ?", id).Updates(map[string]interface{}{
			"generation_requirement": requirement, "time_range_start": time.Now().Add(-time.Hour),
		})
		if err := db.Exec(`CREATE TRIGGER bounded_topic_update BEFORE UPDATE OF topic ON summary_task
			WHEN length(NEW.topic) > 2300 BEGIN SELECT RAISE(ABORT, 'topic too long'); END`).Error; err != nil {
			t.Fatal(err)
		}
		body := map[string]interface{}{}
		if sendTopic {
			body["topic"] = requirement
		}
		encoded, _ := json.Marshal(body)
		r := setupRegenerateRouter(NewTaskHandler(db, nil, ""))
		w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", id), "creator1", json.RawMessage(encoded))
		if w.Code != 200 {
			t.Fatalf("regenerate: %d %s", w.Code, w.Body)
		}
		var task model.SummaryTask
		db.First(&task, id)
		assertStoredLongRequirement(t, task, requirement)
	}
}

func TestPersonalEditTerminalStatesAndRetention(t *testing.T) {
	for _, status := range []int{model.StatusCompleted, model.StatusFailed, model.StatusCancelled, model.StatusPending, model.StatusProcessing} {
		db, id, _ := seedAgentRegeneration(t)
		db.Model(&model.SummaryTask{}).Where("id = ?", id).Update("status", status)
		r := setupPersonalEditRouter(NewPersonalHandler(db, "", nil))
		count := service.PersonalResultVersionKeepLimit + 2
		for i := 0; i < count; i++ {
			w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", id), "creator1",
				map[string]interface{}{"content": fmt.Sprintf("edit %d", i)})
			if status == model.StatusPending || status == model.StatusProcessing {
				if w.Code != 409 {
					t.Fatalf("in-flight edit allowed: %d", w.Code)
				}
				break
			}
			if w.Code != 200 {
				t.Fatalf("terminal edit: %d %s", w.Code, w.Body)
			}
		}
		var versions int64
		db.Model(&model.PersonalResultVersion{}).Where("task_id = ?", id).Count(&versions)
		if versions > int64(service.PersonalResultVersionKeepLimit) {
			t.Fatal("edit history exceeds retention")
		}
	}
}

func TestRegeneratePreservesRestoredBaselineIdentity(t *testing.T) {
	db := setupRegenerateDB(t)
	id, participantID, prID := seedCompletedTask(t, db)
	old := model.PersonalResultVersion{TaskID: id, ParticipantRefID: participantID, UserID: "creator1", Version: 1, Content: "restored body"}
	newer := model.PersonalResultVersion{TaskID: id, ParticipantRefID: participantID, UserID: "creator1", Version: 2, Content: "newer body"}
	db.Create(&old)
	db.Create(&newer)
	db.Model(&model.PersonalResult{}).Where("id = ?", prID).Updates(map[string]interface{}{"current_version_id": old.ID, "content": old.Content})
	w := doJSONRequest(setupRegenerateRouter(NewTaskHandler(db, nil, "")), "POST",
		fmt.Sprintf("/api/v1/summaries/%d/regenerate", id), "creator1", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var pr model.PersonalResult
	db.First(&pr, prID)
	if pr.CurrentVersionID == nil || *pr.CurrentVersionID != old.ID {
		t.Fatal("exact baseline discarded")
	}
}

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
