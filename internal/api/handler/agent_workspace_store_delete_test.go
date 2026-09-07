//go:build cgo

package handler

// Regression tests for the delete-then-workspace P0 (yujiawei review 5087701899,
// reproduced by his store-level harness at head 5354951; Jerry-Xin re-reproduced
// in review 5087740714):
//
//	after complete:  workflow_task_id=1 terminal_msg=0
//	poll1 fold:      result="workflow_completed" terminal=true clear=false
//	after poll1:     workflow_task_id=1 terminal_msg=3
//	poll2 fold:      result="error" terminal=true clear=true      <- delete detected
//	after poll2:     workflow_task_id=1                           <- NOT cleared
//	stateFromSnapshot after delete: err=load workspace workflow task: record not found
//
// Ordinary product actions: start a workflow -> worker finishes -> open the
// workspace once (persists the terminal message) -> delete the summary from the
// list -> every chat/confirm/history render of the workspace 500s forever.
//
// The reconcile steps mirror handleSummaryWorkspaceHistory's polling flow
// (agent_summary_workspace.go:930-947) verbatim.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// deleteWorkflowScope is the single-group scope every shape in this file uses.
func deleteWorkflowScope() summaryWorkspaceContext {
	return summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
}

// seedCompletedWorkflow starts a personal workflow and completes the task row
// out-of-band (the worker), leaving the completion UNRECONCILED (terminal
// message not yet persisted), exactly like the fast-worker window.
func seedCompletedWorkflow(t *testing.T) (*AgentWorkspaceStore, *summaryWorkspaceCoordinator, WorkspaceSessionKey, model.SummaryTask) {
	t.Helper()
	db := newWorkspaceStoreTestDB(t)
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("migrate workflow state: %v", err)
	}
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "user-1", SessionID: "session-delete-workflow"}

	begin := beginWorkspaceTurnForTest(t, store, key, "request-start", 1, deleteWorkflowScope())
	task := model.SummaryTask{
		ID: 77, TaskNo: "SUM-DEL-77", SpaceID: key.SpaceID, CreatorID: key.UserID,
		Title: "待删除总结", Status: model.StatusCompleted, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed workflow task: %v", err)
	}
	payload := json.RawMessage(`{"result_type":"workflow_started","reply":"已开始"}`)
	if _, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
		Key: key, TurnID: begin.Turn.ID, Attempt: begin.Turn.Attempt,
		Messages:   workspaceConversationMessages("开始总结", "已开始", 1, workspaceResultWorkflowStarted, payload),
		ResultType: workspaceResultWorkflowStarted, ResponsePayload: payload, ScopeVersion: 1,
		Workflow: &WorkspaceWorkflowMutation{TaskID: task.ID, Scope: "personal"},
	}); err != nil {
		t.Fatalf("complete workflow start: %v", err)
	}
	coordinator := &summaryWorkspaceCoordinator{db: db, store: store}
	return store, coordinator, key, task
}

