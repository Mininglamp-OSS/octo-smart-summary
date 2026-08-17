//go:build cgo

package handler

// Regression tests for the "summary-detail-source-chip 乱码 / 裸 ID" bug
// (owner report 2026-08-17, live task ST20260811jcw59o84):
//
//  1. Write side — POST /api/v1/summaries/agent (CreateAgentSummary) stored
//     source_id only and left source_name empty (the old code comment said
//     "a future PR can plumb imDB in"). The front-end chip renders
//     `source_name || source_id`, so agent-created summaries showed the raw
//     channel hex id ("来自fef0c26327664232a2fd73ce2fb98ec6"). The agent path
//     now resolves names exactly like CreateSummary (task.go) does.
//
//  2. Read side — rows already persisted with an empty source_name (10 live
//     rows at report time) are resolved live on list/detail/share via
//     displaySourceName. A non-empty stored name is a creation-time snapshot
//     and is NEVER overwritten (a later group rename must not rewrite
//     history — same semantics as the instant path).
//
// Every test below must go RED if its corresponding fix line is reverted.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupSourceNameImDB creates an IM DB (sqlite) with the `group` table the
// group branch of service.ResolveSourceNameWithType reads. Same schema shape
// as setupBotCreateTest.
func setupSourceNameImDB(t *testing.T) *gorm.DB {
	t.Helper()
	imDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	if err := imDB.Exec(`CREATE TABLE "group" (group_no TEXT, name TEXT, space_id TEXT, status INTEGER, updated_at DATETIME)`).Error; err != nil {
		t.Fatalf("create group table: %v", err)
	}
	return imDB
}

