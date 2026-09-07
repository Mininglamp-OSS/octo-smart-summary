//go:build cgo

package handler

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Pentest 4.11 (逻辑漏洞·垂直越权) regression — ListSummaries empty-X-Space-Id
// fail-closed hard gate.
//
// The report's mechanism: space-level authorization hinges on the X-Space-Id
// header; when a caller simply REMOVES that header the backend must NOT fall
// open. summary's SpaceMiddleware deliberately allows an empty space_id
// (space_id="" => "no isolation") and StrictSpaceMiddleware only enforces the
// header on WRITE methods, so a GET like /api/v1/summaries reaches the handler
// with space_id="". SummaryTask.SpaceID is `not null default ''`, so a
// space_id='' row CAN exist; without an explicit guard the list query
// `space_id=''` would MATCH such a row and return it (fail-open cross-space
// read == the 4.11 bypass).
//
// ListSummaries (task.go) now short-circuits spaceID=="" -> 40008/404 BEFORE
// the query. The detail/accept/submit/personal/members read paths already have
// their own empty-space regressions in members_auth_test.go; this file closes
// the one remaining summary READ surface — the list endpoint — against the
// exact "delete the header" PoC.
// ---------------------------------------------------------------------------

// seedEmptySpaceListTask seeds a task whose SpaceID is the empty string, owned
// by creator1, into the list test DB. If the empty-space hard gate were absent,
// a header-less list call would run `space_id=” AND creator_id='creator1'`,
// match this row, and return total=1 (the fail-open the guard must prevent).
func seedEmptySpaceListTask(t *testing.T, db *gorm.DB) {
	t.Helper()
	task := model.SummaryTask{
		TaskNo:      "TST-LIST-EMPTYSPACE-001",
		SpaceID:     "", // <-- the anomalous space_id='' row the gate must hide
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1"})
}

// 4.11: the creator of a space_id=” task, listing with NO X-Space-Id header,
// must get 404/40008 and NO task rows — the header-deletion bypass is closed.
// FAIL-before: empty header -> query `space_id=”` matches the seeded row ->
// 200 with total=1 (cross-space roster of an anomalous row leaks). PASS-after:
// the spaceID=="" short-circuit 404s before any query.
func TestListSummaries_EmptySpaceHeader_Returns404NoLeak(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedEmptySpaceListTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	// creator1 is a genuine creator of the space_id='' task, but sends no header.
	w := doRequestWithSpace(r, "GET", "/api/v1/summaries", "creator1", "")
	assertCrossSpace404(t, w) // 404 + resp.Code == 40008

	// The task must not have leaked through the empty-space request.
	if bytes.Contains(w.Body.Bytes(), []byte("TST-LIST-EMPTYSPACE-001")) ||
		bytes.Contains(w.Body.Bytes(), []byte("\"items\"")) {
		t.Errorf("empty-space ListSummaries must NOT return any task, body=%s", w.Body.String())
	}
}

// 4.11 companion (non-empty but wrong space): a creator of a space1 task who
// lists with a DIFFERENT, non-empty X-Space-Id must NOT see it. The space_id
// filter scopes the query, so the result is a clean 200 with total=0 — no
// cross-space enumeration. (Unlike the empty header, a wrong non-empty header
// is a normal scoped query, not the hard-gate 404.)
func TestListSummaries_WrongSpaceHeader_SeesNothing(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB) // task lives in space1, creator1 + participant1
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequestWithSpace(r, "GET", "/api/v1/summaries", "creator1", "other_space")
	if w.Code != http.StatusOK {
		t.Fatalf("wrong-space list must be a scoped 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 0 {
		t.Errorf("wrong-space list must not surface another space's task, got total=%d", resp.Data.Total)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("TST-LIST-001")) {
		t.Errorf("wrong-space list must not leak the space1 task, body=%s", w.Body.String())
	}
}

// No-regression inverse: the SAME creator with the CORRECT space header still
// sees their task (200, total=1), proving the empty-space gate did not
// over-block legitimate in-space list reads.
func TestListSummaries_EmptySpaceGate_NoRegressionInSpace(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB) // space1, creator1 + participant1
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequestWithSpace(r, "GET", "/api/v1/summaries", "creator1", "space1")
	if w.Code != http.StatusOK {
		t.Fatalf("in-space creator must still get 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 1 {
		t.Errorf("in-space creator must still see their task, got total=%d", resp.Data.Total)
	}
}
