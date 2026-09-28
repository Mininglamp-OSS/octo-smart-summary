//go:build cgo

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// PR1 commit 2: source MERGE + permission consumption for mixed scopes.

func TestMergeMixedWorkflowSourcesKeepsBothClasses(t *testing.T) {
	chat := []service.SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1", SourceName: "项目群"},
		{SourceType: model.SourceDirect, SourceID: "dm-1", SourceName: "直聊"},
	}
	docs := []service.SummaryWorkflowSource{
		{SourceType: model.SourceDocument, SourceID: "doc-1", SourceName: "方案", SnapshotContent: "c", SourceHash: "h"},
	}
	merged := mergeMixedWorkflowSources(chat, docs)
	if len(merged) != 3 {
		t.Fatalf("merged = %d sources, want 3 (chat first, docs appended): %#v", len(merged), merged)
	}
	if merged[0].SourceID != "group-1" || merged[2].SourceID != "doc-1" {
		t.Fatalf("merge order wrong: %#v", merged)
	}
}

// A document and a chat channel with the SAME id must NOT collapse — dedup is
// per (source_type, source_id), never bare id (plan §1.2).
func TestMergeMixedWorkflowSourcesSameIDDifferentType(t *testing.T) {
	chat := []service.SummaryWorkflowSource{{SourceType: model.SourceGroup, SourceID: "obj-1"}}
	docs := []service.SummaryWorkflowSource{{SourceType: model.SourceDocument, SourceID: "obj-1", SnapshotContent: "c", SourceHash: "h"}}
	merged := mergeMixedWorkflowSources(chat, docs)
	if len(merged) != 2 {
		t.Fatalf("same id across types collapsed: %#v", merged)
	}
}

func TestMergeMixedWorkflowSourcesDeduplicatesSameType(t *testing.T) {
	chat := []service.SummaryWorkflowSource{{SourceType: model.SourceGroup, SourceID: "group-1"}}
	docs := []service.SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"}, // duplicate key, must drop
		{SourceType: model.SourceDocument, SourceID: "doc-1", SnapshotContent: "c", SourceHash: "h"},
	}
	merged := mergeMixedWorkflowSources(chat, docs)
	if len(merged) != 2 {
		t.Fatalf("merge = %d, want 2: %#v", len(merged), merged)
	}
}

// summaryWorkspaceSources emits snapshot-less SourceDocument rows for a mixed
// scope; prepareDocumentSummarySourcesFromRefs emits the snapshot-carrying
// versions. The merge must keep the SNAPSHOT-carrying document rows (from the
// second arg) and drop the snapshot-less ones (from the first), or
// validateDocumentWorkflowInput rejects the task with "文档来源缺少正文快照".
func TestMergeMixedWorkflowSourcesPrefersFetchedDocumentSnapshot(t *testing.T) {
	// chat side carries a snapshot-less document row (as summaryWorkspaceSources does)
	chat := []service.SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1", SourceName: "项目群"},
		{SourceType: model.SourceDocument, SourceID: "doc-1", SourceName: "方案"},
	}
	// fetched document sources carry the snapshot
	docs := []service.SummaryWorkflowSource{
		{SourceType: model.SourceDocument, SourceID: "doc-1", SourceName: "方案", SourceVersion: "v3", SourceHash: "h", SnapshotContent: "正文"},
	}
	merged := mergeMixedWorkflowSources(chat, docs)
	if len(merged) != 2 {
		t.Fatalf("merged = %d sources, want 2 (1 chat + 1 snapshot-carrying document): %#v", len(merged), merged)
	}
	var doc *service.SummaryWorkflowSource
	for i := range merged {
		if merged[i].SourceType == model.SourceDocument {
			doc = &merged[i]
		}
	}
	if doc == nil {
		t.Fatal("document source dropped entirely")
	}
	if doc.SnapshotContent != "正文" || doc.SourceHash != "h" || doc.SourceVersion != "v3" {
		t.Fatalf("document source kept the snapshot-less row: %#v", doc)
	}
}

// validateWorkspaceScope must PERMISSION-CHECK chat sources in a mixed scope
// instead of short-circuiting to valid (plan §4.3). Uses a sqlite imDB where
// the group exists but the user is NOT a member → sourcesValid=false (no
// lookup error; a definitive deny, not a transport failure).
func TestValidateWorkspaceScopeChecksChatSourcesInMixedScope(t *testing.T) {
	imDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec(`CREATE TABLE "group" (group_no TEXT NOT NULL, name TEXT, space_id TEXT, status INTEGER DEFAULT 1, updated_at DATETIME)`)
	imDB.Exec(`CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, status INTEGER DEFAULT 1, is_deleted INTEGER DEFAULT 0)`)
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('group-1', '项目群', 'space-1', 1)`)
	// No group_member row: the actor does not belong to the group.

	coordinator := &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}, imDB: imDB}
	validation, lookupErr := coordinator.validateWorkspaceScope(
		context.Background(), "space-1", "user-1",
		summaryWorkspaceContext{
			SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
			Documents:        []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}},
		},
	)
	if lookupErr != nil {
		t.Fatalf("lookup error: %v", lookupErr)
	}
	if validation.sourcesValid {
		t.Fatal("mixed scope with an unauthorized chat must NOT report sourcesValid=true")
	}
}

// The same mixed scope with a real membership resolves sourcesValid=true.
func TestValidateWorkspaceScopeMixedWithMembership(t *testing.T) {
	imDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec(`CREATE TABLE "group" (group_no TEXT NOT NULL, name TEXT, space_id TEXT, status INTEGER DEFAULT 1, updated_at DATETIME)`)
	imDB.Exec(`CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, status INTEGER DEFAULT 1, is_deleted INTEGER DEFAULT 0)`)
	imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status) VALUES ('group-1', '项目群', 'space-1', 1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('group-1', 'user-1', 0)`)

	coordinator := &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}, imDB: imDB}
	validation, lookupErr := coordinator.validateWorkspaceScope(
		context.Background(), "space-1", "user-1",
		summaryWorkspaceContext{
			SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
			Documents:        []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}},
		},
	)
	if lookupErr != nil {
		t.Fatalf("lookup error: %v", lookupErr)
	}
	if !validation.sourcesValid {
		t.Fatal("mixed scope with an authorized chat must report sourcesValid=true")
	}
}

