//go:build cgo

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestLegacyPersonalUpgradeRefineDoesNotInventEdit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			db, h, _, b := seedOptimization(t, "personal")
			raw := fmt.Sprintf(`{"snapshot_version":1,"task_id":%d,"content_version":1,"scope":{"channel_ids":["original-source"]}}`, b.Task.ID)
			v := model.PersonalResultVersion{TaskID: b.Task.ID, UserID: "test-user", Version: 1, Content: b.Personal.Content, OperationType: "generate"}
			if err := db.Create(&v).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&b.Personal).Updates(map[string]interface{}{"snapshot_json": raw, "current_version_id": v.ID}).Error; err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Refined legacy\"}}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"content":"Refined legacy"},"finish_reason":"stop"}]}`)
				}
			}))
			defer srv.Close()
			ph := NewPersonalHandler(db, "", nil)
			ph.SetLLM(service.NewLLMClient(srv.URL, "test", "test", 5, 256, false, 5, nil))
			r := setupAgentSummaryRouter(h)
			if stream {
				r.POST("/api/v1/summaries/:id/refine", ph.RefinePersonalSummaryStream)
			} else {
				r.POST("/api/v1/summaries/:id/refine", ph.RefinePersonalSummary)
			}
			w := provenanceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/refine", b.Task.ID), `{"feedback":"shorter","base_version":1}`, "test-user")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var rows []model.PersonalResultVersion
			if err := db.Where("task_id = ?", b.Task.ID).Order("version").Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].OperationType != "generate" || rows[1].Version != 2 {
				t.Fatalf("phantom baseline: %+v", rows)
			}
			if rows[0].SnapshotJSON == nil || *rows[0].SnapshotJSON != raw {
				t.Fatal("legacy current provenance not recovered")
			}
			snapshot := decodeResultSnapshot(rows[1].SnapshotJSON)
			if snapshot == nil || snapshot.ContentVersion != 2 || snapshot.ParentSnapshotVersion == nil || *snapshot.ParentSnapshotVersion != 1 {
				t.Fatalf("bad new provenance: %+v", snapshot)
			}
		})
	}
}

func TestLegacyPersonalBaselineNormalizesWithoutInventingHistory(t *testing.T) {
	for _, scenario := range []string{"matching", "no-pointer", "different-content", "different-version", "foreign-task", "known-change"} {
		t.Run(scenario, func(t *testing.T) {
			db, _, _, b := seedOptimization(t, "personal")
			raw := fmt.Sprintf(`{"snapshot_version":1,"task_id":%d,"content_version":1,"scope":{"channel_ids":["original-source"]}}`, b.Task.ID)
			if scenario == "different-version" {
				raw = fmt.Sprintf(`{"task_id":%d,"content_version":9}`, b.Task.ID)
			}
			if scenario == "foreign-task" {
				raw = `{"task_id":999,"content_version":1}`
			}
			v := model.PersonalResultVersion{TaskID: b.Task.ID, UserID: "test-user", Version: 1, Content: b.Personal.Content}
			if scenario == "different-content" {
				v.Content = "Other old content"
			}
			if scenario == "known-change" {
				known := fmt.Sprintf(`{"task_id":%d,"content_version":1,"scope":{"channel_ids":["other"]}}`, b.Task.ID)
				v.SnapshotJSON = &known
			}
			if err := db.Create(&v).Error; err != nil {
				t.Fatal(err)
			}
			b.Personal.SnapshotJSON = &raw
			if scenario != "no-pointer" {
				b.Personal.CurrentVersionID = &v.ID
			}
			if err := db.Save(&b.Personal).Error; err != nil {
				t.Fatal(err)
			}
			// Direct helper coverage also checks upgrade recovery without a current pointer.
			if err := service.NormalizePersonalVersionSnapshot(db, b.Personal, &v); err != nil {
				t.Fatal(err)
			}
			if scenario == "matching" || scenario == "no-pointer" {
				if v.SnapshotJSON == nil || *v.SnapshotJSON != raw {
					t.Fatal("matching snapshot not backfilled")
				}
			} else if scenario != "known-change" && v.SnapshotJSON != nil {
				t.Fatal("unrelated historical scope was fabricated")
			}
			if scenario == "different-content" {
				return
			}
			_, err := ensurePersonalVersionBaseline(db, b.Personal)
			if err != nil {
				t.Fatal(err)
			}
			var count int64
			db.Model(&model.PersonalResultVersion{}).Where("task_id = ?", b.Task.ID).Count(&count)
			expected := int64(1)
			if scenario == "known-change" || scenario == "different-version" || scenario == "foreign-task" {
				expected = 2
			}
			if count != expected {
				t.Fatalf("history count %d want %d", count, expected)
			}
		})
	}
}

