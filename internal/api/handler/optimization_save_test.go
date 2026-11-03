//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/artifact"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func seedOptimization(t *testing.T, kind string) (*gorm.DB, *AgentSummaryHandler, workspaceSaveFixture, optimizationBase) {
	t.Helper()
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.PersonalResultVersion{}); err != nil {
		t.Fatal(err)
	}
	task := model.SummaryTask{TaskNo: "OPT-1", SpaceID: "test-space", CreatorID: "test-user", Title: "Original", Status: model.StatusCompleted, SummaryMode: model.ModeByPerson, TriggerType: model.TriggerAgent}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	p := model.SummaryParticipant{TaskID: task.ID, UserID: "test-user", Status: model.ParticipantAccepted}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if kind == "team" {
		if err := db.Create(&model.SummaryResult{TaskID: task.ID, Content: "Original content", Version: 1}).Error; err != nil {
			t.Fatal(err)
		}
	} else {
		if err := db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: p.ID, UserID: "test-user", Content: "Original content", WorkerStatus: model.PersonalStatusCompleted}).Error; err != nil {
			t.Fatal(err)
		}
	}
	f := seedWorkspaceSaveFixture(t, db, "optimize-session")
	var scope summaryWorkspaceContext
	if err := json.Unmarshal([]byte(f.Session.ScopeJSON), &scope); err != nil {
		t.Fatal(err)
	}
	scope.ReferencedTaskIDs = []int64{task.ID}
	scopeJSON, _, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&f.Session).Update("scope_json", string(scopeJSON)).Error; err != nil {
		t.Fatal(err)
	}
	b, err := loadOptimizationBase(db, "test-space", "test-user", task.ID, kind, false)
	if err != nil {
		t.Fatal(err)
	}
	f.Body["save_mode"] = "new_version"
	f.Body["target"] = optimizationTarget{task.ID, kind, b.revision()}
	return db, NewAgentSummaryHandler(db, nil, "", "", "", 0, 0), f, b
}

func TestOptimizationSaveVersionAndReplay(t *testing.T) {
	for _, kind := range []string{"team", "personal"} {
		t.Run(kind, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, kind)
			r := setupAgentSummaryRouter(h)
			headers := map[string]string{"Idempotency-Key": "optimization-save"}
			first := doAgentSave(t, r, f.Body, headers)
			if first.Code != http.StatusOK {
				t.Fatalf("save: %d %s", first.Code, first.Body.String())
			}
			second := doAgentSave(t, r, f.Body, headers)
			if second.Code != http.StatusOK {
				t.Fatalf("replay: %d %s", second.Code, second.Body.String())
			}
			var count int64
			db.Model(&model.SummaryTask{}).Count(&count)
			if count != 1 {
				t.Fatalf("created extra tasks: %d", count)
			}
			current, err := loadOptimizationBase(db, "test-space", "test-user", b.Task.ID, kind, false)
			if err != nil {
				t.Fatal(err)
			}
			content := current.Result.Content
			if kind == "personal" {
				content = current.Personal.Content
			}
			var snapshot *model.Snapshot
			if kind == "personal" {
				snapshot = current.Personal.GetSnapshot()
			} else {
				snapshot = decodeResultSnapshot(current.Result.SnapshotJSON)
			}
			if snapshot == nil || snapshot.ContentVersion != 2 {
				t.Fatalf("missing version-specific generation snapshot: %#v", snapshot)
			}
			if content != "开场说明不应保存\n\n# 服务端可信总结正文" {
				t.Fatalf("wrong content: %q", content)
			}
			if kind == "team" {
				db.Model(&model.SummaryResult{}).Where("task_id = ?", b.Task.ID).Count(&count)
			} else {
				db.Model(&model.PersonalResultVersion{}).Where("task_id = ?", b.Task.ID).Count(&count)
			}
			if count != 2 {
				t.Fatalf("history count=%d", count)
			}
			f.Body["title"] = "different request"
			if response := doAgentSave(t, r, f.Body, headers); response.Code != 409 {
				t.Fatalf("changed replay: %d", response.Code)
			}
		})
	}
}

