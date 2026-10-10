package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// Mixed document+chat invariant tests. Mixed admission is now unconditional
// (the phase-1 gate was removed when the worker executor landed), so these
// tests exercise the invariant subset directly: personal-only, no participants,
// no origin auto-reply, snapshot presence, and the combined source cap.

func mixedDocSource(id string) SummaryWorkflowSource {
	content := "文档内容 " + id
	hash := sha256.Sum256([]byte(content))
	return SummaryWorkflowSource{
		SourceType:      model.SourceDocument,
		SourceID:        id,
		SnapshotContent: content,
		SourceHash:      hex.EncodeToString(hash[:]),
	}
}

func mixedWorkflowInput() LegacyCreateSummaryWorkflowInput {
	return LegacyCreateSummaryWorkflowInput{
		ActorID:   "user-1",
		CreatorID: "user-1",
		SpaceID:   "space-1",
	}
}

func TestValidateDocumentWorkflowInputMixedAccepted(t *testing.T) {
	in := mixedWorkflowInput()
	sources := []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		mixedDocSource("doc-1"),
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr != nil {
		t.Fatalf("mixed sources must validate: %v", bizErr)
	}
}

func TestValidateDocumentWorkflowInputMixedRejectsParticipants(t *testing.T) {
	in := mixedWorkflowInput()
	in.Participants = []SummaryWorkflowParticipant{{UserID: "user-2", UserName: "同事"}}
	sources := []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		mixedDocSource("doc-1"),
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("mixed scope with participants must be rejected (phase-1 personal-only)")
	}
}

func TestValidateDocumentWorkflowInputMixedRejectsBadSnapshot(t *testing.T) {
	in := mixedWorkflowInput()
	sources := []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		{SourceType: model.SourceDocument, SourceID: "doc-1"}, // no snapshot
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("document source without snapshot must be rejected even in mixed scope")
	}
}

func TestValidateDocumentWorkflowInputMixedCountCap(t *testing.T) {
	in := mixedWorkflowInput()
	sources := make([]SummaryWorkflowSource, 0, MixedMaxTotalSources+1)
	sources = append(sources, SummaryWorkflowSource{SourceType: model.SourceGroup, SourceID: "group-1"})
	for i := 0; i < MixedMaxTotalSources; i++ {
		sources = append(sources, mixedDocSource("doc-"+string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("mixed scope over the combined cap must be rejected")
	}
}

// The combined cap must be exercised in ISOLATION: documents stay at the
// per-document maximum so the >MixedMaxTotalSources branch (not the 10-document
// cap) produces the rejection, and the admitted boundary case (documents ≤10,
// chat+documents = 30) passes. A05 pins both ends of 「合计 30/31 个」.
func TestValidateDocumentWorkflowInputCombinedCapIsolating(t *testing.T) {
	in := mixedWorkflowInput()
	sources := make([]SummaryWorkflowSource, 0, MixedMaxTotalSources)
	for i := 0; i < MaxDocumentSummarySourceCount; i++ {
		sources = append(sources, mixedDocSource(fmt.Sprintf("doc-%d", i)))
	}
	for i := 0; i < MixedMaxTotalSources-MaxDocumentSummarySourceCount; i++ {
		sources = append(sources, SummaryWorkflowSource{SourceType: model.SourceGroup, SourceID: fmt.Sprintf("group-%d", i)})
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr != nil {
		t.Fatalf("30 sources (10 docs + 20 chats) must be admitted: %v", bizErr)
	}

	over := append(sources, SummaryWorkflowSource{SourceType: model.SourceGroup, SourceID: "group-over"})
	bizErr := validateDocumentWorkflowInput(in, over)
	if bizErr == nil {
		t.Fatal("31 sources (10 docs + 21 chats) must be rejected by the combined cap")
	}
	if !strings.Contains(bizErr.Error(), "混合来源总数不能超过30个") {
		t.Fatalf("rejection must come from the combined cap, got: %v", bizErr)
	}
}

func TestValidateDocumentWorkflowInputPureDocumentsUnchanged(t *testing.T) {
	// Mixed admission must not change the pure-document path.
	in := mixedWorkflowInput()
	in.TimeRange = &SummaryWorkflowTimeRange{}
	sources := []SummaryWorkflowSource{mixedDocSource("doc-1")}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("pure-document scope with explicit time range must stay rejected")
	}
}