func seedImGroup(t *testing.T, imDB *gorm.DB, groupNo, name string) {
	t.Helper()
	if err := imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status, updated_at) VALUES (?, ?, ?, 1, ?)`,
		groupNo, name, "space1", time.Now()).Error; err != nil {
		t.Fatalf("seed group: %v", err)
	}
}

// doAgentCreateRequest posts a create-agent-summary request with the given
// sources array and returns the recorder. Mirrors the request shape used by
// TestCreateAgentSummary_ProvidedOriginChannelDirectPass.
func doAgentCreateRequest(t *testing.T, h *AgentSummaryHandler, sessionID string, sources []map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	r := setupAgentSummaryRouter(h)
	reqBody := map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   "CH-SRCNAME",
		"origin_channel_type": 1,
		"title":               "Source name test",
		"sources":             sources,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)
	return w
}

// TestCreateAgentSummary_SourceNameResolvedFromIMDB pins the write-side fix:
// the agent path resolves source names from the IM DB (parity with
// CreateSummary), so newly created agent summaries never persist an empty
// source_name. Reverting the ResolveSourceNameWithType call in
// CreateAgentSummary makes this RED (persisted name becomes "").
func TestCreateAgentSummary_SourceNameResolvedFromIMDB(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	imDB := setupSourceNameImDB(t)
	const groupNo = "fef0c26327664232a2fd73ce2fb98ec6"
	seedImGroup(t, imDB, groupNo, "一个群聊")

	// Seed: session with an assistant deliverable (required by the handler).
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: "session-srcname-001",
		Role:      "assistant",
		Content:   "Agent-generated deliverable.",
	})

	h := NewAgentSummaryHandler(db, imDB, "", "", "", 0, 0)
	w := doAgentCreateRequest(t, h, "session-srcname-001", []map[string]interface{}{
		{"source_type": 1, "source_id": groupNo},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var src model.SummarySource
	if err := db.First(&src).Error; err != nil {
		t.Fatalf("source row not persisted: %v", err)
	}
	if src.SourceID != groupNo {
		t.Fatalf("source_id = %q, want %q", src.SourceID, groupNo)
	}
	if src.SourceName != "一个群聊(群聊)" {
		t.Errorf("source_name = %q, want %q (resolved from IM DB with type suffix)", src.SourceName, "一个群聊(群聊)")
	}
}

// TestCreateAgentSummary_ClientSourceNameNotTrusted pins that a
// client-supplied source_name is dropped in favour of the canonical IM DB
// resolution — same contract as the instant path (CreateSummary ignores
// req source_name; see task.go) and the schedule path
// (buildScheduledTaskSources: "never trust a client-supplied source_name").
func TestCreateAgentSummary_ClientSourceNameNotTrusted(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	imDB := setupSourceNameImDB(t)
	const groupNo = "81f67cec0180463a8bd74117e5582125"
	seedImGroup(t, imDB, groupNo, "真实群名")

	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: "session-srcname-002",
		Role:      "assistant",
		Content:   "Agent-generated deliverable.",
	})

	h := NewAgentSummaryHandler(db, imDB, "", "", "", 0, 0)
	w := doAgentCreateRequest(t, h, "session-srcname-002", []map[string]interface{}{
		{"source_type": 1, "source_id": groupNo, "source_name": "客户端伪造的名字"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var src model.SummarySource
	if err := db.First(&src).Error; err != nil {
		t.Fatalf("source row not persisted: %v", err)
	}
	if src.SourceName != "真实群名(群聊)" {
		t.Errorf("source_name = %q, want %q (client-supplied name must be ignored)", src.SourceName, "真实群名(群聊)")
	}
}

// TestCreateAgentSummary_SourceNameNilImDBFallback pins graceful degradation:
// with no IM DB wired (imDB == nil) the deterministic "来源-xxxxxxxx"
// fallback is stored instead of an empty string, so the chip never regresses
// to a raw id.
func TestCreateAgentSummary_SourceNameNilImDBFallback(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	const groupNo = "fef0c26327664232a2fd73ce2fb98ec6"

	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: "session-srcname-003",
		Role:      "assistant",
		Content:   "Agent-generated deliverable.",
	})

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentCreateRequest(t, h, "session-srcname-003", []map[string]interface{}{
		{"source_type": 1, "source_id": groupNo},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var src model.SummarySource
	if err := db.First(&src).Error; err != nil {
		t.Fatalf("source row not persisted: %v", err)
	}
	if src.SourceName != "来源-fef0c263(群聊)" {
		t.Errorf("source_name = %q, want deterministic fallback %q", src.SourceName, "来源-fef0c263(群聊)")
	}
}

// seedEmptyNameSourceTask seeds one completed task whose summary_source row
// has an EMPTY source_name — the exact shape of the 10 live rows written by
// the agent path before this fix (e.g. task ST20260811jcw59o84).
func seedEmptyNameSourceTask(t *testing.T, db *gorm.DB, taskNo, sourceID string) int64 {
	t.Helper()
	task := model.SummaryTask{
		TaskNo:      taskNo,
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummarySource{
		TaskID: task.ID, SourceType: model.SourceGroup, SourceID: sourceID, // SourceName left empty on purpose
	}).Error; err != nil {
		t.Fatalf("create source: %v", err)
	}
	return task.ID
}

// TestGetSummary_EmptySourceNameResolvedLive is the live-bug regression:
// ST20260811jcw59o84 rendered "来自fef0c26327664232a2fd73ce2fb98ec6" because
// the stored name was empty and the chip falls back to source_id. The detail
// endpoint must resolve such rows live from the IM DB. Reverting
// displaySourceName back to s.SourceName makes this RED.
func TestGetSummary_EmptySourceNameResolvedLive(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	imDB.Exec(`CREATE TABLE IF NOT EXISTS "group" (group_no TEXT, name TEXT, space_id TEXT, status INTEGER, updated_at DATETIME)`)
	const groupNo = "fef0c26327664232a2fd73ce2fb98ec6"
	seedImGroup(t, imDB, groupNo, "一个群聊")
	taskID := seedEmptyNameSourceTask(t, db, "TST-SRCNAME-DETAIL", groupNo)

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Sources []struct {
				SourceID   string `json:"source_id"`
				SourceName string `json:"source_name"`
			} `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(resp.Data.Sources))
	}
	if resp.Data.Sources[0].SourceName != "一个群聊(群聊)" {
		t.Errorf("detail source_name = %q, want %q (empty stored name must be resolved live)",
			resp.Data.Sources[0].SourceName, "一个群聊(群聊)")
	}
}

