package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// TestGetSummary_IncludesCreatorID verifies the task detail DTO exposes the
// real creator_id so the client can stamp the correct author instead of the
// observing user's login. See OCT-35.
func TestGetSummary_IncludesCreatorID(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "CID-001",
		SpaceID:        "space1",
		CreatorID:      "creator-user",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
	}
	db.Create(&task)
	// A non-creator participant observes the task; the DTO must still report
	// the real creator, not the requesting participant.
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "observer-user", UserName: "Observer"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator-user", UserName: "Creator"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req.Header.Set("Token", "observer-user")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})

	got, present := data["creator_id"]
	if !present {
		t.Fatalf("creator_id missing from detail DTO; keys=%v", keysOf(data))
	}
	if got != "creator-user" {
		t.Errorf("creator_id: want creator-user, got %v", got)
	}
}

// TestListSummaries_IncludesCreatorID verifies the list DTO exposes creator_id
// for every item. See OCT-35.
func TestListSummaries_IncludesCreatorID(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "CID-LST-001",
		SpaceID:        "space1",
		CreatorID:      "creator-user",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now.Add(-24 * time.Hour),
		TimeRangeEnd:   now,
	}
	db.Create(&task)
	// Requesting user is a participant but not the creator.
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "observer-user", UserName: "Observer"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summaries", nil)
	req.Header.Set("Token", "observer-user")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	items := data["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0].(map[string]interface{})

	got, present := item["creator_id"]
	if !present {
		t.Fatalf("creator_id missing from list DTO; keys=%v", keysOf(item))
	}
	if got != "creator-user" {
		t.Errorf("creator_id: want creator-user, got %v", got)
	}
}

func keysOf(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
