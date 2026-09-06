//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

type workspaceScopeScript struct {
	turns []agent.AssistantTurn
	next  int
}

func (s *workspaceScopeScript) Chat(context.Context, []agent.Message, []agent.Tool) (agent.AssistantTurn, error) {
	if s.next >= len(s.turns) {
		return agent.AssistantTurn{}, fmt.Errorf("workspace scope script exhausted")
	}
	turn := s.turns[s.next]
	s.next++
	return turn, nil
}

func workspaceScopeToolCall(id, name, arguments string) agent.ToolCall {
	call := agent.ToolCall{ID: id, Type: "function"}
	call.Function.Name = name
	call.Function.Arguments = arguments
	return call
}

func workspaceScopeTestRunner(
	t *testing.T,
	firstDiscovery, lateDiscovery []agent.ChannelScope,
	declaration string,
	captured *[]agent.ChannelScope,
	preview string,
) *agent.Runner {
	t.Helper()
	registry := agent.NewRegistry()
	discoveryCalls := 0
	registry.Register(agent.Tool{Type: "function", Function: agent.ToolFunction{Name: "list_channels", Parameters: map[string]any{"type": "object"}}}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		discoveryCalls++
		channels := firstDiscovery
		if discoveryCalls > 1 {
			channels = lateDiscovery
		}
		candidates := make([]modelChannelInfo, 0, len(channels))
		for _, channel := range channels {
			candidates = append(candidates, modelChannelInfo{
				ChannelID: channel.ChannelID, ChannelType: channel.ChannelType,
				ChannelName: channel.ChannelName, IsArchived: channel.IsArchived,
			})
		}
		return fmt.Sprintf(`{"accepted":%t}`, agent.AuthorizeDiscoveredChannels(ctx, pipelineChannels(candidates))), nil
	})
	scopeSchema, scopeHandler := agent.SetSummaryScopeTool()
	registry.Register(scopeSchema, scopeHandler)
	registry.Register(agent.Tool{Type: "function", Function: agent.ToolFunction{Name: "capture_scope", Parameters: map[string]any{"type": "object"}}}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		*captured = agent.AllowedChannelScopes(ctx)
		return "captured", nil
	})
	emitSchema, emitHandler := agent.EmitSummaryResponseTool()
	registry.RegisterTerminal(emitSchema, emitHandler)

	turns := []agent.AssistantTurn{{ToolCalls: []agent.ToolCall{workspaceScopeToolCall("discover-1", "list_channels", `{}`)}}}
	if declaration != "" {
		turns = append(turns,
			agent.AssistantTurn{ToolCalls: []agent.ToolCall{workspaceScopeToolCall("declare", "set_summary_scope", declaration)}},
			agent.AssistantTurn{ToolCalls: []agent.ToolCall{workspaceScopeToolCall("discover-late", "list_channels", `{}`)}},
		)
	}
	turns = append(turns, agent.AssistantTurn{ToolCalls: []agent.ToolCall{workspaceScopeToolCall("capture", "capture_scope", `{}`)}})
	turns = append(turns, agent.AssistantTurn{ToolCalls: []agent.ToolCall{workspaceScopeToolCall("emit", "emit_summary_response", preview)}})
	return agent.NewRunner(&workspaceScopeScript{turns: turns}, registry, agent.NewPool(1), agent.Policy{
		MaxSteps: 8, MaxTokens: 8000, StepTimeout: time.Second, TerminalTool: "emit_summary_response",
	})
}

// modelChannelInfo is a local fixture shape kept separate from the pipeline
// package so the test data remains concise.
type modelChannelInfo struct {
	ChannelID   string
	ChannelType int
	ChannelName string
	IsArchived  bool
}

func pipelineChannels(values []modelChannelInfo) []pipeline.ChannelInfo {
	channels := make([]pipeline.ChannelInfo, 0, len(values))
	for _, value := range values {
		channels = append(channels, pipeline.ChannelInfo{
			ChannelID: value.ChannelID, ChannelType: value.ChannelType,
			ChannelName: value.ChannelName, IsArchived: value.IsArchived,
		})
	}
	return channels
}

func runWorkspaceScopeTurn(
	t *testing.T,
	runner *agent.Runner,
	contextValue summaryWorkspaceContext,
	requestID string,
) WorkspaceSnapshot {
	t.Helper()
	db := newWorkspaceStoreTestDB(t)
	store := NewAgentWorkspaceStore(db)
	key := WorkspaceSessionKey{SpaceID: "space-a", UserID: "actor", SessionID: "scope-" + requestID}
	begin := beginWorkspaceTurnForTest(t, store, key, requestID, 1, contextValue)
	handler := newAgentChatHandlerWithRunner(runner, "test-workspace-system", newFakeHistoryStore(), 10)
	handler.workspace = &summaryWorkspaceCoordinator{
		db: db, imDB: newSummaryWorkspaceIMValidationDB(t), store: store,
	}
	snapshot, err := handler.completeWorkspaceAgentTurn(
		context.Background(), &summaryWorkspaceResponder{}, key, begin.Turn.ID, begin.Turn.Attempt,
		agentChatRequest{SessionID: key.SessionID, RequestID: requestID, ScopeVersion: 1, Message: "总结项目进展"},
		contextValue, begin.Snapshot, service.SummaryRouteAgentPreview, true, false,
	)
	if err != nil {
		t.Fatalf("complete workspace Agent turn: %v", err)
	}
	return snapshot
}