// TestGetSummary_StoredSourceNameNotOverwritten pins snapshot semantics: a
// non-empty stored name was resolved at creation time and must survive a
// later group rename. displaySourceName must NOT re-resolve non-empty names.
func TestGetSummary_StoredSourceNameNotOverwritten(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	imDB.Exec(`CREATE TABLE IF NOT EXISTS "group" (group_no TEXT, name TEXT, space_id TEXT, status INTEGER, updated_at DATETIME)`)
	const groupNo = "81f67cec0180463a8bd74117e5582125"
	seedImGroup(t, imDB, groupNo, "改名后的新群名")

	task := model.SummaryTask{
		TaskNo:      "TST-SRCNAME-SNAPSHOT",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummarySource{
		TaskID: task.ID, SourceType: model.SourceGroup, SourceID: groupNo,
		SourceName: "创建时的旧群名(群聊)", // snapshot taken before the rename
	}).Error; err != nil {
		t.Fatalf("create source: %v", err)
	}

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Sources []struct {
				SourceName string `json:"source_name"`
			} `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(resp.Data.Sources))
	}
	if resp.Data.Sources[0].SourceName != "创建时的旧群名(群聊)" {
		t.Errorf("detail source_name = %q, want stored snapshot %q (a live rename must not rewrite history)",
			resp.Data.Sources[0].SourceName, "创建时的旧群名(群聊)")
	}
}

// TestGetSummary_EmptySourceNameNilImDB pins that an empty stored name with
// no IM DB wired degrades to the deterministic placeholder instead of an
// empty string (which would make the chip fall back to the raw id again).
func TestGetSummary_EmptySourceNameNilImDB(t *testing.T) {
	db, _ := setupListTestDBs(t)
	const groupNo = "fef0c26327664232a2fd73ce2fb98ec6"
	taskID := seedEmptyNameSourceTask(t, db, "TST-SRCNAME-NILIM", groupNo)

	h := NewTaskHandler(db, nil, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Sources []struct {
				SourceName string `json:"source_name"`
			} `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(resp.Data.Sources))
	}
	if resp.Data.Sources[0].SourceName != "来源-fef0c263(群聊)" {
		t.Errorf("detail source_name = %q, want deterministic fallback %q",
			resp.Data.Sources[0].SourceName, "来源-fef0c263(群聊)")
	}
}

// TestListSummaries_EmptySourceNameResolvedLive mirrors the detail test on
// the list endpoint (the summary-list page renders the same chip data).
func TestListSummaries_EmptySourceNameResolvedLive(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	imDB.Exec(`CREATE TABLE IF NOT EXISTS "group" (group_no TEXT, name TEXT, space_id TEXT, status INTEGER, updated_at DATETIME)`)
	const groupNo = "1b1f1113189540b697742ef264c38091"
	seedImGroup(t, imDB, groupNo, "产品需求评审群")
	seedEmptyNameSourceTask(t, db, "TST-SRCNAME-LIST", groupNo)

	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	resp := parseListResponse(t, w)
	if resp.Data.Total != 1 {
		t.Fatalf("expected total 1, got %d", resp.Data.Total)
	}
	item, ok := resp.Data.Items[0].(map[string]interface{})
	if !ok {
		t.Fatalf("item not an object: %v", resp.Data.Items[0])
	}
	srcs, ok := item["sources"].([]interface{})
	if !ok || len(srcs) != 1 {
		t.Fatalf("expected 1 source in list item, got: %v", item["sources"])
	}
	only := srcs[0].(map[string]interface{})
	if only["source_name"] != "产品需求评审群(群聊)" {
		t.Errorf("list source_name = %v, want %q (empty stored name must be resolved live)",
			only["source_name"], "产品需求评审群(群聊)")
	}
}
