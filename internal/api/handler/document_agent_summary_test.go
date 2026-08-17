//go:build cgo

package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
)

type fakeDocumentSourceClient struct {
	docs map[string]*documentSummarySource
}

func (f fakeDocumentSourceClient) FetchSummarySource(ctx context.Context, spaceID, userID, documentID, version string, header http.Header) (*documentSummarySource, error) {
	if doc, ok := f.docs[documentID]; ok {
		cp := *doc
		if cp.Version == "" {
			cp.Version = version
		}
		return &cp, nil
	}
	return nil, errNoAgentOutput
}

func setupDocumentAgentSummaryRouter(h *AgentSummaryHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/agent/document", h.CreateDocumentAgentSummary)
	return r
}

func newDocumentSummaryLLM(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected LLM path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"## 文档总结\n- 关键结论 [1]"},"finish_reason":"stop"}],"usage":{"total_tokens":12,"completion_tokens":6}}`))
	}))
	return srv, srv.URL
}

func TestCreateDocumentAgentSummary_PersistsDocumentSummary(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	llmSrv, llmURL := newDocumentSummaryLLM(t)
	defer llmSrv.Close()
	h := NewAgentSummaryHandler(db, nil, llmURL, "test-key", "test-model", 5, 256)
	h.documentClient = fakeDocumentSourceClient{docs: map[string]*documentSummarySource{
		"doc-1": {
			DocumentID: "doc-1",
			Title:      "方案设计.md",
			Version:    "v1",
			Chunks: []documentSourceChunk{{
				ChunkID: "chunk-1",
				Page:    2,
				Text:    "这是文档正文。",
			}},
		},
	}}
	r := setupDocumentAgentSummaryRouter(h)

	body := []byte(`{"document_refs":[{"document_id":"doc-1","version":"v1"}],"idempotency_key":"idem-1"}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/agent/document", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var task model.SummaryTask
	if err := db.First(&task).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.TriggerType != model.TriggerAgent || task.Status != model.StatusCompleted {
		t.Fatalf("task trigger/status = %d/%d", task.TriggerType, task.Status)
	}
	var src model.SummarySource
	if err := db.Where("task_id = ?", task.ID).First(&src).Error; err != nil {
		t.Fatalf("load source: %v", err)
	}
	if src.SourceType != model.SourceDocument || src.SourceID != "doc-1" || src.SourceName != "方案设计.md" || src.SourceVersion != "v1" {
		t.Fatalf("unexpected source: %+v", src)
	}
	var pr model.PersonalResult
	if err := db.Where("task_id = ?", task.ID).First(&pr).Error; err != nil {
		t.Fatalf("load personal result: %v", err)
	}
	if pr.Content == "" {
		t.Fatal("personal result content is empty")
	}
	cits := pr.GetCitations()
	if len(cits) != 1 || cits[0].Type != "document" || cits[0].DocumentID != "doc-1" || cits[0].Page != 2 {
		t.Fatalf("unexpected citations: %+v", cits)
	}
}

func TestCreateDocumentAgentSummary_IdempotentConcurrentReplay(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	llmSrv, llmURL := newDocumentSummaryLLM(t)
	defer llmSrv.Close()
	h := NewAgentSummaryHandler(db, nil, llmURL, "test-key", "test-model", 5, 256)
	h.documentClient = fakeDocumentSourceClient{docs: map[string]*documentSummarySource{
		"doc-1": {DocumentID: "doc-1", Title: "doc", Content: "content"},
	}}
	r := setupDocumentAgentSummaryRouter(h)
	body := []byte(`{"document_refs":[{"document_id":"doc-1"}],"idempotency_key":"idem-race"}`)

	const n = 2
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/agent/document", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Token", "test-user")
			req.Header.Set("X-Space-Id", "test-space")
			r.ServeHTTP(w, req)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d expected 200, got %d", i, code)
		}
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count = %d, want 1", count)
	}
}

func TestCreateDocumentAgentSummary_RejectsMultipleVersionsOfSameDocument(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	llmSrv, llmURL := newDocumentSummaryLLM(t)
	defer llmSrv.Close()
	h := NewAgentSummaryHandler(db, nil, llmURL, "test-key", "test-model", 5, 256)
	h.documentClient = fakeDocumentSourceClient{docs: map[string]*documentSummarySource{
		"doc-1": {DocumentID: "doc-1", Title: "doc", Content: "content"},
	}}
	r := setupDocumentAgentSummaryRouter(h)

	body := []byte(`{"document_refs":[{"document_id":"doc-1","version":"v1"},{"document_id":"doc-1","version":"v2"}],"idempotency_key":"idem-multi-version"}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/agent/document", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 0 {
		t.Fatalf("task count = %d, want 0", count)
	}
}

func TestCreateDocumentAgentSummary_IdempotentReplay(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	llmSrv, llmURL := newDocumentSummaryLLM(t)
	defer llmSrv.Close()
	h := NewAgentSummaryHandler(db, nil, llmURL, "test-key", "test-model", 5, 256)
	h.documentClient = fakeDocumentSourceClient{docs: map[string]*documentSummarySource{
		"doc-1": {DocumentID: "doc-1", Title: "doc", Content: "content"},
	}}
	r := setupDocumentAgentSummaryRouter(h)
	body := []byte(`{"document_refs":[{"document_id":"doc-1"}],"idempotency_key":"idem-1"}`)

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/agent/document", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Token", "test-user")
		req.Header.Set("X-Space-Id", "test-space")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("iter %d expected 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count = %d, want 1", count)
	}
}

func TestCreateDocumentAgentSummary_IdempotencyConflict(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	llmSrv, llmURL := newDocumentSummaryLLM(t)
	defer llmSrv.Close()
	h := NewAgentSummaryHandler(db, nil, llmURL, "test-key", "test-model", 5, 256)
	h.documentClient = fakeDocumentSourceClient{docs: map[string]*documentSummarySource{
		"doc-1": {DocumentID: "doc-1", Title: "doc1", Content: "content"},
		"doc-2": {DocumentID: "doc-2", Title: "doc2", Content: "content"},
	}}
	r := setupDocumentAgentSummaryRouter(h)

	first := []byte(`{"document_refs":[{"document_id":"doc-1"}],"idempotency_key":"idem-1"}`)
	second := []byte(`{"document_refs":[{"document_id":"doc-2"}],"idempotency_key":"idem-1"}`)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/agent/document", bytes.NewReader(first))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/summaries/agent/document", bytes.NewReader(second))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("second expected 409, got %d: %s", w.Code, w.Body.String())
	}
}
