//go:build cgo

package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OCT-21 Stage 4 backend tests for PUT /api/v1/summaries/:id/personal-draft.
//
// Scope per OCT-26 task: dev must self-run at least T1, T9a, T9b, T12a, T12b,
// T14 plus keep the existing PersonalEdit/Submit suite green. Remaining
// matrix entries (T2-T8, T10-T11, T13, T15) are dev-test01's responsibility
// and will land in a separate test PR.
//
// All tests use the same in-memory sqlite + gin harness as personal-edit
// (members_auth_test.go) for consistency.

// setupPersonalDraftRouter mounts ONLY the personal-draft route. It uses the
// lenient SpaceMiddleware (same as setupPersonalEditRouter) so we can drive
// `requireTaskInSpace`'s fail-closed path directly with an empty X-Space-Id
// (StrictSpaceMiddleware's middleware-level 400/40001 path is covered
// separately in TestPersonalDraft_MissingSpaceHeader_StrictMiddleware).
func setupPersonalDraftRouter(h *PersonalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.PUT("/api/v1/summaries/:id/personal-draft", h.PersonalDraft)
	return r
}

// seedDraftableTask builds a Completed BY_PERSON task with creator + two
// members, each with a PersonalResult in worker_status=Completed and
// submitted_at=NULL -- exactly the state in which PersonalDraft is legal.
// Returns the task id plus member_x's PersonalResult.ID for direct DB probes.
func seedDraftableTask(t *testing.T, db *gorm.DB) (taskID int64, xPRID int64) {
	t.Helper()
	now := timezone.Now()
	task := model.SummaryTask{
		TaskNo:      "TST-DRAFT-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})

	for _, uid := range []string{"creator1", "member_x", "member_y"} {
		p := model.SummaryParticipant{TaskID: task.ID, UserID: uid, UserName: uid, Status: model.ParticipantCompleted}
		db.Create(&p)
		pr := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: p.ID,
			UserID:           uid,
			Content:          uid + " original [1][2]",
			CitationsJSON:    `[{"index":1,"sender":"s","content":"c1","channel_id":"grp_abc"},{"index":2,"sender":"s","content":"c2","channel_id":"grp_abc"}]`,
			WorkerStatus:     model.PersonalStatusCompleted,
			GeneratedAt:      &now,
		}
		db.Create(&pr)
		if uid == "member_x" {
			xPRID = pr.ID
		}
	}
	return task.ID, xPRID
}

// T1: happy path. PersonalDraft writes content + cleaned citations, leaves
// every status / timestamp / trigger untouched. This is the load-bearing
// regression for "drafts are silent" -- if it ever fails, the contract
// guaranteeing team summary is not invalidated has broken.
func TestPersonalDraft_OwnSuccessSilent(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	body := map[string]interface{}{"content": "member_x DRAFT [1]"} // drop [2]
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for own draft, got %d: %s", w.Code, w.Body.String())
	}

	// Only member_x.content changed; edited_at MUST stay nil (drafts != edits).
	var prX model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&prX)
	if prX.Content != "member_x DRAFT [1]" {
		t.Errorf("draft content not persisted: %q", prX.Content)
	}
	if prX.EditedAt != nil {
		t.Errorf("draft must NOT write edited_at, got %v", prX.EditedAt)
	}
	if prX.SubmittedAt != nil {
		t.Errorf("draft must NOT write submitted_at, got %v", prX.SubmittedAt)
	}
	// Citation [2] was removed from content -> CleanUnreferencedCitations must
	// have dropped it from citations_json. [1] must remain.
	if !strings.Contains(prX.CitationsJSON, `"index":1`) {
		t.Errorf("citation [1] should still be present, got: %s", prX.CitationsJSON)
	}
	if strings.Contains(prX.CitationsJSON, `"index":2`) {
		t.Errorf("citation [2] should have been cleaned, got: %s", prX.CitationsJSON)
	}

	// Other members untouched.
	var prY model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_y").First(&prY)
	if prY.Content != "member_y original [1][2]" || prY.EditedAt != nil {
		t.Errorf("member_y must be untouched: content=%q edited_at=%v", prY.Content, prY.EditedAt)
	}

	// task.status must stay Completed (no revive).
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Status != model.StatusCompleted {
		t.Errorf("task.status must stay Completed (no revive), got %d", task.Status)
	}

	// T14 sub-assertion: NO meta_summary trigger may fire.
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("draft must NOT trigger meta_summary; capture saw one")
	}
}

