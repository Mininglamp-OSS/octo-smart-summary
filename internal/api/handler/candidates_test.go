package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupCandidateImDB(t *testing.T) *gorm.DB {
	t.Helper()
	imDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec(`CREATE TABLE "group" (group_no TEXT NOT NULL, name TEXT, space_id TEXT, status INTEGER DEFAULT 1)`)
	imDB.Exec(`CREATE TABLE thread (id INTEGER PRIMARY KEY, short_id TEXT, name TEXT, group_no TEXT, status INTEGER DEFAULT 1, message_count INTEGER DEFAULT 0)`)
	imDB.Exec(`CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)`)
	return imDB
}

func setupCandidateRouter(h *CandidateHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summary-chat-candidates", h.SearchChatCandidates)
	return r
}

func doCandidateRequest(r *gin.Engine, userID, chatType, keyword string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	path := "/api/v1/summary-chat-candidates?chat_type=" + chatType
	if keyword != "" {
		path += "&keyword=" + keyword
	}
	req := httptest.NewRequest("GET", path, nil)
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

// doCandidateRequestRaw issues a request with a verbatim query string, allowing
// arbitrary params such as include_archived.
func doCandidateRequestRaw(r *gin.Engine, userID, query string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/summary-chat-candidates?"+query, nil)
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func TestSearchChatCandidates_ThreadUsesGroupMember(t *testing.T) {
	imDB := setupCandidateImDB(t)

	// Seed: user is a member of group "grp1" which has two threads.
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'TestGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Thread A', 'grp1', 1, 5)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (2, 'th002', 'Thread B', 'grp1', 1, 3)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = "" // SQLite does not support MySQL collation clauses
	r := setupCandidateRouter(h)

	w := doCandidateRequest(r, "user1", "thread", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 threads, got %d: %v", len(resp.Data), resp.Data)
	}

	names := map[string]bool{}
	for _, item := range resp.Data {
		names[item["name"].(string)] = true
		if item["chat_type"] != "thread" {
			t.Errorf("expected chat_type=thread, got %v", item["chat_type"])
		}
	}
	if !names["Thread A"] || !names["Thread B"] {
		t.Errorf("expected Thread A and Thread B, got %v", names)
	}
}

func TestSearchChatCandidates_ThreadExcludesDeletedGroupMember(t *testing.T) {
	imDB := setupCandidateImDB(t)

	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'TestGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Thread A', 'grp1', 1, 5)`)
	// User has is_deleted = 1
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'removed_user', 1)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequest(r, "removed_user", "thread", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 0 {
		t.Errorf("expected 0 threads for deleted member, got %d", len(resp.Data))
	}
}

func TestSearchChatCandidates_GroupExcludesDeletedMember(t *testing.T) {
	imDB := setupCandidateImDB(t)

	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'TestGroup', 'space1', 1)`)
	// User has is_deleted = 1
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'removed_user', 1)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequest(r, "removed_user", "group", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 0 {
		t.Errorf("expected 0 groups for deleted member, got %d", len(resp.Data))
	}
}

func TestSearchChatCandidates_ThreadExcludesNonMember(t *testing.T) {
	imDB := setupCandidateImDB(t)

	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'TestGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Thread A', 'grp1', 1, 5)`)
	// user2 is NOT in group_member at all
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'other_user', 0)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequest(r, "user2", "thread", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 0 {
		t.Errorf("expected 0 threads for non-member, got %d", len(resp.Data))
	}
}

func TestSearchChatCandidates_ThreadExcludesEmptyMessageCount(t *testing.T) {
	imDB := setupCandidateImDB(t)

	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'TestGroup', 'space1', 1)`)
	// Thread with messages - should appear
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Active Thread', 'grp1', 1, 10)`)
	// Thread with zero messages - should be filtered out
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (2, 'th002', '[文件]', 'grp1', 1, 0)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequest(r, "user1", "thread", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 thread (empty excluded), got %d: %v", len(resp.Data), resp.Data)
	}
	if resp.Data[0]["name"] != "Active Thread" {
		t.Errorf("expected 'Active Thread', got %v", resp.Data[0]["name"])
	}
}

// seedArchiveScenario seeds one group with three threads: Active (1), Archived
// (2) and Deleted (3), each with messages, and makes user1 a member.
func seedArchiveScenario(imDB *gorm.DB) {
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'TestGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Active Thread', 'grp1', 1, 5)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (2, 'th002', 'Archived Thread', 'grp1', 2, 4)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (3, 'th003', 'Deleted Thread', 'grp1', 3, 9)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)
}

func threadNameSet(data []map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, item := range data {
		if name, ok := item["name"].(string); ok {
			names[name] = true
		}
	}
	return names
}

func TestSearchChatCandidates_DefaultExcludesArchivedAndDeleted(t *testing.T) {
	imDB := setupCandidateImDB(t)
	seedArchiveScenario(imDB)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	// No include_archived param -> default false: only Active (status=1).
	w := doCandidateRequest(r, "user1", "thread", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 thread (only active), got %d: %v", len(resp.Data), resp.Data)
	}
	if resp.Data[0]["name"] != "Active Thread" {
		t.Errorf("expected 'Active Thread', got %v", resp.Data[0]["name"])
	}
	if resp.Data[0]["is_archived"] != false {
		t.Errorf("active thread is_archived should be false, got %v", resp.Data[0]["is_archived"])
	}
}

func TestSearchChatCandidates_IncludeArchivedFalseExplicit(t *testing.T) {
	imDB := setupCandidateImDB(t)
	seedArchiveScenario(imDB)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequestRaw(r, "user1", "chat_type=thread&include_archived=false")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 thread for include_archived=false, got %d: %v", len(resp.Data), resp.Data)
	}
	if resp.Data[0]["name"] != "Active Thread" {
		t.Errorf("expected 'Active Thread', got %v", resp.Data[0]["name"])
	}
}

func TestSearchChatCandidates_IncludeArchivedTrue(t *testing.T) {
	imDB := setupCandidateImDB(t)
	seedArchiveScenario(imDB)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequestRaw(r, "user1", "chat_type=thread&include_archived=true")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	// Active + Archived, but NEVER Deleted.
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 threads (active+archived), got %d: %v", len(resp.Data), resp.Data)
	}
	names := threadNameSet(resp.Data)
	if !names["Active Thread"] || !names["Archived Thread"] {
		t.Errorf("expected Active + Archived threads, got %v", names)
	}
	if names["Deleted Thread"] {
		t.Errorf("Deleted thread must never be returned, got %v", names)
	}

	// is_archived must be true only for the archived thread.
	for _, item := range resp.Data {
		want := item["name"] == "Archived Thread"
		if item["is_archived"] != want {
			t.Errorf("thread %v: is_archived = %v, want %v", item["name"], item["is_archived"], want)
		}
	}
}

func TestSearchChatCandidates_GroupAndDirectHaveIsArchivedFalse(t *testing.T) {
	imDB := setupCandidateImDB(t)
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'TestGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequest(r, "user1", "group", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 group, got %d: %v", len(resp.Data), resp.Data)
	}
	v, ok := resp.Data[0]["is_archived"]
	if !ok {
		t.Fatalf("group candidate missing is_archived field: %v", resp.Data[0])
	}
	if v != false {
		t.Errorf("group is_archived should be false, got %v", v)
	}
}
