//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// The three SUM-19 response fields (referenceable / reference_artifact_type
// / reference_unavailable_reason) are the contract the frontend picker
// depends on. These handler-level tests assert them on actual HTTP responses
// from ListSummaries and GetSummary — the coverage gap flagged in the R7
// review (unit tests covered referenceableFromLoaded, but no test locked the
// wire contract).

// seedReferenceContractTasks creates one completed task with a non-empty
// team result (referenceable) and one processing task (not_completed).
func seedReferenceContractTasks(t *testing.T, db *gorm.DB) (doneID, openID int64) {
	t.Helper()
	done := model.SummaryTask{
		TaskNo:      "TST-RC-DONE",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&done).Error; err != nil {
		t.Fatalf("create completed task: %v", err)
	}
	open := model.SummaryTask{
		TaskNo:      "TST-RC-OPEN",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusProcessing,
	}
	if err := db.Create(&open).Error; err != nil {
		t.Fatalf("create processing task: %v", err)
	}
	return done.ID, open.ID
}

func TestListSummaries_ReferenceableContract(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	doneID, openID := seedReferenceContractTasks(t, db)
	addTeamResult(t, db, doneID, "team result content")

	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 2 {
		t.Fatalf("expected total 2, got %d", resp.Data.Total)
	}

	fieldsByTaskID := map[float64]map[string]interface{}{}
	for _, item := range resp.Data.Items {
		m, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("item is not an object: %v", item)
		}
		fieldsByTaskID[m["task_id"].(float64)] = m
	}

	done := fieldsByTaskID[float64(doneID)]
	if done == nil {
		t.Fatalf("completed task %d missing from list", doneID)
	}
	if done["referenceable"] != true {
		t.Errorf("completed team-result task: referenceable = %v, want true", done["referenceable"])
	}
	if done["reference_artifact_type"] != "team_result" {
		t.Errorf("completed team-result task: reference_artifact_type = %v, want team_result", done["reference_artifact_type"])
	}
	if done["reference_unavailable_reason"] != "" {
		t.Errorf("completed team-result task: reference_unavailable_reason = %v, want empty", done["reference_unavailable_reason"])
	}

	open := fieldsByTaskID[float64(openID)]
	if open == nil {
		t.Fatalf("processing task %d missing from list", openID)
	}
	if open["referenceable"] != false {
		t.Errorf("processing task: referenceable = %v, want false", open["referenceable"])
	}
	if open["reference_artifact_type"] != "" {
		t.Errorf("processing task: reference_artifact_type = %v, want empty", open["reference_artifact_type"])
	}
	if open["reference_unavailable_reason"] != "not_completed" {
		t.Errorf("processing task: reference_unavailable_reason = %v, want not_completed", open["reference_unavailable_reason"])
	}
}

func TestGetSummary_ReferenceableContract(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	doneID, openID := seedReferenceContractTasks(t, db)
	addTeamResult(t, db, doneID, "team result content")

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	for _, tc := range []struct {
		name        string
		taskID      int64
		wantRefable bool
		wantType    string
		wantReason  string
	}{
		{"completed team result", doneID, true, "team_result", ""},
		{"not completed", openID, false, "", "not_completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", tc.taskID), "creator1")
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			var resp struct {
				Code int `json:"code"`
				Data struct {
					Referenceable          bool   `json:"referenceable"`
					ReferenceArtifactType  string `json:"reference_artifact_type"`
					ReferenceUnavailReason string `json:"reference_unavailable_reason"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
			}
			if resp.Code != 0 {
				t.Fatalf("expected code 0, got %d", resp.Code)
			}
			if resp.Data.Referenceable != tc.wantRefable {
				t.Errorf("referenceable = %v, want %v", resp.Data.Referenceable, tc.wantRefable)
			}
			if resp.Data.ReferenceArtifactType != tc.wantType {
				t.Errorf("reference_artifact_type = %q, want %q", resp.Data.ReferenceArtifactType, tc.wantType)
			}
			if resp.Data.ReferenceUnavailReason != tc.wantReason {
				t.Errorf("reference_unavailable_reason = %q, want %q", resp.Data.ReferenceUnavailReason, tc.wantReason)
			}
		})
	}
}
