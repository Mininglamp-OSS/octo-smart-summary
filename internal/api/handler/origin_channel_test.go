//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupOriginTestDB(t *testing.T) (db *gorm.DB, imDB *gorm.DB) {
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
	)
	imDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	return db, imDB
}

func setupOriginRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries", h.CreateSummary)
	r.GET("/api/v1/summaries", h.ListSummaries)
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	r.GET("/api/v1/summaries/:id/result", h.GetResult)
	r.GET("/api/v1/summary-templates", h.GetTemplates)
	return r
}

func doCreateRequest(r *gin.Engine, body interface{}, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", userID)
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func TestCreateSummary_WithOriginChannel(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "group_123",
		"origin_channel_type": 1,
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	taskID := int64(data["task_id"].(float64))

	var task model.SummaryTask
	db.First(&task, taskID)
	if task.OriginChannelID != "group_123" {
		t.Errorf("origin_channel_id: want group_123, got %s", task.OriginChannelID)
	}
	if task.OriginChannelType != 1 {
		t.Errorf("origin_channel_type: want 1, got %d", task.OriginChannelType)
	}

	// Verify auto-fill source was created
	var sources []model.SummarySource
	db.Where("task_id = ?", taskID).Find(&sources)
	if len(sources) != 1 {
		t.Fatalf("expected 1 source (auto-filled), got %d", len(sources))
	}
	if sources[0].SourceType != 1 {
		t.Errorf("source_type: want 1, got %d", sources[0].SourceType)
	}
	if sources[0].SourceID != "group_123" {
		t.Errorf("source_id: want group_123, got %s", sources[0].SourceID)
	}
}

func TestCreateSummary_OriginChannelValidation_InvalidType(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "group_123",
		"origin_channel_type": 5,
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["code"].(float64)) != 40001 {
		t.Errorf("expected code 40001, got %v", resp["code"])
	}
}

func TestCreateSummary_OriginChannelValidation_TypeWithoutID(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "",
		"origin_channel_type": 2,
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["code"].(float64)) != 40001 {
		t.Errorf("expected code 40001, got %v", resp["code"])
	}
}

func TestCreateSummary_OriginChannel_NoAutoFillWhenSourcesProvided(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "group_123",
		"origin_channel_type": 1,
		"sources": []map[string]interface{}{
			{"source_type": 1, "source_id": "group_456"},
		},
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	taskID := int64(data["task_id"].(float64))

	var sources []model.SummarySource
	db.Where("task_id = ?", taskID).Find(&sources)
	if len(sources) != 1 {
		t.Fatalf("expected 1 source (user-provided), got %d", len(sources))
	}
	if sources[0].SourceID != "group_456" {
		t.Errorf("source_id should be user-provided group_456, got %s", sources[0].SourceID)
	}
}

func TestListSummaries_FilterByOriginChannelID(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task1 := model.SummaryTask{
		TaskNo:            "LST-001",
		SpaceID:           "space1",
		CreatorID:         "user1",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		OriginChannelID:   "group_aaa",
		OriginChannelType: 1,
		TimeRangeStart:    now.Add(-24 * time.Hour),
		TimeRangeEnd:      now,
	}
	task2 := model.SummaryTask{
		TaskNo:            "LST-002",
		SpaceID:           "space1",
		CreatorID:         "user1",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		OriginChannelID:   "group_bbb",
		OriginChannelType: 1,
		TimeRangeStart:    now.Add(-24 * time.Hour),
		TimeRangeEnd:      now,
	}
	db.Create(&task1)
	db.Create(&task2)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summaries?origin_channel_id=group_aaa", nil)
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	total := int(data["total"].(float64))
	if total != 1 {
		t.Errorf("expected 1 result, got %d", total)
	}
	items := data["items"].([]interface{})
	item := items[0].(map[string]interface{})
	if item["origin_channel_id"] != "group_aaa" {
		t.Errorf("expected origin_channel_id=group_aaa, got %v", item["origin_channel_id"])
	}
	if int(item["origin_channel_type"].(float64)) != 1 {
		t.Errorf("expected origin_channel_type=1, got %v", item["origin_channel_type"])
	}
}

func TestGetSummary_IncludesOriginFields(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:            "GET-001",
		SpaceID:           "space1",
		CreatorID:         "user1",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		OriginChannelID:   "thread_xyz",
		OriginChannelType: 2,
		TimeRangeStart:    now.Add(-24 * time.Hour),
		TimeRangeEnd:      now,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "user1", UserName: "U1"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})

	if data["origin_channel_id"] != "thread_xyz" {
		t.Errorf("origin_channel_id: want thread_xyz, got %v", data["origin_channel_id"])
	}
	if int(data["origin_channel_type"].(float64)) != 2 {
		t.Errorf("origin_channel_type: want 2, got %v", data["origin_channel_type"])
	}
}

