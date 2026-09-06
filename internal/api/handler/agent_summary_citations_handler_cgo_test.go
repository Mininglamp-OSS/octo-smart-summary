//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

func bindWorkspacePreviewRun(
	t *testing.T,
	db *gorm.DB,
	fixture workspaceSaveFixture,
	message *model.AgentMessage,
	requestID string,
	resultType string,
	parentMessageID int64,
	artifactVersion int,
	content string,
) string {
	t.Helper()
	runSessionID := summaryWorkspaceReplacementAgentSessionID(
		fixture.Session.SpaceID, fixture.Session.SessionID, fixture.Session.ScopeVersion, requestID,
	)
	run, _, err := summaryrun.NewStore(db).CreateOrGetRun(
		context.Background(), fixture.Session.UserID, runSessionID, requestID, model.ScopePolicyOpen,
	)
	if err != nil {
		t.Fatalf("create run %s: %v", requestID, err)
	}
	turn := model.AgentSummaryTurn{
		SpaceID: fixture.Session.SpaceID, UserID: fixture.Session.UserID, SessionID: fixture.Session.SessionID,
		RequestID: requestID, RequestHash: "hash-" + requestID, ScopeVersion: fixture.Session.ScopeVersion,
		Status: "completed", Attempt: 1, RunID: run.RunID,
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("create turn %s: %v", requestID, err)
	}
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType: resultType, Reply: "预览已更新。", ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: content, Version: artifactVersion, ParentMessageID: parentMessageID,
		},
	})
	if err != nil {
		t.Fatalf("marshal preview %s: %v", requestID, err)
	}
	payload := string(payloadJSON)
	if message.ID == 0 {
		*message = model.AgentMessage{
			SpaceID: fixture.Session.SpaceID, UserID: fixture.Session.UserID, SessionID: fixture.Session.SessionID,
			Role: "assistant", Content: "预览已更新。", ResultType: resultType, ResponsePayload: &payload,
			ScopeVersion: fixture.Session.ScopeVersion, ArtifactVersion: artifactVersion,
			SnapshotVersion: workspaceSnapshotVersion, ParentMessageID: parentMessageID,
			RunID: run.RunID, TurnID: turn.ID,
		}
		if err := db.Create(message).Error; err != nil {
			t.Fatalf("create preview %s: %v", requestID, err)
		}
	} else if err := db.Model(&model.AgentMessage{}).Where("id = ?", message.ID).Updates(map[string]interface{}{
		"content": "预览已更新。", "result_type": resultType, "response_payload_json": payload,
		"artifact_version": artifactVersion, "parent_message_id": parentMessageID,
		"run_id": run.RunID, "turn_id": turn.ID,
	}).Error; err != nil {
		t.Fatalf("update preview %s: %v", requestID, err)
	}
	return runSessionID
}

func TestCreateAgentSummary_WorkspaceNoFetchRevisionInheritsParentCitations(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummaryTurn{}, &model.AgentSummarySpec{}, &model.AgentEvidenceArtifact{}, &model.AgentCitationManifest{}); err != nil {
		t.Fatalf("migrate run binding tables: %v", err)
	}
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-no-fetch-revision")
	parent := fixture.Message
	parentSessionID := bindWorkspacePreviewRun(t, db, fixture, &parent, "grounded", agent.SummaryResultAgentPreview, 0, 3, "A [1] B [2] C [3]")
	seedEvidenceRow(t, db, fixture.Session.UserID, parentSessionID, "grounded-evidence", []pipeline.Message{
		{ChannelID: "channel-workspace", ChannelType: 2, MessageSeq: 1, SenderUID: "alice", SenderName: "Alice", Content: "A", Timestamp: time.Now().Unix()},
		{ChannelID: "channel-workspace", ChannelType: 2, MessageSeq: 2, SenderUID: "bob", SenderName: "Bob", Content: "B", Timestamp: time.Now().Unix() + 1},
		{ChannelID: "channel-workspace", ChannelType: 2, MessageSeq: 3, SenderUID: "carol", SenderName: "Carol", Content: "C", Timestamp: time.Now().Unix() + 2},
	})
	revisionOne := model.AgentMessage{}
	bindWorkspacePreviewRun(t, db, fixture, &revisionOne, "revision-1", agent.SummaryResultAgentRevision, parent.ID, 4, "A [1] B [2] C [3]")
	revisionTwo := model.AgentMessage{}
	bindWorkspacePreviewRun(t, db, fixture, &revisionTwo, "revision-2", agent.SummaryResultAgentRevision, revisionOne.ID, 5, "B [2] C [3]")
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Updates(map[string]interface{}{
		"latest_preview_message_id": revisionTwo.ID, "artifact_version": 5,
	}).Error; err != nil {
		t.Fatalf("select latest revision: %v", err)
	}
	fixture.Body["agent_message_id"] = revisionTwo.ID
	fixture.Body["expected_artifact_version"] = 5
	fixture.Body["request_id"] = "revision-2"

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "no-fetch-revision"})
	if w.Code != http.StatusOK {
		t.Fatalf("save status=%d: %s", w.Code, w.Body.String())
	}
	var saved model.PersonalResult
	if err := db.First(&saved).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	citations := saved.GetCitations()
	if len(citations) != 2 || citations[0].Index != 2 || citations[1].Index != 3 {
		t.Fatalf("saved citations = %+v, want inherited parent indexes 2 and 3", citations)
	}
}

