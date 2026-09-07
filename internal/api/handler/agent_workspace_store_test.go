//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newWorkspaceStoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.AgentSummarySession{}, &model.AgentSummaryTurn{}, &model.AgentMessage{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func beginWorkspaceTurnForTest(t *testing.T, store *AgentWorkspaceStore, key WorkspaceSessionKey, requestID string, scopeVersion int, scope summaryWorkspaceContext) WorkspaceBeginTurnResult {
	t.Helper()
	scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := store.BeginTurn(context.Background(), WorkspaceBeginTurnInput{
		Key:           key,
		RequestID:     requestID,
		RequestHash:   summaryWorkspaceRequestHash("chat", "总结", scopeVersion, scopeHash),
		ScopeVersion:  scopeVersion,
		ScopeJSON:     scopeJSON,
		ScopeHash:     scopeHash,
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("begin turn: %v", err)
	}
	return begin
}

func TestAgentWorkspaceStoreLoadHistoryUsesCurrentScopeOnly(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-scope-history"}
	scope1 := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "群一"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	first := beginWorkspaceTurnForTest(t, store, key, "request-scope-1", 1, scope1)
	firstAgentSessionID := summaryWorkspaceAgentSessionID(key.SpaceID, key.SessionID, 1)
	if first.Snapshot.Session.AgentSessionID != firstAgentSessionID {
		t.Fatalf("scope 1 agent_session_id = %q, want %q", first.Snapshot.Session.AgentSessionID, firstAgentSessionID)
	}
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key:          key,
		TurnID:       first.Turn.ID,
		Attempt:      first.Turn.Attempt,
		Messages:     workspaceConversationMessages("旧范围问题", "旧范围回答", 1, workspaceResultExplanation, json.RawMessage(`{"result_type":"explanation","reply":"旧范围回答"}`)),
		ResultType:   workspaceResultExplanation,
		ScopeVersion: 1,
	}); err != nil {
		t.Fatalf("complete scope 1: %v", err)
	}

	scope2 := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-2", ChatType: "group", Name: "群二"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	second := beginWorkspaceTurnForTest(t, store, key, "request-scope-2", 2, scope2)
	secondAgentSessionID := summaryWorkspaceAgentSessionID(key.SpaceID, key.SessionID, 2)
	if second.Snapshot.Session.AgentSessionID != secondAgentSessionID || secondAgentSessionID == firstAgentSessionID {
		t.Fatalf("scope 2 agent_session_id = %q, want rotated %q", second.Snapshot.Session.AgentSessionID, secondAgentSessionID)
	}
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key:          key,
		TurnID:       second.Turn.ID,
		Attempt:      second.Turn.Attempt,
		Messages:     workspaceConversationMessages("新范围问题", "新范围回答", 2, workspaceResultExplanation, json.RawMessage(`{"result_type":"explanation","reply":"新范围回答"}`)),
		ResultType:   workspaceResultExplanation,
		ScopeVersion: 2,
	}); err != nil {
		t.Fatalf("complete scope 2: %v", err)
	}

	history, err := store.LoadHistory(context.Background(), key)
	if err != nil {
		t.Fatalf("load current-scope history: %v", err)
	}
	if len(history) != 2 || history[0].Content != "新范围问题" || history[1].Content != "新范围回答" {
		t.Fatalf("current-scope history = %#v, want only scope 2 messages", history)
	}
}

func TestAgentWorkspaceStorePreviewReplayAndScopeInvalidation(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-1"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	begin := beginWorkspaceTurnForTest(t, store, key, "request-1", 1, scope)
	if begin.Disposition != WorkspaceTurnAcquired {
		t.Fatalf("disposition=%s", begin.Disposition)
	}
	payload, _ := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "已生成预览。",
		ExecutionTarget: "agent_preview",
		Preview:         &agent.SummaryResponsePreview{Content: "# 总结", Version: 1},
	})
	snapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key:             key,
		TurnID:          begin.Turn.ID,
		Attempt:         begin.Turn.Attempt,
		Messages:        workspaceConversationMessages("总结", "已生成预览。", 1, workspaceResultAgentPreview, payload),
		ResultType:      workspaceResultAgentPreview,
		ResponsePayload: payload,
		ScopeVersion:    1,
		SnapshotVersion: 1,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if snapshot.CurrentPreview == nil || snapshot.CurrentPreview.ArtifactVersion != 1 || snapshot.CurrentPreview.SnapshotVersion != 1 {
		t.Fatalf("preview not folded: %#v", snapshot.CurrentPreview)
	}

	replay := beginWorkspaceTurnForTest(t, store, key, "request-1", 1, scope)
	if replay.Disposition != WorkspaceTurnReplay || replay.Turn.ResponseMessageID != snapshot.CurrentPreview.ID {
		t.Fatalf("replay=%#v", replay)
	}

	changed := scope
	changed.Template = &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "按周报总结"}
	next := beginWorkspaceTurnForTest(t, store, key, "request-2", 2, changed)
	if next.Snapshot.CurrentPreview != nil || next.Snapshot.Session.LatestPreviewMessageID != 0 {
		t.Fatalf("scope change kept stale preview: %#v", next.Snapshot)
	}
	_, err = store.BeginTurn(context.Background(), WorkspaceBeginTurnInput{
		Key:           key,
		RequestID:     "request-stale",
		RequestHash:   "hash",
		ScopeVersion:  1,
		ScopeJSON:     []byte(`{"selected_channels":[],"participants":[],"template":null,"time_range":null,"referenced_task_ids":[]}`),
		ScopeHash:     "scope",
		LeaseDuration: time.Minute,
	})
	if !errors.Is(err, ErrWorkspaceScopeConflict) {
		t.Fatalf("stale scope err=%v", err)
	}
}

