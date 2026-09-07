//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

type workspaceSaveFixture struct {
	Session model.AgentSummarySession
	Message model.AgentMessage
	Body    map[string]interface{}
}

func TestCreateAgentSummary_WorkspaceSaveFailsClosedWhenRolloutDisabled(t *testing.T) {
	h := NewAgentSummaryHandler(nil, nil, "", "", "", 0, 0)
	h.ConfigureSummaryWorkspace(false)
	r := setupAgentSummaryRouter(h)
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":                "workspace-disabled",
		"scope_version":             1,
		"expected_artifact_version": 1,
	}, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("workspace save status=%d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}

func workspaceSaveInt(value int) *int { return &value }

func seedWorkspaceSaveFixture(t *testing.T, db *gorm.DB, sessionID string) workspaceSaveFixture {
	return seedWorkspaceSaveFixtureWithTimeRange(t, db, sessionID, nil)
}

func seedWorkspaceSaveFixtureWithTimeRange(t *testing.T, db *gorm.DB, sessionID string, timeRange *summaryWorkspaceTimeRange) workspaceSaveFixture {
	t.Helper()
	if err := db.AutoMigrate(&model.AgentSummarySession{}); err != nil {
		t.Fatalf("migrate workspace session: %v", err)
	}

	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "channel-workspace", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		TimeRange:         timeRange,
		ReferencedTaskIDs: []int64{},
	}
	scopeJSON, _, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatalf("marshal workspace scope: %v", err)
	}
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "已生成预览。",
		ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: "开场说明不应保存\n\n# 服务端可信总结正文",
			Version: 3,
		},
	})
	if err != nil {
		t.Fatalf("marshal workspace payload: %v", err)
	}
	payload := string(payloadJSON)
	message := model.AgentMessage{
		SpaceID:         "test-space",
		UserID:          "test-user",
		SessionID:       sessionID,
		Role:            "assistant",
		Content:         "这里只是对话气泡，不是总结正文。",
		ResultType:      agent.SummaryResultAgentPreview,
		ResponsePayload: &payload,
		ScopeVersion:    2,
		ArtifactVersion: 3,
		SnapshotVersion: workspaceSnapshotVersion,
	}
	if err := db.Create(&message).Error; err != nil {
		t.Fatalf("seed workspace preview: %v", err)
	}
	session := model.AgentSummarySession{
		SpaceID:                "test-space",
		UserID:                 "test-user",
		SessionID:              sessionID,
		AgentSessionID:         summaryWorkspaceAgentSessionID("test-space", sessionID, 2),
		ContractVersion:        summaryWorkspaceContractVersion,
		State:                  "preview_ready",
		StateVersion:           2,
		ScopeVersion:           2,
		ScopeJSON:              string(scopeJSON),
		ArtifactVersion:        3,
		LatestPreviewMessageID: message.ID,
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatalf("seed workspace session: %v", err)
	}
	return workspaceSaveFixture{
		Session: session,
		Message: message,
		Body: map[string]interface{}{
			"session_id":                sessionID,
			"agent_message_id":          message.ID,
			"snapshot_version":          workspaceSnapshotVersion,
			"scope_version":             session.ScopeVersion,
			"expected_artifact_version": session.ArtifactVersion,
			"title":                     "工作台总结",
		},
	}
}

