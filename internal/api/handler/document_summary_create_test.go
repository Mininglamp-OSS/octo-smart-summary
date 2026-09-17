//go:build cgo

package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

type recordingDocumentSourceClient struct {
	mu       sync.Mutex
	docs     map[string]*documentSummarySource
	errs     map[string]error
	versions map[string]string
	tokens   map[string]string
}

func (c *recordingDocumentSourceClient) FetchSummarySource(_ context.Context, _, _, documentID, version string, header http.Header) (*documentSummarySource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.versions == nil {
		c.versions = map[string]string{}
		c.tokens = map[string]string{}
	}
	c.versions[documentID] = version
	c.tokens[documentID] = header.Get("Token")
	if err := c.errs[documentID]; err != nil {
		return nil, err
	}
	return c.docs[documentID], nil
}

func TestCreateDocumentSummaryPersistsNormalizedSnapshots(t *testing.T) {
	db, imDB := setupTestDBs(t)
	client := &recordingDocumentSourceClient{docs: map[string]*documentSummarySource{
		"d_1": {
			DocumentID: "d_1", Title: "方案一", Version: "v7", Content: "不应使用的 content",
			Chunks: []documentSourceChunk{{Title: "第一节", Text: "正文一"}},
		},
		"d_2": {DocumentID: "d_2", Title: "方案二", Version: "v9", Content: strings.Repeat("文", maxDocumentPromptRunes+1)},
	}}
	h := NewTaskHandler(db, imDB, "")
	h.documentClient = client
	w := doCreateSummary(setupCreateRouter(h), map[string]interface{}{
		"title": "文档总结",
		"sources": []map[string]interface{}{
			{"source_type": model.SourceDocument, "source_id": "d_1"},
			{"source_type": model.SourceDocument, "source_id": "d_2"},
		},
	}, "creator1")
	if w.Code != http.StatusOK || respCode(t, w) != 0 {
		t.Fatalf("create response = %d %s", w.Code, w.Body.String())
	}

	var sources []model.SummarySource
	if err := db.Order("source_id").Find(&sources).Error; err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].SourceName != "方案一" || sources[0].SourceVersion != "v7" || len(sources[0].SourceHash) != 64 {
		t.Fatalf("sources = %#v", sources)
	}
	var snapshots []model.SummarySourceSnapshot
	if err := db.Order("summary_source_id").Find(&snapshots).Error; err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].Content != "### 第一节\n正文一" || !snapshots[0].Truncated || !snapshots[1].Truncated {
		t.Fatalf("snapshot states = count:%d first_content:%q first_truncated:%t second_truncated:%t", len(snapshots), snapshots[0].Content, snapshots[0].Truncated, snapshots[1].Truncated)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.versions["d_1"] != "" || client.versions["d_2"] != "" {
		t.Fatalf("versions = %#v, want current-version fetches", client.versions)
	}
	if client.tokens["d_1"] != "creator1" || client.tokens["d_2"] != "creator1" {
		t.Fatalf("tokens = %#v, want caller Token forwarded", client.tokens)
	}
}

