package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Test 1: 有 [n] 标记 + 有完整 tool 轨迹 → citations 非空、结构正确
func TestBuildCitationsForSession_WithMarkersAndMessages(t *testing.T) {
	agent.ResetForTest()
	db, skip := setupTestDB(t)
	if skip {
		return
	}

	// Store messages in cache
	cache := agent.GetMessageCache()
	messages := []pipeline.Message{
		{
			ChannelID:   "channel-1",
			MessageSeq:  100,
			SenderUID:   "user-1",
			SenderName:  "Alice",
			Content:     "Hello world",
			Timestamp:   1704103200,
			SendTime:    "2024-01-01T10:00:00Z",
			ChannelType: 1,
		},
		{
			ChannelID:   "channel-1",
			MessageSeq:  101,
			SenderUID:   "user-2",
			SenderName:  "Bob",
			Content:     "Hi there",
			Timestamp:   1704103300,
			SendTime:    "2024-01-01T10:05:00Z",
			ChannelType: 1,
		},
	}
	handle := cache.Store(messages, "test-user")

	// Insert tool message with handle
	toolReturn := map[string]interface{}{
		"messages_handle": handle,
		"total":           2,
	}
	content, _ := json.Marshal(toolReturn)
	msg := model.AgentMessage{
		UserID: "test-user", SessionID: "session-1",
		Role:      "tool",
		Content:   string(content),
		Name:      "fetch_channel",
	}
	db.Create(&msg)

	h := &AgentSummaryHandler{db: db}

	// Test: content with [1][2] markers should produce 2 citations
	contentWithMarker := "Alice said hello [1] and Bob replied [2]."
	cits, err := h.buildCitationsForSession(context.Background(), "session-1", contentWithMarker, "test-user")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(cits) != 2 {
		t.Errorf("expected 2 citations with [1][2] markers, got %d", len(cits))
	}

	// Verify citation structure matches traditional workflow
	for _, cit := range cits {
		if cit.Index == 0 {
			t.Error("Index should be 1-indexed, got 0")
		}
		if cit.Sender == "" {
			t.Error("Sender should not be empty")
		}
		if cit.Content == "" {
			t.Error("Content should not be empty")
		}
		// These fields should match pipeline.Message structure
		if cit.ChannelID == "" || cit.MessageSeq == 0 {
			t.Error("ChannelID/MessageSeq should be populated for deep links")
		}
	}
}

// Test 2: 无 [n] 标记 → 返回空数组
func TestBuildCitationsForSession_NoMarkers(t *testing.T) {
	agent.ResetForTest()
	db, skip := setupTestDB(t)
	if skip {
		return
	}

	// Store messages in cache
	cache := agent.GetMessageCache()
	messages := []pipeline.Message{
		{
			ChannelID:  "channel-1",
			MessageSeq: 100,
			SenderName: "Alice",
			Content:    "Hello",
			Timestamp:  1704103200,
		},
	}
	handle := cache.Store(messages, "test-user")

	// Insert tool message
	toolReturn := map[string]interface{}{
		"messages_handle": handle,
		"total":           1,
	}
	content, _ := json.Marshal(toolReturn)
	msg := model.AgentMessage{
		UserID: "test-user", SessionID: "session-1",
		Role:      "tool",
		Content:   string(content),
		Name:      "fetch_channel",
	}
	db.Create(&msg)

	h := &AgentSummaryHandler{db: db}

	// Test: content without [n] markers should return empty array
	contentNoMarkers := "This is a summary without any citation markers."
	cits, err := h.buildCitationsForSession(context.Background(), "session-1", contentNoMarkers, "test-user")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(cits) != 0 {
		t.Errorf("expected empty citations without markers, got %d", len(cits))
	}
}

// Test 3a: cache 过期/无 tool message → 返回空数组 + err == nil (正常路径)
func TestBuildCitationsForSession_EmptyToolTrace(t *testing.T) {
	agent.ResetForTest()
	db, skip := setupTestDB(t)
	if skip {
		return
	}

	h := &AgentSummaryHandler{db: db}

	// Scenario 1: handle 不存在于 cache (cache miss)
	toolReturn := map[string]interface{}{
		"messages_handle": "expired-handle-123",
		"total":           10,
	}
	content, _ := json.Marshal(toolReturn)
	msg := model.AgentMessage{
		UserID: "test-user", SessionID: "session-1",
		Role:      "tool",
		Content:   string(content),
		Name:      "fetch_channel",
	}
	db.Create(&msg)

	cits, err := h.buildCitationsForSession(context.Background(), "session-1", "Some content [1]", "test-user")
	if err != nil {
		t.Errorf("expected no error on cache miss, got %v", err)
	}
	if len(cits) != 0 {
		t.Errorf("expected empty citations on cache miss, got %d", len(cits))
	}

	// Scenario 2: session 下没有任何 tool message
	cits2, err2 := h.buildCitationsForSession(context.Background(), "no-such-session", "Content [1]", "test-user")
	if err2 != nil {
		t.Errorf("expected no error when no tool messages, got %v", err2)
	}
	if len(cits2) != 0 {
		t.Errorf("expected empty citations when no tool messages, got %d", len(cits2))
	}
}

