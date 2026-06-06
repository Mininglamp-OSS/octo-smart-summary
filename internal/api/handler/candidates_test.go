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
	imDB.Exec(`CREATE TABLE group_setting (group_no TEXT NOT NULL, uid TEXT NOT NULL, save INTEGER DEFAULT 0, top INTEGER DEFAULT 0, mute INTEGER DEFAULT 0)`)
	imDB.Exec(`CREATE TABLE conversation_extra (uid TEXT NOT NULL, channel_id TEXT NOT NULL, channel_type INTEGER DEFAULT 2, updated_at TEXT)`)
	imDB.Exec(`CREATE TABLE user (uid TEXT NOT NULL, name TEXT, robot INTEGER DEFAULT 0)`)
	imDB.Exec(`CREATE TABLE robot (robot_id TEXT, creator_uid TEXT, status INTEGER DEFAULT 1)`)
	imDB.Exec(`CREATE TABLE space_member (uid TEXT, space_id TEXT, status INTEGER DEFAULT 1)`)
	imDB.Exec(`CREATE TABLE friend (uid TEXT, to_uid TEXT, is_deleted INTEGER DEFAULT 0)`)
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
	return doCandidateRequestFilter(r, userID, chatType, keyword, "")
}

func doCandidateRequestFilter(r *gin.Engine, userID, chatType, keyword, filter string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	path := "/api/v1/summary-chat-candidates?chat_type=" + chatType
	if keyword != "" {
		path += "&keyword=" + keyword
	}
	if filter != "" {
		path += "&filter=" + filter
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

func TestSearchChatCandidates_FilterFollowed(t *testing.T) {
	imDB := setupCandidateImDB(t)

	// Two groups: user follows grp1 but not grp2
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'FollowedGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp2', 'UnfollowedGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp2', 'user1', 0)`)
	imDB.Exec(`INSERT INTO group_setting (group_no, uid, save) VALUES ('grp1', 'user1', 1)`)
	imDB.Exec(`INSERT INTO group_setting (group_no, uid, save) VALUES ('grp2', 'user1', 0)`)

	// Thread under followed group
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Thread In Followed', 'grp1', 1, 5)`)
	// Thread under unfollowed group
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (2, 'th002', 'Thread In Unfollowed', 'grp2', 1, 3)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequestFilter(r, "user1", "", "", "followed")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Should get: FollowedGroup + Thread In Followed = 2 items
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 items (1 group + 1 thread), got %d: %v", len(resp.Data), resp.Data)
	}

	names := map[string]bool{}
	for _, item := range resp.Data {
		names[item["name"].(string)] = true
		// All items should have is_followed=true
		if item["is_followed"] != true {
			t.Errorf("expected is_followed=true for %v, got %v", item["name"], item["is_followed"])
		}
	}
	if !names["FollowedGroup"] {
		t.Errorf("expected FollowedGroup in results, got %v", names)
	}
	if !names["Thread In Followed"] {
		t.Errorf("expected Thread In Followed in results, got %v", names)
	}
	if names["UnfollowedGroup"] || names["Thread In Unfollowed"] {
		t.Errorf("unfollowed items should not appear, got %v", names)
	}
}

func TestSearchChatCandidates_FilterRecent(t *testing.T) {
	imDB := setupCandidateImDB(t)

	// Two groups, only grp1 has recent conversation activity
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'RecentGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp2', 'InactiveGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp2', 'user1', 0)`)
	imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type, updated_at) VALUES ('user1', 'grp1', 2, '2026-06-06T10:00:00Z')`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequestFilter(r, "user1", "", "", "recent")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Should get only RecentGroup (grp1 has conversation_extra entry)
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 item, got %d: %v", len(resp.Data), resp.Data)
	}
	if resp.Data[0]["name"] != "RecentGroup" {
		t.Errorf("expected RecentGroup, got %v", resp.Data[0]["name"])
	}
	if resp.Data[0]["last_active_at"] != "2026-06-06T10:00:00Z" {
		t.Errorf("expected last_active_at=2026-06-06T10:00:00Z, got %v", resp.Data[0]["last_active_at"])
	}
}

func TestSearchChatCandidates_FilterEmptyIsBackwardCompatible(t *testing.T) {
	imDB := setupCandidateImDB(t)

	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'Group1', 'space1', 1)`)
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp2', 'Group2', 'space1', 1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp2', 'user1', 0)`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	// No filter = returns all groups (backward compatible)
	w := doCandidateRequestFilter(r, "user1", "group", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 groups with empty filter, got %d", len(resp.Data))
	}
}

