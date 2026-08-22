//go:build cgo

package handler

// Session-Finalize v0 handler tests. cgo-gated for the same reason the BE-2
// suite is: setupAgentSummaryTestDB uses the sqlite3 driver.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

func finalizeTestPtr(s string) *string { return &s }

func setupFinalizeRouter(h *AgentSummaryHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/agent/finalize", h.FinalizeAgentSummary)
	return r
}

func doFinalize(t *testing.T, r http.Handler, body map[string]interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent/finalize", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(w, req)
	return w
}

func finalizeTaskID(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	var resp struct {
		Code int `json:"code"`
		Data struct {
			TaskID int64 `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	return resp.Data.TaskID
}

// A finalize task must be born Pending (so the poller claims it) with the
// TriggerAgentFinalize discriminator and a POSITIVE freeze bound. The bound is
// the whole idempotency story: without it the worker would merge replies the
// user never saw when they clicked save.
func TestFinalize_CreatesPendingTaskWithFreezeBound(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	seedAssistantMessage(t, db, "test-user", "sess-fin-1", "第一段结论")
	newest := seedAssistantMessage(t, db, "test-user", "sess-fin-1", "第二段结论")

	w := doFinalize(t, r, map[string]interface{}{"session_id": "sess-fin-1", "title": "定稿"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.First(&task, finalizeTaskID(t, w)).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.Status != model.StatusPending {
		t.Errorf("status = %d, want Pending — the poller only claims Pending rows", task.Status)
	}
	if task.TriggerType != model.TriggerAgentFinalize {
		t.Errorf("trigger_type = %d, want TriggerAgentFinalize (routes the worker away from the fetch pipeline)", task.TriggerType)
	}
	if task.AgentMessageID != newest.ID {
		t.Errorf("freeze bound = %d, want %d (newest assistant id at save time)", task.AgentMessageID, newest.ID)
	}
	if task.AgentSessionID != "sess-fin-1" {
		t.Errorf("agent_session_id = %q, want the finalized session", task.AgentSessionID)
	}
}

// Replies produced AFTER the save must not move the bound of an already-created
// task — that is what makes the deliverable stable across worker retries.
func TestFinalize_FreezeBoundExcludesLaterReplies(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	atSave := seedAssistantMessage(t, db, "test-user", "sess-fin-2", "保存时已有")
	w := doFinalize(t, r, map[string]interface{}{"session_id": "sess-fin-2"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	later := seedAssistantMessage(t, db, "test-user", "sess-fin-2", "保存后才产出")

	var task model.SummaryTask
	if err := db.First(&task, finalizeTaskID(t, w)).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.AgentMessageID != atSave.ID {
		t.Fatalf("freeze bound = %d, want %d — a post-save reply (%d) must not be inside the bound",
			task.AgentMessageID, atSave.ID, later.ID)
	}
}

// A session with no usable assistant content cannot be finalized.
func TestFinalize_NoUsableContent_400(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	// A tool-call wrapper is process noise, not a finalizable fragment.
	if err := db.Create(&model.AgentMessage{
		UserID: "test-user", SessionID: "sess-fin-3", Role: "assistant",
		Content: "calling", ToolCalls: finalizeTestPtr(`[{"id":"c1"}]`),
	}).Error; err != nil {
		t.Fatalf("seed tool-call message: %v", err)
	}

	w := doFinalize(t, r, map[string]interface{}{"session_id": "sess-fin-3"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
}

// The in-flight guard runs AFTER the idempotency preflight, so a client that
// retries because it never saw the first 202 replays its own task instead of
// being 409'd by it. This ordering is the whole reason the preflight exists.
func TestFinalize_IdempotentReplayBeatsInFlightGuard(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	seedAssistantMessage(t, db, "test-user", "sess-fin-4", "内容")
	body := map[string]interface{}{"session_id": "sess-fin-4", "title": "同一份"}
	hdr := map[string]string{"Idempotency-Key": "finalize-key-1"}

	first := doFinalize(t, r, body, hdr)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", first.Code, first.Body.String())
	}
	// The first task is now Pending — i.e. "in flight". A naive guard-first
	// ordering would 409 here.
	second := doFinalize(t, r, body, hdr)
	if second.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202 — the in-flight guard must not reject a same-key replay (body=%s)",
			second.Code, second.Body.String())
	}
	if a, b := finalizeTaskID(t, first), finalizeTaskID(t, second); a != b {
		t.Fatalf("replay returned task %d, want the original %d", b, a)
	}

	var count int64
	db.Model(&model.SummaryTask{}).Where("agent_session_id = ?", "sess-fin-4").Count(&count)
	if count != 1 {
		t.Fatalf("task count = %d, want 1 — a replay must not create a second finalize run", count)
	}
}

// Without a key there is nothing to replay, so the in-flight guard is the only
// thing standing between a double-click and two concurrent finalize runs.
func TestFinalize_InFlightGuardBlocksSecondRunWithoutKey(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	seedAssistantMessage(t, db, "test-user", "sess-fin-5", "内容")
	if w := doFinalize(t, r, map[string]interface{}{"session_id": "sess-fin-5"}, nil); w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", w.Code)
	}
	w := doFinalize(t, r, map[string]interface{}{"session_id": "sess-fin-5"}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409 while the first run is still in flight", w.Code)
	}
}

// A soft-deleted finalize task is invisible to the poller, so counting it as
// in-flight would 409 that session forever.
func TestFinalize_SoftDeletedTaskDoesNotBlockForever(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	seedAssistantMessage(t, db, "test-user", "sess-fin-6", "内容")
	first := doFinalize(t, r, map[string]interface{}{"session_id": "sess-fin-6"}, nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.Code)
	}
	now := timezone.Now()
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", finalizeTaskID(t, first)).
		Update("deleted_at", &now).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	w := doFinalize(t, r, map[string]interface{}{"session_id": "sess-fin-6"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — a soft-deleted task is not in flight (body=%s)", w.Code, w.Body.String())
	}
}

// The request hash must be a function of request-owned fields only. Two byte
// identical bodies must hash identically no matter what the server resolves
// from mutable session state in between.
func TestFinalizeHashReq_IsStableAcrossServerState(t *testing.T) {
	req := finalizeAgentSummaryReq{
		SessionID:               "sess-x",
		Title:                   "标题",
		ExpectedSessionRevision: 3,
		ReferencedTaskIDs:       []int64{7},
	}
	a := canonicalAgentSaveRequestHash("u1", finalizeHashReq(req, "标题"))
	b := canonicalAgentSaveRequestHash("u1", finalizeHashReq(req, "标题"))
	if a != b {
		t.Fatalf("identical requests hashed differently: %s vs %s", a, b)
	}
}

// Both save routes share one idempotency namespace keyed only on
// (space, user, key). Without a route discriminator a client reusing one key
// across both endpoints gets a cross-route replay: /finalize handing back a
// completed sync task no worker will touch, or vice versa.
func TestFinalizeHashReq_DiscriminatesFromSyncSaveRoute(t *testing.T) {
	sessionID, title := "sess-y", "同题"
	finalizeHash := canonicalAgentSaveRequestHash("u1", finalizeHashReq(
		finalizeAgentSummaryReq{SessionID: sessionID, Title: title}, title))
	syncHash := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{
		SessionID: sessionID,
		Title:     title,
	})
	if finalizeHash == syncHash {
		t.Fatal("finalize and sync save hashed identically — one key reused across routes would cross-replay")
	}
}

// --- BLOCKING 2a: replay must survive the session cleanup ------------------

// The sibling sync route DELETEs every agent_message row of the session as part
// of a successful save — by design ("temporary workshop"). If the freeze/content
// check ran before the idempotency preflight, a byte-identical retry arriving
// after that DELETE would get 40004 "本次会话还没有可定稿的内容" instead of a 202
// replay of the task it already owns — and since the 202 is the client's ONLY
// handle on that task, it would lose it permanently.
func TestFinalize_ReplayAfterSessionCleanup(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	seedAssistantMessage(t, db, "test-user", "sess-fin-cleanup", "内容")
	body := map[string]interface{}{"session_id": "sess-fin-cleanup", "title": "同一份"}
	hdr := map[string]string{"Idempotency-Key": "cleanup-key-1"}

	first := doFinalize(t, r, body, hdr)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", first.Code, first.Body.String())
	}

	// The sync save (or a cleanup cron) empties the workshop.
	if err := db.Where("user_id = ? AND session_id = ?", "test-user", "sess-fin-cleanup").
		Delete(&model.AgentMessage{}).Error; err != nil {
		t.Fatalf("simulate session cleanup: %v", err)
	}

	second := doFinalize(t, r, body, hdr)
	if second.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202 — a same-key replay must not depend on session rows the first request's siblings destroy (body=%s)",
			second.Code, second.Body.String())
	}
	if a, b := finalizeTaskID(t, first), finalizeTaskID(t, second); a != b {
		t.Fatalf("replay returned task %d, want the original %d", b, a)
	}
}

// A replay must report the task's REAL status, not a hardcoded "GENERATING".
// A client that lost the first 202 has no idea how much time passed; telling it
// GENERATING for a Completed task makes it poll for a transition that already
// happened.
func TestFinalize_ReplayReportsRealTaskStatus(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	seedAssistantMessage(t, db, "test-user", "sess-fin-status", "内容")
	body := map[string]interface{}{"session_id": "sess-fin-status"}
	hdr := map[string]string{"Idempotency-Key": "status-key-1"}

	first := doFinalize(t, r, body, hdr)
	taskID := finalizeTaskID(t, first)
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
		Update("status", model.StatusCompleted).Error; err != nil {
		t.Fatalf("complete the task: %v", err)
	}

	second := doFinalize(t, r, body, hdr)
	var resp struct {
		Data struct {
			Status   interface{} `json:"status"`
			Replayed bool        `json:"replayed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := resp.Data.Status.(float64); !ok || int(got) != model.StatusCompleted {
		t.Fatalf("replay status = %v, want the real task status %d — not a hardcoded GENERATING",
			resp.Data.Status, model.StatusCompleted)
	}
	if !resp.Data.Replayed {
		t.Fatal("replay must be marked replayed:true, like the sync route")
	}
}

// --- BLOCKING 2b: symmetric guard, sync save vs in-flight finalize --------

// CreateAgentSummary hard-DELETEs every agent_message row of the session inside
// its transaction. A queued finalize task has already frozen its input as an id
// bound over exactly those rows, so if the DELETE lands first the worker loads
// ZERO replies, every retry hits the same empty set, and the task dies Failed
// after WorkerMaxRetry — while the user holds a 202 and a task handle, with the
// cause invisible from the finalize endpoint. The finalize route refuses a
// second finalize on an in-flight session; this is the other half of that guard.
func TestCreateAgentSummary_RefusedWhileFinalizeInFlight(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	finalizeR := setupFinalizeRouter(h)
	saveR := setupAgentSummaryRouter(h)

	sessionID := "sess-sym-1"
	seedAssistantMessage(t, db, "test-user", sessionID, "会话产出")

	if w := doFinalize(t, finalizeR, map[string]interface{}{"session_id": sessionID},
		map[string]string{"Idempotency-Key": "sym-key-1"}); w.Code != http.StatusAccepted {
		t.Fatalf("finalize status = %d, want 202 (body=%s)", w.Code, w.Body.String())
	}

	w := doAgentSave(t, saveR, map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   "CH-1",
		"origin_channel_type": 1,
		"title":               "同步保存",
	}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("sync save status = %d, want 409 while a finalize is in flight (body=%s)", w.Code, w.Body.String())
	}

	// The load-bearing consequence: the finalize's frozen input still exists.
	var remaining int64
	db.Model(&model.AgentMessage{}).Where("user_id = ? AND session_id = ?", "test-user", sessionID).Count(&remaining)
	if remaining == 0 {
		t.Fatal("the refused save still destroyed the in-flight finalize's frozen input")
	}
}