func TestCreateAgentSummary_WorkspaceSaveUsesPayloadAndPreservesHistory(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-ok")
	if err := db.Create(&model.AgentMessage{
		SpaceID: "test-space", UserID: "test-user", SessionID: fixture.Session.SessionID,
		Role: "user", Content: "请生成总结",
	}).Error; err != nil {
		t.Fatalf("seed workspace user message: %v", err)
	}

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)
	w := doAgentSave(t, r, fixture.Body, map[string]string{"Idempotency-Key": "workspace-save-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("workspace save want 200, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").Take(&task).Error; err != nil {
		t.Fatalf("load saved task: %v", err)
	}
	if task.OriginChannelID != "channel-workspace" || task.OriginChannelType != model.OriginChannelGroup {
		t.Fatalf("origin = %q/%d, want workspace channel", task.OriginChannelID, task.OriginChannelType)
	}
	if got, want := task.TimeRangeEnd.Sub(task.TimeRangeStart), time.Duration(service.AgentSummaryDefaultTimeRangeDays)*24*time.Hour; got != want {
		t.Fatalf("default workspace save range=%s, want %s", got, want)
	}
	var result model.PersonalResult
	if err := db.Where("task_id = ? AND user_id = ?", task.ID, "test-user").Take(&result).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	if result.Content != "开场说明不应保存\n\n# 服务端可信总结正文" {
		t.Fatalf("saved content = %q, want response_payload_json.preview.content", result.Content)
	}
	if result.Content == fixture.Message.Content {
		t.Fatal("workspace save used the conversational assistant reply")
	}
	snapshot := result.GetSnapshot()
	if snapshot == nil {
		t.Fatal("workspace save snapshot is missing")
	}
	if snapshot.Scope.TimeRange.Start != task.TimeRangeStart.Format(time.RFC3339) || snapshot.Scope.TimeRange.End != task.TimeRangeEnd.Format(time.RFC3339) {
		t.Fatalf("snapshot time range=%+v, want task range %s..%s", snapshot.Scope.TimeRange, task.TimeRangeStart.Format(time.RFC3339), task.TimeRangeEnd.Format(time.RFC3339))
	}
	var source model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Take(&source).Error; err != nil {
		t.Fatalf("load source derived from workspace scope: %v", err)
	}
	if source.SourceID != "channel-workspace" || source.SourceType != model.SourceGroup {
		t.Fatalf("source = %+v, want workspace channel", source)
	}

	var messageCount int64
	if err := db.Model(&model.AgentMessage{}).
		Where("space_id = ? AND user_id = ? AND session_id = ?", "test-space", "test-user", fixture.Session.SessionID).
		Count(&messageCount).Error; err != nil {
		t.Fatalf("count retained history: %v", err)
	}
	if messageCount != 2 {
		t.Fatalf("workspace save retained %d messages, want 2", messageCount)
	}
	var savedMessage model.AgentMessage
	if err := db.First(&savedMessage, fixture.Message.ID).Error; err != nil {
		t.Fatalf("reload saved preview: %v", err)
	}
	if savedMessage.SavedTaskID != task.ID {
		t.Fatalf("message saved_task_id=%d, want %d", savedMessage.SavedTaskID, task.ID)
	}
	var savedSession model.AgentSummarySession
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", "test-space", "test-user", fixture.Session.SessionID).Take(&savedSession).Error; err != nil {
		t.Fatalf("reload workspace session: %v", err)
	}
	if savedSession.LatestPreviewSavedTaskID != task.ID {
		t.Fatalf("session latest_preview_saved_task_id=%d, want %d", savedSession.LatestPreviewSavedTaskID, task.ID)
	}
	if savedSession.StateVersion != fixture.Session.StateVersion+1 {
		t.Fatalf("session state_version=%d, want %d", savedSession.StateVersion, fixture.Session.StateVersion+1)
	}

	// Same-key replay succeeds before the strict already-saved gate and must not
	// create a second formal summary.
	replay := doAgentSave(t, r, fixture.Body, map[string]string{"Idempotency-Key": "workspace-save-key"})
	if replay.Code != http.StatusOK {
		t.Fatalf("workspace replay want 200, got %d: %s", replay.Code, replay.Body.String())
	}
	var taskCount int64
	if err := db.Model(&model.SummaryTask{}).Where("creator_id = ?", "test-user").Count(&taskCount).Error; err != nil {
		t.Fatalf("count replay tasks: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("workspace replay created %d tasks, want 1", taskCount)
	}
	differentKey := doAgentSave(t, r, fixture.Body, map[string]string{"Idempotency-Key": "workspace-save-other-key"})
	if differentKey.Code != http.StatusConflict {
		t.Fatalf("already-saved preview with another key want 409, got %d: %s", differentKey.Code, differentKey.Body.String())
	}
}

