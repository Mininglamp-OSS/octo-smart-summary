//go:build cgo

package handler

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestResolveAgentMessageRequestIDUsesSpaceScopedWorkspaceRun(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	ctx := context.Background()
	internalSessionID := summaryWorkspaceAgentSessionID("space-a", "public-session", 1)
	run, _, err := summaryrun.NewStore(db).CreateOrGetRun(
		ctx, "user-1", internalSessionID, "request-1", model.ScopePolicyClosed,
	)
	if err != nil {
		t.Fatalf("create workspace run: %v", err)
	}
	turn := model.AgentSummaryTurn{
		SpaceID: "space-a", UserID: "user-1", SessionID: "public-session",
		RequestID: "request-1", RequestHash: "hash-1", ScopeVersion: 1,
		Status: "completed", Attempt: 1, RunID: run.RunID,
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("create workspace turn: %v", err)
	}
	message := model.AgentMessage{SpaceID: "space-a", ScopeVersion: 1, RunID: run.RunID, TurnID: turn.ID}

	got, err := resolveAgentMessageRequestID(ctx, db, "user-1", "public-session", "request-1", message)
	if err != nil || got != "request-1" {
		t.Fatalf("resolve workspace request = %q, %v; want request-1", got, err)
	}

	message.SpaceID = "space-b"
	if _, err := resolveAgentMessageRequestID(ctx, db, "user-1", "public-session", "request-1", message); !errors.Is(err, errAgentMessageRunMismatch) {
		t.Fatalf("cross-space run binding error = %v, want errAgentMessageRunMismatch", err)
	}
}

func TestResolveAgentMessageRequestIDAcceptsRotatedWorkspaceRun(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	ctx := context.Background()
	const (
		spaceID   = "space-a"
		userID    = "user-1"
		sessionID = "public-session"
		requestID = "replace-request"
	)
	internalSessionID := summaryWorkspaceReplacementAgentSessionID(spaceID, sessionID, 1, requestID)
	run, _, err := summaryrun.NewStore(db).CreateOrGetRun(
		ctx, userID, internalSessionID, requestID, model.ScopePolicyOpen,
	)
	if err != nil {
		t.Fatalf("create replacement run: %v", err)
	}
	turn := model.AgentSummaryTurn{
		SpaceID: spaceID, UserID: userID, SessionID: sessionID,
		RequestID: requestID, RequestHash: "replace-hash", ScopeVersion: 1,
		Status: "completed", Attempt: 1, RunID: run.RunID,
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("create replacement turn: %v", err)
	}
	message := model.AgentMessage{
		SpaceID: spaceID, UserID: userID, SessionID: sessionID,
		ScopeVersion: 1, RunID: run.RunID, TurnID: turn.ID,
	}

	got, err := resolveAgentMessageRequestID(ctx, db, userID, sessionID, requestID, message)
	if err != nil || got != requestID {
		t.Fatalf("resolve replacement request = %q, %v; want %s", got, err, requestID)
	}
	binding, err := resolveAgentMessageRunBinding(ctx, db, userID, sessionID, requestID, message)
	if err != nil {
		t.Fatalf("resolve replacement binding: %v", err)
	}
	if binding.EvidenceSessionID != internalSessionID {
		t.Fatalf("evidence session id = %q, want %q", binding.EvidenceSessionID, internalSessionID)
	}
}

func TestBuildSnapshotV1FiltersWorkspaceToolMessagesBySpace(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	now := time.Now()
	rows := []model.AgentMessage{
		{SpaceID: "space-a", UserID: "user-1", SessionID: "public-session", Role: "tool", Name: "fetch_channel", CreatedAt: now},
		{SpaceID: "space-b", UserID: "user-1", SessionID: "public-session", Role: "tool", Name: "peek_channel", CreatedAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed workspace tool messages: %v", err)
	}

	task := &model.SummaryTask{ID: 1, Title: "总结", TimeRangeStart: now.Add(-time.Hour), TimeRangeEnd: now}
	snapshot := (&AgentSummaryHandler{}).buildSnapshotV1(db, "public-session", "user-1", task, nil, "space-a")
	if want := []string{"fetch_channel x 1"}; !reflect.DeepEqual(snapshot.ToolSummary, want) {
		t.Fatalf("tool summary = %#v, want %#v", snapshot.ToolSummary, want)
	}
}