func TestAgentWorkspaceStoreProposalToken(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-2"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{{UserID: "user-2", UserName: "成员"}},
		ReferencedTaskIDs: []int64{},
	}
	begin := beginWorkspaceTurnForTest(t, store, key, "request-proposal", 1, scope)
	proposalJSON, _ := json.Marshal(summaryWorkspaceProposal{Participants: scope.Participants, Requirement: "提交进展"})
	payload, _ := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultWorkflowConfirmation,
		Reply:           "请确认。",
		ExecutionTarget: "team_workflow",
		Confirmation:    map[string]json.RawMessage{"proposal": proposalJSON},
	})
	snapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key:             key,
		TurnID:          begin.Turn.ID,
		Attempt:         begin.Turn.Attempt,
		Messages:        workspaceConversationMessages("发起协作", "请确认。", 1, workspaceResultWorkflowConfirm, payload),
		ResultType:      workspaceResultWorkflowConfirm,
		ResponsePayload: payload,
		ScopeVersion:    1,
		Proposal:        &WorkspaceProposalMutation{JSON: proposalJSON},
	})
	if err != nil {
		t.Fatalf("complete proposal: %v", err)
	}
	if snapshot.Session.PendingProposalVersion != 1 || snapshot.Session.PendingProposalToken == "" {
		t.Fatalf("proposal not signed: %#v", snapshot.Session)
	}
}

func TestAgentWorkspaceStoreScopeChangeClearsFoldedArtifacts(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-scope-clear"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	begin := beginWorkspaceTurnForTest(t, store, key, "request-1", 1, scope)
	if err := store.FailTurn(context.Background(), WorkspaceTurnFailure{Key: key, TurnID: begin.Turn.ID, Attempt: begin.Turn.Attempt, ErrorCode: "seed"}); err != nil {
		t.Fatalf("release seed turn: %v", err)
	}
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", begin.Snapshot.Session.ID).Updates(map[string]interface{}{
		"latest_preview_message_id":      11,
		"latest_preview_saved_task_id":   12,
		"pending_proposal_status":        "pending",
		"pending_proposal_token":         "token",
		"pending_proposal_json":          `{"requirement":"old"}`,
		"pending_proposal_message_id":    13,
		"pending_proposal_scope_version": 1,
		"pending_proposal_task_id":       14,
		"workflow_task_id":               15,
		"workflow_scope":                 "team",
		"workflow_scope_version":         1,
		"workflow_started_message_id":    16,
		"workflow_terminal_message_id":   17,
	}).Error; err != nil {
		t.Fatalf("seed folded artifacts: %v", err)
	}

	changed := scope
	changed.Template = &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "按周报总结"}
	next := beginWorkspaceTurnForTest(t, store, key, "request-2", 2, changed)
	session := next.Snapshot.Session
	if session.LatestPreviewMessageID != 0 || session.LatestPreviewSavedTaskID != 0 {
		t.Fatalf("scope change kept preview refs: %#v", session)
	}
	if session.PendingProposalStatus != "" || session.PendingProposalToken != "" || session.PendingProposalJSON != nil ||
		session.PendingProposalMessageID != 0 || session.PendingProposalScopeVersion != 0 || session.PendingProposalTaskID != 0 {
		t.Fatalf("scope change kept proposal refs: %#v", session)
	}
	if session.WorkflowTaskID != 0 || session.WorkflowScope != "" || session.WorkflowScopeVersion != 0 ||
		session.WorkflowStartedMessageID != 0 || session.WorkflowTerminalMessageID != 0 {
		t.Fatalf("scope change kept workflow refs: %#v", session)
	}
}

