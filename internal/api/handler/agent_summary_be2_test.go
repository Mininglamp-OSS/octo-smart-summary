//go:build cgo

package handler

// SUM-BE2 handler tests: trusted draft reference (agent_message_id +
// snapshot_version), idempotency (Idempotency-Key header), and the "agent
// draft never enters the traditional Processor" contract.
//
// These are cgo-gated because setupAgentSummaryTestDB (see
// agent_summary_test.go) uses the sqlite3 driver.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"

	"gorm.io/gorm"
)

// seedAssistantMessage writes one assistant reply into agent_message and
// returns the generated row (id, ...). All BE-2 tests reference a real DB
// message so the ownership WHERE clause in loadAgentMessageForSave has
// something to match.
func seedAssistantMessage(t *testing.T, db *gorm.DB, userID, sessionID, content string) model.AgentMessage {
	t.Helper()
	msg := model.AgentMessage{
		UserID:    userID,
		SessionID: sessionID,
		Role:      "assistant",
		Content:   content,
	}
	if err := db.Create(&msg).Error; err != nil {
		t.Fatalf("seed assistant message: %v", err)
	}
	return msg
}

func doAgentSave(t *testing.T, r http.Handler, body map[string]interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(w, req)
	return w
}

// -----------------------------------------------------------------------
// 1. Message ownership / role / session / tool-call filtering
// -----------------------------------------------------------------------

func TestCreateAgentSummary_BE2_MessageIDCrossUser_404(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed a message owned by SOMEONE ELSE on the same session id.
	other := seedAssistantMessage(t, db, "someone-else", "sess-x", "not mine")

	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-x",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    other.ID,
		"snapshot_version":    1,
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for cross-user message id, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40004 {
		t.Errorf("expected code=40004 (no output), got %v", resp["code"])
	}
	// Draft preservation contract: the row we planted must still be there.
	var count int64
	db.Model(&model.AgentMessage{}).Where("id = ?", other.ID).Count(&count)
	if count != 1 {
		t.Errorf("cross-user rejection should NOT delete the victim's draft; want 1 row, got %d", count)
	}
}