func TestCreateAgentSummary_WorkspaceReferencedPreviewAndRevisionBorrowCitations(t *testing.T) {
	for _, test := range []struct {
		name     string
		revision bool
	}{
		{name: "preview"},
		{name: "revision", revision: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
			db := setupAgentSummaryTestDB(t)
			if err := db.AutoMigrate(&model.AgentSummarySession{}, &model.AgentSummaryRun{}, &model.AgentSummaryTurn{}, &model.AgentSummarySpec{}, &model.AgentEvidenceArtifact{}, &model.AgentCitationManifest{}); err != nil {
				t.Fatalf("migrate workspace tables: %v", err)
			}
			fixture := seedWorkspaceSaveFixture(t, db, "workspace-reference-"+test.name)
			refTask := createCompletedTask(t, db, fixture.Session.SpaceID, fixture.Session.UserID, model.OriginChannelGroup)
			refTask.SummaryMode = 1
			refTask.OriginChannelID = "reference-channel"
			if err := db.Save(&refTask).Error; err != nil {
				t.Fatalf("update referenced task: %v", err)
			}
			refResult := addTeamResult(t, db, refTask.ID, "Referenced evidence [1]")
			refResult.SetCitations([]model.Citation{{
				Index: 1, ChannelID: "reference-channel", ChannelType: 2, MessageSeq: 9, Content: "referenced evidence",
			}})
			if err := db.Save(&refResult).Error; err != nil {
				t.Fatalf("save referenced citations: %v", err)
			}
			scope := summaryWorkspaceContext{
				SelectedChannels: []summaryWorkspaceChannel{}, Participants: []summaryWorkspaceParticipant{},
				ReferencedTaskIDs: []int64{refTask.ID},
			}
			scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(scope)
			if err != nil {
				t.Fatalf("marshal referenced scope: %v", err)
			}
			if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Updates(map[string]interface{}{
				"scope_json": string(scopeJSON), "scope_hash": scopeHash,
			}).Error; err != nil {
				t.Fatalf("persist referenced scope: %v", err)
			}

			selected := fixture.Message
			bindWorkspacePreviewRun(t, db, fixture, &selected, "reference-preview", agent.SummaryResultAgentPreview, 0, 3, "Refined reference [1]")
			artifactVersion := 3
			requestID := "reference-preview"
			if test.revision {
				revision := model.AgentMessage{}
				bindWorkspacePreviewRun(t, db, fixture, &revision, "reference-revision", agent.SummaryResultAgentRevision, selected.ID, 4, "Shorter reference [1]")
				selected = revision
				artifactVersion = 4
				requestID = "reference-revision"
			}
			if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Updates(map[string]interface{}{
				"latest_preview_message_id": selected.ID, "artifact_version": artifactVersion,
			}).Error; err != nil {
				t.Fatalf("select referenced preview: %v", err)
			}
			fixture.Body["agent_message_id"] = selected.ID
			fixture.Body["expected_artifact_version"] = artifactVersion
			fixture.Body["request_id"] = requestID

			h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
			w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "reference-" + test.name})
			if w.Code != http.StatusOK {
				t.Fatalf("save status=%d: %s", w.Code, w.Body.String())
			}
			var saved model.PersonalResult
			if err := db.Where("task_id <> ?", refTask.ID).First(&saved).Error; err != nil {
				t.Fatalf("load saved workspace result: %v", err)
			}
			citations := saved.GetCitations()
			if len(citations) != 1 || citations[0].ChannelID != "reference-channel" {
				t.Fatalf("saved citations = %+v, want referenced artifact citation", citations)
			}
		})
	}
}