func TestGetSummary_IncludesTeamCitations(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:            "GET-TC-001",
		SpaceID:           "space1",
		CreatorID:         "user1",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		OriginChannelID:   "group_tc",
		OriginChannelType: 1,
		TimeRangeStart:    now.Add(-24 * time.Hour),
		TimeRangeEnd:      now,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "user1", UserName: "U1"})

	// A result carrying team citations ([Pn] -> participant) plus a plain
	// citation. The detail (GetSummary) path must surface team_citations so the
	// frontend can render [Pn] badges; without the handler change this assertion
	// fails (team_citations key absent).
	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "team summary referencing [P1] and message [1]",
		Version:     1,
		GeneratedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	result.SetTeamCitations([]model.TeamCitation{
		{Index: 1, UserID: "user1", UserName: "U1"},
	})
	db.Create(&result)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})

	resultOut, ok := data["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result missing or wrong type: %v", data["result"])
	}

	tc, ok := resultOut["team_citations"].([]interface{})
	if !ok {
		t.Fatalf("team_citations missing or wrong type in result: %v", resultOut["team_citations"])
	}
	if len(tc) != 1 {
		t.Fatalf("expected 1 team citation, got %d", len(tc))
	}
	first := tc[0].(map[string]interface{})
	if int(first["index"].(float64)) != 1 {
		t.Errorf("team citation index: want 1, got %v", first["index"])
	}
	if first["user_id"] != "user1" {
		t.Errorf("team citation user_id: want user1, got %v", first["user_id"])
	}
	if first["user_name"] != "U1" {
		t.Errorf("team citation user_name: want U1, got %v", first["user_name"])
	}

	// Plain citations key remains present and independent of team citations.
	if _, ok := resultOut["citations"]; !ok {
		t.Errorf("citations key should still be present alongside team_citations")
	}
}

func TestGetTemplates_ReturnsCorrectStructure(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summary-templates", nil)
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	templates := data["templates"].([]interface{})

	if len(templates) != 8 {
		t.Fatalf("expected 8 templates, got %d", len(templates))
	}

	expectedIDs := []string{"project_progress", "task_tracking", "weekly_report", "chat_content", "personal_weekly_report", "okr_alignment", "todo_extraction", "feedback_triage"}
	for i, tmpl := range templates {
		m := tmpl.(map[string]interface{})
		if m["id"] != expectedIDs[i] {
			t.Errorf("template[%d] id: want %s, got %v", i, expectedIDs[i], m["id"])
		}
		if m["label"] == nil || m["label"] == "" {
			t.Errorf("template[%d] label is empty", i)
		}
		if m["icon"] == nil || m["icon"] == "" {
			t.Errorf("template[%d] icon is empty", i)
		}
		if m["pattern"] == nil || m["pattern"] == "" {
			t.Errorf("template[%d] pattern is empty", i)
		}
		if m["type"] == nil || m["type"] == "" {
			t.Errorf("template[%d] type is empty", i)
		}
	}

	// Built-in templates use the two-field model; no parameterized placeholders are exposed.
	taskTracking := templates[1].(map[string]interface{})
	if taskTracking["type"] != "fixed" {
		t.Errorf("task_tracking type: want fixed, got %v", taskTracking["type"])
	}
	if _, ok := taskTracking["placeholders"]; ok {
		t.Errorf("task_tracking placeholders should be omitted in two-field model")
	}
}

// --- Round 2 additions ---

// TestGetSummary_MultiPersonPermissions (B1) constructs a real BY_PERSON task
// with MULTIPLE participants and a creator, then asserts the permission split:
//   - can_schedule == true for the creator (task-level config; allowed for
//     single/multi participant, no Completed requirement implied by participant
//     count).
//   - can_edit == false for the creator when len(participants) > 1. The team
//     edit endpoint (PUT /summaries/:id/edit) rejects multi-person tasks, so
//     can_edit keeps the len(participants) <= 1 gate to avoid a "visible but
//     unusable" edit button. can_edit and can_schedule are decoupled.
func TestGetSummary_MultiPersonPermissions(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "GET-PERM-001",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
	}
	db.Create(&task)
	// Multiple participants (> 1) — this is the case the old gate broke.
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "C1"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant2", UserName: "P2"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant3", UserName: "P3"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	perms, ok := data["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions missing or wrong type: %v", data["permissions"])
	}

	if perms["can_schedule"] != true {
		t.Errorf("creator of multi-person BY_PERSON task must have can_schedule=true, got %v", perms["can_schedule"])
	}
	if perms["can_edit"] != false {
		t.Errorf("creator of multi-person BY_PERSON task must have can_edit=false (team edit endpoint rejects multi-person), got %v", perms["can_edit"])
	}

	// A non-creator participant must NOT get can_schedule.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req2.Header.Set("Token", "participant2")
	req2.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 for participant2, got %d: %s", w2.Code, w2.Body.String())
	}
	var resp2 map[string]interface{}
	json.Unmarshal(w2.Body.Bytes(), &resp2)
	perms2 := resp2["data"].(map[string]interface{})["permissions"].(map[string]interface{})
	if perms2["can_schedule"] == true {
		t.Errorf("non-creator participant must NOT have can_schedule=true, got %v", perms2["can_schedule"])
	}
}