func TestCreateAgentSummary_WorkspaceSaveAcceptsReplacementRunIdentity(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(
		&model.AgentSummarySession{},
		&model.AgentSummaryTurn{},
		&model.AgentSummaryRun{},
		&model.AgentSummarySpec{},
		&model.AgentEvidenceArtifact{},
	); err != nil {
		t.Fatalf("migrate replacement save tables: %v", err)
	}
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-replacement")
	const requestID = "replace-request"
	replacementID := summaryWorkspaceReplacementAgentSessionID(
		fixture.Session.SpaceID, fixture.Session.SessionID, fixture.Session.ScopeVersion, requestID,
	)
	run, _, err := summaryrun.NewStore(db).CreateOrGetRun(
		context.Background(), fixture.Session.UserID, replacementID, requestID, model.ScopePolicyOpen,
	)
	if err != nil {
		t.Fatalf("create replacement run: %v", err)
	}
	turn := model.AgentSummaryTurn{
		SpaceID: fixture.Session.SpaceID, UserID: fixture.Session.UserID, SessionID: fixture.Session.SessionID,
		RequestID: requestID, RequestHash: "replace-hash", ScopeVersion: fixture.Session.ScopeVersion,
		Status: "completed", Attempt: 1, RunID: run.RunID,
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("create replacement turn: %v", err)
	}
	if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Updates(map[string]interface{}{
		"run_id":  run.RunID,
		"turn_id": turn.ID,
	}).Error; err != nil {
		t.Fatalf("bind preview to replacement run: %v", err)
	}
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).
		Update("agent_session_id", replacementID).Error; err != nil {
		t.Fatalf("persist replacement identity: %v", err)
	}
	fixture.Body["request_id"] = requestID

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{
		"Idempotency-Key": "workspace-replacement-key",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("replacement preview save want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAgentSummary_WorkspaceSaveUsesServerEffectiveScope(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-effective-scope")
	start := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	emptyScope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{},
		Participants:      []summaryWorkspaceParticipant{},
		Template:          &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "总结进展"},
		ReferencedTaskIDs: []int64{},
	}
	scopeJSON, _, err := marshalSummaryWorkspaceContext(emptyScope)
	if err != nil {
		t.Fatal(err)
	}
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "已按最近聊天生成预览。",
		ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: "# 最近聊天总结",
			Version: fixture.Message.ArtifactVersion,
			EffectiveScope: &agent.SummaryResponseEffectiveScope{
				Channels:  []agent.SummaryResponseChannel{{ChannelID: "recent-group", ChannelType: model.ChannelTypeGroup, ChannelName: "最近群聊"}},
				TimeRange: &agent.SummaryResponseTimeRange{Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339), Label: "最近 7 天（默认）"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Update("scope_json", string(scopeJSON)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Update("response_payload_json", string(payloadJSON)).Error; err != nil {
		t.Fatal(err)
	}

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)
	w := doAgentSave(t, r, fixture.Body, map[string]string{"Idempotency-Key": "workspace-effective-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("workspace save want 200, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.OriginChannelID != "recent-group" || task.OriginChannelType != model.OriginChannelGroup {
		t.Fatalf("origin = %q/%d, want inferred recent group", task.OriginChannelID, task.OriginChannelType)
	}
	if !task.TimeRangeStart.Equal(start) || !task.TimeRangeEnd.Equal(end) {
		t.Fatalf("time range = %s..%s, want %s..%s", task.TimeRangeStart, task.TimeRangeEnd, start, end)
	}
	var source model.SummarySource
	if err := db.Where("task_id = ?", task.ID).Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	if source.SourceID != "recent-group" || source.SourceType != model.SourceGroup {
		t.Fatalf("saved source = %#v", source)
	}
}

func TestCreateAgentSummary_WorkspaceSavePreservesSelectedTimeRange(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	start := time.Date(2026, 8, 1, 9, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	end := time.Date(2026, 8, 8, 18, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	fixture := seedWorkspaceSaveFixtureWithTimeRange(t, db, "workspace-save-selected-range", &summaryWorkspaceTimeRange{
		Start: start.Format(time.RFC3339),
		End:   end.Format(time.RFC3339),
		Label: "8 月第一周",
	})

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "workspace-selected-range"})
	if w.Code != http.StatusOK {
		t.Fatalf("workspace save want 200, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").Take(&task).Error; err != nil {
		t.Fatalf("load saved task: %v", err)
	}
	if !task.TimeRangeStart.Equal(start) || !task.TimeRangeEnd.Equal(end) {
		t.Fatalf("task time range=%s..%s, want %s..%s", task.TimeRangeStart, task.TimeRangeEnd, start, end)
	}
	var result model.PersonalResult
	if err := db.Where("task_id = ? AND user_id = ?", task.ID, "test-user").Take(&result).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	snapshot := result.GetSnapshot()
	if snapshot == nil {
		t.Fatal("workspace save snapshot is missing")
	}
	if snapshot.Scope.TimeRange.Start != start.Format(time.RFC3339) || snapshot.Scope.TimeRange.End != end.Format(time.RFC3339) {
		t.Fatalf("snapshot time range=%+v, want %s..%s", snapshot.Scope.TimeRange, start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
}

func TestCreateAgentSummary_WorkspaceSaveRequiresIdempotencyKey(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-no-key")
	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)

	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("workspace save without Idempotency-Key want 400, got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Code != 40005 {
		t.Fatalf("workspace save without Idempotency-Key code=%d, want 40005", response.Code)
	}
	var taskCount int64
	if err := db.Model(&model.SummaryTask{}).Count(&taskCount).Error; err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("workspace save without Idempotency-Key created %d tasks", taskCount)
	}
}

func TestCreateAgentSummary_WorkspaceMessagesCannotUseLegacySave(t *testing.T) {
	tests := []struct {
		name       string
		spaceID    string
		turnID     int64
		resultType string
	}{
		{name: "explanation result type", resultType: workspaceResultExplanation},
		{name: "workflow turn", turnID: 42, resultType: workspaceResultWorkflowStarted},
		{name: "workspace space id", spaceID: "test-space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupAgentSummaryTestDB(t)
			message := model.AgentMessage{
				SpaceID:         tt.spaceID,
				UserID:          "test-user",
				SessionID:       "workspace-legacy-bypass",
				TurnID:          tt.turnID,
				Role:            "assistant",
				Content:         "workspace conversational response",
				ResultType:      tt.resultType,
				SnapshotVersion: workspaceSnapshotVersion,
			}
			if err := db.Create(&message).Error; err != nil {
				t.Fatalf("seed workspace message: %v", err)
			}

			h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
			w := doAgentSave(t, setupAgentSummaryRouter(h), map[string]interface{}{
				"session_id":          message.SessionID,
				"origin_channel_id":   "channel-1",
				"origin_channel_type": 1,
				"agent_message_id":    message.ID,
				"snapshot_version":    workspaceSnapshotVersion,
			}, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("legacy save of workspace message want 400, got %d: %s", w.Code, w.Body.String())
			}
			var taskCount int64
			if err := db.Model(&model.SummaryTask{}).Count(&taskCount).Error; err != nil {
				t.Fatalf("count tasks: %v", err)
			}
			if taskCount != 0 {
				t.Fatalf("legacy save of workspace message created %d tasks", taskCount)
			}
		})
	}
}

func TestLoadWorkspacePreviewForSaveRejectsStaleIdentityAndVersions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *gorm.DB, *workspaceSaveFixture, *createAgentSummaryReq)
	}{
		{
			name: "cross space session",
			mutate: func(_ *testing.T, _ *gorm.DB, _ *workspaceSaveFixture, req *createAgentSummaryReq) {
				// The caller's space argument is changed below for this case.
				req.SessionID = "cross-space"
			},
		},
		{
			name: "request scope version",
			mutate: func(_ *testing.T, _ *gorm.DB, _ *workspaceSaveFixture, req *createAgentSummaryReq) {
				req.ScopeVersion = workspaceSaveInt(1)
			},
		},
		{
			name: "message scope version",
			mutate: func(t *testing.T, db *gorm.DB, fixture *workspaceSaveFixture, _ *createAgentSummaryReq) {
				if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Update("scope_version", 1).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "request artifact version",
			mutate: func(_ *testing.T, _ *gorm.DB, _ *workspaceSaveFixture, req *createAgentSummaryReq) {
				req.ExpectedArtifactVersion = workspaceSaveInt(2)
			},
		},
		{
			name: "message artifact version",
			mutate: func(t *testing.T, db *gorm.DB, fixture *workspaceSaveFixture, _ *createAgentSummaryReq) {
				if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Update("artifact_version", 2).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "snapshot version",
			mutate: func(_ *testing.T, _ *gorm.DB, _ *workspaceSaveFixture, req *createAgentSummaryReq) {
				req.SnapshotVersion = 2
			},
		},
		{
			name: "payload preview version",
			mutate: func(t *testing.T, db *gorm.DB, fixture *workspaceSaveFixture, _ *createAgentSummaryReq) {
				badPayload := `{"result_type":"agent_preview","reply":"ok","execution_target":"agent_preview","preview":{"content":"body","version":2}}`
				if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Update("response_payload_json", badPayload).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "not latest preview",
			mutate: func(t *testing.T, db *gorm.DB, fixture *workspaceSaveFixture, _ *createAgentSummaryReq) {
				if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).Update("latest_preview_message_id", fixture.Message.ID+99).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non saveable result type",
			mutate: func(t *testing.T, db *gorm.DB, fixture *workspaceSaveFixture, _ *createAgentSummaryReq) {
				if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Update("result_type", agent.SummaryResultExplanation).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupAgentSummaryTestDB(t)
			fixture := seedWorkspaceSaveFixture(t, db, "workspace-stale")
			req := createAgentSummaryReq{
				SessionID:               fixture.Session.SessionID,
				AgentMessageID:          fixture.Message.ID,
				SnapshotVersion:         workspaceSnapshotVersion,
				ScopeVersion:            workspaceSaveInt(fixture.Session.ScopeVersion),
				ExpectedArtifactVersion: workspaceSaveInt(fixture.Session.ArtifactVersion),
			}
			tt.mutate(t, db, &fixture, &req)
			spaceID := "test-space"
			if tt.name == "cross space session" {
				req.SessionID = fixture.Session.SessionID
				spaceID = "other-space"
			}
			_, err := loadWorkspacePreviewForSave(db, spaceID, "test-user", req, false)
			if !errors.Is(err, errWorkspacePreviewSaveStale) {
				t.Fatalf("error=%v, want workspace stale", err)
			}
		})
	}
}

func TestValidateWorkspacePreviewSaveRequestKeepsLegacyOptional(t *testing.T) {
	if err := validateWorkspacePreviewSaveRequest(createAgentSummaryReq{SessionID: "legacy"}); err != nil {
		t.Fatalf("legacy request rejected: %v", err)
	}
	if err := validateWorkspacePreviewSaveRequest(createAgentSummaryReq{
		SessionID: "workspace", ScopeVersion: workspaceSaveInt(1),
	}); err == nil {
		t.Fatal("half-supplied workspace versions were accepted")
	}
}