func TestCreateAgentSummary_WorkspaceReferencedPreviewStripsRedactedMarkers(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.AgentSummarySession{}, &model.AgentSummaryRun{}, &model.AgentSummaryTurn{}, &model.AgentSummarySpec{}, &model.AgentEvidenceArtifact{}, &model.AgentCitationManifest{}); err != nil {
		t.Fatalf("migrate workspace tables: %v", err)
	}
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-reference-redacted")
	refTask := createCompletedTask(t, db, fixture.Session.SpaceID, fixture.Session.UserID, model.OriginChannelGroup)
	refTask.OriginChannelID = "reference-channel"
	if err := db.Save(&refTask).Error; err != nil {
		t.Fatalf("update referenced task: %v", err)
	}
	refResult := addTeamResult(t, db, refTask.ID, "Private evidence [1]")
	refResult.SetCitations([]model.Citation{{Index: 1, ChannelID: "reference-channel", Content: "private"}})
	if err := db.Save(&refResult).Error; err != nil {
		t.Fatalf("save redacted citations: %v", err)
	}
	scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{}, Participants: []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{refTask.ID},
	})
	if err != nil {
		t.Fatalf("marshal referenced scope: %v", err)
	}
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Updates(map[string]interface{}{
		"scope_json": string(scopeJSON), "scope_hash": scopeHash,
	}).Error; err != nil {
		t.Fatalf("persist referenced scope: %v", err)
	}
	selected := fixture.Message
	bindWorkspacePreviewRun(t, db, fixture, &selected, "reference-redacted", agent.SummaryResultAgentPreview, 0, 3, "Refined private evidence [1]")
	fixture.Body["request_id"] = "reference-redacted"

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "reference-redacted"})
	if w.Code != http.StatusOK {
		t.Fatalf("save status=%d: %s", w.Code, w.Body.String())
	}
	var saved model.PersonalResult
	if err := db.Where("task_id <> ?", refTask.ID).First(&saved).Error; err != nil {
		t.Fatalf("load saved workspace result: %v", err)
	}
	if saved.Content != "Refined private evidence" || len(saved.GetCitations()) != 0 {
		t.Fatalf("saved result content=%q citations=%+v", saved.Content, saved.GetCitations())
	}
}

func TestCreateAgentSummary_WorkspaceRevisionPrefersCurrentRunEvidence(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummaryTurn{}, &model.AgentSummarySpec{}, &model.AgentEvidenceArtifact{}, &model.AgentCitationManifest{}); err != nil {
		t.Fatalf("migrate run binding tables: %v", err)
	}
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-current-evidence")
	parent := fixture.Message
	parentSessionID := bindWorkspacePreviewRun(t, db, fixture, &parent, "old-grounding", agent.SummaryResultAgentPreview, 0, 3, "Old [1]")
	seedEvidenceRow(t, db, fixture.Session.UserID, parentSessionID, "old-evidence", []pipeline.Message{{
		ChannelID: "old-channel", ChannelType: 2, MessageSeq: 1, SenderUID: "old", SenderName: "Old", Content: "old", Timestamp: 1,
	}})
	revision := model.AgentMessage{}
	currentSessionID := bindWorkspacePreviewRun(t, db, fixture, &revision, "new-grounding", agent.SummaryResultAgentRevision, parent.ID, 4, "New [1]")
	seedEvidenceRow(t, db, fixture.Session.UserID, currentSessionID, "new-evidence", []pipeline.Message{{
		ChannelID: "new-channel", ChannelType: 2, MessageSeq: 2, SenderUID: "new", SenderName: "New", Content: "new", Timestamp: 2,
	}})
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Updates(map[string]interface{}{
		"latest_preview_message_id": revision.ID, "artifact_version": 4,
	}).Error; err != nil {
		t.Fatalf("select latest revision: %v", err)
	}
	fixture.Body["agent_message_id"] = revision.ID
	fixture.Body["expected_artifact_version"] = 4
	fixture.Body["request_id"] = "new-grounding"

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "current-evidence"})
	if w.Code != http.StatusOK {
		t.Fatalf("save status=%d: %s", w.Code, w.Body.String())
	}
	var saved model.PersonalResult
	if err := db.First(&saved).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	citations := saved.GetCitations()
	if len(citations) != 1 || citations[0].ChannelID != "new-channel" {
		t.Fatalf("saved citations = %+v, want current run evidence", citations)
	}
}