func TestWorkspaceColdStartWithoutScopeDeclarationReturnsClarification(t *testing.T) {
	var captured []agent.ChannelScope
	runner := workspaceScopeTestRunner(t,
		[]agent.ChannelScope{{ChannelID: "group-a", ChannelType: model.ChannelTypeGroup, ChannelName: "A群"}},
		nil, "", &captured,
		`{"result_type":"agent_preview","reply":"已生成","execution_target":"agent_preview","preview":{"content":"预览","version":1}}`,
	)
	snapshot := runWorkspaceScopeTurn(t, runner, emptySummaryWorkspaceContext(), "cold-start")
	if len(captured) != 0 {
		t.Fatalf("discovery became fetchable without declaration: %#v", captured)
	}
	if snapshot.Session.State != workspaceResultClarification || snapshot.CurrentPreview != nil {
		t.Fatalf("cold start state=%q preview=%#v, want clarification without preview", snapshot.Session.State, snapshot.CurrentPreview)
	}
	if len(snapshot.Messages) == 0 {
		t.Fatal("cold start clarification message is missing")
	}
	last := snapshot.Messages[len(snapshot.Messages)-1]
	if last.ResultType != agent.SummaryResultClarification || last.Content != "请先告诉我需要总结哪个聊天或哪些参与者。" {
		t.Fatalf("persisted clarification = type %q content %q", last.ResultType, last.Content)
	}
}

func TestWorkspaceColdStartWithParticipantsOnlyAsksForChat(t *testing.T) {
	var captured []agent.ChannelScope
	runner := workspaceScopeTestRunner(t, nil, nil, "", &captured,
		`{"result_type":"agent_preview","reply":"已生成","execution_target":"agent_preview","preview":{"content":"预览","version":1}}`,
	)
	contextValue := emptySummaryWorkspaceContext()
	contextValue.Participants = []summaryWorkspaceParticipant{{UserID: "member", UserName: "成员 A"}}
	snapshot := runWorkspaceScopeTurn(t, runner, contextValue, "participants-only")
	if len(snapshot.Messages) == 0 {
		t.Fatal("participants-only clarification message is missing")
	}
	last := snapshot.Messages[len(snapshot.Messages)-1]
	if last.Content != "请先告诉我需要总结哪个聊天。" {
		t.Fatalf("participants-only clarification = %q", last.Content)
	}
}

func TestWorkspaceDeclaredScopeIsFetchablePersistedAndReported(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		initial     []summaryWorkspaceChannel
		wantChannel []string
	}{
		{name: "replace", mode: agent.WorkspaceSourceReplace, initial: []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}, wantChannel: []string{"group-c"}},
		{name: "extend", mode: agent.WorkspaceSourceExtend, initial: []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}, wantChannel: []string{"group-a", "group-c"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var captured []agent.ChannelScope
			declaration := fmt.Sprintf(`{"source_mode":%q,"channels":[{"channel_id":"group-c","channel_type":2}]}`, test.mode)
			runner := workspaceScopeTestRunner(t,
				[]agent.ChannelScope{{ChannelID: "group-c", ChannelType: model.ChannelTypeGroup, ChannelName: "C群"}},
				[]agent.ChannelScope{{ChannelID: "group-b", ChannelType: model.ChannelTypeGroup, ChannelName: "B群"}},
				declaration, &captured,
				`{"result_type":"agent_preview","reply":"已生成","execution_target":"agent_preview","preview":{"content":"预览","version":1}}`,
			)
			contextValue := emptySummaryWorkspaceContext()
			contextValue.SelectedChannels = test.initial
			snapshot := runWorkspaceScopeTurn(t, runner, contextValue, "declared-"+test.name)

			if got := channelIDsFromAgentScope(captured); !equalStrings(got, test.wantChannel) {
				t.Fatalf("fetchable scope=%v, want %v", got, test.wantChannel)
			}
			var persisted summaryWorkspaceContext
			if err := json.Unmarshal([]byte(snapshot.Session.ScopeJSON), &persisted); err != nil {
				t.Fatalf("decode persisted scope: %v", err)
			}
			if got := channelIDsFromWorkspaceScope(persisted.SelectedChannels); !equalStrings(got, test.wantChannel) {
				t.Fatalf("persisted scope=%v, want %v", got, test.wantChannel)
			}
			if snapshot.CurrentPreview == nil || snapshot.CurrentPreview.ResponsePayload == nil {
				t.Fatal("current preview payload is missing")
			}
			var response agent.SummaryResponsePayload
			if err := json.Unmarshal([]byte(*snapshot.CurrentPreview.ResponsePayload), &response); err != nil || response.Preview == nil || response.Preview.EffectiveScope == nil {
				t.Fatalf("load preview effective scope: response=%#v err=%v", response, err)
			}
			if got := channelIDsFromResponseScope(response.Preview.EffectiveScope.Channels); !equalStrings(got, test.wantChannel) {
				t.Fatalf("reported effective scope=%v, want %v", got, test.wantChannel)
			}
		})
	}
}

func channelIDsFromAgentScope(channels []agent.ChannelScope) []string {
	ids := make([]string, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.ChannelID)
	}
	return ids
}

func channelIDsFromWorkspaceScope(channels []summaryWorkspaceChannel) []string {
	ids := make([]string, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.ChatID)
	}
	return ids
}

func channelIDsFromResponseScope(channels []agent.SummaryResponseChannel) []string {
	ids := make([]string, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.ChannelID)
	}
	return ids
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