func TestOptimizationSaveProtectsEditsAndDraft(t *testing.T) {
	for _, kind := range []string{"team", "personal"} {
		t.Run(kind, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, kind)
			if kind == "team" {
				db.Model(&b.Result).Update("content", "Concurrent edit")
			} else {
				db.Model(&b.Personal).Update("content", "Concurrent edit")
			}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "conflict-save"})
			if w.Code != 409 {
				t.Fatalf("expected conflict: %d %s", w.Code, w.Body.String())
			}
			var session model.AgentSummarySession
			db.First(&session, f.Session.ID)
			if session.State != "preview_ready" {
				t.Fatalf("draft lost: %s", session.State)
			}
		})
	}
}

func TestOptimizationSaveAuthorization(t *testing.T) {
	for _, change := range []string{"space", "creator", "deleted", "reference", "status"} {
		t.Run(change, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, "team")
			switch change {
			case "space":
				db.Model(&b.Task).Update("space_id", "other")
			case "creator":
				db.Model(&b.Task).Update("creator_id", "other")
			case "deleted":
				db.Delete(&b.Task)
			case "reference":
				db.Model(&f.Session).Update("scope_json", `{"selected_channels":[],"participants":[],"referenced_task_ids":[]}`)
			case "status":
				db.Model(&b.Task).Update("status", model.StatusPending)
			}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "denied-save"})
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("expected rejection: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// A task-only reference always resolves to the shared artifact when it exists.
// Do not advertise a personal write that generation cannot safely produce.
func TestOptimizationTargetMatchesReferenceWithBothResults(t *testing.T) {
	db, h, _, b := seedOptimization(t, "personal")
	team := model.SummaryResult{TaskID: b.Task.ID, Content: "Shared content", Version: 1}
	if err := db.Create(&team).Error; err != nil {
		t.Fatal(err)
	}
	r := setupAgentSummaryRouter(h)
	r.GET("/api/v1/summaries/:id/optimization-target", h.GetOptimizationTarget)
	for _, tt := range []struct {
		query  string
		status int
	}{{"", 200}, {"?result_kind=team", 200}, {"?result_kind=personal", 409}} {
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/summaries/%d/optimization-target%s", b.Task.ID, tt.query), nil)
		req.Header.Set("token", "test-user")
		req.Header.Set("X-Space-Id", "test-space")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tt.status {
			t.Fatalf("%s: %d %s", tt.query, w.Code, w.Body.String())
		}
		if tt.status == 200 && !strings.Contains(w.Body.String(), `"result_kind":"team"`) {
			t.Fatal(w.Body.String())
		}
	}
}

func TestOptimizationPersonalMetadataDoesNotFabricateHistory(t *testing.T) {
	db, h, f, b := seedOptimization(t, "personal")
	if err := db.Model(&b.Personal).Updates(map[string]interface{}{"total_token_used": 123, "model_version": "original-model", "msg_count": 17}).Error; err != nil {
		t.Fatal(err)
	}
	b, err := loadOptimizationBase(db, "test-space", "test-user", b.Task.ID, "personal", false)
	if err != nil {
		t.Fatal(err)
	}
	f.Body["target"] = optimizationTarget{b.Task.ID, "personal", b.revision()}
	w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "personal-metadata"})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if err := db.First(&b.Personal, b.Personal.ID).Error; err != nil {
		t.Fatal(err)
	}
	current, err := ensurePersonalVersionBaseline(db, b.Personal)
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&model.PersonalResultVersion{}).Count(&count)
	if count != 2 || current.TotalTokenUsed != 123 || current.ModelVersion != "original-model" || current.MsgCount != 17 {
		t.Fatalf("phantom history or lost metadata: count=%d version=%+v", count, current)
	}
	if *b.Personal.CurrentVersionID != current.ID {
		t.Fatal("version pointer mismatch")
	}
}