func TestSummaryWorkspaceDocumentWorkflowPersistsSnapshots(t *testing.T) {
	db, imDB := setupTestDBs(t)
	if err := db.AutoMigrate(
		&model.AgentMessage{},
		&model.AgentSummarySession{},
		&model.AgentSummaryTurn{},
		&model.SummaryWorkflowIdempotency{},
	); err != nil {
		t.Fatalf("migrate workspace tables: %v", err)
	}
	client := &recordingDocumentSourceClient{docs: map[string]*documentSummarySource{
		"d_1": {DocumentID: "d_1", Title: "Workbench方案", Version: "v1", Content: "文档正文"},
	}}
	h := &AgentChatHandler{
		documentClient: client,
		workspace: &summaryWorkspaceCoordinator{
			db:       db,
			imDB:     imDB,
			store:    NewAgentWorkspaceStore(db),
			workflow: service.NewSummaryWorkflowService(db, imDB, 31, 90),
		},
	}
	key := WorkspaceSessionKey{SpaceID: "space-1", UserID: "creator1", SessionID: "workspace-doc"}
	scope := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{},
		Documents:         []summaryWorkspaceDocument{{DocumentID: "d_1", Title: "Workbench方案"}},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
		Template:          &summaryWorkspaceTemplate{TemplateID: "doc", Label: "文档总结", Requirement: "总结文档"},
	}
	begin := beginWorkspaceTurnForTest(t, h.workspace.store, key, "request-doc", 1, scope)
	if begin.Disposition != WorkspaceTurnAcquired {
		t.Fatalf("begin disposition = %s, want acquired", begin.Disposition)
	}

	snapshot, err := h.completeWorkspaceWorkflow(
		context.Background(), http.Header{"Token": []string{"creator1"}}, key, begin.Turn.ID, begin.Turn.Attempt,
		"workspace-doc-workflow-001", "开始总结", 1, scope, "总结文档",
		service.SummaryWorkflowPersonal, false,
	)
	if err != nil {
		t.Fatalf("complete document workflow: %v", err)
	}
	if snapshot.Session.WorkflowTaskID == 0 {
		t.Fatalf("workflow task was not recorded: %#v", snapshot.Session)
	}

	var source model.SummarySource
	if err := db.First(&source, "task_id = ? AND source_type = ?", snapshot.Session.WorkflowTaskID, model.SourceDocument).Error; err != nil {
		t.Fatalf("load document source: %v", err)
	}
	var persistedSnapshot model.SummarySourceSnapshot
	if err := db.First(&persistedSnapshot, "summary_source_id = ?", source.ID).Error; err != nil {
		t.Fatalf("load document snapshot: %v", err)
	}
	if source.SourceName != "Workbench方案" || source.SourceVersion != "v1" || persistedSnapshot.Content != "文档正文" {
		t.Fatalf("source=%#v snapshot=%#v", source, persistedSnapshot)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.tokens["d_1"] != "creator1" {
		t.Fatalf("forwarded token = %q, want creator1", client.tokens["d_1"])
	}
}

func TestCreateDocumentSummaryRejectsAggregateOverBudget(t *testing.T) {
	db, imDB := setupTestDBs(t)
	content := strings.Repeat("文", 70000)
	h := NewTaskHandler(db, imDB, "")
	h.documentClient = &recordingDocumentSourceClient{docs: map[string]*documentSummarySource{
		"d_1": {Content: content}, "d_2": {Content: content}, "d_3": {Content: content},
	}}
	w := doCreateSummary(setupCreateRouter(h), map[string]interface{}{
		"sources": []map[string]interface{}{
			{"source_type": model.SourceDocument, "source_id": "d_1"},
			{"source_type": model.SourceDocument, "source_id": "d_2"},
			{"source_type": model.SourceDocument, "source_id": "d_3"},
		},
	}, "creator1")
	if w.Code != http.StatusRequestEntityTooLarge || respCode(t, w) != 40007 {
		t.Fatalf("create response = %d %s", w.Code, w.Body.String())
	}
	var taskCount int64
	if err := db.Model(&model.SummaryTask{}).Count(&taskCount).Error; err != nil || taskCount != 0 {
		t.Fatalf("task count=%d err=%v, want zero", taskCount, err)
	}
}

func TestDocumentSnapshotContentKeepsChunkTitlesWithinPerDocumentBudget(t *testing.T) {
	doc := &documentSummarySource{}
	for i := 0; i < 10; i++ {
		doc.Chunks = append(doc.Chunks, documentSourceChunk{
			Title: strings.Repeat("题", maxDocumentTitleRunes),
			Text:  strings.Repeat("正文", maxDocumentChunkRunes),
		})
	}
	normalizeFetchedDocumentSource(doc, documentRefReq{DocumentID: "d_1"})
	content := documentSnapshotContent(doc)
	if !doc.Truncated {
		t.Fatal("snapshot was capped without recording truncation")
	}
	if got := len([]rune(content)); got != maxDocumentPromptRunes {
		t.Fatalf("snapshot runes=%d, want %d", got, maxDocumentPromptRunes)
	}
	if !strings.HasPrefix(content, documentChunkTitlePrefix) {
		t.Fatalf("snapshot lost chunk title: %q", content[:20])
	}
}