func TestCreateAgentSummary_BE2_MessageIDWrongSession_404(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed for this user but on a DIFFERENT session id.
	msg := seedAssistantMessage(t, db, "test-user", "sess-A", "hi")

	// Client asks to save that id but claims a different session.
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-B",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    msg.ID,
		"snapshot_version":    1,
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong-session id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAgentSummary_BE2_MessageIDUserRole_404(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed a role=user row (not an assistant reply).
	userRow := model.AgentMessage{
		UserID:    "test-user",
		SessionID: "sess-U",
		Role:      "user",
		Content:   "please summarize",
	}
	if err := db.Create(&userRow).Error; err != nil {
		t.Fatalf("seed user row: %v", err)
	}
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-U",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    userRow.ID,
		"snapshot_version":    1,
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for user-role id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAgentSummary_BE2_MessageIDToolCall_404(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Assistant row that carries a tool_calls payload (intermediate "call
	// this tool" step, never a save target).
	tc := "[{\"id\":\"call-1\",\"type\":\"function\"}]"
	row := model.AgentMessage{
		UserID:    "test-user",
		SessionID: "sess-T",
		Role:      "assistant",
		Content:   "invoking tool...",
		ToolCalls: &tc,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed tool-call row: %v", err)
	}
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-T",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    row.ID,
		"snapshot_version":    1,
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for tool-call id, got %d: %s", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------------------
// 2. Snapshot version conflict — AGENT_DRAFT_STALE (40901 / 409)
// -----------------------------------------------------------------------

func TestCreateAgentSummary_BE2_SnapshotVersionConflict_409(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)
	msg := seedAssistantMessage(t, db, "test-user", "sess-v", "final answer")

	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-v",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    msg.ID,
		"snapshot_version":    2, // BE-2 only writes v1
	}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for snapshot_version=2, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40901 {
		t.Errorf("expected AGENT_DRAFT_STALE code=40901, got %v", resp["code"])
	}
}

func TestCreateAgentSummary_BE2_MessageIDWithoutVersion_400(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)
	msg := seedAssistantMessage(t, db, "test-user", "sess-half", "final")

	// Half-supplied: msg id without snapshot_version.
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-half",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    msg.ID,
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for half-supplied ref, got %d: %s", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------------------
// 3. Idempotency — same key + same body = replay; same key + different body = 409
// -----------------------------------------------------------------------

func TestCreateAgentSummary_BE2_IdempotencyReplaysSameTask(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.SummaryAgentSaveIdempotency{}); err != nil {
		t.Fatalf("migrate idempotency table: %v", err)
	}
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	msg := seedAssistantMessage(t, db, "test-user", "sess-idem", "final answer")

	body := map[string]interface{}{
		"session_id":          "sess-idem",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"title":               "Weekly",
		"agent_message_id":    msg.ID,
		"snapshot_version":    1,
	}
	headers := map[string]string{"Idempotency-Key": "req-abc-001"}

	// First save.
	w1 := doAgentSave(t, r, body, headers)
	if w1.Code != http.StatusOK {
		t.Fatalf("first save want 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var r1 map[string]interface{}
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	taskID1 := r1["data"].(map[string]interface{})["task_id"]

	// Re-seed the assistant message: the first save's tx DELETEs it. A
	// replay must NOT need the draft any more (the whole point of
	// idempotency is that the client can retry without holding session
	// state). If the preflight works, we never re-read agent_message and
	// the replay succeeds.
	//
	// If BE-2 accidentally re-reads on replay, this test fails with
	// 40004 — a strong regression signal that the preflight is missing.

	w2 := doAgentSave(t, r, body, headers)
	if w2.Code != http.StatusOK {
		t.Fatalf("replay want 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var r2 map[string]interface{}
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	data2 := r2["data"].(map[string]interface{})
	if data2["task_id"] != taskID1 {
		t.Errorf("replay task_id mismatch: first=%v replay=%v", taskID1, data2["task_id"])
	}
	if data2["replayed"] != true {
		t.Errorf("replay must set data.replayed=true, got %v", data2["replayed"])
	}

	// Only ONE row in summary_task for this user — the replay must not
	// have written a second task.
	var count int64
	db.Model(&model.SummaryTask{}).Where("creator_id = ?", "test-user").Count(&count)
	if count != 1 {
		t.Errorf("idempotent replay produced %d tasks, want 1", count)
	}
}

func TestCreateAgentSummary_BE2_IdempotencyMismatch409(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.SummaryAgentSaveIdempotency{}); err != nil {
		t.Fatalf("migrate idempotency table: %v", err)
	}
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	msg1 := seedAssistantMessage(t, db, "test-user", "sess-mismatch-A", "answer A")

	body1 := map[string]interface{}{
		"session_id":          "sess-mismatch-A",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"title":               "Weekly",
		"agent_message_id":    msg1.ID,
		"snapshot_version":    1,
	}
	headers := map[string]string{"Idempotency-Key": "req-mismatch-001"}

	w1 := doAgentSave(t, r, body1, headers)
	if w1.Code != http.StatusOK {
		t.Fatalf("first save want 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var r1 map[string]interface{}
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	originalTaskID := r1["data"].(map[string]interface{})["task_id"]

	// Seed a second session + message so the second request has a valid
	// draft to reference. The DIFFERENT session id is what forces the
	// hash mismatch (session_id is a hashed axis).
	msg2 := seedAssistantMessage(t, db, "test-user", "sess-mismatch-B", "answer B")
	body2 := map[string]interface{}{
		"session_id":          "sess-mismatch-B",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"title":               "Weekly",
		"agent_message_id":    msg2.ID,
		"snapshot_version":    1,
	}

	w2 := doAgentSave(t, r, body2, headers)
	if w2.Code != http.StatusConflict {
		t.Fatalf("mismatched retry want 409, got %d: %s", w2.Code, w2.Body.String())
	}
	var r2 map[string]interface{}
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	if r2["code"].(float64) != 40009 {
		t.Errorf("expected code=40009, got %v", r2["code"])
	}
	if r2["data"].(map[string]interface{})["task_id"] != originalTaskID {
		t.Errorf("409 must echo the original task_id, got %v", r2["data"])
	}
}

// -----------------------------------------------------------------------
// 4. Agent draft never enters the traditional personal / meta Processor
// -----------------------------------------------------------------------

func TestCreateAgentSummary_BE2_AgentDraftDoesNotEnterPersonalProcessor(t *testing.T) {
	// The traditional worker (worker/personal_processor.go) picks up
	// PersonalResult rows with worker_status = PersonalStatusPending. This
	// test asserts that the Agent save path writes worker_status =
	// PersonalStatusCompleted straight away, so no worker ever sees the
	// draft. Design section 2.2: "不能把 Agent 草稿交给 personal/meta Processor
	// 再生成一次".
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	msg := seedAssistantMessage(t, db, "test-user", "sess-proc", "final content")

	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-proc",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    msg.ID,
		"snapshot_version":    1,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("save want 200, got %d: %s", w.Code, w.Body.String())
	}

	// 1. summary_task.trigger_type MUST be TriggerAgent (== 3).
	// 2. summary_task.status MUST be StatusCompleted (Agent tasks skip
	//    Pending/Processing entirely).
	// 3. NO summary_personal_result row for this task may have
	//    worker_status = PersonalStatusPending (the worker's pickup
	//    predicate) — everything must be Completed.
	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").Order("id DESC").First(&task).Error; err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.TriggerType != model.TriggerAgent {
		t.Errorf("trigger_type=%d, want TriggerAgent=%d", task.TriggerType, model.TriggerAgent)
	}
	if task.Status != model.StatusCompleted {
		t.Errorf("task.status=%d, want StatusCompleted=%d (agent path skips worker)",
			task.Status, model.StatusCompleted)
	}
	// SUM-BE2 audit-trail columns must be set.
	if task.AgentSessionID != "sess-proc" || task.AgentMessageID != msg.ID || task.SnapshotVersion != 1 {
		t.Errorf("audit trail wrong: session=%q msg_id=%d snap_v=%d (want sess-proc/%d/1)",
			task.AgentSessionID, task.AgentMessageID, task.SnapshotVersion, msg.ID)
	}

	var pendingCount int64
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND worker_status = ?", task.ID, model.PersonalStatusPending).
		Count(&pendingCount)
	if pendingCount != 0 {
		t.Errorf("agent save produced %d PersonalResult(s) with worker_status=Pending — the traditional Processor would pick them up",
			pendingCount)
	}
}

// -----------------------------------------------------------------------
// 5. Legacy path (no BE-2 fields) still works and now writes the audit trail
// -----------------------------------------------------------------------

func TestCreateAgentSummary_BE2_LegacyPathStillWorksAndWritesAudit(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	msg := seedAssistantMessage(t, db, "test-user", "sess-legacy", "old client answer")

	// No agent_message_id, no snapshot_version, no Idempotency-Key. This
	// is what a pre-FE-2 client sends. Must succeed AND the audit trail
	// must be filled from the resolved (latest-assistant) row.
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-legacy",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy save want 200, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").First(&task).Error; err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.AgentSessionID != "sess-legacy" {
		t.Errorf("audit AgentSessionID=%q want sess-legacy", task.AgentSessionID)
	}
	if task.AgentMessageID != msg.ID {
		t.Errorf("audit AgentMessageID=%d want %d (resolved from latest assistant)", task.AgentMessageID, msg.ID)
	}
	if task.SnapshotVersion != 1 {
		t.Errorf("audit SnapshotVersion=%d want 1", task.SnapshotVersion)
	}
}

// -----------------------------------------------------------------------
// 6. Failed save preserves the draft (agent_message row is not deleted)
// -----------------------------------------------------------------------

func TestCreateAgentSummary_BE2_ValidationFailureKeepsDraft(t *testing.T) {
	// A save that fails validation (here: snapshot_version=2 → 409) must
	// NOT delete the client's draft messages, so they can retry with a
	// corrected snapshot_version and reach the same content. Design
	// section 7.7: "保存失败时保留草稿,可安全重试".
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	// Seed two messages (a user turn + an assistant reply) so we can
	// verify both survive a failed save.
	userTurn := model.AgentMessage{UserID: "test-user", SessionID: "sess-keep", Role: "user", Content: "make it short"}
	if err := db.Create(&userTurn).Error; err != nil {
		t.Fatalf("seed user turn: %v", err)
	}
	msg := seedAssistantMessage(t, db, "test-user", "sess-keep", "the summary")

	// Bad request — stale snapshot version.
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          "sess-keep",
		"origin_channel_id":   "chan-1",
		"origin_channel_type": 1,
		"agent_message_id":    msg.ID,
		"snapshot_version":    99,
	}, nil)
	if w.Code == http.StatusOK {
		t.Fatalf("expected non-2xx, got 200: %s", w.Body.String())
	}

	// Both rows must still be present.
	var count int64
	db.Model(&model.AgentMessage{}).
		Where("user_id = ? AND session_id = ?", "test-user", "sess-keep").
		Count(&count)
	if count != 2 {
		t.Errorf("failed save should not touch the draft; want 2 messages, got %d", count)
	}

	// And no task was created for this user.
	var taskCount int64
	db.Model(&model.SummaryTask{}).Where("creator_id = ?", "test-user").Count(&taskCount)
	if taskCount != 0 {
		t.Errorf("failed save should not create a task, got %d", taskCount)
	}
}
