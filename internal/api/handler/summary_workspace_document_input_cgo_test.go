//go:build cgo

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
)

func TestDocumentSubmissionHandler(t *testing.T) {
	template := &summaryWorkspaceTemplate{TemplateID: "risk", Label: "风险", Requirement: "请说明一下本周风险"}
	for _, tc := range []struct {
		name, message, origin      string
		template                   *summaryWorkspaceTemplate
		documentsOnly, invalidChat bool
		clarifies, rejectsOrigin   bool
	}{
		{name: "polite mixed request", message: "请帮我总结一下群里有什么重要决定"},
		{name: "english mixed request", message: "please summarize what is in these documents"},
		{name: "mixed question compatibility", message: "这些内容有什么区别？"},
		{name: "mixed negation", message: "开始总结：先不要生成，先说明一下来源", clarifies: true},
		{name: "negation in verified template", message: "先不要生成总结", origin: summaryWorkspaceInputTemplate, template: &summaryWorkspaceTemplate{TemplateID: "pause", Label: "暂停", Requirement: "先不要生成总结"}, clarifies: true},
		{name: "invalid chat and negation", message: "先不要生成总结", invalidChat: true, clarifies: true},
		{name: "invalid chat with affirmative", message: "总结本周进展", invalidChat: true, clarifies: true},
		{name: "document command question", message: "开始总结是什么意思？", documentsOnly: true, clarifies: true},
		{name: "document source question", message: "生成总结的来源是什么？", documentsOnly: true, clarifies: true},
		{name: "document english question", message: "start summary? 来源是什么", documentsOnly: true, clarifies: true},
		{name: "document negation", message: "先不要生成总结", documentsOnly: true, clarifies: true},
		{name: "spoofed system", message: template.Requirement, origin: summaryWorkspaceInputSystemIntent, documentsOnly: true, rejectsOrigin: true},
		{name: "missing template", message: template.Requirement, origin: summaryWorkspaceInputTemplate, documentsOnly: true, rejectsOrigin: true},
		{name: "mismatched template", message: "其他要求", template: template, origin: summaryWorkspaceInputTemplate, rejectsOrigin: true},
		{name: "verified system", message: "请根据当前选择生成总结", origin: summaryWorkspaceInputSystemIntent, documentsOnly: true},
		{name: "verified document template", message: template.Requirement, template: template, origin: summaryWorkspaceInputTemplate, documentsOnly: true},
		{name: "inferred mixed template", message: template.Requirement, template: template},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := setupTestDBs(t)
			imDB := newSummaryWorkspaceIMValidationDB(t)
			if err := db.AutoMigrate(&model.AgentMessage{}, &model.AgentSummarySession{}, &model.AgentSummaryTurn{}, &model.SummaryWorkflowIdempotency{}); err != nil {
				t.Fatal(err)
			}
			client := &recordingDocumentSourceClient{docs: map[string]*documentSummarySource{
				"d1": {DocumentID: "d1", Title: "方案", Version: "v1", Content: "文档正文"},
			}}
			store := NewAgentWorkspaceStore(db)
			h := &AgentChatHandler{
				workspaceEntryEnabled: true, documentClient: client,
				workspace: &summaryWorkspaceCoordinator{
					db: db, imDB: imDB, store: store,
					workflow: service.NewSummaryWorkflowService(db, imDB, 31, 90),
				},
			}
			req := agentChatRequest{
				Message: tc.message, InputOrigin: tc.origin, Action: string(service.SummaryActionChat),
				SessionID: "document-input", RequestID: "document-input-1", ScopeVersion: 1,
				SummaryContext: summaryWorkspaceContext{
					Documents:        []summaryWorkspaceDocument{{DocumentID: "d1", Title: "方案"}},
					SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}},
					Template:         tc.template,
				},
			}
			if tc.documentsOnly {
				req.SummaryContext.SelectedChannels = nil
			} else if tc.invalidChat {
				req.SummaryContext.SelectedChannels[0].ChatID = "inaccessible-group"
			}
			call := func() *httptest.ResponseRecorder {
				t.Helper()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/agent/summary-workspace/chat", nil)
				c.Request.Header.Set("Token", "actor")
				c.Set("user_id", "actor")
				c.Set("space_id", "space-a")
				h.handleSummaryWorkspaceChat(c, req, false)
				return rec
			}
			rec := call()
			wantStatus := http.StatusOK
			if tc.rejectsOrigin {
				wantStatus = http.StatusBadRequest
			}
			if rec.Code != wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, wantStatus, rec.Body.String())
			}
			if tc.clarifies || tc.rejectsOrigin {
				var tasks int64
				if err := db.Model(&model.SummaryTask{}).Count(&tasks).Error; err != nil || tasks != 0 {
					t.Fatalf("non-executing request persisted tasks=%d err=%v", tasks, err)
				}
				client.mu.Lock()
				fetches := len(client.tokens)
				client.mu.Unlock()
				if fetches != 0 {
					t.Fatalf("non-executing request fetched %d documents", fetches)
				}
				if tc.rejectsOrigin {
					return
				}
			}
			state, err := store.LoadSnapshot(context.Background(), WorkspaceSessionKey{SpaceID: "space-a", UserID: "actor", SessionID: req.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			if tc.clarifies {
				if state.Session.State != workspaceResultClarification {
					t.Fatalf("state=%s want clarification", state.Session.State)
				}
				if tc.invalidChat && !strings.Contains(rec.Body.String(), "部分所选会话不可用") {
					t.Fatalf("permission failure masked: %s", rec.Body.String())
				}
				return
			}
			if state.Session.WorkflowTaskID == 0 || state.Session.State != workspaceResultWorkflowStarted {
				t.Fatalf("workflow not started: %+v", state.Session)
			}
			var task model.SummaryTask
			if err := db.First(&task, state.Session.WorkflowTaskID).Error; err != nil {
				t.Fatal(err)
			}
			if task.Topic != tc.message || task.OriginChannelID != "" {
				t.Fatalf("wrong requirement or automatic origin: %+v", task)
			}
			var sources []model.SummarySource
			if err := db.Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
				t.Fatal(err)
			}
			wantSources := 2
			if tc.documentsOnly {
				wantSources = 1
			}
			if len(sources) != wantSources {
				t.Fatalf("sources=%d want=%d", len(sources), wantSources)
			}
			foundDocument, foundChat := false, false
			for _, source := range sources {
				if source.SourceType == model.SourceDocument {
					foundDocument = true
					var snapshot model.SummarySourceSnapshot
					if err := db.First(&snapshot, "summary_source_id = ?", source.ID).Error; err != nil {
						t.Fatal(err)
					}
					if len(source.SourceHash) != 64 || source.SourceVersion != "v1" || snapshot.Content != "文档正文" {
						t.Fatalf("document snapshot incomplete: %+v %+v", source, snapshot)
					}
				} else if source.SourceType == model.SourceGroup && source.SourceID == "group-a" {
					foundChat = true
				}
			}
			if !foundDocument || (!tc.documentsOnly && !foundChat) {
				t.Fatalf("source class lost: %+v", sources)
			}
			// An identical request replays the existing result, not a new task.
			if replay := call(); replay.Code != http.StatusOK {
				t.Fatalf("replay failed: %s", replay.Body.String())
			}
			var tasks int64
			if err := db.Model(&model.SummaryTask{}).Count(&tasks).Error; err != nil || tasks != 1 {
				t.Fatalf("replay tasks=%d err=%v", tasks, err)
			}
		})
	}
}