func TestSearchChatCandidates_FilterRecentThread(t *testing.T) {
	imDB := setupCandidateImDB(t)

	// One group with two threads. The parent group is NOT recently active, and only
	// one thread (th001 -> 'grp1____th001') appears in conversation_extra as recent.
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'ParentGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Recent Thread', 'grp1', 1, 5)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (2, 'th002', 'Stale Thread', 'grp1', 1, 7)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)
	// channel_type=5 thread conversation, keyed by composite 'group_no____short_id'.
	imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type, updated_at) VALUES ('user1', 'grp1____th001', 5, '2026-06-06T12:00:00Z')`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequestFilter(r, "user1", "thread", "", "recent")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Only the thread whose composite channel id is recent should surface.
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 recent thread, got %d: %v", len(resp.Data), resp.Data)
	}
	got := resp.Data[0]
	if got["name"] != "Recent Thread" {
		t.Errorf("expected 'Recent Thread', got %v", got["name"])
	}
	if got["chat_id"] != "grp1____th001" {
		t.Errorf("expected chat_id=grp1____th001, got %v", got["chat_id"])
	}
	// last_active_at must be the thread's own activity, not the parent group's.
	if got["last_active_at"] != "2026-06-06T12:00:00Z" {
		t.Errorf("expected thread last_active_at=2026-06-06T12:00:00Z, got %v", got["last_active_at"])
	}
}

func TestSearchChatCandidates_FilterRecentGroupNotLeakingThreads(t *testing.T) {
	imDB := setupCandidateImDB(t)

	// Parent group IS recently active (channel_type=2), but its threads are NOT.
	// The recent group must surface; none of its threads should leak in as recent.
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'RecentGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status, message_count) VALUES (1, 'th001', 'Thread A', 'grp1', 1, 5)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)
	imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type, updated_at) VALUES ('user1', 'grp1', 2, '2026-06-06T10:00:00Z')`)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequestFilter(r, "user1", "", "", "recent")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 item (group only), got %d: %v", len(resp.Data), resp.Data)
	}
	if resp.Data[0]["chat_type"] != "group" || resp.Data[0]["name"] != "RecentGroup" {
		t.Errorf("expected RecentGroup, got %v", resp.Data[0])
	}
}

func TestSearchChatCandidates_FilterRecentMixedGroupDirectOrdering(t *testing.T) {
	imDB := setupCandidateImDB(t)

	// A recent group and a recent direct chat with distinct activity times.
	// The direct is more recent, so it must sort ahead of the group.
	const peerUID = "abcdef0123456789abcdef0123456789" // 32-char hex
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('grp1', 'RecentGroup', 'space1', 1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1', 'user1', 0)`)
	imDB.Exec(`INSERT INTO user (uid, name, robot) VALUES (?, 'Peer Person', 0)`, peerUID)
	imDB.Exec(`INSERT INTO space_member (uid, space_id, status) VALUES (?, 'space1', 1)`, peerUID)

	// Group active earlier, direct active later.
	imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type, updated_at) VALUES ('user1', 'grp1', 2, '2026-06-06T09:00:00Z')`)
	imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type, updated_at) VALUES (?, ?, 1, '2026-06-06T11:00:00Z')`, "user1", peerUID)

	h := NewCandidateHandler(imDB, -1)
	h.collate = ""
	r := setupCandidateRouter(h)

	w := doCandidateRequestFilter(r, "user1", "", "", "recent")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 items (group + direct), got %d: %v", len(resp.Data), resp.Data)
	}
	// Most recent first: the direct (11:00) before the group (09:00).
	if resp.Data[0]["chat_type"] != "direct" {
		t.Errorf("expected direct first (most recent), got %v", resp.Data[0])
	}
	if resp.Data[0]["name"] != "Peer Person" {
		t.Errorf("expected 'Peer Person' first, got %v", resp.Data[0]["name"])
	}
	if resp.Data[1]["chat_type"] != "group" {
		t.Errorf("expected group second, got %v", resp.Data[1])
	}
	if resp.Data[1]["last_active_at"] != "2026-06-06T09:00:00Z" {
		t.Errorf("expected group last_active_at=2026-06-06T09:00:00Z, got %v", resp.Data[1]["last_active_at"])
	}
}
