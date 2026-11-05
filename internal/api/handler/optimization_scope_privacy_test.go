//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestOptimizationUnlistedScopePrivacyAcrossReadsAndRestore(t *testing.T) {
	for _, kind := range []string{"team", "personal"} {
		t.Run(kind, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, kind)
			if err := db.Create(&model.SummaryParticipant{TaskID: b.Task.ID, UserID: "other", Status: model.ParticipantAccepted}).Error; err != nil {
				t.Fatal(err)
			}
			shared := model.SummarySource{TaskID: b.Task.ID, SourceID: "shared-group", SourceType: model.SourceGroup, SourceName: "Shared group"}
			if err := db.Create(&shared).Error; err != nil {
				t.Fatal(err)
			}
			var scope summaryWorkspaceContext
			if err := json.Unmarshal([]byte(f.Session.ScopeJSON), &scope); err != nil {
				t.Fatal(err)
			}
			scope.ReferencedTaskIDs = []int64{b.Task.ID}
			scope.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "shared-group", ChatType: "group", Name: "Shared group"}, {ChatID: "test-user@private-peer", ChatType: "direct", Name: "Private peer"}}
			raw, _, err := marshalSummaryWorkspaceContext(scope)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&f.Session).Update("scope_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "private-scope"})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			r := setupAgentSummaryRouter(h)
			var savedRaw *string
			var paths []string
			var result model.SummaryResult
			if kind == "team" {
				result, err = queryDisplayResult(db, b.Task.ID)
				if err != nil {
					t.Fatal(err)
				}
				savedRaw = result.SnapshotJSON
				eh := NewEditHandler(db)
				r.GET("/api/v1/summaries/:id", NewTaskHandler(db, nil, "").GetSummary)
				r.GET("/api/v1/summaries/:id/versions/:result_id", eh.GetSummaryVersion)
				r.POST("/api/v1/summaries/:id/versions/:result_id/restore", eh.RestoreSummaryVersion)
				paths = []string{fmt.Sprintf("/api/v1/summaries/%d", b.Task.ID), fmt.Sprintf("/api/v1/summaries/%d/versions/%d", b.Task.ID, result.ID)}
			} else {
				var pr model.PersonalResult
				if err := db.Where("task_id = ? AND user_id = ?", b.Task.ID, "test-user").First(&pr).Error; err != nil {
					t.Fatal(err)
				}
				savedRaw = pr.SnapshotJSON
				ph := NewPersonalHandler(db, "", nil)
				r.GET("/api/v1/summaries/:id/personal", ph.GetPersonal)
				r.GET("/api/v1/summaries/:id/personal-versions/:version_id", ph.GetPersonalVersion)
				paths = []string{fmt.Sprintf("/api/v1/summaries/%d/personal", b.Task.ID), fmt.Sprintf("/api/v1/summaries/%d/personal-versions/%d", b.Task.ID, *pr.CurrentVersionID)}
			}
			if savedRaw == nil || !strings.Contains(*savedRaw, "test-user@private-peer") {
				t.Fatal("save destroyed producer provenance")
			}
			var count int64
			db.Model(&model.SummarySource{}).Where("task_id = ? AND source_id = ?", b.Task.ID, "test-user@private-peer").Count(&count)
			if count != 0 {
				t.Fatal("private source published as task source")
			}
			check := func() {
				t.Helper()
				for _, user := range []string{"test-user", "other"} {
					for _, path := range paths {
						w := provenanceRequest(r, "GET", path, "", user)
						if user == "test-user" || kind == "team" {
							if w.Code != 200 || !strings.Contains(w.Body.String(), "shared-group") {
								t.Fatalf("%s %s: %s", user, path, w.Body.String())
							}
						}
						if strings.Contains(w.Body.String(), "test-user@private-peer") != (user == "test-user") {
							t.Fatalf("scope visibility %s %s: %s", user, path, w.Body.String())
						}
					}
					art, err := resolveReferencedArtifact(context.Background(), db, b.Task.ID, "test-space", user)
					if kind == "personal" && user == "other" {
						if err == nil {
							t.Fatal("other read personal artifact")
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					encoded, _ := json.Marshal(art.Snapshot)
					if strings.Contains(string(encoded), "test-user@private-peer") != (user == "test-user") {
						t.Fatalf("reference leaked/dropped scope: %s", encoded)
					}
				}
			}
			check()
			if kind == "team" {
				// A newer displayed version must not affect access to the restored snapshot.
				newer := model.SummaryResult{TaskID: b.Task.ID, Version: 3, Content: "Another version"}
				if err := db.Create(&newer).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&b.Task).Update("current_result_id", newer.ID).Error; err != nil {
					t.Fatal(err)
				}
				w := provenanceRequest(r, "POST", paths[1]+"/restore", "{}", "test-user")
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				check()
				var stored model.SummaryResult
				if err := db.First(&stored, result.ID).Error; err != nil {
					t.Fatal(err)
				}
				if !sameSnapshotJSON(savedRaw, stored.SnapshotJSON) {
					t.Fatal("reading/restoring mutated stored provenance")
				}
			}
		})
	}
}

func TestSnapshotScopeUnknownOwnerFailsClosed(t *testing.T) {
	db, _, _, b := seedOptimization(t, "team")
	if err := db.Create(&model.SummarySource{TaskID: b.Task.ID, SourceID: "derived", SourceType: model.SourceGroup, Derived: true}).Error; err != nil {
		t.Fatal(err)
	}
	raw := `{"scope":{"channel_ids":["private","derived"],"channel_names":["PRIVATE NAME","DERIVED NAME"],"sources":[{"source_id":"private","source_name":"PRIVATE NAME","source_type":3},{"source_id":"derived","source_name":"DERIVED NAME","source_type":1}],"document_ids":["private-doc","derived"]}}`
	for _, owner := range []string{"", "other", "test-user"} {
		got := projectResultSnapshot(db, nil, b.Task, &raw, "test-user", owner)
		encoded, _ := json.Marshal(got)
		if strings.Contains(string(encoded), "derived") || strings.Contains(string(encoded), "DERIVED NAME") {
			t.Fatalf("derived source exposed: %s", encoded)
		}
		for _, secret := range []string{"private", "PRIVATE NAME", "private-doc"} {
			if strings.Contains(string(encoded), secret) != (owner == "test-user") {
				t.Fatalf("owner=%q snapshot=%s", owner, encoded)
			}
		}
	}
}
