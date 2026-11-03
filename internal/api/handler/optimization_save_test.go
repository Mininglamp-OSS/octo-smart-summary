//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
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
