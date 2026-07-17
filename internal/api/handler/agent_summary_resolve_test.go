package handler

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupResolveTestDB creates an in-memory SQLite DB for testing
// Returns (db, skip=true) when CGO is not available
func setupResolveTestDB(t *testing.T) (*gorm.DB, bool) {
	// 使用 ":memory:" 独立 DB(SUM-158 P1-B5)。
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

// TestResolveOriginChannelFromSession_ValidFetchChannel tests the happy path
// where a fetch_channel call exists and we successfully extract channel info.
func TestResolveOriginChannelFromSession_ValidFetchChannel(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	handler := &AgentSummaryHandler{db: db}

	sessionID := "test-session-001"
	toolCallsJSON := `[{"id":"call_abc","type":"function","function":{"name":"fetch_channel","arguments":"{\"channel_id\":\"CH123\",\"channel_type\":2,\"time_start\":\"2024-01-01T00:00:00Z\",\"time_end\":\"2024-01-02T00:00:00Z\"}"}}]`

	msg := model.AgentMessage{
		UserID: "test-user", SessionID: sessionID,
		Role:      "assistant",
		Content:   "",
		ToolCalls: &toolCallsJSON,
	}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatalf("failed to insert test message: %v", err)
	}

	channelID, channelType, err := handler.resolveOriginChannelFromSession(context.Background(), sessionID, "test-user")

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if channelID != "CH123" {
		t.Errorf("expected channel_id=CH123, got %s", channelID)
	}
	if channelType != 2 {
		t.Errorf("expected channel_type=2, got %d", channelType)
	}
}

// TestResolveOriginChannelFromSession_DefaultChannelType tests that when
// channel_type is 0 or missing, it defaults to 1 (group).
func TestResolveOriginChannelFromSession_DefaultChannelType(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	handler := &AgentSummaryHandler{db: db}

	sessionID := "test-session-002"
	// channel_type explicitly 0
	toolCallsJSON := `[{"id":"call_xyz","type":"function","function":{"name":"fetch_channel","arguments":"{\"channel_id\":\"CH456\",\"channel_type\":0}"}}]`

	msg := model.AgentMessage{
		UserID: "test-user", SessionID: sessionID,
		Role:      "assistant",
		ToolCalls: &toolCallsJSON,
	}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatalf("failed to insert test message: %v", err)
	}

	channelID, channelType, err := handler.resolveOriginChannelFromSession(context.Background(), sessionID, "test-user")

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if channelID != "CH456" {
		t.Errorf("expected channel_id=CH456, got %s", channelID)
	}
	if channelType != 1 {
		t.Errorf("expected channel_type=1 (defaulted), got %d", channelType)
	}
}

// TestResolveOriginChannelFromSession_NoFetchChannel tests the case where
// the session has tool calls but none are fetch_channel.
func TestResolveOriginChannelFromSession_NoFetchChannel(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	handler := &AgentSummaryHandler{db: db}

	sessionID := "test-session-003"
	// Only other tools, no fetch_channel
	toolCallsJSON := `[{"id":"call_time","type":"function","function":{"name":"get_current_time","arguments":"{}"}}]`

	msg := model.AgentMessage{
		UserID: "test-user", SessionID: sessionID,
		Role:      "assistant",
		ToolCalls: &toolCallsJSON,
	}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatalf("failed to insert test message: %v", err)
	}

	channelID, channelType, err := handler.resolveOriginChannelFromSession(context.Background(), sessionID, "test-user")

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if channelID != "" {
		t.Errorf("expected empty channel_id, got %s", channelID)
	}
	if channelType != 0 {
		t.Errorf("expected channel_type=0, got %d", channelType)
	}
}

// TestResolveOriginChannelFromSession_EmptySession tests that an empty session
// (no messages) returns empty without error.
func TestResolveOriginChannelFromSession_EmptySession(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	handler := &AgentSummaryHandler{db: db}

	channelID, channelType, err := handler.resolveOriginChannelFromSession(context.Background(), "no-such-session", "test-user")

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if channelID != "" {
		t.Errorf("expected empty channel_id, got %s", channelID)
	}
	if channelType != 0 {
		t.Errorf("expected channel_type=0, got %d", channelType)
	}
}