// Test 3b: DB 查询失败 → buildCitationsForSession 返回 err != nil
// (handler 层 agent_summary.go:212-217 会把这个 err 吞成空数组+log,不 500)
func TestBuildCitationsForSession_DBQueryError(t *testing.T) {
	agent.ResetForTest()
	db, skip := setupTestDB(t)
	if skip {
		return
	}

	// Close the underlying connection to force query error
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get underlying DB: %v", err)
	}
	sqlDB.Close()

	h := &AgentSummaryHandler{db: db}

	// Test: DB query error should return err != nil
	cits, err := h.buildCitationsForSession(context.Background(), "session-1", "Some content [1]", "test-user")

	// buildCitationsForSession DOES return error when DB query fails
	// (agent_summary_citations.go:41-44)
	if err == nil {
		t.Error("expected error from DB query failure, got nil")
	}

	// The handler layer (agent_summary.go:212-217) catches this error
	// and falls back to cits=nil (empty array) without blocking persist
	if cits != nil {
		t.Errorf("expected nil citations on error, got %v", cits)
	}

	// This test verifies the "fallback doesn't block persist" red line:
	// - buildCitationsForSession reports error honestly (err != nil)
	// - handler logs it and uses empty citations instead of 500ing
}

// Test 4: peek_channel 采样式多条不同消息,断言不会互相吞掉
func TestBuildCitationsForSession_PeekChannelMultipleMessages(t *testing.T) {
	agent.ResetForTest()
	db, skip := setupTestDB(t)
	if skip {
		return
	}

	// Simulate peek_channel: returns handle (full messages in cache)
	cache := agent.GetMessageCache()
	messages := []pipeline.Message{
		{
			ChannelID:   "channel-1",
			MessageSeq:  100,
			SenderUID:   "user-1",
			SenderName:  "Alice",
			Content:     "Message one",
			Timestamp:   1704103200,
			SendTime:    "2024-01-01T10:00:00Z",
			ChannelType: 1,
		},
		{
			ChannelID:   "channel-1",
			MessageSeq:  101,
			SenderUID:   "user-2",
			SenderName:  "Bob",
			Content:     "Message two",
			Timestamp:   1704103300,
			SendTime:    "2024-01-01T10:05:00Z",
			ChannelType: 1,
		},
		{
			ChannelID:   "channel-1",
			MessageSeq:  102,
			SenderUID:   "user-3",
			SenderName:  "Charlie",
			Content:     "Message three",
			Timestamp:   1704103400,
			SendTime:    "2024-01-01T10:10:00Z",
			ChannelType: 1,
		},
	}
	handle := cache.Store(messages, "test-user")

	// peek_channel returns handle (full messages in cache)
	toolReturn := map[string]interface{}{
		"messages_handle": handle,
		"total":           3,
	}
	content, _ := json.Marshal(toolReturn)
	msg := model.AgentMessage{
		UserID: "test-user", SessionID: "session-1",
		Role:      "tool",
		Content:   string(content),
		Name:      "peek_channel",
	}
	db.Create(&msg)

	h := &AgentSummaryHandler{db: db}

	// Test: all 3 messages should be recovered from cache via handle
	contentWithMarkers := "Alice [1], Bob [2], and Charlie [3] all spoke."
	cits, err := h.buildCitationsForSession(context.Background(), "session-1", contentWithMarkers, "test-user")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// Should have 3 citations, not collapsed to 1
	if len(cits) != 3 {
		t.Errorf("expected 3 citations (one per message), got %d", len(cits))
	}

	// Verify each has unique Index and proper fields
	seenIndexes := make(map[int]bool)
	for _, cit := range cits {
		if seenIndexes[cit.Index] {
			t.Errorf("duplicate Index %d found", cit.Index)
		}
		seenIndexes[cit.Index] = true

		if cit.ChannelID == "" || cit.MessageSeq == 0 {
			t.Error("ChannelID/MessageSeq should not be empty/zero (needed for deep links)")
		}
		if cit.Sender == "" || cit.Content == "" {
			t.Error("Sender/Content should not be empty")
		}
	}

	// Verify Index values are 1, 2, 3 (in time order)
	if len(cits) == 3 {
		if cits[0].Index != 1 || cits[1].Index != 2 || cits[2].Index != 3 {
			t.Errorf("expected Index [1,2,3], got [%d,%d,%d]",
				cits[0].Index, cits[1].Index, cits[2].Index)
		}
	}
}

// Test helpers

func setupTestDB(t *testing.T) (*gorm.DB, bool) {
	// 使用 ":memory:"(不加 file:: / ?cache=shared)确保每个测试独立 DB 不串。
	// SUM-158 P1-B5 修复：共享缓存曾导致 owner-scoped 查询跨 test 污染。
	// skill cgo-test-recipe.md 项目公约：新测试都必须用私有 DB。
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil, true
	}

	if err := db.AutoMigrate(&model.AgentMessage{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	return db, false
}
