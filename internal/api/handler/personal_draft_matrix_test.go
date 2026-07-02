//go:build cgo

// OCT-21 Stage 4 — dev-test01 complementary backend matrix for
// PUT /api/v1/summaries/:id/personal-draft.
//
// dev-work02 (OCT-26) shipped personal_draft_test.go covering the dev-owned
// subset (T1, T4-strict, T9a, T9b, T12a, T12b, T14 + a latency probe). This
// file adds the REMAINING v2 §3 matrix entries that were left to dev-test01:
//
//   T2  non-participant            -> 404/40008
//   T3  cross-space task           -> 404/40008
//   T5  A's write never hits B     -> only self row mutated
//   T6  empty / whitespace content -> 400/40010
//   T7  content > 500KB            -> 400/40010
//   T8  worker_status 0/1/3        -> 400/40005 (Submit-aligned)
//   T10 citations cleanup          -> dropped [2] removed from citations_json
//   T11 task not found             -> 404/40008
//   T13 black-box regression       -> task.status/submitted_at/edited_at/participant unchanged
//   T15 PersonalEdit/Submit intact -> sibling handlers still present (behavioural suites stay green)
//
// Reuses the dev's helpers (seedDraftableTask, setupPersonalDraftRouter) and the
// package-wide helpers (newTriggerCapture, doJSONRequest, assertBizCode,
// setupMembersTestDB). Nothing here is redefined.
//
// Error code note: v2 B2b reserved 40015 for "draft already submitted", but the
// OCT-26 implementation found that slot taken and used 40016 (see personal.go:675
// / :734). The dev's tests assert 40016; the entries here are the codes that are
// NOT 40016 (the 409 cases live in the dev's file).

package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
)

func draftURL(taskID int64) string {
	return fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID)
}

// T2 — a non-participant of the task is rejected 404/40008 (participation not leaked).
func TestPersonalDraft_T2_NonParticipant_404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	w := doJSONRequest(r, "PUT", draftURL(taskID), "outsider", map[string]interface{}{"content": "hax [1]"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-participant: want 404, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40008)
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("rejected non-participant must not trigger meta_summary")
	}
}

// T3 — the task lives in space1; a caller presenting space2 sees 404/40008
// (requireTaskInSpace cross-space isolation; no existence leak). The dev router
// uses the lenient SpaceMiddleware so the space header is passed through to the
// handler rather than being short-circuited by StrictSpaceMiddleware.
func TestPersonalDraft_T3_CrossSpace_404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db) // space1
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalDraftRouter(h)

	body, _ := json.Marshal(map[string]interface{}{"content": "cross space [1]"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", draftURL(taskID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "member_x")
	req.Header.Set("X-Space-Id", "space2") // wrong space
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-space: want 404, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40008)
}

// T5 — creator1's draft must only ever mutate creator1's own PR row; member_x /
// member_y rows are byte-for-byte untouched.
func TestPersonalDraft_T5_OnlySelfRowMutated(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalDraftRouter(h)

	type snap struct{ content, cit string }
	before := map[string]snap{}
	for _, uid := range []string{"member_x", "member_y"} {
		var pr model.PersonalResult
		db.Where("task_id = ? AND user_id = ?", taskID, uid).First(&pr)
		before[uid] = snap{pr.Content, pr.CitationsJSON}
	}

	w := doJSONRequest(r, "PUT", draftURL(taskID), "creator1", map[string]interface{}{"content": "creator self draft [1]"})
	if w.Code != http.StatusOK {
		t.Fatalf("self draft: want 200, got %d: %s", w.Code, w.Body.String())
	}

	for _, uid := range []string{"member_x", "member_y"} {
		var pr model.PersonalResult
		db.Where("task_id = ? AND user_id = ?", taskID, uid).First(&pr)
		if pr.Content != before[uid].content || pr.CitationsJSON != before[uid].cit {
			t.Errorf("%s row mutated by creator1 draft: content %q->%q", uid, before[uid].content, pr.Content)
		}
	}
	var self model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "creator1").First(&self)
	if self.Content != "creator self draft [1]" {
		t.Errorf("self row not updated: %q", self.Content)
	}
}

// T6 — empty or whitespace-only content -> 400/40010, no write.
func TestPersonalDraft_T6_EmptyContent_400(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalDraftRouter(h)

	for _, c := range []string{"", "   ", "\n\t  "} {
		w := doJSONRequest(r, "PUT", draftURL(taskID), "member_x", map[string]interface{}{"content": c})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("content=%q: want 400, got %d: %s", c, w.Code, w.Body.String())
		}
		assertBizCode(t, w, 40010)
	}
	var pr model.PersonalResult
	db.First(&pr, xPRID)
	if pr.Content != "member_x original [1][2]" {
		t.Errorf("empty-content rejection must not write, got %q", pr.Content)
	}
}