func TestTeamManualEditMirrorsProducingProvenance(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			db, h, _, b := seedOptimization(t, "team")
			teamRaw := fmt.Sprintf(`{"task_id":%d,"content_version":1,"scope":{"channel_ids":["team-source"]}}`, b.Task.ID)
			personalRaw := fmt.Sprintf(`{"task_id":%d,"content_version":7,"scope":{"channel_ids":["old-personal-source"]}}`, b.Task.ID)
			if known {
				if err := db.Model(&b.Result).Update("snapshot_json", teamRaw).Error; err != nil {
					t.Fatal(err)
				}
			}
			v := model.PersonalResultVersion{TaskID: b.Task.ID, UserID: "test-user", Version: 7, Content: "Old personal", SnapshotJSON: &personalRaw}
			if err := db.Create(&v).Error; err != nil {
				t.Fatal(err)
			}
			pr := model.PersonalResult{TaskID: b.Task.ID, UserID: "test-user", Content: v.Content, SnapshotJSON: &personalRaw, CurrentVersionID: &v.ID, WorkerStatus: model.PersonalStatusCompleted}
			if err := db.Create(&pr).Error; err != nil {
				t.Fatal(err)
			}
			r := setupAgentSummaryRouter(h)
			r.PUT("/api/v1/summaries/:id/edit", NewEditHandler(db).EditSummary)
			w := provenanceRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/edit", b.Task.ID), fmt.Sprintf(`{"content":"Edited team","base_result_id":%d}`, b.Result.ID), "test-user")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if err := db.First(&pr, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			snapshot := pr.GetSnapshot()
			if pr.Content != "Edited team" || snapshot == nil || snapshot.ContentVersion != 7 || snapshot.ParentSnapshotVersion != nil {
				t.Fatalf("bad personal mirror: %+v", snapshot)
			}
			if known {
				if len(snapshot.Scope.ChannelIDs) != 1 || snapshot.Scope.ChannelIDs[0] != "team-source" {
					t.Fatal("stale personal provenance")
				}
			} else if snapshot.ProvenanceStatus != "unknown" || len(snapshot.Scope.ChannelIDs) != 0 {
				t.Fatal("borrowed missing display provenance")
			}
			var history model.PersonalResultVersion
			if err := db.First(&history, v.ID).Error; err != nil {
				t.Fatal(err)
			}
			if history.Content != "Old personal" || !sameSnapshotJSON(history.SnapshotJSON, &personalRaw) {
				t.Fatal("manual mirror rewrote old history")
			}
		})
	}
}

func TestPersonalOldContentRemainsReferenceableWithoutVersionWrite(t *testing.T) {
	db, h, _, b := seedOptimization(t, "personal")
	if err := db.Model(&b.Personal).Update("worker_status", model.PersonalStatusProcessing).Error; err != nil {
		t.Fatal(err)
	}
	// Referenceability permits reading an already-produced body; version writes
	// require a stable, completed personal result. They are separate capabilities.
	artifact, err := resolveReferencedArtifact(context.Background(), db, b.Task.ID, "test-space", "test-user")
	if err != nil || artifact.Content != b.Personal.Content {
		t.Fatalf("old body became unreadable: %v", err)
	}
	r := setupAgentSummaryRouter(h)
	r.GET("/api/v1/summaries/:id/optimization-target", h.GetOptimizationTarget)
	w := provenanceRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/optimization-target", b.Task.ID), "", "test-user")
	if w.Code != 404 {
		t.Fatalf("busy personal write advertised: %s", w.Body.String())
	}
}

func TestRestoreRecoversSelectedLegacyProvenance(t *testing.T) {
	for _, scenario := range []string{"pointer", "no-pointer", "null-citations"} {
		t.Run(scenario, func(t *testing.T) {
			db, h, _, b := seedOptimization(t, "personal")
			raw := fmt.Sprintf(`{"snapshot_version":1,"task_id":%d,"content_version":1,"requirement":"original requirement","scope":{"channel_ids":["original-source"]},"tool_trace":[{"tool":"fetch"}]}`, b.Task.ID)
			version := model.PersonalResultVersion{TaskID: b.Task.ID, UserID: "test-user", Version: 1, Content: b.Personal.Content, OperationType: "generate"}
			if err := db.Create(&version).Error; err != nil {
				t.Fatal(err)
			}
			updates := map[string]interface{}{"snapshot_json": raw}
			if scenario != "no-pointer" {
				updates["current_version_id"] = version.ID
			}
			if err := db.Model(&b.Personal).Updates(updates).Error; err != nil {
				t.Fatal(err)
			}
			if scenario == "null-citations" {
				if err := db.Model(&version).Update("citations_json", nil).Error; err != nil {
					t.Fatal(err)
				}
			}
			router := setupAgentSummaryRouter(h)
			router.POST("/api/v1/summaries/:id/personal-versions/:version_id/restore", NewPersonalHandler(db, "", nil).RestorePersonalVersion)
			response := provenanceRequest(router, "POST", fmt.Sprintf("/api/v1/summaries/%d/personal-versions/%d/restore", b.Task.ID, version.ID), "{}", "test-user")
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			var current model.PersonalResult
			if err := db.First(&current, b.Personal.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&version, version.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.SnapshotJSON == nil || version.SnapshotJSON == nil || *current.SnapshotJSON != raw || *version.SnapshotJSON != raw {
				t.Fatal("restore replaced recoverable provenance")
			}
			var count int64
			db.Model(&model.PersonalResultVersion{}).Where("task_id = ?", b.Task.ID).Count(&count)
			if count != 1 {
				t.Fatalf("restore invented %d history rows", count)
			}
		})
	}
}
