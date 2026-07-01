//go:build cgo

package handler

// OCT-50 stage 7A backend tests for PUT /api/v1/summaries/:id/personal-draft.
//
// Adds coverage of the v2 + v2.1 plan's new decision paths:
//
//   T19 – single-person task, summary_result already present (task.status=
//         Completed) → fast-path published-check rejects with 40016. Content
//         is not touched.
//   T20 – multi-person task, participantCount transiently=1 (Accept-in-flight
//         shape) and worker still pending → falls into the existing 690 gate
//         → 40005. This is a regression fence around the pre-existing
//         behaviour: OCT-50 must not silently start writing 40009 here.
//   T21 – Revive-after-Leave shape (Leave/RemoveMember on Completed multi
//         task): task.status flips Completed→Processing, summary_result is
//         RETAINED (revive does not delete it). A draft attempt hits the new
//         fast-path published-check → 40009.
//   T22 – AddMembers-then-Accept revive shape (identical decision-path to
//         T21 but a different producer of the {Processing + summary_result}
//         state): the new member's draft must be rejected with 40009 and the
//         existing member's personal_result must NOT be touched.
//   T23 – Regenerate-mid-tx race: fast-path count observes 0 (Regenerate has
//         not yet committed), tx starts + lockTask acquired, Regenerate
//         commits (worker_status: Completed→Pending, task.status→Pending,
//         summary_result deleted) via a same-tx GORM callback, UPDATE's
//         `worker_status = Completed` clause misses, probe reads back
//         worker_status=Pending → probe-(d) → errDraftRegenerating → 40009.
//   T24 – Gate-in-tx decision-layer verification: fast-path observes
//         summary_result empty, tx starts, a same-tx GORM callback (mirroring
//         T9b's runRaceSubmitWinsDuringUpdate shape) seeds a summary_result
//         row + flips task.status→Completed BEFORE the intra-tx re-check
//         SELECT runs, gate-in-tx returns errDraftPublished → 40016. The
//         double-check for the "reverse" branch is executed by the sibling
//         test TestPersonalDraft_GateInTx_TxScopedPublishedCheck_ReverseNoOp
//         which asserts that with gate-in-tx removed the exact same fixture
//         would let the UPDATE land — but that is not a subtest we can run
//         at head; the reverse evidence is delivered in the PR body via a
//         one-line comment-out sanity run (see the ISSUE COMMENT for the two
//         `go test` outputs).
//
// sqlite `:memory:` limitations:
//
//   • sibling gorm sessions do NOT share the same in-memory DB — see
//     personal_draft_test.go:246-255 (T9b's docstring). Race injection must
//     therefore ride the SAME tx via `db.Callback().Update().Before(...)`,
//     which is why every "race" test in this file uses in-tx GORM callbacks
//     rather than sibling goroutines.
//   • FOR UPDATE is SILENTLY IGNORED on sqlite (see
//     members_auth_test.go:1837-1845). The gate-in-tx tests below therefore
//     verify the DECISION LAYER (SELECT-then-branch), not the wall-clock
//     ordering. Real lock-ordering evidence lives in production MySQL / an
//     integration seam, out of scope for this issue's "personal.go only"
//     change. T24's docstring inside the test body repeats this note so it
//     stays with the code that carries the caveat.
//
//   T25 – Fast-path stale-task.Status race (OCT-62 gpt blocker landing).
//         The handler read `task.Status` once via `requireTaskInSpace` at
//         :709; concurrent `reviveCompletedForRecompute` (Leave/RemoveMember)
//         flips task.status Completed→Processing while KEEPING summary_result
//         intact. Under the pre-OCT-62 fast-path (which dispatched by the
//         stale `task.Status`) this would return 40016 ("published") when the
//         correct answer is 40009 ("revive in progress"). T25 pins the OCT-62
//         fix: fast-path is EXISTENCE-only, so the tx-scoped gate-in-tx reads
//         the FRESH lockTask.Status=Processing and dispatches 40009.
//         Injection rides the SAME in-tx `Before("gorm:query")` seam as T24,
//         but hooks the `summary_task` lockTask SELECT (the SECOND
//         summary_task query of the handler) and flips task.status to
//         Processing right before that SELECT runs, so the SELECT reads back
//         Processing. Reverse direction (early Processing → later Completed →
//         must return 40016) is covered by the reverse mode below.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