// TestGetSummary_ByPersonHidesPlainCitations (B2) asserts that for a BY_PERSON
// task the detail (GetSummary) result does NOT carry plain citations (raw chat
// records) while STILL carrying team_citations ([Pn] -> participant). The old
// handler returned latestResult.GetCitations() unconditionally, leaking other
// members' chat records to every participant. Fail-before / pass-after.
func TestGetSummary_ByPersonHidesPlainCitations(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "GET-PRIV-001",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "C1"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant2", UserName: "P2"})

	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "team summary [P1] referencing message [1]",
		Version:     1,
		GeneratedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	result.SetCitations([]model.Citation{{
		Index:     1,
		Sender:    "participant2",
		Content:   "原始聊天记录原文不应被他人看到",
		ChannelID: "grp_priv",
	}})
	result.SetTeamCitations([]model.TeamCitation{
		{Index: 1, UserID: "participant2", UserName: "P2"},
	})
	db.Create(&result)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	resultOut := resp["data"].(map[string]interface{})["result"].(map[string]interface{})

	// Plain citations must be empty for BY_PERSON.
	cits, _ := resultOut["citations"].([]interface{})
	if len(cits) != 0 {
		t.Errorf("BY_PERSON GetSummary must NOT expose plain citations, got %d: %v", len(cits), resultOut["citations"])
	}
	// team_citations must still be present.
	tc, ok := resultOut["team_citations"].([]interface{})
	if !ok || len(tc) != 1 {
		t.Errorf("BY_PERSON GetSummary must keep team_citations, got %v", resultOut["team_citations"])
	}
}

// TestGetResult_ByPersonHidesPlainCitations (B2) — same privacy guarantee on the
// GetResult endpoint. Updated for P1-A: stripping is now gated on TRUE
// multi-person (participantCount>1), not on SummaryMode==ModeByPerson (which
// fired for single-person tasks too and wrongly wiped their own [n] sources).
// This task therefore seeds TWO participants so the privacy strip still applies.
func TestGetResult_ByPersonHidesPlainCitations(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "GETR-PRIV-001",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "C1"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant2", UserName: "P2"})

	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "team summary [P1] message [1]",
		Version:     1,
		GeneratedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	result.SetCitations([]model.Citation{{
		Index: 1, Sender: "x", Content: "raw chat leak", ChannelID: "grp",
	}})
	result.SetTeamCitations([]model.TeamCitation{
		{Index: 1, UserID: "creator1", UserName: "C1"},
	})
	db.Create(&result)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d/result", task.ID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})

	cits, _ := data["citations"].([]interface{})
	if len(cits) != 0 {
		t.Errorf("BY_PERSON GetResult must NOT expose plain citations, got %d: %v", len(cits), data["citations"])
	}
	tc, ok := data["team_citations"].([]interface{})
	if !ok || len(tc) != 1 {
		t.Errorf("BY_PERSON GetResult must keep team_citations, got %v", data["team_citations"])
	}
}

// TestGetSummary_ByGroupKeepsPlainCitations (B2 control) — BY_GROUP tasks have
// no per-person privacy concern, so plain citations must be preserved.
func TestGetSummary_ByGroupKeepsPlainCitations(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	// SummaryMode 1 == BY_GROUP (only ModeByPerson=2 is a named constant).
	task := model.SummaryTask{
		TaskNo:         "GET-GRP-001",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    1,
		Status:         model.StatusCompleted,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
	}
	db.Create(&task)

	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "group summary message [1]",
		Version:     1,
		GeneratedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	result.SetCitations([]model.Citation{{
		Index: 1, Sender: "x", Content: "group chat citation (ok to show)", ChannelID: "grp",
	}})
	db.Create(&result)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	resultOut := resp["data"].(map[string]interface{})["result"].(map[string]interface{})
	cits, _ := resultOut["citations"].([]interface{})
	if len(cits) != 1 {
		t.Errorf("BY_GROUP GetSummary must keep plain citations, got %d: %v", len(cits), resultOut["citations"])
	}
}
