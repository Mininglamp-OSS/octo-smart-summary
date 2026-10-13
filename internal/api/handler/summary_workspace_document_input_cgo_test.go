//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
)

func TestDocumentSubmissionHandler(t *testing.T) {
	template := &summaryWorkspaceTemplate{TemplateID: "risk", Label: "风险", Requirement: "请说明一下本周风险"}
	cases := []struct {
		name, message, origin      string
		wantTopic                  string
		template                   *summaryWorkspaceTemplate
		documentsOnly, invalidChat bool
		chatOnly                   bool
		clarifies, rejects         bool
		unregisteredTemplate       bool
		participants, references   bool
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
		{name: "spoofed system downgrades", message: template.Requirement, origin: summaryWorkspaceInputSystemIntent, documentsOnly: true, clarifies: true},
		{name: "missing template downgrades", message: template.Requirement, origin: summaryWorkspaceInputTemplate, documentsOnly: true, clarifies: true},
		{name: "mismatched template downgrades", message: "其他要求", template: template, origin: summaryWorkspaceInputTemplate, wantTopic: template.Requirement + "\n\n其他要求"},
		{name: "forged document template", message: template.Requirement, template: template, origin: summaryWorkspaceInputTemplate, documentsOnly: true, unregisteredTemplate: true, clarifies: true},
		{name: "forged inferred document template", message: template.Requirement, template: template, documentsOnly: true, unregisteredTemplate: true, clarifies: true},
		{name: "forged builtin requirement", message: "来源是什么？", template: &summaryWorkspaceTemplate{TemplateID: "project_progress", Label: "进展", Requirement: "来源是什么？"}, origin: summaryWorkspaceInputTemplate, documentsOnly: true, unregisteredTemplate: true, clarifies: true},
		{name: "copy drift document", message: "请生成当前所选内容的总结", origin: summaryWorkspaceInputSystemIntent, documentsOnly: true},
		{name: "third locale mixed", message: "Résumez les documents sélectionnés", origin: summaryWorkspaceInputSystemIntent},
		{name: "template case folding", message: "explain weekly risks", template: &summaryWorkspaceTemplate{TemplateID: "english", Label: "Risks", Requirement: "Explain Weekly Risks"}, origin: summaryWorkspaceInputTemplate, documentsOnly: true, wantTopic: "Explain Weekly Risks"},
		{name: "unknown origin", message: "总结", origin: "model", rejects: true},
		{name: "document participants", message: "先不要总结", documentsOnly: true, participants: true, rejects: true},
		{name: "mixed participants", message: "先不要总结", participants: true, rejects: true},
		{name: "document references generate", message: "总结本周进展", documentsOnly: true, references: true, clarifies: true},
		{name: "document references explain", message: "来源是什么", documentsOnly: true, references: true, clarifies: true},
		{name: "mixed references", message: "总结本周进展", references: true, rejects: true},
		{name: "verified system", message: "请根据当前选择生成总结", origin: summaryWorkspaceInputSystemIntent, documentsOnly: true},
		{name: "verified document template", message: template.Requirement, template: template, origin: summaryWorkspaceInputTemplate, documentsOnly: true},
		{name: "inferred mixed template", message: template.Requirement, template: template},
		{name: "chat template content constraint", chatOnly: true, message: "请总结会议要点，不要生成行动项",
			origin: summaryWorkspaceInputTemplate, template: &summaryWorkspaceTemplate{TemplateID: "chat-content", Label: "会议", Requirement: "请总结会议要点，不要生成行动项"}},
		{name: "chat inferred template content constraint", chatOnly: true, message: "不要生成表格",
			template: &summaryWorkspaceTemplate{TemplateID: "chat-table", Label: "格式", Requirement: "不要生成表格"}},
	}
	// The reviewer examples must traverse normalization, authorization and
	// task creation, not merely exercise the route helper with made-up flags.
	for _, message := range documentScopedNegationRequests {
		tc := cases[0]
		tc.name, tc.message = "scoped negation mixed "+message, message
		if message == "开始总结，表格不用生成" {
			// Existing command normalization strips the execution label but
			// must retain the actual content restriction as the requirement.
			tc.wantTopic = "表格不用生成"
		}
		cases = append(cases, tc)
		tc.name, tc.documentsOnly = "scoped negation document "+message, true
		cases = append(cases, tc)
	}
	for _, message := range documentNegationQuestions {
		tc := cases[0]
		tc.name, tc.message = "quoted question mixed "+message, message
		cases = append(cases, tc)
		tc.name, tc.documentsOnly, tc.clarifies = "quoted question document "+message, true, true
		cases = append(cases, tc)
	}
	for _, message := range documentExecutionNegations {
		tc := cases[0]
		tc.name, tc.message, tc.clarifies = "execution negation mixed "+message, message, true
		cases = append(cases, tc)
		tc.name, tc.documentsOnly = "execution negation document "+message, true
		cases = append(cases, tc)
	}
	for i, tc := range cases {
		// setupTestDBs embeds t.Name in a SQLite URI. Keep punctuation (notably
		// ? and the # suffix for duplicate whitespace-normalized names) out.
		name := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return r
			}
			return '_'
		}, tc.name)
		t.Run(fmt.Sprintf("%03d_%s", i, name), func(t *testing.T) {
			db, _ := setupTestDBs(t)
			imDB := newSummaryWorkspaceIMValidationDB(t)
			if err := db.AutoMigrate(&model.AgentMessage{}, &model.AgentSummarySession{}, &model.AgentSummaryTurn{}, &model.SummaryWorkflowIdempotency{}, &model.SummaryUserTemplate{}); err != nil {
				t.Fatal(err)
			}
			if tc.template != nil && !tc.unregisteredTemplate {
				if err := db.Create(&model.SummaryUserTemplate{
					SpaceID: "space-a", UserID: "actor", TemplateID: tc.template.TemplateID,
					Label: tc.template.Label, Pattern: tc.template.Requirement, IsCustom: true,
				}).Error; err != nil {
					t.Fatal(err)
				}
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
			if tc.chatOnly {
				req.SummaryContext.Documents = nil
			}
			if tc.participants {
				req.SummaryContext.Participants = []summaryWorkspaceParticipant{{UserID: "other"}}
			}
			if tc.references {
				req.SummaryContext.ReferencedTaskIDs = []int64{999}
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
			if tc.rejects {
				wantStatus = http.StatusBadRequest
			}
			if rec.Code != wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, wantStatus, rec.Body.String())
			}
			if tc.clarifies || tc.rejects {
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
				if tc.rejects {
					var turns int64
					if err := db.Model(&model.AgentSummaryTurn{}).Count(&turns).Error; err != nil || turns != 0 {
						t.Fatalf("rejected request persisted turns=%d err=%v", turns, err)
					}
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
				if tc.references {
					var restored summaryWorkspaceContext
					if err := json.Unmarshal([]byte(state.Session.ScopeJSON), &restored); err != nil {
						t.Fatal(err)
					}
					if len(restored.ReferencedTaskIDs) != 1 || restored.ReferencedTaskIDs[0] != 999 {
						t.Fatalf("reference silently discarded: %+v", restored)
					}
					// Rehydrate a persisted document+reference scope and ask a
					// question: return recoverable clarification, not repeated 400s.
					req.SummaryContext = restored
					req.RequestID, req.Message = "document-input-2", "这些文档里有什么风险？"
					question := call()
					if question.Code != http.StatusOK || !strings.Contains(question.Body.String(), "请移除引用总结") {
						t.Fatalf("persisted scope cannot recover: %s", question.Body.String())
					}
					var tasks int64
					if err := db.Model(&model.SummaryTask{}).Count(&tasks).Error; err != nil || tasks != 0 {
						t.Fatalf("reference turn executed: tasks=%d err=%v", tasks, err)
					}
					client.mu.Lock()
					fetches := len(client.tokens)
					client.mu.Unlock()
					if fetches != 0 {
						t.Fatalf("reference question fetched %d documents", fetches)
					}
					// Removing the chip is the advertised recovery action and
					// must actually unblock execution without a new session.
					req.SummaryContext.ReferencedTaskIDs = nil
					req.ScopeVersion++
					req.RequestID, req.Message = "document-input-3", "请总结这些文档"
					recovered := call()
					if recovered.Code != http.StatusOK {
						t.Fatalf("scope recovery failed: %s", recovered.Body.String())
					}
					if err := db.Model(&model.SummaryTask{}).Count(&tasks).Error; err != nil || tasks != 1 {
						t.Fatalf("recovered tasks=%d err=%v", tasks, err)
					}
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
			wantTopic := tc.wantTopic
			if wantTopic == "" {
				wantTopic = tc.message
			}
			wantOrigin := ""
			if tc.chatOnly {
				wantOrigin = "group-a" // Existing chat-only origin behavior.
			}
			if task.Topic != wantTopic || task.OriginChannelID != wantOrigin {
				t.Fatalf("wrong requirement or automatic origin: %+v", task)
			}
			var sources []model.SummarySource
			if err := db.Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
				t.Fatal(err)
			}
			wantSources := 2
			if tc.documentsOnly || tc.chatOnly {
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
			if foundDocument != !tc.chatOnly || (!tc.documentsOnly && !foundChat) {
				t.Fatalf("source class lost: %+v", sources)
			}
			if tc.chatOnly {
				client.mu.Lock()
				fetches := len(client.tokens)
				client.mu.Unlock()
				if fetches != 0 {
					t.Fatalf("chat-only turn fetched %d documents", fetches)
				}
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

func TestVerifyDocumentTemplateRegistry(t *testing.T) {
	builtin := builtinTopicTemplates()[0]
	now := time.Now()
	for _, tc := range []struct {
		name, id, requirement string
		record                *model.SummaryUserTemplate
		want                  bool
	}{
		{name: "builtin", id: builtin.ID, requirement: builtin.Pattern, want: true},
		{name: "forged builtin content", id: builtin.ID, requirement: "what is the source?"},
		{name: "unknown id", id: "forged", requirement: "what is the source?"},
		{name: "own custom", id: "custom", requirement: "EXPLAIN RISKS", record: &model.SummaryUserTemplate{SpaceID: "s", UserID: "u", IsCustom: true, Pattern: "Explain Risks"}, want: true},
		{name: "other user", id: "custom", requirement: "Explain Risks", record: &model.SummaryUserTemplate{SpaceID: "s", UserID: "other", IsCustom: true, Pattern: "Explain Risks"}},
		{name: "other space", id: "custom", requirement: "Explain Risks", record: &model.SummaryUserTemplate{SpaceID: "other", UserID: "u", IsCustom: true, Pattern: "Explain Risks"}},
		{name: "deleted custom", id: "custom", requirement: "Explain Risks", record: &model.SummaryUserTemplate{SpaceID: "s", UserID: "u", IsCustom: true, Pattern: "Explain Risks", DeletedAt: &now}},
		{name: "override", id: builtin.ID, requirement: "Explain Risks", record: &model.SummaryUserTemplate{SpaceID: "s", UserID: "u", Pattern: "Explain Risks"}, want: true},
		{name: "stale builtin after override", id: builtin.ID, requirement: builtin.Pattern, record: &model.SummaryUserTemplate{SpaceID: "s", UserID: "u", Pattern: "Explain Risks"}},
		{name: "deleted override restores builtin", id: builtin.ID, requirement: builtin.Pattern, record: &model.SummaryUserTemplate{SpaceID: "s", UserID: "u", Pattern: "Explain Risks", DeletedAt: &now}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := setupTestDBs(t)
			if err := db.AutoMigrate(&model.SummaryUserTemplate{}); err != nil {
				t.Fatal(err)
			}
			if tc.record != nil {
				tc.record.TemplateID = tc.id
				if err := db.Create(tc.record).Error; err != nil {
					t.Fatal(err)
				}
			}
			w := &summaryWorkspaceCoordinator{db: db}
			got, err := w.verifyDocumentTemplate(context.Background(), "s", "u", &summaryWorkspaceTemplate{TemplateID: tc.id, Requirement: tc.requirement})
			if err != nil || got != tc.want {
				t.Fatalf("verified=%t want=%t err=%v", got, tc.want, err)
			}
		})
	}
}
