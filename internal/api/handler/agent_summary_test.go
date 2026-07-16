//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupAgentSummaryTestDB creates an in-memory test DB with all required tables
func setupAgentSummaryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	// Auto-migrate all tables needed by CreateAgentSummary
	if err := db.AutoMigrate(
		&model.AgentMessage{},
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
	); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	return db
}

// mockTokenResolver 已在 auth_test.go 定义,此处复用(不重复声明)。
// 该 mock 实现 middleware.TokenResolver 接口 (ResolveUID)。

// setupAgentSummaryRouter sets up a test gin router with the handler
func setupAgentSummaryRouter(h *AgentSummaryHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/agent", h.CreateAgentSummary)
	return r
}

// TestCreateAgentSummary_ProvidedOriginChannelDirectPass tests backward compatibility:
// when origin_channel_id and origin_channel_type are explicitly provided with valid values,
// behavior is unchanged (no resolve, direct validation and pass).
func TestCreateAgentSummary_ProvidedOriginChannelDirectPass(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed: session with assistant message (deliverable content)
	sessionID := "session-valid-001"
	content := "This is the agent-generated summary."
	db.Create(&model.AgentMessage{
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
	})

	// Request with explicitly provided origin_channel_id and type
	channelID := "CH-PROVIDED"
	reqBody := map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   channelID,
		"origin_channel_type": 1,
		"title":               "Test Summary",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 0 {
		t.Errorf("expected code=0, got %v", resp["code"])
	}

	// Verify DB persisted the provided origin_channel values
	var task model.SummaryTask
	db.First(&task)
	if task.OriginChannelID != channelID {
		t.Errorf("expected origin_channel_id=%s, got %s", channelID, task.OriginChannelID)
	}
	if task.OriginChannelType != 1 {
		t.Errorf("expected origin_channel_type=1, got %d", task.OriginChannelType)
	}
}

// TestCreateAgentSummary_NotProvidedResolveSuccess tests the new behavior:
// when origin_channel_id is not provided (nil), the handler resolves from session
// and successfully creates the task with the resolved values.
func TestCreateAgentSummary_NotProvidedResolveSuccess(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed: session with assistant message (deliverable) and fetch_channel tool call
	sessionID := "session-resolve-001"
	content := "Agent summary from resolved channel."
	toolCallsJSON := `[{"id":"call_fetch","type":"function","function":{"name":"fetch_channel","arguments":"{\"channel_id\":\"CH-RESOLVED\",\"channel_type\":2}"}}]`

	db.Create(&model.AgentMessage{
		SessionID: sessionID,
		Role:      "assistant",
		ToolCalls: &toolCallsJSON,
	})
	db.Create(&model.AgentMessage{
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
	})

	// Request WITHOUT origin_channel_id (omit the field entirely)
	reqBody := map[string]interface{}{
		"session_id": sessionID,
		"title":      "Resolved Summary",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 0 {
		t.Errorf("expected code=0, got %v", resp["code"])
	}

	// Verify DB used the resolved values
	var task model.SummaryTask
	db.First(&task)
	if task.OriginChannelID != "CH-RESOLVED" {
		t.Errorf("expected resolved origin_channel_id=CH-RESOLVED, got %s", task.OriginChannelID)
	}
	if task.OriginChannelType != 2 {
		t.Errorf("expected resolved origin_channel_type=2, got %d", task.OriginChannelType)
	}
}

// TestCreateAgentSummary_NotProvidedResolveFailure tests that when origin_channel_id
// is not provided and the session has no fetch_channel tool call, the handler
// returns 400 40001 with the new error message.
func TestCreateAgentSummary_NotProvidedResolveFailure(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed: session with only assistant content, no fetch_channel tool call
	sessionID := "session-no-fetch"
	content := "Summary without origin channel."
	db.Create(&model.AgentMessage{
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
	})

	// Request WITHOUT origin_channel_id
	reqBody := map[string]interface{}{
		"session_id": sessionID,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}

	expectedMsg := "origin_channel_id 未传且无法从 session 反查(session 无 fetch_channel 调用)"
	if resp["message"].(string) != expectedMsg {
		t.Errorf("expected new message, got: %s", resp["message"])
	}
}

// TestCreateAgentSummary_ExplicitlyEmptyString tests that when origin_channel_id
// is explicitly provided as an empty string, the handler returns the old error message.
func TestCreateAgentSummary_ExplicitlyEmptyString(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed: session with assistant content
	sessionID := "session-empty-string"
	content := "Summary content."
	db.Create(&model.AgentMessage{
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
	})

	// Request with origin_channel_id explicitly set to empty string
	reqBody := map[string]interface{}{
		"session_id":        sessionID,
		"origin_channel_id": "", // explicitly empty
	}
	bodyBytes, _ := json.Marshal(reqBody)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}

	expectedMsg := "origin_channel_id 不能为空"
	if resp["message"].(string) != expectedMsg {
		t.Errorf("expected old message '%s', got: %s", expectedMsg, resp["message"])
	}
}

// TestCreateAgentSummary_InvalidChannelType tests that when origin_channel_type
// is out of valid range (1-3), the handler returns 400 40001 with the original message.
func TestCreateAgentSummary_InvalidChannelType(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed: session with assistant content
	sessionID := "session-invalid-type"
	content := "Summary content."
	db.Create(&model.AgentMessage{
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
	})

	// Request with invalid origin_channel_type (0 or 4+)
	reqBody := map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   "CH-VALID",
		"origin_channel_type": 0, // invalid (below 1)
	}
	bodyBytes, _ := json.Marshal(reqBody)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}

	expectedMsg := "origin_channel_type 必须是 1(群)/2(thread)/3(DM)"
	if resp["message"].(string) != expectedMsg {
		t.Errorf("expected type error message, got: %s", resp["message"])
	}
}
