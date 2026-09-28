package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// PR1 contract for MIXED document+chat sources. Mixed admission is now
// unconditional (the phase-1 gate was removed when the worker executor
// landed). The frozen contract
// (docs/mixed-document-chat-summary-development-plan.md §4.2):
//   - capabilities broadcast the additive mixed_sources field (always true);
//   - a mixed context (channels + documents, no extra participants)
//     normalizes cleanly and keeps the chat time range;
//   - participants and referenced tasks combined with documents stay rejected;
//   - raw request arrays stay bounded: documents ≤
//     MaxDocumentSummarySourceCount, chat+documents combined ≤
//     mixedMaxTotalSources.

func TestSummaryWorkspaceCapabilitiesAdvertisesMixedSources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	handler := &AgentChatHandler{
		workspaceEntryEnabled: true,
		workspace:             &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}},
		documentClient:        &capabilityDocumentSourceClient{},
	}

	handler.SummaryWorkspaceCapabilities(context)

	var payload struct {
		Data struct {
			MixedSources bool `json:"mixed_sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if !payload.Data.MixedSources {
		t.Fatal("mixed_sources = false, want true (admission unconditional)")
	}
}

// capabilityDocumentSourceClient is a minimal documentSourceClient stub for
// capability-endpoint tests; FetchSummarySource is never invoked here.
type capabilityDocumentSourceClient struct{}

func (*capabilityDocumentSourceClient) FetchSummarySource(_ context.Context, _, _, _, _ string, _ http.Header) (*documentSummarySource, error) {
	return nil, nil
}

// mixedContext returns a valid mixed document+chat scope with an explicit
// (non-default) chat time range.
func mixedContext() summaryWorkspaceContext {
	return summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{
			{ChatID: "group-1", ChatType: "group", Name: "项目群"},
		},
		Documents: []summaryWorkspaceDocument{
			{DocumentID: "doc-1", Title: "产品方案"},
		},
		TimeRange: &summaryWorkspaceTimeRange{
			Start:  "2026-09-15T00:00:00+08:00",
			End:    "2026-09-22T00:00:00+08:00",
			Label:  "最近 7 天",
			Source: summaryWorkspaceTimeRangeSourcePicker,
		},
	}
}

func TestNormalizeSummaryWorkspaceContextAcceptsMixed(t *testing.T) {
	got, err := normalizeSummaryWorkspaceContext(mixedContext())
	if err != nil {
		t.Fatalf("normalize mixed context: %v", err)
	}
	if len(got.SelectedChannels) != 1 || len(got.Documents) != 1 {
		t.Fatalf("mixed scope dropped a source class: %#v", got)
	}
	if got.TimeRange == nil || got.TimeRange.Source != summaryWorkspaceTimeRangeSourcePicker {
		t.Fatalf("mixed scope must keep the explicit chat time range, got %#v", got.TimeRange)
	}
}

// Participants × documents stay rejected — phase 1 is personal-only.
func TestNormalizeSummaryWorkspaceContextRejectsMixedWithParticipants(t *testing.T) {
	context := mixedContext()
	context.Participants = []summaryWorkspaceParticipant{
		{UserID: "user-2", UserName: "同事"},
	}
	_, err := normalizeSummaryWorkspaceContext(context)
	if err == nil {
		t.Fatal("expected mixed scope with extra participants to be rejected")
	}
}

// Referenced tasks × documents stay rejected in phase 1: the mixed entry does
// not stack the reference-summary combination (plan §1.3 item 5).
func TestNormalizeSummaryWorkspaceContextRejectsMixedWithReferences(t *testing.T) {
	context := mixedContext()
	context.ReferencedTaskIDs = []int64{101}
	_, err := normalizeSummaryWorkspaceContext(context)
	if err == nil {
		t.Fatal("expected mixed scope with referenced tasks to be rejected")
	}
}

// Deduplication must not become a request-body amplifier: the RAW array is
// bounded before dedup (plan §1.2).
func TestNormalizeSummaryWorkspaceContextCapsRawMixedArrays(t *testing.T) {
	context := mixedContext()
	rawDocuments := make([]summaryWorkspaceDocument, 0, maxSummaryWorkspaceDocuments+1)
	for i := 0; i <= maxSummaryWorkspaceDocuments; i++ {
		rawDocuments = append(rawDocuments, summaryWorkspaceDocument{DocumentID: "doc-1", Title: "方案"})
	}
	context.Documents = rawDocuments
	_, err := normalizeSummaryWorkspaceContext(context)
	if err == nil {
		t.Fatal("expected raw document array over the limit to be rejected before dedup")
	}
}

func TestMixedSourceCountLimit(t *testing.T) {
	if mixedMaxTotalSources <= 0 {
		t.Fatalf("mixedMaxTotalSources = %d, must be positive", mixedMaxTotalSources)
	}
	if mixedMaxTotalSources < maxSummaryWorkspaceDocuments {
		t.Fatalf("mixedMaxTotalSources = %d must allow the pure-document maximum %d",
			mixedMaxTotalSources, maxSummaryWorkspaceDocuments)
	}
}