func TestCreateAgentSummary_WorkspaceRevisionRejectsUnresolvableCitationSequence(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummaryTurn{}, &model.AgentSummarySpec{}, &model.AgentEvidenceArtifact{}, &model.AgentCitationManifest{}); err != nil {
		t.Fatalf("migrate run binding tables: %v", err)
	}
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-missing-evidence")
	parent := fixture.Message
	bindWorkspacePreviewRun(t, db, fixture, &parent, "empty-parent", agent.SummaryResultAgentPreview, 0, 3, "Missing [1]")
	revision := model.AgentMessage{}
	bindWorkspacePreviewRun(t, db, fixture, &revision, "empty-revision", agent.SummaryResultAgentRevision, parent.ID, 4, "Missing [1]")
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Updates(map[string]interface{}{
		"latest_preview_message_id": revision.ID, "artifact_version": 4,
	}).Error; err != nil {
		t.Fatalf("select latest revision: %v", err)
	}
	fixture.Body["agent_message_id"] = revision.ID
	fixture.Body["expected_artifact_version"] = 4
	fixture.Body["request_id"] = "empty-revision"

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "missing-evidence"})
	if w.Code != http.StatusConflict {
		t.Fatalf("save status=%d, want conflict: %s", w.Code, w.Body.String())
	}
	var response struct {
		Code int `json:"code"`
		Data struct {
			Reason         string `json:"reason"`
			RecoveryAction string `json:"recovery_action"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if response.Code != 40902 || response.Data.Reason != "workspace_citation_unresolved" || response.Data.RecoveryAction != "regenerate_preview" {
		t.Fatalf("citation conflict = %+v", response)
	}
	var count int64
	if err := db.Model(&model.SummaryTask{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed save persisted %d task(s), err=%v", count, err)
	}
}

func TestCreateAgentSummary_WorkspaceSaveUsesGeneratingRunEvidenceIdentity(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "off")
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.AgentSummaryRun{}, &model.AgentSummaryTurn{}); err != nil {
		t.Fatalf("migrate run binding tables: %v", err)
	}
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-run-evidence")
	const requestID = "extend-request"
	runSessionID := summaryWorkspaceReplacementAgentSessionID(
		fixture.Session.SpaceID, fixture.Session.SessionID, fixture.Session.ScopeVersion, requestID,
	)
	run, _, err := summaryrun.NewStore(db).CreateOrGetRun(
		context.Background(), fixture.Session.UserID, runSessionID, requestID, model.ScopePolicyOpen,
	)
	if err != nil {
		t.Fatalf("create generating run: %v", err)
	}
	turn := model.AgentSummaryTurn{
		SpaceID: fixture.Session.SpaceID, UserID: fixture.Session.UserID, SessionID: fixture.Session.SessionID,
		RequestID: requestID, RequestHash: "extend-hash", ScopeVersion: fixture.Session.ScopeVersion,
		Status: "completed", Attempt: 1, RunID: run.RunID,
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("create generating turn: %v", err)
	}
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "已生成预览。",
		ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: "Alice confirmed the plan [1]",
			Version: fixture.Message.ArtifactVersion,
		},
	})
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Updates(map[string]interface{}{
		"run_id": run.RunID, "turn_id": turn.ID, "response_payload_json": string(payloadJSON),
	}).Error; err != nil {
		t.Fatalf("bind preview to generating run: %v", err)
	}
	seedEvidenceRow(t, db, fixture.Session.UserID, runSessionID, "run-evidence", []pipeline.Message{{
		ChannelID: "channel-workspace", ChannelType: 2, MessageSeq: 1,
		SenderUID: "alice", SenderName: "Alice", Content: "confirmed the plan",
	}})
	fixture.Body["request_id"] = requestID

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{
		"Idempotency-Key": "workspace-run-evidence-key",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("workspace save status=%d: %s", w.Code, w.Body.String())
	}
	var saved model.PersonalResult
	if err := db.First(&saved).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	citations := saved.GetCitations()
	if len(citations) != 1 || citations[0].ChannelID != "channel-workspace" {
		t.Fatalf("saved citations = %+v, want evidence from generating run identity", citations)
	}
}

// History hydration does not carry the generation request_id. A strict workspace
// save must therefore use the request id derived from the selected preview's
// persisted run binding when it resolves the frozen citation manifest.
func TestCreateAgentSummary_WorkspaceSaveDerivesRequestIDForFrozenCitations(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(
		&model.AgentSummaryRun{},
		&model.AgentSummarySpec{},
		&model.AgentEvidenceArtifact{},
		&model.AgentCitationManifest{},
		&model.AgentSummaryTurn{},
	); err != nil {
		t.Fatalf("migrate V2 tables: %v", err)
	}
	seedV2Scenario(t, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	fixture := seedWorkspaceSaveFixture(t, db, "session-1")
	internalSessionID := summaryWorkspaceAgentSessionID(
		fixture.Session.SpaceID,
		fixture.Session.SessionID,
		fixture.Session.ScopeVersion,
	)
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", "run-v2-1").
		Update("session_id", internalSessionID).Error; err != nil {
		t.Fatalf("scope workspace run: %v", err)
	}
	if err := db.Model(&model.AgentMessageEvidence{}).
		Where("user_id = ? AND session_id = ?", "test-user", "session-1").
		Update("session_id", internalSessionID).Error; err != nil {
		t.Fatalf("scope workspace evidence: %v", err)
	}
	if err := db.Model(&model.AgentEvidenceArtifact{}).Where("run_id = ?", "run-v2-1").
		Update("session_id", internalSessionID).Error; err != nil {
		t.Fatalf("scope workspace artifact: %v", err)
	}
	turn := model.AgentSummaryTurn{
		SpaceID: fixture.Session.SpaceID, UserID: fixture.Session.UserID, SessionID: fixture.Session.SessionID,
		RequestID: "req-1", RequestHash: "workspace-history-save", ScopeVersion: fixture.Session.ScopeVersion,
		Status: "completed", Attempt: 1, RunID: "run-v2-1",
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("seed workspace turn: %v", err)
	}
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "preview ready",
		ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: "Charlie said [3]",
			Version: fixture.Message.ArtifactVersion,
		},
	})
	if err != nil {
		t.Fatalf("marshal preview payload: %v", err)
	}
	if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Updates(map[string]interface{}{
		"run_id":                "run-v2-1",
		"turn_id":               turn.ID,
		"response_payload_json": string(payloadJSON),
	}).Error; err != nil {
		t.Fatalf("bind workspace preview to run: %v", err)
	}

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{
		"Idempotency-Key": "workspace-history-save",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			FinishStatus string `json:"finish_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if response.Data.FinishStatus != string(finishgate.Partial) {
		t.Fatalf("finish_status = %q, want derived request_id to finalize as PARTIAL", response.Data.FinishStatus)
	}

	var saved model.PersonalResult
	if err := db.First(&saved).Error; err != nil {
		t.Fatalf("load saved deliverable: %v", err)
	}
	citations := saved.GetCitations()
	if len(citations) != 0 {
		t.Fatalf("saved citations = %+v, want frozen manifest to drop post-freeze Charlie [3]", citations)
	}
}