// runHistoryPoll mirrors handleSummaryWorkspaceHistory's reconcile flow:
// load → Unscoped task probe → terminal fold → ReconcileWorkflow.
func runHistoryPoll(t *testing.T, store *AgentWorkspaceStore, coordinator *summaryWorkspaceCoordinator, key WorkspaceSessionKey) WorkspaceSnapshot {
	t.Helper()
	snapshot, err := store.LoadSnapshot(context.Background(), key)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			t.Fatal("session disappeared")
		}
		t.Fatalf("load snapshot: %v", err)
	}
	if snapshot.Session.WorkflowTaskID == 0 {
		return snapshot
	}
	var task model.SummaryTask
	taskErr := store.db.Unscoped().
		Where("id = ? AND space_id = ? AND creator_id = ?", snapshot.Session.WorkflowTaskID, key.SpaceID, key.UserID).
		Take(&task).Error
	resultType, reply, terminal, clearWorkflow := workspaceWorkflowTerminalState(task, taskErr)
	if !terminal {
		return snapshot
	}
	messageID := int64(0)
	if clearWorkflow {
		messageID = snapshot.Session.WorkflowTerminalMessageID
	}
	reconciled, err := store.ReconcileWorkflow(context.Background(), WorkspaceWorkflowReconcile{
		Key:           key,
		TaskID:        snapshot.Session.WorkflowTaskID,
		ScopeVersion:  snapshot.Session.WorkflowScopeVersion,
		ResultType:    resultType,
		Reply:         reply,
		ClearWorkflow: clearWorkflow,
		MessageID:     messageID,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return reconciled
}

// SHAPE 1: history poll after delete must self-heal: fold a terminal error
// artifact AND clear the dangling workflow pointer (the P0 short-circuit).
func TestWorkspaceDeleteAfterCompletionHistorySelfHeals(t *testing.T) {
	store, coordinator, key, task := seedCompletedWorkflow(t)
	_ = coordinator

	// Poll 1 (before delete): folds workflow_completed, persists terminal message.
	poll1 := runHistoryPoll(t, store, coordinator, key)
	if poll1.Session.WorkflowTaskID != task.ID || poll1.Session.WorkflowTerminalMessageID == 0 {
		t.Fatalf("poll 1 must persist the completed terminal artifact: %#v", poll1.Session)
	}

	// The user deletes the completed summary (soft delete, no session cleanup).
	now := time.Now()
	if err := store.db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"deleted_at": now, "status": -1}).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}

	// Poll 2 (after delete): must detect the delete, fold a terminal error,
	// and CLEAR the workflow pointer despite the terminal message existing.
	poll2 := runHistoryPoll(t, store, coordinator, key)
	if poll2.Session.WorkflowTaskID != 0 {
		t.Fatalf("P0: workflow_task_id must be cleared after delete (was %d) — dangling pointer bricks the workspace", poll2.Session.WorkflowTaskID)
	}
	if poll2.Session.WorkflowTerminalMessageID == 0 {
		t.Fatalf("poll 2 must keep the terminal error artifact for replay")
	}

	// After healing: every later render must succeed.
	if _, err := store.LoadSnapshot(context.Background(), key); err != nil {
		t.Fatalf("load after healing: %v", err)
	}
	final := runHistoryPoll(t, store, coordinator, key)
	if _, err := coordinator.stateFromSnapshot(context.Background(), final); err != nil {
		t.Fatalf("stateFromSnapshot after healing must not error: %v", err)
	}
}

func TestWorkspaceDeleteBeforeCompletionPollPersistsErrorWithoutWorkflowState(t *testing.T) {
	store, coordinator, key, task := seedCompletedWorkflow(t)
	now := time.Now()
	if err := store.db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"deleted_at": now, "status": -1}).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}

	snapshot := runHistoryPoll(t, store, coordinator, key)
	if snapshot.Session.WorkflowTaskID != 0 {
		t.Fatalf("workflow pointer = %d, want cleared", snapshot.Session.WorkflowTaskID)
	}
	history, err := coordinator.historyFromSnapshot(context.Background(), key.SessionID, snapshot)
	if err != nil {
		t.Fatalf("history after delete: %v", err)
	}
	if history.State.Workflow != nil {
		t.Fatalf("error history must not expose an invalid workflow state: %#v", history.State.Workflow)
	}
	if len(history.Messages) == 0 || history.Messages[len(history.Messages)-1].ResultType != workspaceResultError {
		t.Fatalf("history messages = %#v, want terminal error message", history.Messages)
	}
}