func TestAgentWorkspaceStoreCompletionClearsMutuallyExclusiveArtifacts(t *testing.T) {
	tests := []struct {
		name       string
		resultType string
		proposal   *WorkspaceProposalMutation
		workflow   *WorkspaceWorkflowMutation
		assert     func(*testing.T, model.AgentSummarySession)
	}{
		{
			name:       "preview clears proposal and workflow",
			resultType: workspaceResultAgentPreview,
			assert: func(t *testing.T, session model.AgentSummarySession) {
				if session.LatestPreviewMessageID == 0 {
					t.Fatal("preview reference was not recorded")
				}
				if session.PendingProposalStatus != "" || session.PendingProposalJSON != nil || session.PendingProposalMessageID != 0 {
					t.Fatalf("preview kept proposal refs: %#v", session)
				}
				if session.WorkflowTaskID != 0 || session.WorkflowStartedMessageID != 0 || session.WorkflowTerminalMessageID != 0 {
					t.Fatalf("preview kept workflow refs: %#v", session)
				}
			},
		},
		{
			name:       "proposal clears preview and workflow",
			resultType: workspaceResultWorkflowConfirm,
			proposal:   &WorkspaceProposalMutation{JSON: json.RawMessage(`{"requirement":"new"}`), Token: "new-token"},
			assert: func(t *testing.T, session model.AgentSummarySession) {
				if session.LatestPreviewMessageID != 0 || session.LatestPreviewSavedTaskID != 0 {
					t.Fatalf("proposal kept preview refs: %#v", session)
				}
				if session.PendingProposalStatus != "pending" || session.PendingProposalMessageID == 0 {
					t.Fatalf("proposal reference was not recorded: %#v", session)
				}
				if session.WorkflowTaskID != 0 || session.WorkflowStartedMessageID != 0 || session.WorkflowTerminalMessageID != 0 {
					t.Fatalf("proposal kept workflow refs: %#v", session)
				}
			},
		},
		{
			name:       "direct team workflow clears preview and unrelated proposal",
			resultType: workspaceResultWorkflowStarted,
			workflow:   &WorkspaceWorkflowMutation{TaskID: 99, Scope: "team"},
			assert: func(t *testing.T, session model.AgentSummarySession) {
				if session.LatestPreviewMessageID != 0 || session.LatestPreviewSavedTaskID != 0 {
					t.Fatalf("workflow kept preview refs: %#v", session)
				}
				if session.WorkflowTaskID != 99 || session.WorkflowStartedMessageID == 0 {
					t.Fatalf("workflow reference was not recorded: %#v", session)
				}
				if session.PendingProposalStatus != "" || session.PendingProposalJSON != nil || session.PendingProposalTaskID != 0 {
					t.Fatalf("direct workflow retained unrelated proposal state: %#v", session)
				}
			},
		},
		{
			name:       "confirmed team workflow binds proposal",
			resultType: workspaceResultWorkflowStarted,
			workflow:   &WorkspaceWorkflowMutation{TaskID: 101, Scope: "team", ConfirmsProposal: true},
			assert: func(t *testing.T, session model.AgentSummarySession) {
				if session.PendingProposalStatus != "confirmed" || session.PendingProposalTaskID != 101 {
					t.Fatalf("confirmed workflow did not bind proposal: %#v", session)
				}
			},
		},
		{
			name:       "personal workflow does not retain a team proposal",
			resultType: workspaceResultWorkflowStarted,
			workflow:   &WorkspaceWorkflowMutation{TaskID: 100, Scope: "personal"},
			assert: func(t *testing.T, session model.AgentSummarySession) {
				if session.PendingProposalStatus != "" || session.PendingProposalToken != "" || session.PendingProposalJSON != nil ||
					session.PendingProposalMessageID != 0 || session.PendingProposalScopeVersion != 0 || session.PendingProposalTaskID != 0 {
					t.Fatalf("personal workflow retained or invented proposal state: %#v", session)
				}
				if session.WorkflowTaskID != 100 || session.WorkflowScope != "personal" || session.WorkflowStartedMessageID == 0 {
					t.Fatalf("personal workflow reference was not recorded: %#v", session)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newWorkspaceStoreTestDB(t)
			store := NewAgentWorkspaceStore(db)
			key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-transition"}
			scope := summaryWorkspaceContext{
				SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
				Participants:      []summaryWorkspaceParticipant{},
				ReferencedTaskIDs: []int64{},
			}
			begin := beginWorkspaceTurnForTest(t, store, key, "request-1", 1, scope)
			if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", begin.Snapshot.Session.ID).Updates(map[string]interface{}{
				"latest_preview_message_id":      11,
				"latest_preview_saved_task_id":   12,
				"pending_proposal_status":        "pending",
				"pending_proposal_token":         "old-token",
				"pending_proposal_json":          `{"requirement":"old"}`,
				"pending_proposal_message_id":    13,
				"pending_proposal_scope_version": 1,
				"pending_proposal_task_id":       14,
				"workflow_task_id":               15,
				"workflow_scope":                 "team",
				"workflow_scope_version":         1,
				"workflow_started_message_id":    16,
				"workflow_terminal_message_id":   17,
			}).Error; err != nil {
				t.Fatalf("seed folded artifacts: %v", err)
			}
			payload := json.RawMessage(`{"result_type":"` + tt.resultType + `","reply":"ok"}`)
			snapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
				Key:             key,
				TurnID:          begin.Turn.ID,
				Attempt:         begin.Turn.Attempt,
				Messages:        workspaceConversationMessages("request", "ok", 1, tt.resultType, payload),
				ResultType:      tt.resultType,
				ResponsePayload: payload,
				ScopeVersion:    1,
				SnapshotVersion: 1,
				Proposal:        tt.proposal,
				Workflow:        tt.workflow,
			})
			if err != nil {
				t.Fatalf("complete turn: %v", err)
			}
			tt.assert(t, snapshot.Session)
		})
	}
}

