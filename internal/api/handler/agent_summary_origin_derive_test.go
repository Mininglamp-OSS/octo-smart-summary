//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// postAgentSave issues POST /api/v1/summaries/agent with the given body and
// returns the recorder. Shared by all origin-derive tests.
func postAgentSave(r interface {
	ServeHTTP(w http.ResponseWriter, req *http.Request)
}, body map[string]interface{}) *httptest.ResponseRecorder {
	bodyBytes, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)
	return w
}

// seedPipelineRefTask seeds a completed pipeline-style summary task:
// origin_channel_id empty, creator = test-user (accessible), given task_no.
func seedPipelineRefTask(t *testing.T, h *AgentSummaryHandler, taskNo string) model.SummaryTask {
	t.Helper()
	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:         taskNo,
		SpaceID:        "test-space",
		CreatorID:      "test-user",
		Title:          "pipeline summary",
		Topic:          "pipeline summary",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerScheduled,
		// OriginChannelID deliberately empty — the shape that tier-3 misses.
	}
	if err := h.db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	return ref
}

// TestCreateAgentSummary_DeriveOriginFromSingleSource is the owner's core
// scenario (2026-08-13): reference a pipeline-generated summary (no
// origin_channel_id), let the agent refine it, save. Tier-3 borrow finds the
// task but origin is empty → tier-4 derives from the task's summary_source
// rows → save succeeds with the source channel as origin.
func TestCreateAgentSummary_DeriveOriginFromSingleSource(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-001")
	if err := db.Create(&model.SummarySource{
		TaskID:     ref.ID,
		SourceType: model.SourceGroup,
		SourceID:   "CH-PIPELINE",
	}).Error; err != nil {
		t.Fatalf("seed summary_source: %v", err)
	}

	sessionID := "session-refine-pipeline"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined pipeline summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined from pipeline",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The created task (the newest one) must carry the derived origin.
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if len(tasks) == 0 {
		t.Fatal("no task created")
	}
	created := tasks[0]
	if created.OriginChannelID != "CH-PIPELINE" {
		t.Errorf("origin_channel_id = %q, want CH-PIPELINE (derived from summary_source)", created.OriginChannelID)
	}
	if created.OriginChannelType != model.OriginChannelGroup {
		t.Errorf("origin_channel_type = %d, want %d (SourceGroup, same enum)", created.OriginChannelType, model.OriginChannelGroup)
	}
}

// TestCreateAgentSummary_DeriveOriginMultiSourceInheritsFirst: a task
// generated from two channels inherits the FIRST source row (by id, creation
// order) — deterministic, consistent with tier-3's first-referenced-task
// precedent.
func TestCreateAgentSummary_DeriveOriginMultiSourceInheritsFirst(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-002")
	for _, src := range []struct {
		id   string
		typ  int
	}{
		{"CH-FIRST", model.SourceGroup},
		{"CH-SECOND", model.SourceGroup},
	} {
		if err := db.Create(&model.SummarySource{
			TaskID:     ref.ID,
			SourceType: src.typ,
			SourceID:   src.id,
		}).Error; err != nil {
			t.Fatalf("seed summary_source: %v", err)
		}
	}

	sessionID := "session-refine-multi"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined multi-source summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined multi-source",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if tasks[0].OriginChannelID != "CH-FIRST" {
		t.Errorf("origin_channel_id = %q, want CH-FIRST (first usable source row wins)", tasks[0].OriginChannelID)
	}
}

// TestCreateAgentSummary_DeriveOriginSkipsUnusableRows: source rows with an
// empty source_id or an out-of-range source_type are skipped; the next usable
// row wins. If nothing usable remains the save still 40001s.
func TestCreateAgentSummary_DeriveOriginSkipsUnusableRows(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-003")
	// Row 1: empty id (unusable). Row 2: source_type=0 (unusable). Row 3: valid.
	for _, src := range []struct {
		id  string
		typ int
	}{
		{"", model.SourceGroup},
		{"CH-ZERO-TYPE", 0},
		{"CH-VALID", model.SourceThread},
	} {
		if err := db.Create(&model.SummarySource{
			TaskID:     ref.ID,
			SourceType: src.typ,
			SourceID:   src.id,
		}).Error; err != nil {
			t.Fatalf("seed summary_source: %v", err)
		}
	}

	sessionID := "session-refine-skip"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if tasks[0].OriginChannelID != "CH-VALID" || tasks[0].OriginChannelType != model.OriginChannelThread {
		t.Errorf("origin = %q/%d, want CH-VALID/%d", tasks[0].OriginChannelID, tasks[0].OriginChannelType, model.OriginChannelThread)
	}
}

// TestCreateAgentSummary_DeriveOriginNoSourcesStill40001: referenced task
// with NO summary_source rows (legacy/erased data) still dead-ends in 40001 —
// the fail-closed contract is unchanged for genuinely origin-less summaries.
func TestCreateAgentSummary_DeriveOriginNoSourcesStill40001(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-004") // no summary_source rows

	sessionID := "session-refine-nosrc"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}
}

// TestCreateAgentSummary_DeriveOriginNoAccessRefused: a user who cannot read
// the referenced task (not creator, not participant) must NOT derive its
// source channels — same authz posture as tier-3.
func TestCreateAgentSummary_DeriveOriginNoAccessRefused(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:         "ST-PIPE-005",
		SpaceID:        "test-space",
		CreatorID:      "someone-else", // not test-user, no participant row either
		Title:          "foreign pipeline summary",
		Topic:          "foreign pipeline summary",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerScheduled,
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	db.Create(&model.SummarySource{TaskID: ref.ID, SourceType: model.SourceGroup, SourceID: "CH-FOREIGN"})

	sessionID := "session-refine-foreign"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no access → no origin), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}
}

// TestCreateAgentSummary_Tier3BorrowStillWins: when the referenced task HAS
// an origin_channel_id, tier-3 borrows it directly and tier-4 is never
// consulted (regression guard for the pre-existing path).
func TestCreateAgentSummary_Tier3BorrowStillWins(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:            "ST-AGENT-006",
		SpaceID:           "test-space",
		CreatorID:         "test-user",
		Title:             "agent summary",
		Topic:             "agent summary",
		SummaryMode:       model.ModeByPerson,
		TimeRangeStart:    now,
		TimeRangeEnd:      now,
		Status:            model.StatusCompleted,
		TriggerType:       model.TriggerAgent,
		OriginChannelID:   "CH-TIER3",
		OriginChannelType: model.OriginChannelDM,
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	// Even with sources present, tier-3 must win.
	db.Create(&model.SummarySource{TaskID: ref.ID, SourceType: model.SourceGroup, SourceID: "CH-SHOULD-NOT-WIN"})

	sessionID := "session-refine-tier3"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined agent summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if tasks[0].OriginChannelID != "CH-TIER3" || tasks[0].OriginChannelType != model.OriginChannelDM {
		t.Errorf("origin = %q/%d, want CH-TIER3/%d (tier-3 wins over sources)",
			tasks[0].OriginChannelID, tasks[0].OriginChannelType, model.OriginChannelDM)
	}
}
