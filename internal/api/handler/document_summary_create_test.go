//go:build cgo

package handler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
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

func TestCreateDocumentSummaryPersistsCurrentSnapshots(t *testing.T) {
	db, imDB := setupTestDBs(t)
	client := &recordingDocumentSourceClient{docs: map[string]*documentSummarySource{
		"d_1": {DocumentID: "d_1", Title: "方案一", Version: "v7", Content: "正文一"},
		"d_2": {DocumentID: "d_2", Title: "方案二", Version: "v9", Content: "正文二"},
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
	if len(snapshots) != 2 || snapshots[0].Content != "正文一" || snapshots[1].Content != "正文二" {
		t.Fatalf("snapshots = %#v", snapshots)
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