func TestAgentWorkspaceStoreActiveLeaseBlocksScopeMutation(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-active-scope"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	active := beginWorkspaceTurnForTest(t, store, key, "request-active", 1, scope)
	changed := scope
	changed.Template = &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "按周报总结"}
	scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(changed)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := store.BeginTurn(context.Background(), WorkspaceBeginTurnInput{
		Key:           key,
		RequestID:     "request-blocked",
		RequestHash:   summaryWorkspaceRequestHash("chat", "总结", 2, scopeHash),
		ScopeVersion:  2,
		ScopeJSON:     scopeJSON,
		ScopeHash:     scopeHash,
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("begin blocked turn: %v", err)
	}
	if blocked.Disposition != WorkspaceTurnInProgress || blocked.Turn.ID != active.Turn.ID {
		t.Fatalf("blocked result=%#v", blocked)
	}
	if blocked.Snapshot.Session.ScopeVersion != 1 || blocked.Snapshot.Session.ScopeHash != active.Snapshot.Session.ScopeHash ||
		blocked.Snapshot.Session.ActiveTurnID != active.Turn.ID {
		t.Fatalf("blocked request mutated active session: %#v", blocked.Snapshot.Session)
	}
	payload := json.RawMessage(`{"result_type":"clarification","reply":"ok"}`)
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: active.Turn.ID, Attempt: active.Turn.Attempt, Messages: workspaceConversationMessages("request", "ok", 1, workspaceResultClarification, payload),
		ResultType: workspaceResultClarification, ResponsePayload: payload, ScopeVersion: 1,
	}); err != nil {
		t.Fatalf("original lease owner could not complete: %v", err)
	}
}