// The other direction, already enforced: a finalize is refused while another
// finalize for the session is in flight. Pinned here so the pair cannot drift
// apart.
func TestFinalize_RefusedWhileAnotherFinalizeInFlight(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	r := setupFinalizeRouter(h)

	sessionID := "sess-sym-2"
	seedAssistantMessage(t, db, "test-user", sessionID, "会话产出")
	body := map[string]interface{}{"session_id": sessionID}

	if w := doFinalize(t, r, body, map[string]string{"Idempotency-Key": "sym2-a"}); w.Code != http.StatusAccepted {
		t.Fatalf("first finalize status = %d, want 202", w.Code)
	}
	if w := doFinalize(t, r, body, map[string]string{"Idempotency-Key": "sym2-b"}); w.Code != http.StatusConflict {
		t.Fatalf("second finalize status = %d, want 409", w.Code)
	}
}

// A TERMINAL finalize task must not block saves forever — otherwise a single
// Failed finalize would permanently lock the session out of the sync route.
func TestCreateAgentSummary_TerminalFinalizeDoesNotBlockSave(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	finalizeR := setupFinalizeRouter(h)
	saveR := setupAgentSummaryRouter(h)

	sessionID := "sess-sym-3"
	seedAssistantMessage(t, db, "test-user", sessionID, "会话产出")
	first := doFinalize(t, finalizeR, map[string]interface{}{"session_id": sessionID},
		map[string]string{"Idempotency-Key": "sym3-a"})
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", finalizeTaskID(t, first)).
		Update("status", model.StatusFailed).Error; err != nil {
		t.Fatalf("fail the task: %v", err)
	}

	w := doAgentSave(t, saveR, map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   "CH-1",
		"origin_channel_type": 1,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sync save status = %d, want 200 — a terminal finalize is not in flight (body=%s)", w.Code, w.Body.String())
	}
}

// A soft-deleted Pending finalize is invisible to the poller, so counting it as
// in-flight would block the session's saves forever. Mirrors the identical
// condition on the finalize route's own guard.
func TestCreateAgentSummary_SoftDeletedFinalizeDoesNotBlockSave(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, db, "", "", "", 0, 0)
	finalizeR := setupFinalizeRouter(h)
	saveR := setupAgentSummaryRouter(h)

	sessionID := "sess-sym-4"
	seedAssistantMessage(t, db, "test-user", sessionID, "会话产出")
	first := doFinalize(t, finalizeR, map[string]interface{}{"session_id": sessionID},
		map[string]string{"Idempotency-Key": "sym4-a"})
	now := timezone.Now()
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", finalizeTaskID(t, first)).
		Update("deleted_at", &now).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	w := doAgentSave(t, saveR, map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   "CH-1",
		"origin_channel_type": 1,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sync save status = %d, want 200 — a soft-deleted finalize is not in flight (body=%s)", w.Code, w.Body.String())
	}
}