// T7 — content larger than maxContentBytes (500KB) -> 400/40010.
func TestPersonalDraft_T7_ContentTooLong_400(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalDraftRouter(h)

	oversize := strings.Repeat("a", maxContentBytes+1)
	w := doJSONRequest(r, "PUT", draftURL(taskID), "member_x", map[string]interface{}{"content": oversize})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversize: want 400, got %d", w.Code)
	}
	assertBizCode(t, w, 40010)
}

// T8 — worker_status in {Pending, Processing, Failed} -> 400/40005, same code as
// Submit's "personal summary not finished" (personal.go:382-385).
func TestPersonalDraft_T8_WorkerNotCompleted_400(t *testing.T) {
	for _, ws := range []int{model.PersonalStatusPending, model.PersonalStatusProcessing, model.PersonalStatusFailed} {
		db := setupMembersTestDB(t)
		taskID, xPRID := seedDraftableTask(t, db)
		db.Model(&model.PersonalResult{}).Where("id = ?", xPRID).Update("worker_status", ws)
		h := NewPersonalHandler(db, "", nil)
		r := setupPersonalDraftRouter(h)

		w := doJSONRequest(r, "PUT", draftURL(taskID), "member_x", map[string]interface{}{"content": "x [1]"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("ws=%d: want 400, got %d: %s", ws, w.Code, w.Body.String())
		}
		assertBizCode(t, w, 40005)
	}
}

// T10 — citation cleanup: new content references only [1]; CleanUnreferencedCitations
// must drop citation #2 from citations_json while keeping #1.
func TestPersonalDraft_T10_CitationsCleanup(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db) // seeded with [1] and [2]
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalDraftRouter(h)

	w := doJSONRequest(r, "PUT", draftURL(taskID), "member_x", map[string]interface{}{"content": "keep only [1]"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var pr model.PersonalResult
	db.First(&pr, xPRID)
	for _, ci := range pr.GetCitations() {
		if ci.Index == 2 {
			t.Errorf("citation #2 should be cleaned, still present in %s", pr.CitationsJSON)
		}
	}
	if !strings.Contains(pr.CitationsJSON, `"index":1`) {
		t.Errorf("citation #1 should remain, got %s", pr.CitationsJSON)
	}
}

// T11 — task id that does not exist -> 404/40008.
func TestPersonalDraft_T11_TaskNotFound_404(t *testing.T) {
	db := setupMembersTestDB(t)
	_, _ = seedDraftableTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalDraftRouter(h)

	w := doJSONRequest(r, "PUT", draftURL(999999), "member_x", map[string]interface{}{"content": "x [1]"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing task: want 404, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40008)
}

// T13 — black-box regression: after a successful draft, task.status, the
// participant status, submitted_at and edited_at are ALL unchanged.
func TestPersonalDraft_T13_NoSideEffects(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	var taskBefore model.SummaryTask
	db.First(&taskBefore, taskID)
	var partBefore model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&partBefore)

	w := doJSONRequest(r, "PUT", draftURL(taskID), "member_x", map[string]interface{}{"content": "probe [1]"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var taskAfter model.SummaryTask
	db.First(&taskAfter, taskID)
	if taskAfter.Status != taskBefore.Status {
		t.Errorf("task.status changed %d->%d", taskBefore.Status, taskAfter.Status)
	}
	var partAfter model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&partAfter)
	if partAfter.Status != partBefore.Status {
		t.Errorf("participant.status changed %d->%d (must NOT flip to Submitted)", partBefore.Status, partAfter.Status)
	}
	var pr model.PersonalResult
	db.First(&pr, xPRID)
	if pr.SubmittedAt != nil {
		t.Errorf("submitted_at set by draft path: %v", pr.SubmittedAt)
	}
	if pr.EditedAt != nil {
		t.Errorf("edited_at set by draft path: %v", pr.EditedAt)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("draft must not trigger meta_summary")
	}
}

// T15 — guard that the sibling handlers PersonalEdit/Submit still exist with their
// (*gin.Context) signatures. Their behavioural suites (members_auth_test.go,
// schedule_behavior_test.go) run as part of the normal package test and must stay
// green; this is the compile-time canary that the draft work did not delete them.
func TestPersonalDraft_T15_SiblingHandlersIntact(t *testing.T) {
	db := setupMembersTestDB(t)
	h := NewPersonalHandler(db, "", nil)
	var _ func(*gin.Context) = h.PersonalEdit
	var _ func(*gin.Context) = h.Submit
	_ = middleware.StrictSpaceMiddleware()
}