func TestOptimizationTeamMirrorIsAtomicVersion(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		t.Run(fmt.Sprint(multiple), func(t *testing.T) {
			db, h, f, b := seedOptimization(t, "team")
			old := fmt.Sprintf(`{"task_id":%d,"snapshot_version":1,"content_version":1,"requirement":"old source"}`, b.Task.ID)
			p := model.PersonalResult{TaskID: b.Task.ID, UserID: "test-user", Content: "Personal original", WorkerStatus: model.PersonalStatusCompleted, SnapshotJSON: &old, TotalTokenUsed: 123, ModelVersion: "original-model"}
			if err := db.Create(&p).Error; err != nil {
				t.Fatal(err)
			}
			baseline, err := ensurePersonalVersionBaseline(db, p)
			if err != nil {
				t.Fatal(err)
			}
			// Personal numbering deliberately differs from team numbering.
			if err := db.Model(&baseline).Update("version", 4).Error; err != nil {
				t.Fatal(err)
			}
			if multiple {
				if err := db.Create(&model.SummaryParticipant{TaskID: b.Task.ID, UserID: "another", Status: model.ParticipantAccepted}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Model(&b.Result).Updates(map[string]interface{}{"total_token_used": 456, "model_version": "team-model"}).Error; err != nil {
				t.Fatal(err)
			}
			b, err = loadOptimizationBase(db, "test-space", "test-user", b.Task.ID, "team", false)
			if err != nil {
				t.Fatal(err)
			}
			f.Body["target"] = optimizationTarget{b.Task.ID, "team", b.revision()}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "team-mirror"})
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			var task model.SummaryTask
			db.First(&task, b.Task.ID)
			team, err := queryDisplayResult(db, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if team.Version != 2 || team.TotalTokenUsed != 456 || team.ModelVersion != "team-model" {
				t.Fatalf("team metadata lost: %+v", team)
			}
			if err := db.First(&p, p.ID).Error; err != nil {
				t.Fatal(err)
			}
			if multiple {
				if p.Content != "Personal original" || *p.SnapshotJSON != old {
					t.Fatal("multi-person result overwritten")
				}
				return
			}
			current, err := ensurePersonalVersionBaseline(db, p)
			if err != nil {
				t.Fatal(err)
			}
			if p.Content != team.Content || current.Content != team.Content || current.Version != 5 || p.GetSnapshot().ContentVersion != 5 || *p.SnapshotJSON != *current.SnapshotJSON || current.TotalTokenUsed != 123 {
				t.Fatalf("inconsistent mirror: personal=%+v history=%+v", p, current)
			}
			if current.ParentVersionID == nil || *current.ParentVersionID != baseline.ID || p.GetSnapshot().ParentSnapshotVersion == nil || *p.GetSnapshot().ParentSnapshotVersion != 4 {
				t.Fatal("optimized mirror must reference the archived baseline")
			}
			var count int64
			db.Model(&model.PersonalResultVersion{}).Count(&count)
			if count != 2 {
				t.Fatalf("phantom history: %d", count)
			}
			r := setupAgentSummaryRouter(h)
			r.GET("/api/v1/summaries/:id/personal", NewPersonalHandler(db, "", nil).GetPersonal)
			req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/summaries/%d/personal", task.ID), nil)
			req.Header.Set("token", "test-user")
			req.Header.Set("X-Space-Id", "test-space")
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"version":5`) || !strings.Contains(response.Body.String(), `"content_version":5`) {
				t.Fatal(response.Body.String())
			}
		})
	}
}

func TestOptimizationPersonalRevisionIgnoresTeamPointer(t *testing.T) {
	_, _, _, b := seedOptimization(t, "personal")
	before := b.revision()
	id := int64(999)
	b.Task.CurrentResultID = &id
	if b.revision() != before {
		t.Fatal("unrelated team pointer invalidates personal revision")
	}
	raw := "changed snapshot"
	b.Personal.SnapshotJSON = &raw
	if b.revision() == before {
		t.Fatal("personal snapshot change must invalidate revision")
	}
}

func TestRefinedResultSnapshotPreservesProvenance(t *testing.T) {
	raw := `{"snapshot_version":1,"content_version":2,"requirement":"original","scope":{"channel_ids":["chat1"]}}`
	next := decodeResultSnapshot(refinedResultSnapshot(&raw, 3, "shorten"))
	if next == nil || next.ContentVersion != 3 || next.Requirement != "original" || *next.ParentSnapshotVersion != 2 || *next.UserInstruction != "shorten" || next.Scope.ChannelIDs[0] != "chat1" {
		t.Fatalf("bad snapshot: %+v", next)
	}
	if refinedResultSnapshot(nil, 2, "shorten") != nil {
		t.Fatal("invented snapshot for legacy result")
	}
}

func TestOptimizationMirrorFailureRollsBackTeamAndPreview(t *testing.T) {
	db, h, f, b := seedOptimization(t, "team")
	personal := model.PersonalResult{TaskID: b.Task.ID, UserID: "test-user", Content: "Original personal", WorkerStatus: model.PersonalStatusCompleted}
	if err := db.Create(&personal).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePersonalVersionBaseline(db, personal); err != nil {
		t.Fatal(err)
	}
	// Fail after the team version has been inserted, inside personal mirroring.
	if err := db.Exec(`CREATE TRIGGER reject_personal_version BEFORE INSERT ON summary_personal_result_version BEGIN SELECT RAISE(ABORT, 'simulated history failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "mirror-rollback"})
	if w.Code != 500 {
		t.Fatalf("expected failure: %d %s", w.Code, w.Body.String())
	}
	var count int64
	db.Model(&model.SummaryResult{}).Where("task_id = ?", b.Task.ID).Count(&count)
	if count != 1 {
		t.Fatalf("team version survived rollback: %d", count)
	}
	var session model.AgentSummarySession
	db.First(&session, f.Session.ID)
	if session.State != "preview_ready" {
		t.Fatal("preview consumed on rollback")
	}
	db.First(&personal, personal.ID)
	if personal.Content != "Original personal" {
		t.Fatal("personal content changed on rollback")
	}
}

func TestOptimizationRejectsAmbiguousReferenceFallback(t *testing.T) {
	for _, marker := range []string{"[1]", "[P1]", "[P2]"} {
		t.Run(marker, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, "team")
			other := model.SummaryTask{TaskNo: "OPT-B", SpaceID: "test-space", CreatorID: "test-user", Status: model.StatusCompleted, TriggerType: model.TriggerAgent}
			if err := db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			result := model.SummaryResult{TaskID: other.ID, Content: "B " + marker, Version: 1}
			result.SetTeamCitations([]model.TeamCitation{{Index: 1, UserID: "bob"}, {Index: 2, UserID: "carol"}})
			result.SetCitations([]model.Citation{{Index: 1, Content: "message-B"}})
			if err := db.Create(&result).Error; err != nil {
				t.Fatal(err)
			}
			b.Result.SetTeamCitations([]model.TeamCitation{{Index: 1, UserID: "alice"}})
			b.Result.SetCitations([]model.Citation{{Index: 1, Content: "message-A"}})
			if err := db.Model(&b.Result).Updates(map[string]interface{}{"citations_json": b.Result.CitationsJSON, "team_citations_json": b.Result.TeamCitationsJSON, "citation_owner_id": "test-user"}).Error; err != nil {
				t.Fatal(err)
			}
			var scope summaryWorkspaceContext
			json.Unmarshal([]byte(f.Session.ScopeJSON), &scope)
			scope.ReferencedTaskIDs = []int64{other.ID, b.Task.ID}
			raw, _, _ := marshalSummaryWorkspaceContext(scope)
			if err := db.Model(&f.Session).Update("scope_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(agent.SummaryResponsePayload{ExecutionTarget: "agent_preview", ResultType: agent.SummaryResultAgentPreview, Preview: &agent.SummaryResponsePreview{Content: "B " + marker, Version: 3}})
			if err := db.Model(&f.Message).Update("response_payload_json", string(payload)).Error; err != nil {
				t.Fatal(err)
			}
			fresh, err := loadOptimizationBase(db, "test-space", "test-user", b.Task.ID, "team", false)
			if err != nil {
				t.Fatal(err)
			}
			f.Body["target"] = optimizationTarget{b.Task.ID, "team", fresh.revision()}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "ambiguous"})
			if w.Code != 409 || !strings.Contains(w.Body.String(), "workspace_citation_unresolved") {
				t.Fatalf("ambiguous evidence saved: %d %s", w.Code, w.Body.String())
			}

			var count int64
			db.Model(&model.SummaryAgentSaveIdempotency{}).Count(&count)
			if count != 0 {
				t.Fatal("rejection created a binding")
			}
			db.Model(&model.SummaryResult{}).Where("task_id = ?", b.Task.ID).Count(&count)
			if count != 1 {
				t.Fatal("rejection created a version")
			}
			var session model.AgentSummarySession
			db.First(&session, f.Session.ID)
			if session.LatestPreviewSavedTaskID != 0 {
				t.Fatal("rejection consumed preview")
			}
			if _, err := loadWorkspacePreviewForSave(db, "test-space", "test-user", createAgentSummaryReq{
				SessionID: f.Session.SessionID, AgentMessageID: f.Message.ID, SnapshotVersion: workspaceSnapshotVersion,
				ScopeVersion: &f.Session.ScopeVersion, ExpectedArtifactVersion: &f.Session.ArtifactVersion}, false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOptimizationDisclosesPartialRunOnSaveAndReplay(t *testing.T) {
	for _, kind := range []string{"team", "personal"} {
		t.Run(kind, func(t *testing.T) {
			for _, truncated := range []bool{true, false} {
				t.Run(fmt.Sprint(truncated), func(t *testing.T) {
					db, h, f, _ := seedOptimization(t, kind)
					t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
					if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummarySpec{}, &model.AgentSummaryTurn{}, &model.AgentEvidenceArtifact{}); err != nil {
						t.Fatal(err)
					}
					store := summaryrun.NewStore(db)
					ctx := context.Background()
					run, _, err := store.CreateOrGetRun(ctx, "test-user", f.Session.AgentSessionID, "partial-run", model.ScopePolicyClosed)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.RecordChannelFetch(ctx, "test-user", run.RunID, "channel-workspace", truncated, truncated); err != nil {
						t.Fatal(err)
					}
					turn := model.AgentSummaryTurn{SpaceID: "test-space", UserID: "test-user", SessionID: f.Session.SessionID, RequestID: run.RequestID, RequestHash: "partial-hash", ScopeVersion: f.Session.ScopeVersion, Status: "completed", Attempt: 1, RunID: run.RunID}
					if err := db.Create(&turn).Error; err != nil {
						t.Fatal(err)
					}
					if err := db.Model(&f.Message).Updates(map[string]interface{}{"run_id": run.RunID, "turn_id": turn.ID}).Error; err != nil {
						t.Fatal(err)
					}
					f.Body["request_id"] = run.RequestID
					for i := 0; i < 2; i++ {
						w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "partial"})
						if w.Code != 200 || !strings.Contains(w.Body.String(), `"finish_status":"PARTIAL"`) {
							t.Fatalf("missing disclosure: %d %s", w.Code, w.Body.String())
						}
					}
					const callback = "fail-replay-run"
					if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Table == "agent_summary_run" {
							tx.AddError(fmt.Errorf("injected run read failure"))
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer db.Callback().Query().Remove(callback)
					w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "partial"})
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"finish_status":"PARTIAL"`) || !strings.Contains(w.Body.String(), `"tool_error"`) {
						t.Fatalf("replay hid verification failure: %d %s", w.Code, w.Body.String())
					}
				})
			}
		})
	}
}

func TestOptimizationFreshCitationIdentityCollision(t *testing.T) {
	cases := []struct {
		name, kind, marker string
		same, redacted     bool
	}{
		{"team collision", "team", "[1]", false, false},
		{"personal collision", "personal", "[1]", false, false},
		{"team leading zero collision", "team", "[01]", false, false},
		{"personal leading zero collision", "personal", "[01]", false, false},
		{"team same evidence", "team", "[01]", true, false},
		{"personal same evidence", "personal", "[01]", true, false},
		{"redacted reference", "team", "[1]", false, true},
		{"redacted leading zero", "team", "[01]", false, true},
		{"redacted identity unavailable", "team", "[1]", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, marker, same, redacted := tc.kind, tc.marker, tc.same, tc.redacted
			db, h, f, b := seedOptimization(t, kind)
			t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
			if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummaryTurn{}, &model.AgentSummarySpec{}, &model.AgentEvidenceArtifact{}, &model.AgentCitationManifest{}); err != nil {
				t.Fatal(err)
			}
			oldChannel, oldSeq := "old-channel", int64(101)
			if same {
				oldChannel, oldSeq = "new-channel", 202
			}
			b.Result.SetCitations([]model.Citation{{Index: 1, ChannelID: oldChannel, ChannelType: 2, MessageSeq: oldSeq}})
			row := interface{}(&b.Result)
			updates := map[string]interface{}{"content": "Budget approved [1]", "citations_json": b.Result.CitationsJSON, "citation_owner_id": "test-user"}
			if redacted {
				updates["citation_owner_id"] = "other-member"
				if err := db.Create(&model.SummaryParticipant{TaskID: b.Task.ID, UserID: "other-member", Status: model.ParticipantAccepted}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if kind == "personal" {
				row = &b.Personal
				delete(updates, "citation_owner_id")
			}
			if err := db.Model(row).Updates(updates).Error; err != nil {
				t.Fatal(err)
			}
			if redacted {
				reference, err := resolveReferencedArtifact(context.Background(), db, b.Task.ID, "test-space", "test-user")
				if err != nil {
					t.Fatal(err)
				}
				if _, known := reference.CitationMarkers["1"]; !known || len(reference.Citations) != 0 {
					t.Fatal("fixture must retain the marker and redact its evidence")
				}
			}
			message := f.Message
			session := bindWorkspacePreviewRun(t, db, f, &message, "collision", agent.SummaryResultAgentPreview, 0, 3, "Budget approved "+marker)
			pool := []pipeline.Message{{ChannelID: "new-channel", ChannelType: 2, MessageSeq: 202, Content: "Unrelated new evidence", SenderUID: "alice", Timestamp: 1}}
			seedEvidenceRow(t, db, "test-user", session, "collision-evidence", pool)
			if _, _, _, err := artifact.NewStore(db).FreezeFromPool(context.Background(), message.RunID, "test-user", session, pool, artifact.FreezeMeta{}); err != nil {
				t.Fatal(err)
			}
			f.Body["request_id"] = "collision"
			fresh, err := loadOptimizationBase(db, "test-space", "test-user", b.Task.ID, kind, false)
			if err != nil {
				t.Fatal(err)
			}
			f.Body["target"] = optimizationTarget{b.Task.ID, kind, fresh.revision()}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "collision-save"})
			if same && !redacted {
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
			} else {
				if w.Code != 409 || !strings.Contains(w.Body.String(), "workspace_citation_unresolved") {
					t.Fatal(w.Body.String())
				}
				var session model.AgentSummarySession
				db.First(&session, f.Session.ID)
				if session.LatestPreviewSavedTaskID != 0 {
					t.Fatal("collision consumed draft")
				}
				var bindings int64
				if err := db.Model(&model.SummaryAgentSaveIdempotency{}).Count(&bindings).Error; err != nil {
					t.Fatal(err)
				}
				if bindings != 0 {
					t.Fatal("collision created an idempotency binding")
				}
			}
		})
	}
}

func TestOptimizationReferenceContextUsesTypedSources(t *testing.T) {
	db, _, _, b := seedOptimization(t, "team")
	raw := fmt.Sprintf(`{"task_id":%d,"content_version":1,"scope":{"channel_ids":["peer","thread"],"sources":[{"source_id":"peer","source_type":3},{"source_id":"thread","source_type":2}]}}`, b.Task.ID)
	if err := db.Model(&b.Task).Update("origin_channel_type", model.OriginChannelGroup).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&b.Result).Updates(map[string]interface{}{"snapshot_json": raw, "citation_owner_id": "test-user"}).Error; err != nil {
		t.Fatal(err)
	}
	text, _, _ := buildReferencedSummariesContextWithArtifacts(context.Background(), db, "test-space", "test-user", []int64{b.Task.ID})
	for _, expected := range []string{"channel_id=peer channel_type=1", "channel_id=thread channel_type=5"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %s in %s", expected, text)
		}
	}
}
