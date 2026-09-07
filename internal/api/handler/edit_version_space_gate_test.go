//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Pentest 4.11 (逻辑漏洞·垂直越权) — empty-X-Space-Id fail-closed gate on the
// summary version-history reads.
//
// GET /summaries/:id/versions and /summaries/:id/versions/:result_id previously
// queried `id=? AND space_id=?` with NO spaceID=="" short-circuit — the only
// summary read paths missing the hard gate every sibling has
// (authorizeTaskAccess / requireTaskInSpace / ListSummaries / ListSchedules).
// Because SummaryTask.SpaceID is `not null default ''`, an anomalous space_id=''
// row could be read version-by-version by a header-less request from its
// creator/participant (the exact "delete the X-Space-Id header" bypass). The
// handlers now short-circuit spaceID=="" -> 40008/404 before any query.
//
// These tests SEED a real space_id='' task (+ result version) so the gate is
// genuinely exercised: FAIL-before -> the empty-header query `space_id=''`
// matched the seeded row and returned 200; PASS-after -> 404/40008.
// ---------------------------------------------------------------------------

func setupVersionGateRouter(h *EditHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries/:id/versions", h.ListSummaryVersions)
	r.GET("/api/v1/summaries/:id/versions/:result_id", h.GetSummaryVersion)
	return r
}

// seedEmptySpaceVersionTask seeds a space_id=” task owned by creator1 with one
// SummaryResult version. Returns (taskID, resultID).
func seedEmptySpaceVersionTask(t *testing.T, db *gorm.DB) (int64, int64) {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:      "TST-EDIT-EMPTYSPACE-001",
		SpaceID:     "", // <-- the anomalous space_id='' row the gate must hide
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{
		TaskID: task.ID, UserID: "creator1", UserName: "Creator", Status: model.ParticipantCompleted,
	})
	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "empty-space version content",
		Version:     1,
		GeneratedAt: now,
	}
	db.Create(&result)
	return task.ID, result.ID
}

func doGetWithSpace(r *gin.Engine, path, userID, spaceID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	if spaceID != "" {
		req.Header.Set("X-Space-Id", spaceID)
	}
	r.ServeHTTP(w, req)
	return w
}

// 4.11: ListSummaryVersions with NO X-Space-Id against a real creator of a
// space_id=” task must 404/40008 and NOT return the version list.
// FAIL-before: empty header -> `space_id=”` matched -> 200 with versions.
func TestListSummaryVersions_EmptySpaceHeader_Returns404(t *testing.T) {
	db := setupEditDB(t)
	taskID, _ := seedEmptySpaceVersionTask(t, db)
	h := NewEditHandler(db)
	r := setupVersionGateRouter(h)

	w := doGetWithSpace(r, fmt.Sprintf("/api/v1/summaries/%d/versions", taskID), "creator1", "")
	assertCrossSpace404(t, w)

	var resp struct {
		Data struct {
			Versions []interface{} `json:"versions"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data.Versions) != 0 {
		t.Errorf("empty-space ListSummaryVersions must NOT return versions, got %d; body=%s", len(resp.Data.Versions), w.Body.String())
	}
}

// 4.11: GetSummaryVersion with NO X-Space-Id against a real creator of a
// space_id=” task must 404/40008 and NOT return the version content.
// FAIL-before: empty header -> `space_id=”` matched -> 200 with the version.
func TestGetSummaryVersion_EmptySpaceHeader_Returns404(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID := seedEmptySpaceVersionTask(t, db)
	h := NewEditHandler(db)
	r := setupVersionGateRouter(h)

	w := doGetWithSpace(r, fmt.Sprintf("/api/v1/summaries/%d/versions/%d", taskID, resultID), "creator1", "")
	assertCrossSpace404(t, w)

	if strings.Contains(w.Body.String(), "empty-space version content") {
		t.Errorf("empty-space GetSummaryVersion must NOT return the version content; body=%s", w.Body.String())
	}
}

// No-regression inverse: a genuine in-space task read with the CORRECT header
// still returns the versions (200), proving the new gate did not over-block.
func TestSummaryVersions_EmptySpaceGate_NoRegressionInSpace(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db) // space1, creator1, one result
	h := NewEditHandler(db)
	r := setupVersionGateRouter(h)

	wList := doGetWithSpace(r, fmt.Sprintf("/api/v1/summaries/%d/versions", taskID), "creator1", "space1")
	if wList.Code != http.StatusOK {
		t.Fatalf("in-space ListSummaryVersions must be 200, got %d: %s", wList.Code, wList.Body.String())
	}
	wGet := doGetWithSpace(r, fmt.Sprintf("/api/v1/summaries/%d/versions/%d", taskID, resultID), "creator1", "space1")
	if wGet.Code != http.StatusOK {
		t.Fatalf("in-space GetSummaryVersion must be 200, got %d: %s", wGet.Code, wGet.Body.String())
	}
}