func TestAgentWorkspaceStorePersistsEffectiveScopeAndReplaysOriginalRequest(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-effective-scope"}
	original := emptySummaryWorkspaceContext()
	original.TimeRange = &summaryWorkspaceTimeRange{
		Start: "2026-08-29T00:00:00+08:00",
		End:   "2026-09-04T23:59:59+08:00",
		Label: "最近 7 天（默认）",
	}
	begin := beginWorkspaceTurnForTest(t, store, key, "request-effective-scope", 1, original)
	effective := original
	effective.TimeRange = &summaryWorkspaceTimeRange{
		Start: "2026-08-06T00:00:00+08:00",
		End:   "2026-09-04T23:59:59+08:00",
		Label: "最近一个月",
	}
	effectiveJSON, effectiveHash, err := marshalSummaryWorkspaceContext(effective)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"result_type":"clarification","reply":"ok"}`)
	snapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: begin.Turn.ID, Attempt: begin.Turn.Attempt,
		Messages:           workspaceConversationMessages("扩大到一个月", "ok", 1, workspaceResultClarification, payload),
		ResultType:         workspaceResultClarification,
		ResponsePayload:    payload,
		ScopeVersion:       1,
		EffectiveScopeJSON: effectiveJSON,
		EffectiveScopeHash: effectiveHash,
	})
	if err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	var stored summaryWorkspaceContext
	if err := json.Unmarshal([]byte(snapshot.Session.ScopeJSON), &stored); err != nil {
		t.Fatalf("decode stored scope: %v", err)
	}
	if stored.TimeRange == nil || stored.TimeRange.Label != "最近一个月" || snapshot.Session.ScopeHash != effectiveHash {
		t.Fatalf("stored scope=%#v hash=%q", stored.TimeRange, snapshot.Session.ScopeHash)
	}

	replay := beginWorkspaceTurnForTest(t, store, key, "request-effective-scope", 1, original)
	if replay.Disposition != WorkspaceTurnReplay {
		t.Fatalf("replay disposition=%s, want %s", replay.Disposition, WorkspaceTurnReplay)
	}

	next := beginWorkspaceTurnForTest(t, store, key, "request-after-effective-scope", 1, effective)
	if next.Disposition != WorkspaceTurnAcquired {
		t.Fatalf("next disposition=%s, want %s", next.Disposition, WorkspaceTurnAcquired)
	}
}

func TestAgentWorkspaceStoreActiveLeaseBlocksFailedRequestReacquire(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-active-reacquire"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	failed := beginWorkspaceTurnForTest(t, store, key, "request-failed", 1, scope)
	if err := store.FailTurn(context.Background(), WorkspaceTurnFailure{Key: key, TurnID: failed.Turn.ID, Attempt: failed.Turn.Attempt, ErrorCode: "seed"}); err != nil {
		t.Fatalf("fail first request: %v", err)
	}
	active := beginWorkspaceTurnForTest(t, store, key, "request-active", 1, scope)
	retry := beginWorkspaceTurnForTest(t, store, key, "request-failed", 1, scope)
	if retry.Disposition != WorkspaceTurnInProgress || retry.Turn.ID != active.Turn.ID {
		t.Fatalf("retry displaced active lease: %#v", retry)
	}
	if retry.Snapshot.Session.ActiveTurnID != active.Turn.ID {
		t.Fatalf("active turn id=%d, want %d", retry.Snapshot.Session.ActiveTurnID, active.Turn.ID)
	}
	var failedRow model.AgentSummaryTurn
	if err := db.Where("id = ?", failed.Turn.ID).Take(&failedRow).Error; err != nil {
		t.Fatalf("reload failed turn: %v", err)
	}
	if failedRow.Status != "failed" || failedRow.Attempt != 1 {
		t.Fatalf("failed request was reacquired: %#v", failedRow)
	}
}

func TestAgentWorkspaceStoreCompletedReplayDoesNotDisplaceActiveLease(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-replay-active"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	completed := beginWorkspaceTurnForTest(t, store, key, "request-completed", 1, scope)
	payload := json.RawMessage(`{"result_type":"clarification","reply":"ok"}`)
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: completed.Turn.ID, Attempt: completed.Turn.Attempt, Messages: workspaceConversationMessages("request", "ok", 1, workspaceResultClarification, payload),
		ResultType: workspaceResultClarification, ResponsePayload: payload, ScopeVersion: 1,
	}); err != nil {
		t.Fatalf("complete replay seed: %v", err)
	}
	active := beginWorkspaceTurnForTest(t, store, key, "request-active", 1, scope)
	replay := beginWorkspaceTurnForTest(t, store, key, "request-completed", 1, scope)
	if replay.Disposition != WorkspaceTurnReplay || replay.Turn.ID != completed.Turn.ID {
		t.Fatalf("completed request was not replayed: %#v", replay)
	}
	if replay.Snapshot.Session.ActiveTurnID != active.Turn.ID {
		t.Fatalf("replay displaced active turn: got %d, want %d", replay.Snapshot.Session.ActiveTurnID, active.Turn.ID)
	}
}

func TestAgentWorkspaceStoreLeaseTakeoverFencesExpiredAttempt(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-attempt-fence"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	first := beginWorkspaceTurnForTest(t, store, key, "request-takeover", 1, scope)
	if err := db.Model(&model.AgentSummaryTurn{}).Where("id = ?", first.Turn.ID).
		Update("lease_expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire first lease: %v", err)
	}
	second := beginWorkspaceTurnForTest(t, store, key, "request-takeover", 1, scope)
	if second.Disposition != WorkspaceTurnAcquired || second.Turn.ID != first.Turn.ID || second.Turn.Attempt != first.Turn.Attempt+1 {
		t.Fatalf("takeover=%#v, want same turn with next attempt", second)
	}

	payload := json.RawMessage(`{"result_type":"clarification","reply":"stale"}`)
	_, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: first.Turn.ID, Attempt: first.Turn.Attempt,
		Messages:   workspaceConversationMessages("request", "stale", 1, workspaceResultClarification, payload),
		ResultType: workspaceResultClarification, ResponsePayload: payload, ScopeVersion: 1,
	})
	if !errors.Is(err, ErrWorkspaceTurnLeaseLost) {
		t.Fatalf("stale completion err=%v, want lease lost", err)
	}
	if err := store.FailTurn(context.Background(), WorkspaceTurnFailure{
		Key: key, TurnID: first.Turn.ID, Attempt: first.Turn.Attempt, ErrorCode: "STALE",
	}); !errors.Is(err, ErrWorkspaceTurnLeaseLost) {
		t.Fatalf("stale failure err=%v, want lease lost", err)
	}

	var turn model.AgentSummaryTurn
	if err := db.Where("id = ?", second.Turn.ID).Take(&turn).Error; err != nil {
		t.Fatalf("reload takeover turn: %v", err)
	}
	if turn.Status != "running" || turn.Attempt != second.Turn.Attempt {
		t.Fatalf("stale owner mutated takeover turn: %#v", turn)
	}
	var messageCount int64
	if err := db.Model(&model.AgentMessage{}).Where("turn_id = ?", second.Turn.ID).Count(&messageCount).Error; err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 0 {
		t.Fatalf("stale completion persisted %d messages", messageCount)
	}

	payload = json.RawMessage(`{"result_type":"clarification","reply":"current"}`)
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: second.Turn.ID, Attempt: second.Turn.Attempt,
		Messages:   workspaceConversationMessages("request", "current", 1, workspaceResultClarification, payload),
		ResultType: workspaceResultClarification, ResponsePayload: payload, ScopeVersion: 1,
	}); err != nil {
		t.Fatalf("takeover owner complete: %v", err)
	}
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: first.Turn.ID, Attempt: first.Turn.Attempt,
		Messages:   workspaceConversationMessages("request", "stale-after-complete", 1, workspaceResultClarification, payload),
		ResultType: workspaceResultClarification, ResponsePayload: payload, ScopeVersion: 1,
	}); !errors.Is(err, ErrWorkspaceTurnLeaseLost) {
		t.Fatalf("stale completion after takeover completed err=%v, want lease lost", err)
	}
	if err := store.FailTurn(context.Background(), WorkspaceTurnFailure{
		Key: key, TurnID: first.Turn.ID, Attempt: first.Turn.Attempt, ErrorCode: "STALE_AFTER_COMPLETE",
	}); !errors.Is(err, ErrWorkspaceTurnLeaseLost) {
		t.Fatalf("stale failure after takeover completed err=%v, want lease lost", err)
	}
}

func TestLockOrCreateWorkspaceSessionAlwaysReloadsAfterConflict(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-found-rows"}
	persisted := model.AgentSummarySession{
		SpaceID: key.SpaceID, UserID: key.UserID, SessionID: key.SessionID,
		AgentSessionID: "persisted-agent-session", ContractVersion: summaryWorkspaceContractVersion,
		State: "idle", StateVersion: 1, ScopeVersion: 1, ScopeJSON: `{}`, ScopeHash: "persisted-hash",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := db.Create(&persisted).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}

	queryCalls := 0
	if err := db.Callback().Query().After("gorm:query").Register("test:session_miss_once", func(tx *gorm.DB) {
		if tx.Statement.Table != (model.AgentSummarySession{}).TableName() {
			return
		}
		queryCalls++
		if queryCalls == 1 {
			tx.Error = gorm.ErrRecordNotFound
			tx.RowsAffected = 0
		}
	}); err != nil {
		t.Fatalf("register query callback: %v", err)
	}
	if err := db.Callback().Create().After("gorm:create").Register("test:simulate_client_found_rows", func(tx *gorm.DB) {
		if tx.Statement.Table == (model.AgentSummarySession{}).TableName() && tx.Error == nil {
			tx.RowsAffected = 1
		}
	}); err != nil {
		t.Fatalf("register create callback: %v", err)
	}

	in := WorkspaceBeginTurnInput{
		Key: key, ScopeVersion: 2, ScopeJSON: []byte(`{"new":true}`), ScopeHash: "incoming-hash",
	}
	var got model.AgentSummarySession
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		got, err = lockOrCreateWorkspaceSession(tx, in, time.Now())
		return err
	}); err != nil {
		t.Fatalf("lock or create: %v", err)
	}
	if queryCalls < 2 {
		t.Fatalf("session was queried %d times, want initial lookup plus unconditional reload", queryCalls)
	}
	if got.ID != persisted.ID || got.ScopeVersion != persisted.ScopeVersion || got.ScopeHash != persisted.ScopeHash {
		t.Fatalf("returned unpersisted insert candidate: %#v, want existing %#v", got, persisted)
	}
}

func TestAgentWorkspaceStoreProposalConfirmationReplayAndStaleGuard(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-confirm"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{{UserID: "user-2", UserName: "成员"}},
		ReferencedTaskIDs: []int64{},
	}
	proposalTurn := beginWorkspaceTurnForTest(t, store, key, "request-proposal", 1, scope)
	proposalJSON := json.RawMessage(`{"requirement":"提交进展"}`)
	payload := json.RawMessage(`{"result_type":"workflow_confirmation","reply":"请确认"}`)
	proposalSnapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: proposalTurn.Turn.ID, Attempt: proposalTurn.Turn.Attempt,
		Messages:   workspaceConversationMessages("发起协作", "请确认", 1, workspaceResultWorkflowConfirm, payload),
		ResultType: workspaceResultWorkflowConfirm, ResponsePayload: payload, ScopeVersion: 1,
		Proposal: &WorkspaceProposalMutation{JSON: proposalJSON, Token: "proposal-token"},
	})
	if err != nil {
		t.Fatalf("complete proposal: %v", err)
	}
	scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	confirmation := WorkspaceProposalConfirmationInput{
		Begin: WorkspaceBeginTurnInput{
			Key: key, RequestID: "confirm-key", RequestHash: "confirm-hash", ScopeVersion: 1,
			ScopeJSON: scopeJSON, ScopeHash: scopeHash, LeaseDuration: time.Minute,
		},
		ProposalVersion: proposalSnapshot.Session.PendingProposalVersion,
		ProposalToken:   proposalSnapshot.Session.PendingProposalToken,
	}
	begin, err := store.BeginProposalConfirmation(context.Background(), confirmation)
	if err != nil || begin.Disposition != WorkspaceTurnAcquired {
		t.Fatalf("begin confirmation=%#v err=%v", begin, err)
	}
	completedPayload := json.RawMessage(`{"result_type":"workflow_started","reply":"已发起"}`)
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: begin.Turn.ID, Attempt: begin.Turn.Attempt,
		Messages:   workspaceConversationMessages("确认并发起协作", "已发起", 1, workspaceResultWorkflowStarted, completedPayload),
		ResultType: workspaceResultWorkflowStarted, ResponsePayload: completedPayload, ScopeVersion: 1,
		Workflow: &WorkspaceWorkflowMutation{TaskID: 42, Scope: "team", ConfirmsProposal: true},
	}); err != nil {
		t.Fatalf("complete confirmation: %v", err)
	}

	replay, err := store.BeginProposalConfirmation(context.Background(), confirmation)
	if err != nil || replay.Disposition != WorkspaceTurnReplay || replay.Snapshot.Session.WorkflowTaskID != 42 {
		t.Fatalf("confirmation replay=%#v err=%v", replay, err)
	}

	mismatch := confirmation
	mismatch.Begin.RequestHash = "different-hash"
	if _, err := store.BeginProposalConfirmation(context.Background(), mismatch); !errors.Is(err, ErrWorkspaceRequestMismatch) {
		t.Fatalf("mismatched replay err=%v", err)
	}

	stale := confirmation
	stale.Begin.RequestID = "confirm-stale"
	stale.Begin.RequestHash = "stale-hash"
	stale.ProposalToken = "wrong-token"
	if _, err := store.BeginProposalConfirmation(context.Background(), stale); !errors.Is(err, ErrWorkspaceProposalStale) {
		t.Fatalf("stale confirmation err=%v", err)
	}
	var staleCount int64
	if err := db.Model(&model.AgentSummaryTurn{}).Where("request_id = ?", stale.Begin.RequestID).Count(&staleCount).Error; err != nil {
		t.Fatal(err)
	}
	if staleCount != 0 {
		t.Fatalf("stale confirmation created %d turn rows", staleCount)
	}
}

func TestSummaryWorkspaceCompletedReplayUsesCurrentArtifact(t *testing.T) {
	t.Run("preview replay after revision", func(t *testing.T) {
		db := newWorkspaceStoreTestDB(t)
		store := NewAgentWorkspaceStore(db)
		key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-preview-replay"}
		scope := summaryWorkspaceContext{
			SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
			Participants:      []summaryWorkspaceParticipant{},
			ReferencedTaskIDs: []int64{},
		}

		previewBegin := beginWorkspaceTurnForTest(t, store, key, "request-preview", 1, scope)
		previewPayload, _ := json.Marshal(agent.SummaryResponsePayload{
			ResultType: agent.SummaryResultAgentPreview,
			Reply:      "预览一",
			Preview:    &agent.SummaryResponsePreview{Content: "# 预览一", Version: 1},
		})
		previewSnapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
			Key: key, TurnID: previewBegin.Turn.ID, Attempt: previewBegin.Turn.Attempt,
			Messages:   workspaceConversationMessages("生成预览", "预览一", 1, workspaceResultAgentPreview, previewPayload),
			ResultType: workspaceResultAgentPreview, ResponsePayload: previewPayload, ScopeVersion: 1, SnapshotVersion: 1,
		})
		if err != nil {
			t.Fatalf("complete preview: %v", err)
		}

		revisionBegin := beginWorkspaceTurnForTest(t, store, key, "request-revision", 1, scope)
		revisionPayload, _ := json.Marshal(agent.SummaryResponsePayload{
			ResultType: agent.SummaryResultAgentRevision,
			Reply:      "预览二",
			Preview:    &agent.SummaryResponsePreview{Content: "# 预览二", Version: 2, ParentMessageID: previewSnapshot.CurrentPreview.ID},
		})
		revisionSnapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
			Key: key, TurnID: revisionBegin.Turn.ID, Attempt: revisionBegin.Turn.Attempt,
			Messages:   workspaceConversationMessages("修改预览", "预览二", 1, workspaceResultAgentRevision, revisionPayload),
			ResultType: workspaceResultAgentRevision, ResponsePayload: revisionPayload, ScopeVersion: 1, SnapshotVersion: 1,
			ParentMessageID: previewSnapshot.CurrentPreview.ID,
		})
		if err != nil {
			t.Fatalf("complete revision: %v", err)
		}

		replay := beginWorkspaceTurnForTest(t, store, key, "request-preview", 1, scope)
		if replay.Disposition != WorkspaceTurnReplay {
			t.Fatalf("preview replay disposition=%s", replay.Disposition)
		}
		coordinator := &summaryWorkspaceCoordinator{db: db, store: store}
		turn, err := coordinator.turnFromSnapshot(context.Background(), key.SessionID, replay.Snapshot, replay.Turn.ResponseMessageID, replay.Turn.RunID)
		if err != nil {
			t.Fatalf("restore preview replay: %v", err)
		}
		if turn.ResultType != workspaceResultAgentRevision || turn.MessageID != revisionSnapshot.CurrentPreview.ID ||
			turn.State.CurrentPreview == nil || turn.State.CurrentPreview.MessageID != turn.MessageID ||
			turn.State.CurrentPreview.ResultType != turn.ResultType {
			t.Fatalf("replay turn is not strict-adapter safe: %#v", turn)
		}
	})

	t.Run("proposal replay after confirmation", func(t *testing.T) {
		db := newWorkspaceStoreTestDB(t)
		if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}); err != nil {
			t.Fatalf("migrate workflow state: %v", err)
		}
		store := NewAgentWorkspaceStore(db)
		key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-proposal-replay"}
		scope := summaryWorkspaceContext{
			SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
			Participants:      []summaryWorkspaceParticipant{{UserID: "user-2", UserName: "成员"}},
			ReferencedTaskIDs: []int64{},
		}

		proposalBegin := beginWorkspaceTurnForTest(t, store, key, "request-proposal-replay", 1, scope)
		proposalJSON, _ := json.Marshal(summaryWorkspaceProposal{Participants: scope.Participants, Requirement: "提交进展"})
		proposalPayload := json.RawMessage(`{"result_type":"workflow_confirmation","reply":"请确认"}`)
		proposalSnapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
			Key: key, TurnID: proposalBegin.Turn.ID, Attempt: proposalBegin.Turn.Attempt,
			Messages:   workspaceConversationMessages("发起协作", "请确认", 1, workspaceResultWorkflowConfirm, proposalPayload),
			ResultType: workspaceResultWorkflowConfirm, ResponsePayload: proposalPayload, ScopeVersion: 1,
			Proposal: &WorkspaceProposalMutation{JSON: proposalJSON, Token: "proposal-token"},
		})
		if err != nil {
			t.Fatalf("complete proposal: %v", err)
		}
		scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(scope)
		if err != nil {
			t.Fatal(err)
		}
		confirmation, err := store.BeginProposalConfirmation(context.Background(), WorkspaceProposalConfirmationInput{
			Begin: WorkspaceBeginTurnInput{
				Key: key, RequestID: "confirm-proposal-replay", RequestHash: "confirm-proposal-replay-hash",
				ScopeVersion: 1, ScopeJSON: scopeJSON, ScopeHash: scopeHash, LeaseDuration: time.Minute,
			},
			ProposalVersion: proposalSnapshot.Session.PendingProposalVersion,
			ProposalToken:   proposalSnapshot.Session.PendingProposalToken,
		})
		if err != nil {
			t.Fatalf("begin confirmation: %v", err)
		}
		task := model.SummaryTask{
			ID: 42, TaskNo: "SUM-REPLAY-42", SpaceID: key.SpaceID, CreatorID: key.UserID,
			Title: "协作总结", Status: model.StatusProcessing, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := db.Create(&task).Error; err != nil {
			t.Fatalf("seed workflow task: %v", err)
		}
		if err := db.Create(&model.SummaryParticipant{
			TaskID: task.ID, UserID: "user-2", UserName: "成员", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}).Error; err != nil {
			t.Fatalf("seed participant: %v", err)
		}
		workflowPayload := json.RawMessage(`{"result_type":"workflow_started","reply":"已发起"}`)
		workflowSnapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
			Key: key, TurnID: confirmation.Turn.ID, Attempt: confirmation.Turn.Attempt,
			Messages:   workspaceConversationMessages("确认并发起协作", "已发起", 1, workspaceResultWorkflowStarted, workflowPayload),
			ResultType: workspaceResultWorkflowStarted, ResponsePayload: workflowPayload, ScopeVersion: 1,
			Workflow: &WorkspaceWorkflowMutation{TaskID: task.ID, Scope: "team", ConfirmsProposal: true},
		})
		if err != nil {
			t.Fatalf("complete confirmation: %v", err)
		}

		replay := beginWorkspaceTurnForTest(t, store, key, "request-proposal-replay", 1, scope)
		if replay.Disposition != WorkspaceTurnReplay {
			t.Fatalf("proposal replay disposition=%s", replay.Disposition)
		}
		coordinator := &summaryWorkspaceCoordinator{db: db, store: store}
		turn, err := coordinator.turnFromSnapshot(context.Background(), key.SessionID, replay.Snapshot, replay.Turn.ResponseMessageID, replay.Turn.RunID)
		if err != nil {
			t.Fatalf("restore proposal replay: %v", err)
		}
		if turn.ResultType != workspaceResultWorkflowStarted || turn.MessageID != workflowSnapshot.Session.WorkflowStartedMessageID ||
			turn.State.PendingProposal != nil || turn.State.Workflow == nil || turn.State.Workflow.MessageID != turn.MessageID ||
			turn.State.Workflow.ResultType != turn.ResultType {
			t.Fatalf("replay turn is not strict-adapter safe: %#v", turn)
		}
	})
}

func TestSummaryWorkspaceFastCompletedWorkflowKeepsStartedArtifactUntilReconciled(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("migrate workflow state: %v", err)
	}
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-fast-complete"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
	begin := beginWorkspaceTurnForTest(t, store, key, "request-fast-complete", 1, scope)
	task := model.SummaryTask{
		ID: 43, TaskNo: "SUM-FAST-43", SpaceID: key.SpaceID, CreatorID: key.UserID,
		Title: "快速完成总结", Status: model.StatusCompleted, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed completed workflow task: %v", err)
	}
	payload := json.RawMessage(`{"result_type":"workflow_started","reply":"已开始"}`)
	snapshot, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: begin.Turn.ID, Attempt: begin.Turn.Attempt,
		Messages:   workspaceConversationMessages("开始总结", "已开始", 1, workspaceResultWorkflowStarted, payload),
		ResultType: workspaceResultWorkflowStarted, ResponsePayload: payload, ScopeVersion: 1,
		Workflow: &WorkspaceWorkflowMutation{TaskID: task.ID, Scope: "personal"},
	})
	if err != nil {
		t.Fatalf("complete workflow start: %v", err)
	}

	coordinator := &summaryWorkspaceCoordinator{db: db, store: store}
	turn, err := coordinator.turnFromSnapshot(context.Background(), key.SessionID, snapshot, 0, "")
	if err != nil {
		t.Fatalf("build initial response for fast-completed workflow: %v", err)
	}
	if turn.ResultType != workspaceResultWorkflowStarted || turn.State.Workflow == nil ||
		turn.State.Workflow.ResultType != workspaceResultWorkflowStarted ||
		turn.State.Workflow.MessageID != snapshot.Session.WorkflowStartedMessageID {
		t.Fatalf("fast-completed workflow must remain adapter-safe before reconciliation: %#v", turn)
	}
}

func TestAgentWorkspaceStoreReconcileWorkflowErrorOnce(t *testing.T) {
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-workflow-error"}
	scope := summaryWorkspaceContext{SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}}, Participants: []summaryWorkspaceParticipant{}, ReferencedTaskIDs: []int64{}}
	begin := beginWorkspaceTurnForTest(t, store, key, "request-workflow", 1, scope)
	payload := json.RawMessage(`{"result_type":"workflow_started","reply":"已开始"}`)
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: begin.Turn.ID, Attempt: begin.Turn.Attempt, Messages: workspaceConversationMessages("开始总结", "已开始", 1, workspaceResultWorkflowStarted, payload),
		ResultType: workspaceResultWorkflowStarted, ResponsePayload: payload, ScopeVersion: 1,
		Workflow: &WorkspaceWorkflowMutation{TaskID: 99, Scope: "personal"},
	}); err != nil {
		t.Fatal(err)
	}
	in := WorkspaceWorkflowReconcile{Key: key, TaskID: 99, ScopeVersion: 1, ResultType: workspaceResultError, Reply: "总结生成失败，请调整要求后重试。", ClearWorkflow: true}
	first, err := store.ReconcileWorkflow(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.WorkflowTaskID != 0 || first.Session.WorkflowTerminalMessageID == 0 {
		t.Fatalf("workflow error not folded: %#v", first.Session)
	}
	second, err := store.ReconcileWorkflow(context.Background(), in)
	if err != nil {
		t.Fatalf("reconcile replay: %v", err)
	}
	var errorsCount int
	for _, message := range second.Messages {
		if message.ResultType == workspaceResultError {
			errorsCount++
		}
	}
	if errorsCount != 1 {
		t.Fatalf("error terminal messages=%d, want 1", errorsCount)
	}
}