// TestResolveOriginChannelFromSession_FirstCall tests that when multiple
// fetch_channel calls exist, we take the first one (chronologically earliest).
func TestResolveOriginChannelFromSession_FirstCall(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	handler := &AgentSummaryHandler{db: db}

	sessionID := "test-session-004"

	// First message with fetch_channel for CH-FIRST
	toolCalls1 := `[{"id":"call_1","type":"function","function":{"name":"fetch_channel","arguments":"{\"channel_id\":\"CH-FIRST\",\"channel_type\":1}"}}]`
	msg1 := model.AgentMessage{
		UserID: "test-user", SessionID: sessionID,
		Role:      "assistant",
		ToolCalls: &toolCalls1,
	}
	if err := db.Create(&msg1).Error; err != nil {
		t.Fatalf("failed to insert test message 1: %v", err)
	}

	// Second message with fetch_channel for CH-SECOND
	toolCalls2 := `[{"id":"call_2","type":"function","function":{"name":"fetch_channel","arguments":"{\"channel_id\":\"CH-SECOND\",\"channel_type\":3}"}}]`
	msg2 := model.AgentMessage{
		UserID: "test-user", SessionID: sessionID,
		Role:      "assistant",
		ToolCalls: &toolCalls2,
	}
	if err := db.Create(&msg2).Error; err != nil {
		t.Fatalf("failed to insert test message 2: %v", err)
	}

	channelID, channelType, err := handler.resolveOriginChannelFromSession(context.Background(), sessionID, "test-user")

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if channelID != "CH-FIRST" {
		t.Errorf("expected channel_id=CH-FIRST (first call), got %s", channelID)
	}
	if channelType != 1 {
		t.Errorf("expected channel_type=1, got %d", channelType)
	}
}

// TestResolveOriginChannelFromSession_SkipsMalformedJSON tests that malformed
// tool_calls JSON is skipped gracefully without failing the entire resolve.
func TestResolveOriginChannelFromSession_SkipsMalformedJSON(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	handler := &AgentSummaryHandler{db: db}

	sessionID := "test-session-005"

	// First message with malformed JSON
	malformed := "not valid json"
	msg1 := model.AgentMessage{
		UserID: "test-user", SessionID: sessionID,
		Role:      "assistant",
		ToolCalls: &malformed,
	}
	if err := db.Create(&msg1).Error; err != nil {
		t.Fatalf("failed to insert test message 1: %v", err)
	}

	// Second message with valid fetch_channel
	validToolCalls := `[{"id":"call_ok","type":"function","function":{"name":"fetch_channel","arguments":"{\"channel_id\":\"CH-VALID\",\"channel_type\":2}"}}]`
	msg2 := model.AgentMessage{
		UserID: "test-user", SessionID: sessionID,
		Role:      "assistant",
		ToolCalls: &validToolCalls,
	}
	if err := db.Create(&msg2).Error; err != nil {
		t.Fatalf("failed to insert test message 2: %v", err)
	}

	channelID, channelType, err := handler.resolveOriginChannelFromSession(context.Background(), sessionID, "test-user")

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if channelID != "CH-VALID" {
		t.Errorf("expected channel_id=CH-VALID (skips malformed), got %s", channelID)
	}
	if channelType != 2 {
		t.Errorf("expected channel_type=2, got %d", channelType)
	}
}

// TestResolveOriginChannelFromSession_DBError tests that a real DB error
// (not "not found") is returned as an error, not empty result.
// We simulate this by using a closed database connection.
func TestResolveOriginChannelFromSession_DBError(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	
	// Close the DB connection to force an error
	sqlDB, _ := db.DB()
	sqlDB.Close()
	
	handler := &AgentSummaryHandler{db: db}

	channelID, channelType, err := handler.resolveOriginChannelFromSession(context.Background(), "any-session", "test-user")

	// Should return an error (not nil), and empty values
	if err == nil {
		t.Error("expected DB error, got nil")
	}
	if channelID != "" {
		t.Errorf("expected empty channel_id on error, got %s", channelID)
	}
	if channelType != 0 {
		t.Errorf("expected channel_type=0 on error, got %d", channelType)
	}
}
