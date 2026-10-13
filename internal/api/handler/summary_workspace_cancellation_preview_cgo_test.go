//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
)

type workspaceCancellationScopeProbe struct {
	t      *testing.T
	called bool
}

func (p *workspaceCancellationScopeProbe) Chat(ctx context.Context, _ []agent.Message, _ []agent.Tool) (agent.AssistantTurn, error) {
	p.called = true
	if present, locked := agent.WorkspaceScopeLocked(ctx); !present || !locked {
		p.t.Errorf("cancellation gained scope discovery: present=%t locked=%t", present, locked)
	}
	return agent.AssistantTurn{ToolCalls: []agent.ToolCall{
		workspaceScopeToolCall("explain", "emit_summary_response",
			`{"result_type":"explanation","reply":"暂不生成，可继续确认要求。","execution_target":"none"}`),
	}}, nil
}

func TestDocumentCancellationChatPreviewKeepsScopeLocked(t *testing.T) {
	for i, message := range []string{
		"不要总结", "那就不要总结所有内容", "不要生成未登记对象",
		"开始总结：先不要生成", // Must not reopen the explicit-command override.
		"不要生成正式总结，先给预览",
		"“不要总结”。", "“不要总结”，谢谢", "（“不要总结”）",
		"不要生成新对象，不要总结", "总结和表格不用生成",
		"表格、不要总结", "表格 不要总结",
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			db, _ := setupTestDBs(t)
			if err := db.AutoMigrate(&model.AgentMessage{}, &model.AgentSummarySession{}, &model.AgentSummaryTurn{}); err != nil {
				t.Fatal(err)
			}
			store := NewAgentWorkspaceStore(db)
			scope, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
				SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			key := WorkspaceSessionKey{SpaceID: "space-a", UserID: "actor", SessionID: "cancel-preview"}
			begin := beginWorkspaceTurnForTest(t, store, key, "seed-preview", 1, scope)
			payload, err := json.Marshal(agent.SummaryResponsePayload{
				ResultType: agent.SummaryResultAgentPreview, Reply: "预览", ExecutionTarget: "agent_preview",
				Preview: &agent.SummaryResponsePreview{Content: "保留的预览", Version: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.CompleteTurn(context.Background(), WorkspaceTurnCompletion{
				Key: key, TurnID: begin.Turn.ID, Attempt: begin.Turn.Attempt,
				Messages:   workspaceConversationMessages("总结", "预览", 1, workspaceResultAgentPreview, payload),
				ResultType: workspaceResultAgentPreview, ResponsePayload: payload, ScopeVersion: 1, SnapshotVersion: 1,
			})
			if err != nil || before.CurrentPreview == nil {
				t.Fatalf("seed preview: %v", err)
			}
			probe := &workspaceCancellationScopeProbe{t: t}
			registry := agent.NewRegistry()
			schema, emit := agent.EmitSummaryResponseTool()
			registry.RegisterTerminal(schema, emit)
			runner := agent.NewRunner(probe, registry, agent.NewPool(1), agent.Policy{
				MaxSteps: 2, MaxTokens: 4000, StepTimeout: time.Second, TerminalTool: "emit_summary_response",
			})
			h := newAgentChatHandlerWithRunner(runner, "test", newFakeHistoryStore(), 10)
			h.workspaceEntryEnabled = true
			h.workspace = &summaryWorkspaceCoordinator{db: db, imDB: newSummaryWorkspaceIMValidationDB(t), store: store}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/agent/summary-workspace/chat", nil)
			c.Set("user_id", key.UserID)
			c.Set("space_id", key.SpaceID)
			h.handleSummaryWorkspaceChat(c, agentChatRequest{
				Message: message, InputOrigin: summaryWorkspaceInputUser, Action: string(service.SummaryActionChat),
				SessionID: key.SessionID, RequestID: "cancel", ScopeVersion: 1, SummaryContext: scope,
			}, false)
			if rec.Code != http.StatusOK || !probe.called {
				t.Fatalf("status=%d called=%t body=%s", rec.Code, probe.called, rec.Body.String())
			}
			after, err := store.LoadSnapshot(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			if after.Session.State != workspaceResultExplanation || after.CurrentPreview == nil ||
				after.CurrentPreview.ID != before.CurrentPreview.ID {
				t.Fatalf("cancellation revised/discarded preview: %+v", after.Session)
			}
			var tasks int64
			if err := db.Model(&model.SummaryTask{}).Count(&tasks).Error; err != nil || tasks != 0 {
				t.Fatalf("cancellation tasks=%d err=%v", tasks, err)
			}
		})
	}
}
