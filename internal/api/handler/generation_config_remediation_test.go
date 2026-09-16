//go:build cgo

package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestSaveGenerationConfigPersistsRecoveredRequirementBeforeCleanup(t *testing.T) {
	db, id, prID := seedAgentRegeneration(t)
	if err := db.AutoMigrate(&model.AgentMessage{}, &model.AgentSummaryRun{}, &model.AgentSummarySpec{}); err != nil {
		t.Fatal(err)
	}
	run := model.AgentSummaryRun{RunID: "legacy-run", SpecID: "legacy-spec", UserID: "creator1", SessionID: "legacy-session", RequestID: "legacy-request"}
	spec := model.AgentSummarySpec{RunID: run.RunID, SpecID: run.SpecID, UserRequest: "Keep this original requirement"}
	msg := model.AgentMessage{UserID: "creator1", SessionID: run.SessionID, RunID: run.RunID, SpaceID: "space1"}
	for _, row := range []interface{}{&run, &spec, &msg} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", id).Updates(map[string]interface{}{
		"agent_message_id": msg.ID, "agent_session_id": msg.SessionID,
		"time_range_start": time.Now().Add(-time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	h := NewTaskHandler(db, nil, "")
	r := setupRegenerateRouter(h)
	r.PUT("/api/v1/summaries/:id/generation-config", h.SaveGenerationConfig)
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/generation-config", id), "creator1", map[string]interface{}{})
	var saved model.SummaryTask
	db.First(&saved, id)
	if w.Code != 200 || saved.GenerationRequirement == nil || *saved.GenerationRequirement != spec.UserRequest {
		t.Fatalf("status=%d body=%s requirement=%v", w.Code, w.Body, saved.GenerationRequirement)
	}
	var pr model.PersonalResult
	db.First(&pr, prID)
	if saved.Status != model.StatusCompleted || pr.Content != "old personal content" {
		t.Fatal("config-only save started a workflow or altered content")
	}
	for _, row := range []interface{}{&msg, &spec, &run} {
		if err := db.Delete(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	w = doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", id), "creator1", map[string]interface{}{})
	if w.Code != 200 {
		t.Fatalf("regenerate after cleanup: %d %s", w.Code, w.Body)
	}
}

func TestSaveGenerationConfigRejectsBoundScheduleWithoutWrites(t *testing.T) {
	for _, trigger := range []int{model.TriggerAgent, model.TriggerScheduled} {
		t.Run(fmt.Sprint(trigger), func(t *testing.T) {
			db, id, prID := seedAgentRegeneration(t)
			db.AutoMigrate(&model.SummarySchedule{})
			sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "Original", GenerationInstruction: "old"}
			db.Create(&sched)
			db.Model(&sched).Update("is_active", 0) // paused bindings are bindings too
			db.Model(&model.SummaryTask{}).Where("id = ?", id).Updates(map[string]interface{}{"schedule_id": sched.ID, "trigger_type": trigger})
			var before model.SummaryTask
			db.First(&before, id)
			h := NewTaskHandler(db, nil, "")
			r := setupRegenerateRouter(h)
			r.PUT("/api/v1/summaries/:id/generation-config", h.SaveGenerationConfig)
			w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/generation-config", id), "creator1", map[string]interface{}{
				"topic": "NEW", "sources": []sourceReq{{SourceType: model.SourceGroup, SourceID: "other"}},
			})
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "详情页") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var after model.SummaryTask
			var pr model.PersonalResult
			var sources []model.SummarySource
			db.First(&after, id)
			db.First(&pr, prID)
			db.Where("task_id = ?", id).Find(&sources)
			db.First(&sched, sched.ID)
			if after.Topic != before.Topic || after.GenerationRequirement != nil || after.Status != before.Status ||
				pr.Content != "old personal content" || len(sources) != 1 || sources[0].SourceID != "grp_abc" ||
				sched.GenerationInstruction != "old" || sched.IsActive != 0 {
				t.Fatal("rejected config changed task, source, result, or schedule")
			}
		})
	}
}

// PR#251 review P2 pin: a scheduled task's caller-supplied (unchanged) source
// set is validated-for-nothing in validateRegenerationConfigDB and DROPPED by
// saveGenerationScope's writeSources=false guard — never persisted. The
// stored server-owned set must survive byte-identically.
func TestRegenerateScheduledTaskDropsCallerSources(t *testing.T) {
	db := setupRegenerateDB(t)
	id, _, _ := seedCompletedTask(t, db)
	db.Model(&model.SummaryTask{}).Where("id = ?", id).Update("schedule_id", 42)
	r := setupRegenerateRouter(NewTaskHandler(db, nil, ""))
	// Sources mirror the stored set exactly (what an honest client resends).
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", id), "creator1",
		map[string]interface{}{"sources": []sourceReq{{SourceType: model.SourceGroup, SourceID: "grp_abc"}}})
	if w.Code != 200 {
		t.Fatalf("unchanged stored set must pass validation, got %d: %s", w.Code, w.Body)
	}
	var sources []model.SummarySource
	db.Where("task_id = ?", id).Find(&sources)
	if len(sources) != 1 || sources[0].SourceID != "grp_abc" {
		t.Fatalf("stored schedule sources altered: %+v", sources)
	}
}

func TestRegenerateCannotReplaceBoundScheduleSources(t *testing.T) {
	db := setupRegenerateDB(t)
	id, _, _ := seedCompletedTask(t, db)
	db.Model(&model.SummaryTask{}).Where("id = ?", id).Update("schedule_id", 42)
	w := doJSONRequest(setupRegenerateRouter(NewTaskHandler(db, nil, "")), "POST",
		fmt.Sprintf("/api/v1/summaries/%d/regenerate", id), "creator1",
		map[string]interface{}{"sources": []sourceReq{{SourceType: model.SourceGroup, SourceID: "other"}}})
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var task model.SummaryTask
	db.First(&task, id)
	if task.Status != model.StatusCompleted {
		t.Fatal("source replacement requeued task")
	}
}