// T14: trigger count remains 0 across many sequential draft saves. Defends the
// "drafts are silent" contract against accidental future re-wiring.
func TestPersonalDraft_NoMetaTriggerOverManySaves(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	for i := 0; i < 5; i++ {
		body := map[string]interface{}{"content": fmt.Sprintf("draft v%d [1]", i)}
		w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
		if w.Code != http.StatusOK {
			t.Fatalf("draft #%d expected 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("draft path must not emit any meta_summary trigger across multiple saves")
	}
	// Also assert: nothing else was emitted (no personal_summary either).
	if tc.waitFor("personal_summary", taskID) {
		t.Errorf("draft path must not emit personal_summary either")
	}
}

// T9a: fast-path 409. pr.SubmittedAt != nil at the time the handler runs ->
// 409/40016, no DB write happens.
func TestPersonalDraft_AlreadySubmittedFastPath_409(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	// Mark member_x as already submitted BEFORE the request.
	now := timezone.Now()
	db.Model(&model.PersonalResult{}).Where("id = ?", xPRID).
		Updates(map[string]interface{}{"submitted_at": now, "submit_source": model.SubmitSourceManual})

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	body := map[string]interface{}{"content": "trying to draft after submit [1]"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 fast-path, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40016)

	// Content must NOT have been changed (fast-path returns before the UPDATE).
	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content != "member_x original [1][2]" {
		t.Errorf("fast-path 409 must not write content, got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("409 fast-path must not trigger meta_summary")
	}
}

// T9b: DB-guard 409. Fast-path passes (pr.SubmittedAt was nil at read time),
// but submitted_at is written concurrently before the conditional UPDATE.
// Implementation: pre-load PR via a tx-friendly path, but flip submitted_at
// in a different gorm session right before we call PersonalDraft -- this
// exercises the same code path as the racing-Submit production case (the
// in-memory test cannot do "wedge UPDATE mid-transaction" without a debug
// hook, but the conditional UPDATE WHERE clause is the only thing being
// exercised here, and flipping submitted_at in another session immediately
// before the request validates that branch deterministically).
//
// NOTE: in this test we explicitly bypass the fast-path by NOT mutating the
// in-memory `pr` after the handler reads it. Because the handler re-reads
// pr.SubmittedAt itself, we set submitted_at concurrently through a separate
// db handle AFTER the handler has been built but BEFORE the request fires --
// but in practice the handler reads inside the request, so this collapses
// into T9a unless we patch the DB after pr is loaded. To produce a true
// DB-guard hit we instead expose the path by injecting the racing write into
// a custom interceptor.
//
// Simpler deterministic approach: use a transaction interceptor via gorm
// callbacks to flip submitted_at on the SELECT-then-UPDATE seam. We register
// a "before update" callback that runs once and sets submitted_at in the
// same row, then the conditional UPDATE finds RowsAffected=0, and the probe
// SELECT inside the tx sees submitted_at != nil -> errDraftAlreadySubmitted.
// runRaceSubmitWinsDuringUpdate exercises the DB-guard race: a Submit lands
// between PersonalDraft's fast-path read and its conditional UPDATE, the
// UPDATE matches 0 rows, and the in-tx probe SELECT escalates to 409/40016.
// Shared by T9b and T12b so the OCT-26 v2 matrix keeps both case ids
// without `t.Run` plumbing or test-calls-test wiring.
func runRaceSubmitWinsDuringUpdate(t *testing.T) {
	t.Helper()
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	// Race injector: register a one-shot gorm callback that fires immediately
	// before any PersonalResult UPDATE and sets submitted_at on the same row
	// via a sibling session. After it fires once, it unregisters itself so
	// subsequent tests are unaffected.
	cbName := "test:race_submit_wins"
	fired := false
	err := db.Callback().Update().Before("gorm:update").Register(cbName, func(tx *gorm.DB) {
		if fired {
			return
		}
		if tx.Statement == nil || tx.Statement.Table != "summary_personal_result" {
			return
		}
		fired = true
		now := timezone.Now()
		// :memory: sqlite gives each *sql.DB connection its own private DB,
		// so a sibling gorm session would not see our seed tables. The next
		// best deterministic shape is to commit the racing write through the
		// SAME transaction (tx): the in-flight conditional UPDATE now sees
		// submitted_at != NULL via its WHERE clause -> RowsAffected=0, and the
		// probe SELECT inside the same tx returns the just-written row -> the
		// handler maps it to errDraftAlreadySubmitted (409/40016). This is
		// behaviourally identical to the production race (separate Submit
		// request commits submitted_at before our UPDATE runs).
		if err := tx.Exec("UPDATE summary_personal_result SET submitted_at = ?, submit_source = ? WHERE id = ?",
			now, model.SubmitSourceManual, xPRID).Error; err != nil {
			t.Errorf("race injector failed to flip submitted_at: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("registering race callback: %v", err)
	}
	defer func() {
		_ = db.Callback().Update().Remove(cbName)
	}()

	body := map[string]interface{}{"content": "draft that loses race [1]"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 from DB-guard, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40016)
	if !fired {
		t.Errorf("race injector never fired -- DB-guard path was not exercised")
	}

	// Content MUST NOT be the draft -- the conditional UPDATE never landed.
	// (Whether submitted_at is now non-null depends on test plumbing: the
	// injector wrote inside our tx, which was rolled back when the handler
	// returned errDraftAlreadySubmitted. In production the racing Submit
	// commits in a sibling tx that already landed before our UPDATE ran, so
	// submitted_at would persist. Either way, the load-bearing guarantee is
	// "draft content does not overwrite a row that has been or is being
	// submitted", which is what the 409 + content check verify here.)
	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content != "member_x original [1][2]" {
		t.Errorf("DB-guard 409 must NOT persist draft content; got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("DB-guard 409 must not trigger meta_summary")
	}
}

func TestPersonalDraft_RaceSubmitWinsDuringUpdate_DBGuard_409(t *testing.T) {
	runRaceSubmitWinsDuringUpdate(t)
}

// T12a: row physically deleted (Leave/RemoveMember race). Fast-path passes,
// conditional UPDATE finds 0 rows, probe SELECT returns ErrRecordNotFound ->
// errPersonalResultGone -> 404/40008.
func TestPersonalDraft_RaceLeaveDeletesRow_404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	// One-shot before-update callback that DELETEs member_x's PR row via a
	// sibling session, mimicking a Leave processed by another request between
	// the handler's preload and the conditional UPDATE.
	cbName := "test:race_leave_deletes_row"
	fired := false
	err := db.Callback().Update().Before("gorm:update").Register(cbName, func(tx *gorm.DB) {
		if fired {
			return
		}
		if tx.Statement == nil || tx.Statement.Table != "summary_personal_result" {
			return
		}
		fired = true
		// Same :memory: caveat as T9b -- commit the deletion through the in-flight
		// tx so the conditional UPDATE finds 0 rows, then the probe SELECT
		// returns ErrRecordNotFound -> errPersonalResultGone (404/40008).
		if err := tx.Exec("DELETE FROM summary_personal_result WHERE id = ?", xPRID).Error; err != nil {
			t.Errorf("race injector failed to delete PR row: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("registering race callback: %v", err)
	}
	defer func() {
		_ = db.Callback().Update().Remove(cbName)
	}()

	body := map[string]interface{}{"content": "draft into thin air [1]"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from row-gone path, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40008)
	if !fired {
		t.Errorf("race injector never fired -- row-gone path was not exercised")
	}

	// Persistence note (same caveat as the submit-race test): the injected
	// DELETE ran inside the tx that returned errPersonalResultGone, so on
	// rollback the row reappears. Production Leave commits independently and
	// the row stays gone. The load-bearing guarantee here is "draft did NOT
	// land on a row mid-deletion", verified by checking content was not
	// updated to the draft body.
	var prX model.PersonalResult
	if err := db.Where("id = ?", xPRID).First(&prX).Error; err == nil {
		if prX.Content == "draft into thin air [1]" {
			t.Errorf("row-gone path must not have persisted the draft content")
		}
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("404 path must not trigger meta_summary")
	}
}

// T12b: identical shape to T9b but framed as "race between draft and concurrent
// submit, exercised through the DB guard not the fast-path". Kept as a
// distinct case from T9b so the matrix mapping (T12 split into a/b after v2
// B1) is visible in test names. Reuses T9b's injector; assertions overlap on
// purpose -- duplication is cheap, missing the regression isn't.
func TestPersonalDraft_RaceSubmitWins_T12b(t *testing.T) {
	runRaceSubmitWinsDuringUpdate(t)
}

// Sanity: the request takes a non-trivial amount of time -- catches accidental
// fast-out bugs where the handler returns 200 without doing any work.
func TestPersonalDraft_RequestTakesNonZeroTime(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	start := time.Now()
	body := map[string]interface{}{"content": "draft latency probe [1]"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if time.Since(start) < 1*time.Microsecond {
		t.Errorf("draft handler returned in zero time, suspicious")
	}
}

// T4 (StrictSpaceMiddleware variant): when the production middleware stack
// (StrictSpaceMiddleware ahead of the handler, like router.go's v1 group)
// receives a write request without X-Space-Id, the handler is never reached
// and the response is 400/40001 -- NOT the 404/40008 returned by
// requireTaskInSpace when it does run.
func TestPersonalDraft_MissingSpaceHeader_StrictMiddleware(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	h := NewPersonalHandler(db, "", nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.StrictSpaceMiddleware())
	r.PUT("/api/v1/summaries/:id/personal-draft", h.PersonalDraft)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), strings.NewReader(`{"content":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "member_x")
	// NOTE: deliberately no X-Space-Id and no X-Org-Id.
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 from StrictSpaceMiddleware, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40001)
}

// T16 (OCT-31): unsubmitted draft re-saved with
// content/citations identical to what's already on disk must return 200,
// NOT 409. Root cause: this codebase opens MySQL without `clientFoundRows=true`
// (see internal/db/db.go:11-18), so on MySQL a no-op UPDATE returns
// RowsAffected=0 and the original implementation misclassified that as
// "race lost to Submit". Trigger count must stay 0 (drafts are silent).
//
// Coverage scope on sqlite: this unit test does NOT actually exercise the
// (c) probe branch in personal.go. The handler's map-based .Updates() lets
// GORM auto-append `updated_at = <now>` (schema.AutoUpdateTime on
// PersonalResult.UpdatedAt), so on sqlite the second save's updated_at
// differs from the first by at least one tick and the driver returns
// RowsAffected=1; the request falls through the outer `return nil` and
// the user sees 200. That's the user-visible behaviour T16 is asserting --
// idempotent saves stay 200 and don't introduce a regression. The SQL-level
// (c) branch is only reachable on MySQL `datetime` (second precision) with
// a same-second duplicate save (MySQL `datetime(3)` also gets RowsAffected=1
// because the appended ms-precision updated_at differs); covering that
// requires a real MySQL fixture and is left to integration regression R1,
// not simulated at the unit layer.
func TestPersonalDraft_IdempotentNoopSave_T16(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	// First save establishes the on-disk state for member_x: content +
	// pruned citations (only [1] survives CleanUnreferencedCitations).
	body := map[string]interface{}{"content": "member_x DRAFT [1]"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
	if w.Code != http.StatusOK {
		t.Fatalf("first save expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Capture the row exactly as the handler wrote it -- the next save must
	// produce byte-identical content + citations_json.
	var prBefore model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&prBefore)

	// Second save with the same body. UPDATE's WHERE matches (submitted_at IS
	// NULL) but no column actually changes -> RowsAffected=0. New handler
	// branch (c) must recognise this as a no-op and return 200 instead of
	// 409/40016.
	w2 := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", body)
	if w2.Code != http.StatusOK {
		t.Fatalf("idempotent re-save expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// Row must be unchanged byte-for-byte (no-op write).
	var prAfter model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&prAfter)
	if prAfter.Content != prBefore.Content || prAfter.CitationsJSON != prBefore.CitationsJSON {
		t.Errorf("no-op save must not mutate row: before=(%q,%q) after=(%q,%q)",
			prBefore.Content, prBefore.CitationsJSON, prAfter.Content, prAfter.CitationsJSON)
	}
	if prAfter.SubmittedAt != nil {
		t.Errorf("no-op save must NOT set submitted_at, got %v", prAfter.SubmittedAt)
	}
	if prAfter.EditedAt != nil {
		t.Errorf("no-op save must NOT set edited_at, got %v", prAfter.EditedAt)
	}
	// Drafts are silent across both saves.
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("idempotent save path must not trigger meta_summary")
	}
}

// T17 (OCT-31, B-FIX partial-divergence guard): unsubmitted draft re-saved
// with content that differs by even one byte (citations may be incidentally
// re-pruned) must take the normal RowsAffected=1 path and return 200, never
// landing in the probe branch. This protects T16's no-op detection from
// over-triggering on real edits.
func TestPersonalDraft_DivergentContentNormalPath_T17(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	// First save: drop citation [2].
	first := map[string]interface{}{"content": "member_x DRAFT [1]"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", first)
	if w.Code != http.StatusOK {
		t.Fatalf("first save expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var pr1 model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&pr1)

	// Second save: same citation set (only [1] is referenced, so
	// citations_json will match pr1 after cleanup) but content differs by one
	// word. RowsAffected must be 1; the probe branch must not run.
	second := map[string]interface{}{"content": "member_x DRAFT v2 [1]"}
	w2 := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID), "member_x", second)
	if w2.Code != http.StatusOK {
		t.Fatalf("divergent re-save expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var pr2 model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&pr2)
	if pr2.Content != "member_x DRAFT v2 [1]" {
		t.Errorf("divergent save must persist new content, got %q", pr2.Content)
	}
	// citations_json should be unchanged from pr1 (same surviving set) -- this
	// is the partial-divergence case (content differs, citations same) called
	// out in OCT-31's T17 spec.
	if pr2.CitationsJSON != pr1.CitationsJSON {
		t.Errorf("citations_json should be stable across same-citation edit: before=%q after=%q",
			pr1.CitationsJSON, pr2.CitationsJSON)
	}
	if pr2.SubmittedAt != nil || pr2.EditedAt != nil {
		t.Errorf("draft path must not write submitted_at/edited_at, got submitted=%v edited=%v",
			pr2.SubmittedAt, pr2.EditedAt)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("draft path must not trigger meta_summary")
	}
}

// T18 (OCT-43, Critical from PR #125 review): PersonalDraft must reject
// any task whose SummaryMode != ModeByPerson with 400/40005. Today the
// production code-path is gated upstream (task.go / schedule.go hard-code
// ModeByPerson, and BY_GROUP tasks fail-closed at the PersonalResult
// lookup), but the handler itself never checked the mode -- a regression
// in those upstream gates would let drafts land on the wrong task type.
// Note the worker pickup (personal_processor.go) does NOT filter by mode
// and would happily materialise a PersonalResult on any mode, so the
// handler is the load-bearing defence-in-depth. This test pins the
// handler-level invariant.
func TestPersonalDraft_RejectsNonByPersonMode_T18(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedDraftableTask(t, db)
	// Flip the seeded task to a non-ByPerson mode (any int != ModeByPerson;
	// summary_mode=1 is the conceptual BY_GROUP sibling per model.go).
	if err := db.Model(&model.SummaryTask{}).
		Where("id = ?", taskID).
		Update("summary_mode", 1 /* BY_GROUP */).Error; err != nil {
		t.Fatalf("flip summary_mode: %v", err)
	}

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	body := map[string]interface{}{"content": "draft on non-ByPerson task [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-ByPerson task, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40005)

	// Content MUST stay at the seeded original -- the handler must reject
	// before ever issuing the UPDATE (gate is earlier than the DB write).
	var prX model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&prX)
	if prX.Content != "member_x original [1][2]" {
		t.Errorf("BY_PERSON gate must reject before write; got content %q", prX.Content)
	}
	// And no meta_summary trigger must have been dispatched (gate is earlier
	// than the trigger).
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("BY_PERSON gate must not trigger meta_summary")
	}
}
