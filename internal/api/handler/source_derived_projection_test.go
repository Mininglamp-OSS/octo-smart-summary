//go:build cgo

package handler

// R9 P1 (yujiawei, PR #190 review 4926742282):
//
// "The backfill's stated purpose is internal ... But it writes into
// summary_source, which is not an internal table — it is serialized straight
// into the public API response on both the list and detail endpoints, and
// into the agent prompt ... Alice creates a personal summary with no
// explicit sources ... the new backfill persists every fetched channel ...
// Alice later adds Bob to the task ... Bob is now a SummaryParticipant, so
// canAccessTaskDB passes and GET /api/v1/summaries/:id returns the full
// sources array to him."
//
// Fix shape chosen (reviewer's option 1 + 2): backfilled rows carry
// derived=1 and are excluded from every user-visible projection (list,
// detail, share snapshot, reference-context bullets); DM channels are
// skipped entirely by the backfill. Tier-4 origin derivation
// (deriveOriginFromSummarySources) still reads derived rows — that is the
// only consumer they exist for.
//
// These tests pin the projection contract at the HTTP layer.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// seedDerivedProjectionTask creates one completed BY_PERSON task owned by
// "creator1" with one user-specified source and one worker-backfilled
// (derived) source.
func seedDerivedProjectionTask(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	task := model.SummaryTask{
		TaskNo:      "TST-DERIVED-PROJ",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	explicit := model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp-explicit", SourceName: "用户选的群"}
	if err := db.Create(&explicit).Error; err != nil {
		t.Fatalf("create explicit source: %v", err)
	}
	derived := model.SummarySource{TaskID: task.ID, SourceType: model.SourceThread, SourceID: "grp-x____thr-auto", SourceName: "自动抓取的子区", Derived: true}
	if err := db.Create(&derived).Error; err != nil {
		t.Fatalf("create derived source: %v", err)
	}
	return task.ID
}

func TestGetSummary_DerivedSourcesNotExposed(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	taskID := seedDerivedProjectionTask(t, db)

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Sources []struct {
				SourceType int    `json:"source_type"`
				SourceID   string `json:"source_id"`
				SourceName string `json:"source_name"`
			} `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Data.Sources) != 1 {
		t.Fatalf("expected 1 source (explicit only, derived filtered), got %d: %+v", len(resp.Data.Sources), resp.Data.Sources)
	}
	if resp.Data.Sources[0].SourceID != "grp-explicit" {
		t.Errorf("exposed source = %q, want grp-explicit", resp.Data.Sources[0].SourceID)
	}
}

func TestListSummaries_DerivedSourcesNotExposed(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedDerivedProjectionTask(t, db)

	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	resp := parseListResponse(t, w)
	if resp.Data.Total != 1 {
		t.Fatalf("expected total 1, got %d", resp.Data.Total)
	}
	item, ok := resp.Data.Items[0].(map[string]interface{})
	if !ok {
		t.Fatalf("item not an object: %v", resp.Data.Items[0])
	}
	srcs, ok := item["sources"].([]interface{})
	if !ok {
		t.Fatalf("sources missing or wrong type: %v", item["sources"])
	}
	if len(srcs) != 1 {
		t.Fatalf("expected 1 source in list projection (derived filtered), got %d: %v", len(srcs), srcs)
	}
	only := srcs[0].(map[string]interface{})
	if only["source_id"] != "grp-explicit" {
		t.Errorf("list exposed source = %v, want grp-explicit", only["source_id"])
	}
}
