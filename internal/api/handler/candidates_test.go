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