// SHAPE 2: delete-then-chat — a fresh chat turn after the delete must not 500.
func TestWorkspaceDeleteThenChatTurnDoesNotError(t *testing.T) {
	store, coordinator, key, task := seedCompletedWorkflow(t)

	poll1 := runHistoryPoll(t, store, coordinator, key)
	if poll1.Session.WorkflowTerminalMessageID == 0 {
		t.Fatalf("precondition: terminal message persisted")
	}
	now := time.Now()
	if err := store.db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"deleted_at": now, "status": -1}).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}

	// Fresh chat turn after the delete: the handler holds begin.Snapshot and
	// renders it via turnFromSnapshot at :348 / :290. Must not error.
	begin := beginWorkspaceTurnForTest(t, store, key, "request-after-delete", 1, deleteWorkflowScope())
	if begin.Disposition == WorkspaceTurnReplay {
		if _, err := coordinator.turnFromSnapshot(context.Background(), key.SessionID, begin.Snapshot, begin.Turn.ResponseMessageID, begin.Turn.RunID); err != nil {
			t.Fatalf("P0: replay render after delete must not error: %v", err)
		}
		return
	}
	// Fresh acquisition: the handler renders the last-known snapshot state.
	if _, err := coordinator.stateFromSnapshot(context.Background(), begin.Snapshot); err != nil {
		t.Fatalf("P0: stateFromSnapshot on fresh turn after delete must not error: %v", err)
	}
}

// SHAPE 3: delete-then-replay before any History poll. The persisted response
// is workflow_started, but the task has disappeared; replay must return a
// contract-valid top-level error instead of combining an error workflow state
// with the old message and returning HTTP 500.
func TestWorkspaceDeleteThenReplayBeforeHistoryReturnsErrorTurn(t *testing.T) {
	store, coordinator, key, task := seedCompletedWorkflow(t)
	now := time.Now()
	if err := store.db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"deleted_at": now, "status": -1}).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}

	scope := deleteWorkflowScope()
	scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.BeginTurn(context.Background(), WorkspaceBeginTurnInput{
		Key:           key,
		RequestID:     "request-start",
		RequestHash:   summaryWorkspaceRequestHash("chat", "总结", 1, scopeHash),
		ScopeVersion:  1,
		ScopeJSON:     scopeJSON,
		ScopeHash:     scopeHash,
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("replay begin: %v", err)
	}
	turn, err := coordinator.turnFromSnapshot(context.Background(), key.SessionID, replay.Snapshot, replay.Turn.ResponseMessageID, replay.Turn.RunID)
	if err != nil {
		t.Fatalf("replay before History must not error: %v", err)
	}
	if turn.ResultType != workspaceResultError || turn.State.Workflow != nil {
		t.Fatalf("replay turn = %#v, want top-level error with no workflow state", turn)
	}
}

// SHAPE 4: a completed artifact exists, then the task is deleted and the old
// request is replayed before the deletion-healing History poll.
func TestWorkspaceDeleteCompletedThenReplayBeforeHistoryReturnsErrorTurn(t *testing.T) {
	store, coordinator, key, task := seedCompletedWorkflow(t)

	poll1 := runHistoryPoll(t, store, coordinator, key)
	if poll1.Session.WorkflowTerminalMessageID == 0 {
		t.Fatalf("precondition: terminal message persisted")
	}
	now := time.Now()
	if err := store.db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"deleted_at": now, "status": -1}).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}
	scope := deleteWorkflowScope()
	scopeJSON, scopeHash, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.BeginTurn(context.Background(), WorkspaceBeginTurnInput{
		Key:           key,
		RequestID:     "request-start",
		RequestHash:   summaryWorkspaceRequestHash("chat", "总结", 1, scopeHash),
		ScopeVersion:  1,
		ScopeJSON:     scopeJSON,
		ScopeHash:     scopeHash,
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("replay begin: %v", err)
	}
	if replay.Disposition != WorkspaceTurnReplay {
		t.Fatalf("expected replay, got %s", replay.Disposition)
	}
	turn, err := coordinator.turnFromSnapshot(context.Background(), key.SessionID, replay.Snapshot, replay.Turn.ResponseMessageID, replay.Turn.RunID)
	if err != nil {
		t.Fatalf("completed replay before History must not error: %v", err)
	}
	if turn.ResultType != workspaceResultError || turn.State.Workflow != nil {
		t.Fatalf("replay turn = %#v, want top-level error with no workflow state", turn)
	}
}
