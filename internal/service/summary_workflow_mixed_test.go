//go:build cgo

package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// PR1: mixed-scope persistence + idempotency (plan §4.3 steps 4-5 and the
// "no fake conflicts on retry" rule). Mixed admission is gated behind
// MixedSourcesAdmissionEnabled (default OFF); these tests turn it on.

func mixedSourcesForTest() []SummaryWorkflowSource {
	return []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		mixedDocSource("doc-1"),
	}
}

func TestMixedWorkflowPersistsBothSourceClassesAndSnapshot(t *testing.T) {
	enableMixedAdmission(t)
	svc, db := newSummaryWorkflowTestService(t)

	in := baseSummaryWorkflowInput()
	in.Sources = mixedSourcesForTest()
	created, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("mixed create: %v", err)
	}

	var sources []model.SummarySource
	if err := db.Where("task_id = ?", created.Task.ID).Find(&sources).Error; err != nil {
		t.Fatalf("load sources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("persisted %d sources, want 2 (chat + document): %#v", len(sources), sources)
	}
	seenTypes := map[int]bool{}
	for _, source := range sources {
		seenTypes[source.SourceType] = true
	}
	if !seenTypes[model.SourceGroup] || !seenTypes[model.SourceDocument] {
		t.Fatalf("source classes wrong: %#v", sources)
	}

	var snapshots int64
	db.Model(&model.SummarySourceSnapshot{}).Count(&snapshots)
	if snapshots != 1 {
		t.Fatalf("snapshot rows = %d, want 1 (document only)", snapshots)
	}
}

func TestMixedWorkflowIdempotentReplayKeepsOriginalSnapshot(t *testing.T) {
	enableMixedAdmission(t)
	svc, db := newSummaryWorkflowTestService(t)

	in := baseSummaryWorkflowInput()
	in.Sources = mixedSourcesForTest()
	in.IdempotencyKey = "mixed-create-001"

	first, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// A replay (e.g. client retry) returns the SAME task — the original
	// document snapshot must be reused, not re-fetched or overwritten.
	second, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Task.ID != first.Task.ID {
		t.Fatalf("replay = %#v, want same task", second)
	}
	var snapshots int64
	db.Model(&model.SummarySourceSnapshot{}).Count(&snapshots)
	if snapshots != 1 {
		t.Fatalf("snapshot rows after replay = %d, want 1 (original preserved)", snapshots)
	}
}

func TestMixedWorkflowSourceChangeWithSameKeyIsMismatch(t *testing.T) {
	enableMixedAdmission(t)
	svc, _ := newSummaryWorkflowTestService(t)

	in := baseSummaryWorkflowInput()
	in.Sources = mixedSourcesForTest()
	in.IdempotencyKey = "mixed-create-002"

	if _, err := svc.CreateFromLegacyHTTP(context.Background(), in); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Same key, different user request (source set changed) → conflict.
	in.Sources = []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-2"},
		mixedDocSource("doc-1"),
	}
	_, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	mismatch, ok := err.(*SummaryWorkflowIdempotencyError)
	if !ok {
		t.Fatalf("error = %v, want SummaryWorkflowIdempotencyError", err)
	}
	if mismatch.Reason != "request_mismatch" {
		t.Fatalf("reason = %q, want request_mismatch", mismatch.Reason)
	}
}

// Mixed create is admitted only when the gate is ON (worker executor landed).
func TestMixedWorkflowAdmittedWhenGateOn(t *testing.T) {
	enableMixedAdmission(t)
	svc, _ := newSummaryWorkflowTestService(t)

	in := baseSummaryWorkflowInput()
	in.Sources = mixedSourcesForTest()
	created, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("mixed create must be admitted with the gate on: %v", err)
	}
	if created.Task.ID == 0 {
		t.Fatal("mixed create returned no task")
	}
}

// The gate defaults OFF: a mixed create must be rejected with the clear
// contract error, not silently admitted into a worker that can't run it.
func TestMixedWorkflowRejectedWhenGateOff(t *testing.T) {
	// No enableMixedAdmission call — exercise the default-OFF path.
	svc, _ := newSummaryWorkflowTestService(t)

	in := baseSummaryWorkflowInput()
	in.Sources = mixedSourcesForTest()
	if _, err := svc.CreateFromLegacyHTTP(context.Background(), in); err == nil {
		t.Fatal("mixed create must be rejected when the admission gate is OFF")
	}
}

// Plan §1.3/A17: a phase-1 document-bearing task must not persist an origin
// channel (no auto-reply into a chat whose members were never authorized on
// the document). The mixed branch enforces this at the service boundary; the
// pure-document branch already did. A caller-supplied origin must be rejected.
func TestMixedWorkflowRejectsOriginChannel(t *testing.T) {
	enableMixedAdmission(t)
	svc, _ := newSummaryWorkflowTestService(t)

	in := baseSummaryWorkflowInput()
	in.Sources = mixedSourcesForTest()
	in.OriginChannelID = "group-origin"
	in.OriginChannelType = model.OriginChannelGroup
	if _, err := svc.CreateFromLegacyHTTP(context.Background(), in); err == nil {
		t.Fatal("mixed create with an origin channel must be rejected")
	}
}

// The service mixed branch must hold the per-document cap itself (the pure-
// document cap of MaxDocumentSummarySourceCount), not just the combined cap —
// 11 documents + 1 chat is below the combined 30 but above the document cap.
func TestMixedWorkflowRejectsOverDocumentCap(t *testing.T) {
	enableMixedAdmission(t)
	svc, _ := newSummaryWorkflowTestService(t)

	in := baseSummaryWorkflowInput()
	sources := make([]SummaryWorkflowSource, 0, MaxDocumentSummarySourceCount+2)
	sources = append(sources, SummaryWorkflowSource{SourceType: model.SourceGroup, SourceID: "group-1"})
	for i := 0; i <= MaxDocumentSummarySourceCount; i++ {
		sources = append(sources, mixedDocSource(fmt.Sprintf("doc-%d", i)))
	}
	in.Sources = sources
	if _, err := svc.CreateFromLegacyHTTP(context.Background(), in); err == nil {
		t.Fatal("mixed create with 11 documents must be rejected by the per-document cap")
	}
}