func TestCreateDocumentSummarySharesPreviewAdmissionLimit(t *testing.T) {
	releases := make([]func(), 0, documentSummaryMaxInFlightPerUser)
	for i := 0; i < documentSummaryMaxInFlightPerUser; i++ {
		release, ok := documentSummaryLimiterInstance.acquire("creator-limit")
		if !ok {
			t.Fatal("could not reserve preview limiter slot for test")
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	db, imDB := setupTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	h.documentClient = &recordingDocumentSourceClient{docs: map[string]*documentSummarySource{"d_1": {Content: "正文"}}}
	w := doCreateSummary(setupCreateRouter(h), map[string]interface{}{
		"sources": []map[string]interface{}{{"source_type": model.SourceDocument, "source_id": "d_1"}},
	}, "creator-limit")
	if w.Code != http.StatusTooManyRequests || respCode(t, w) != 42902 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("create response = %d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
}

func TestCreateDocumentSummaryRequiresAllFetchesBeforeWriting(t *testing.T) {
	db, imDB := setupTestDBs(t)
	client := &recordingDocumentSourceClient{
		docs: map[string]*documentSummarySource{"d_1": {Content: "正文一"}},
		errs: map[string]error{"d_2": &documentSourceError{status: http.StatusBadRequest, message: "missing"}},
	}
	h := NewTaskHandler(db, imDB, "")
	h.documentClient = client
	w := doCreateSummary(setupCreateRouter(h), map[string]interface{}{
		"sources": []map[string]interface{}{
			{"source_type": model.SourceDocument, "source_id": "d_1"},
			{"source_type": model.SourceDocument, "source_id": "d_2"},
		},
	}, "creator1")
	if w.Code != http.StatusBadRequest || respCode(t, w) != 40003 {
		t.Fatalf("create response = %d %s", w.Code, w.Body.String())
	}
	for _, table := range []interface{}{&model.SummaryTask{}, &model.SummarySource{}, &model.SummarySourceSnapshot{}} {
		var count int64
		if err := db.Model(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("model %T count=%d err=%v, want zero", table, count, err)
		}
	}
}

func TestCreateDocumentSummaryRejectsMixedOrTeamRequests(t *testing.T) {
	for _, test := range []struct {
		name string
		body map[string]interface{}
	}{
		{"mixed", map[string]interface{}{"sources": []map[string]interface{}{{"source_type": model.SourceDocument, "source_id": "d_1"}, {"source_type": model.SourceGroup, "source_id": "g_1"}}}},
		{"participant", map[string]interface{}{"sources": []map[string]interface{}{{"source_type": model.SourceDocument, "source_id": "d_1"}}, "participants": []map[string]interface{}{{"user_id": "u2"}}}},
		{"time range", map[string]interface{}{"sources": []map[string]interface{}{{"source_type": model.SourceDocument, "source_id": "d_1"}}, "time_range": map[string]interface{}{"start": "2026-09-01T00:00:00Z", "end": "2026-09-02T00:00:00Z"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, imDB := setupTestDBs(t)
			h := NewTaskHandler(db, imDB, "")
			h.documentClient = &recordingDocumentSourceClient{docs: map[string]*documentSummarySource{"d_1": {Content: "正文"}}}
			w := doCreateSummary(setupCreateRouter(h), test.body, "creator1")
			if w.Code != http.StatusBadRequest || respCode(t, w) != 40001 {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestMapDocumentSummaryCreateErrorFallback(t *testing.T) {
	got := mapDocumentSummaryCreateError(errors.New("network"))
	if got.status != http.StatusBadGateway || got.code != 50202 {
		t.Fatalf("mapped error = %#v", got)
	}
}