// seedPublishedSummary inserts one summary_result row for taskID that mirrors
// what worker.saveLatestResultAndCompleteTask would produce (version=1,
// generated_at=now, non-empty content), and sets task.status to the caller-
// specified target (StatusCompleted / StatusProcessing). Used by T19, T21,
// T22 to reproduce "summary is/was published" fixtures without wiring the
// worker into the handler test.
func seedPublishedSummary(t *testing.T, db *gorm.DB, taskID int64, taskStatus int) {
	t.Helper()
	now := timezone.Now()
	res := model.SummaryResult{
		TaskID:      taskID,
		Content:     "seeded team summary body",
		Version:     1,
		GeneratedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := db.Create(&res).Error; err != nil {
		t.Fatalf("seed summary_result: %v", err)
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
		Update("status", taskStatus).Error; err != nil {
		t.Fatalf("update task.status: %v", err)
	}
}

// T19 — single-person task whose team summary has ALREADY been published:
// summary_result exists AND task.status=Completed. Draft attempt is rejected
// by the new fast-path published-check with 40016 ("该总结已发布，请改走编辑
// 接口"). personal_result.content MUST NOT change.
func TestPersonalDraft_SinglePerson_Published_FastPath_40016_T19(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	seedPublishedSummary(t, db, taskID, model.StatusCompleted)

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	body := map[string]interface{}{"content": "draft after publish [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 from published fast-path, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40016)

	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content != "member_x original [1][2]" {
		t.Errorf("published fast-path must not write content, got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("published fast-path must not trigger meta_summary")
	}
}

// T20 — Regression fence: multi-person task, worker_status=Pending on the
// caller's row (Accept-in-flight, personal worker not yet done), summary_result
// absent. This case is claimed by the existing 690 gate (40005) and OCT-50
// must not silently start returning 40009 here just because the new
// published-check would otherwise see count=0. The value of this test is
// negative: it pins the 690 gate as the FIRST reject, ahead of any new gate.
func TestPersonalDraft_MultiPerson_WorkerPending_StaysAt40005_T20(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	// Flip only member_x back to Pending; leave everyone else Completed and no
	// summary_result seeded.
	if err := db.Model(&model.PersonalResult{}).Where("id = ?", xPRID).
		Update("worker_status", model.PersonalStatusPending).Error; err != nil {
		t.Fatalf("flip worker_status: %v", err)
	}

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	body := map[string]interface{}{"content": "premature draft [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400/40005 from 690 gate, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40005)
}

// T21 / T22 — Revive-post-publish (multi-person task): task.status flipped
// Completed→Processing and summary_result retained. The concrete revive
// producers differ (Leave/RemoveMember vs AddMembers-Accept), but both land
// on the same decision path in PersonalDraft: fast-path published-check sees
// summary_result present + task.status=Processing → errDraftRegenerating →
// 40009. Testing the DECISION PATH via a shared seed fixture rather than
// re-running the whole revive plumbing keeps the tests scoped to the OCT-50
// change surface.
func TestPersonalDraft_Revive_LeaveRemove_FastPath_40009_T21(t *testing.T) {
	// The Leave/RemoveMember producer path lands as: task.status=Processing +
	// summary_result present. Producer semantics are exercised in the
	// dedicated revive tests elsewhere; here we assert only that the draft
	// handler dispatches this state to 40009.
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	seedPublishedSummary(t, db, taskID, model.StatusProcessing)

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	body := map[string]interface{}{"content": "draft during revive [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409/40009 from revive fast-path, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40009)

	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content != "member_x original [1][2]" {
		t.Errorf("revive fast-path must not write content, got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("revive fast-path must not trigger meta_summary")
	}
}

// T22 — same decision path as T21 but framed as the AddMembers-Accept revive
// shape (the new member's PersonalResult is Completed + submitted_at=NULL by
// the time PersonalDraft can hit, and task.status/summary_result mirror T21).
// Kept as a distinct test so the matrix explicitly names both revive
// producers.
func TestPersonalDraft_Revive_AddMemberAccept_FastPath_40009_T22(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	seedPublishedSummary(t, db, taskID, model.StatusProcessing)

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	body := map[string]interface{}{"content": "new member draft after revive [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409/40009 from revive fast-path, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40009)

	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content != "member_x original [1][2]" {
		t.Errorf("revive fast-path must not overwrite personal_result content, got %q", prX.Content)
	}
}

// T23 — Regenerate-mid-tx race → probe-(d) → 40009.
//
// Fixture: fast-path count observes 0 (no summary_result seeded, task.status
// left Completed). The tx starts, lockTask succeeds. A one-shot in-tx
// callback registered before `gorm:update` mutates the SAME row's
// worker_status Completed→Pending BEFORE the UPDATE runs. The UPDATE's
// `worker_status = Completed` clause now misses (RowsAffected=0). The probe
// SELECT reads back worker_status=Pending → errDraftRegenerating → 40009.
//
// This is behaviourally equivalent to the production race where a Regenerate
// tx (task.go:927-966) commits between the fast-path count and the intra-tx
// UPDATE. See probe-(d) branch documentation in personal.go for why this
// branch is unreachable via revive (revive does NOT touch worker_status).
//
// Injection pattern mirrors T9b's runRaceSubmitWinsDuringUpdate
// (personal_draft_test.go:225-260); the sqlite `:memory:` sibling-session
// caveat noted there applies here too.
func TestPersonalDraft_Regenerate_ProbeD_WorkerStatusFlipped_40009_T23(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	cbName := "test:oct50_regen_flips_worker_status"
	fired := false
	err := db.Callback().Update().Before("gorm:update").Register(cbName, func(tx *gorm.DB) {
		if fired {
			return
		}
		if tx.Statement == nil || tx.Statement.Table != "summary_personal_result" {
			return
		}
		fired = true
		// Same in-tx write pattern as T9b — sqlite :memory: gives sibling
		// sessions private DBs, so the racing write must commit through the
		// in-flight tx so the UPDATE's WHERE observes the mutation.
		if err := tx.Exec("UPDATE summary_personal_result SET worker_status = ? WHERE id = ?",
			model.PersonalStatusPending, xPRID).Error; err != nil {
			t.Errorf("regen race injector failed: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("registering regen callback: %v", err)
	}
	defer func() {
		_ = db.Callback().Update().Remove(cbName)
	}()

	body := map[string]interface{}{"content": "draft racing regenerate [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 from probe-(d), got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40009)
	if !fired {
		t.Errorf("regen race injector never fired -- probe-(d) path was not exercised")
	}

	// Draft content must NOT have landed (UPDATE missed on worker_status
	// clause; tx rolled back on errDraftRegenerating).
	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content == "draft racing regenerate [1]" {
		t.Errorf("probe-(d) 409 must NOT persist draft content, got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("probe-(d) 409 must not trigger meta_summary")
	}
}

// T24 — Gate-in-tx decision-layer verification (§2.4 core blocker landing).
//
// Fixture: fast-path count observes 0 (no summary_result yet). The tx starts
// and lockTask acquires. A one-shot in-tx callback registered before
// `gorm:query` — targeting the summary_result table — is the seam we use to
// interpose BETWEEN the tx-scoped lockTask (query on summary_task) and the
// tx-scoped intra-tx re-check on summary_result. When the callback fires it
// SEEDs a summary_result row into the same tx and flips task.status→Completed
// via the same tx. The following intra-tx SELECT on summary_result (from the
// handler's gate-in-tx) then returns the newly-seeded row → errDraftPublished
// → 40016.
//
// Important honest disclosure (from v2.1 Patch 1):
//   - sqlite `:memory:` sibling gorm sessions do NOT share the DB
//     (personal_draft_test.go:246-255), so we CANNOT stage a wall-clock race
//     from a sibling goroutine. Injection therefore rides the SAME tx.
//   - sqlite `:memory:` SILENTLY IGNORES `FOR UPDATE`
//     (members_auth_test.go:1837-1845), so this test does NOT exercise the
//     wall-clock lock ordering — it verifies the DECISION LAYER
//     (SELECT-then-branch on summary_result inside the tx). Real lock-order
//     evidence lives in production MySQL / an integration seam, out of scope
//     for the "personal.go only" change.
//
// Double-check (see the OCT-50 comment thread and PR body): if the
// gate-in-tx SELECT-then-branch block is temporarily commented out in
// personal.go, this test flips from PASS to FAIL — the draft UPDATE would
// otherwise land because the fast-path never observed the seeded row (fast-
// path runs OUTSIDE any tx and reads from the pre-callback view). That is the
// reverse-direction "the branch is load-bearing" evidence.
func TestPersonalDraft_GateInTx_TxScopedPublishedCheck_BlocksAfterFastPath_40016_T24(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	// Fixture setup:
	//   • seedDraftableTask left task.status = Completed and no summary_result.
	//   • The fast-path published-check therefore runs Select id FROM
	//     summary_result → 0 rows → falls through into the tx.
	//   • The tx-scoped lockTask reads task.status = Completed.
	//   • The tx-scoped intra-tx SELECT on summary_result then fires the
	//     callback below, which seeds one summary_result row into the same tx.
	//   • That SELECT returns the seeded row, lockTask.Status = Completed →
	//     gate-in-tx returns errDraftPublished → 40016.

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	// Interposition seam. Handler emits TWO summary_result queries: the
	// fast-path one (outside any tx) and the intra-tx one (inside the
	// gate-in-tx). We must seed BEFORE the second query only — seeding
	// before the first would collapse this into the fast-path published-
	// check path (T19 already covers that). A shared summaryResultCallSeen
	// counter skips the fast-path invocation.
	cbName := "test:oct50_gate_in_tx_publish"
	summaryResultQueries := 0
	fired := false
	err := db.Callback().Query().Before("gorm:query").Register(cbName, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "summary_result" {
			return
		}
		summaryResultQueries++
		// Skip the fast-path SELECT (call #1). Fire on the intra-tx SELECT
		// (call #2), which the handler emits AFTER lockTask has already
		// captured task.status.
		if summaryResultQueries < 2 || fired {
			return
		}
		fired = true
		now := timezone.Now()
		// Seed a summary_result row for this task inside the same tx.
		// After this INSERT commits into the tx-visible state, the
		// currently-in-flight SELECT (still queued behind this Before
		// callback) returns the seeded row → gate-in-tx dispatches
		// errDraftPublished → 40016.
		if err := tx.Session(&gorm.Session{NewDB: true}).Exec(
			"INSERT INTO summary_result (task_id, content, citations_json, team_citations_json, total_msg_count, total_token_used, model_version, version, generated_at, created_at, updated_at) VALUES (?, ?, '', '', 0, 0, '', 1, ?, ?, ?)",
			taskID, "in-tx seeded summary body", now, now, now).Error; err != nil {
			t.Errorf("gate-in-tx race injector failed to seed summary_result: %v", err)
			return
		}
	})
	if err != nil {
		t.Fatalf("registering gate-in-tx callback: %v", err)
	}
	defer func() {
		_ = db.Callback().Query().Remove(cbName)
	}()

	body := map[string]interface{}{"content": "draft racing publish [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 from gate-in-tx published-check, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40016)
	if !fired {
		t.Errorf("gate-in-tx race injector never fired -- the intra-tx re-check branch was not exercised")
	}

	// Draft content MUST NOT have landed. The tx rolled back on
	// errDraftPublished (which happens BEFORE the UPDATE), so we assert the
	// personal_result content remains the seeded original.
	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content == "draft racing publish [1]" {
		t.Errorf("gate-in-tx 40016 must NOT persist draft content, got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("gate-in-tx 40016 must not trigger meta_summary")
	}
}
// T25 — Fast-path stale-task.Status race (OCT-62 gpt blocker).
//
// Fixture: seedDraftableTask leaves task.status=Completed; then
// seedPublishedSummary seeds a summary_result row (still task.status=Completed
// -- i.e. the "already published" shape). Under the pre-OCT-62 fast-path this
// would take the 40016 branch by looking at the (about-to-be-stale) task.Status
// captured by requireTaskInSpace at :709.
//
// Race injection: a Before("gorm:query") callback on `summary_task` fires on
// the SECOND summary_task query (the tx-scoped `lockTask` SELECT at :878;
// call #1 is the outer `requireTaskInSpace` at :155, which happens BEFORE the
// tx opens and captures the stale Completed). When the callback fires it
// UPDATEs task.status → Processing via the same tx (mirroring T24's
// same-tx-write pattern), so the in-flight `First(&lockTask, taskID)` reads
// back the fresh Processing status. The gate-in-tx then observes
// summary_result HIT + lockTask.Status=Processing → errDraftRegenerating →
// 40009 (the CORRECT dispatch for the concurrent-revive shape).
//
// Reverse assertion (buggy pre-OCT-62 code): if the fast-path were restored to
// its "dispatch by task.Status" version, this test would fail with 40016 —
// because :709 captured Completed, and the fast-path would return without
// ever reaching the gate-in-tx where the fresh status lives. This is the
// exact scenario gpt's code review flagged (OCT-61 blocker).
//
// sqlite :memory: caveats (same as T24):
//   - FOR UPDATE is silently ignored, so this test does not exercise real
//     lock-order — it verifies the DECISION LAYER (gate-in-tx dispatches by
//     fresh lockTask.Status, not by stale outer task.Status).
//   - Sibling sessions do not share the DB, so the injected UPDATE must ride
//     the SAME tx via `tx.Session(&gorm.Session{NewDB: true}).Exec`.
func TestPersonalDraft_FastPath_TaskStatusFlipped_ByRevive_40009_T25(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	// Seed the "already published" shape: summary_result row present,
	// task.status still Completed. The fast-path (post-OCT-62) probes
	// summary_result, HITS, but does NOT dispatch — it falls through into the
	// tx. Inside the tx, the lockTask SELECT is where we flip status.
	seedPublishedSummary(t, db, taskID, model.StatusCompleted)

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	// Interposition seam. Handler emits TWO summary_task queries:
	//   1. requireTaskInSpace() at personal.go:155 — OUTSIDE the tx, reads the
	//      pre-race Completed. This capture is what makes the pre-OCT-62
	//      fast-path stale.
	//   2. `SELECT ... FOR UPDATE` on summary_task inside the tx at :878
	//      (lockTask). This is the AUTHORITATIVE read the gate-in-tx branches
	//      on. We interpose here, flip task.status → Processing in-tx BEFORE
	//      GORM runs the queued SELECT, so the SELECT reads back Processing.
	cbName := "test:oct62_stale_task_status_race"
	summaryTaskQueries := 0
	fired := false
	err := db.Callback().Query().Before("gorm:query").Register(cbName, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "summary_task" {
			return
		}
		summaryTaskQueries++
		// Skip the outer requireTaskInSpace (call #1). Fire on the tx-scoped
		// lockTask SELECT (call #2), which is the SELECT the gate-in-tx
		// dispatches on.
		if summaryTaskQueries < 2 || fired {
			return
		}
		fired = true
		// Flip task.status Completed → Processing in the SAME tx. Under sqlite
		// :memory: sibling sessions cannot see each other, so the write must
		// go through the current tx via `NewDB: true` (mirroring T24).
		if err := tx.Session(&gorm.Session{NewDB: true}).Exec(
			"UPDATE summary_task SET status = ? WHERE id = ?",
			model.StatusProcessing, taskID).Error; err != nil {
			t.Errorf("stale-status race injector failed to flip task.status: %v", err)
			return
		}
	})
	if err != nil {
		t.Fatalf("registering stale-status callback: %v", err)
	}
	defer func() {
		_ = db.Callback().Query().Remove(cbName)
	}()

	body := map[string]interface{}{"content": "draft racing revive [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 from gate-in-tx (fresh Processing), got %d: %s", w.Code, w.Body.String())
	}
	// The KEY assertion: 40009, NOT 40016. Under the pre-OCT-62 fast-path
	// (dispatch by stale task.Status=Completed) this would be 40016 — that is
	// the bug gpt flagged.
	assertBizCode(t, w, 40009)
	if !fired {
		t.Errorf("stale-status race injector never fired -- lockTask SELECT branch was not exercised")
	}

	// Draft content MUST NOT have landed: gate-in-tx rolled back on
	// errDraftRegenerating BEFORE the UPDATE.
	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content == "draft racing revive [1]" {
		t.Errorf("gate-in-tx 40009 must NOT persist draft content, got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("gate-in-tx 40009 must not trigger meta_summary")
	}
}

// T25b — Reverse direction of T25. Early read is Processing (revive already
// in flight when requireTaskInSpace ran); revive then completes and publishes
// (task.status flips Processing → Completed) between the outer read and the
// tx-scoped lockTask. Correct dispatch is 40016 (published), not 40009. Under
// pre-OCT-62 fast-path this would incorrectly return 40009. This pins the
// reverse case gpt described in OCT-61.
func TestPersonalDraft_FastPath_TaskStatusFlipped_ByPublish_40016_T25b(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, xPRID := seedDraftableTask(t, db)
	// Seed the pre-race shape: summary_result present, task.status = Processing
	// (revive in flight). The outer requireTaskInSpace captures Processing;
	// then in-tx callback flips to Completed so the fresh lockTask reads
	// Completed and gate-in-tx dispatches 40016.
	seedPublishedSummary(t, db, taskID, model.StatusProcessing)

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalDraftRouter(h)

	cbName := "test:oct62_stale_task_status_race_reverse"
	summaryTaskQueries := 0
	fired := false
	err := db.Callback().Query().Before("gorm:query").Register(cbName, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "summary_task" {
			return
		}
		summaryTaskQueries++
		if summaryTaskQueries < 2 || fired {
			return
		}
		fired = true
		if err := tx.Session(&gorm.Session{NewDB: true}).Exec(
			"UPDATE summary_task SET status = ? WHERE id = ?",
			model.StatusCompleted, taskID).Error; err != nil {
			t.Errorf("reverse race injector failed to flip task.status: %v", err)
			return
		}
	})
	if err != nil {
		t.Fatalf("registering reverse callback: %v", err)
	}
	defer func() {
		_ = db.Callback().Query().Remove(cbName)
	}()

	body := map[string]interface{}{"content": "draft racing publish (reverse) [1]"}
	w := doJSONRequest(r, "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-draft", taskID),
		"member_x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 from gate-in-tx (fresh Completed), got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40016)
	if !fired {
		t.Errorf("reverse race injector never fired")
	}

	var prX model.PersonalResult
	db.Where("id = ?", xPRID).First(&prX)
	if prX.Content == "draft racing publish (reverse) [1]" {
		t.Errorf("gate-in-tx 40016 must NOT persist draft content, got %q", prX.Content)
	}
	if tc.waitFor("meta_summary", taskID) {
		t.Errorf("gate-in-tx 40016 must not trigger meta_summary")
	}
}