func TestValidateWorkspaceScopeDocumentsOnlyShortCircuits(t *testing.T) {
	coordinator := &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}}
	validation, lookupErr := coordinator.validateWorkspaceScope(
		context.Background(), "space-1", "user-1",
		summaryWorkspaceContext{Documents: []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}}},
	)
	if lookupErr != nil {
		t.Fatalf("lookup error: %v", lookupErr)
	}
	if !validation.sourcesValid {
		t.Fatal("documents-only scope has no chat sources; sourcesValid should stay true")
	}
}

// An EMPTY scope (no chats AND no documents) must not report sourcesValid=true:
// it is not a valid source set, and pre-PR routing sent it to clarification
// rather than firing an agent preview on a scope routing already knew was
// unsummarizable. This pins the round-2 P1-1 regression fix.
func TestValidateWorkspaceScopeEmptyScopeIsNotValid(t *testing.T) {
	coordinator := &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}}
	validation, lookupErr := coordinator.validateWorkspaceScope(
		context.Background(), "space-1", "user-1",
		summaryWorkspaceContext{},
	)
	if lookupErr != nil {
		t.Fatalf("lookup error: %v", lookupErr)
	}
	if validation.sourcesValid {
		t.Fatal("empty scope (no chats, no documents) must report sourcesValid=false")
	}
}

// deriveWorkspaceRoute: mixed + invalid chat sources → clarification, never a
// silent workflow.
func TestDeriveWorkspaceRouteMixedInvalidSourcesClarifies(t *testing.T) {
	context := summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "群"}},
		Documents:        []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}},
	}
	route := deriveWorkspaceRoute(context, service.SummaryActionChat, service.SummaryIntentGenerate,
		true, true, true, false, WorkspaceSnapshot{}, true, false, true)
	if route != service.SummaryRouteClarification {
		t.Fatalf("route = %v, want clarification for mixed scope with invalid sources", route)
	}
}

func TestDeriveWorkspaceRouteMixedValidSourcesPersonalWorkflow(t *testing.T) {
	context := summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "群"}},
		Documents:        []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}},
	}
	route := deriveWorkspaceRoute(context, service.SummaryActionChat, service.SummaryIntentGenerate,
		true, true, true, false, WorkspaceSnapshot{}, true, true, true)
	if route != service.SummaryRoutePersonalWorkflow {
		t.Fatalf("route = %v, want personal workflow for valid mixed scope", route)
	}
}

// Mixed scopes keep the chat time range through agent-context materialization.
func TestMaterializeWorkspaceAgentContextKeepsMixedTimeRange(t *testing.T) {
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }}
	contextValue := summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "群"}},
		Documents:        []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}},
		TimeRange: &summaryWorkspaceTimeRange{
			Start:  "2026-09-15T00:00:00+08:00",
			End:    "2026-09-22T00:00:00+08:00",
			Label:  "最近 7 天",
			Source: summaryWorkspaceTimeRangeSourcePicker,
		},
	}
	got, _, err := coordinator.materializeWorkspaceAgentContext(
		context.Background(), "space-1", "user-1", contextValue,
		WorkspaceSnapshot{}, "总结", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got.TimeRange == nil {
		t.Fatal("mixed scope lost its chat time range during materialization")
	}
}

func TestSummaryWorkspaceTitleMixed(t *testing.T) {
	title := summaryWorkspaceTitle(summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "项目群"}},
		Documents:        []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "产品方案"}},
	})
	if title != "产品方案与项目群总结" {
		t.Fatalf("mixed title = %q", title)
	}
}

// Plan §1.3/A17: a document-bearing scope must not derive an origin channel.
// summaryWorkspaceOrigin must return empty whenever documents are present, even
// with chats selected, so a mixed scope never persists origin_channel_id.
func TestSummaryWorkspaceOriginEmptyForDocumentScope(t *testing.T) {
	cases := []struct {
		name    string
		context summaryWorkspaceContext
	}{
		{
			name: "mixed documents + chat",
			context: summaryWorkspaceContext{
				SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "群"}},
				Documents:        []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}},
			},
		},
		{
			name: "documents only",
			context: summaryWorkspaceContext{
				Documents: []summaryWorkspaceDocument{{DocumentID: "doc-1", Title: "方案"}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, typ := summaryWorkspaceOrigin(c.context)
			if id != "" || typ != 0 {
				t.Fatalf("origin = (%q, %d), want empty for a document-bearing scope", id, typ)
			}
		})
	}

	// Contrast: a pure-chat scope still derives origin from the first channel.
	id, typ := summaryWorkspaceOrigin(summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-1", ChatType: "group", Name: "群"}},
	})
	if id != "group-1" || typ != model.OriginChannelGroup {
		t.Fatalf("pure-chat origin = (%q, %d), want (group-1, group)", id, typ)
	}
}

var _ = http.StatusOK
