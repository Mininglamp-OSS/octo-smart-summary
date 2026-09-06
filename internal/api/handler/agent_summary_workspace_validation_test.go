//go:build cgo

package handler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newSummaryWorkspaceIMValidationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	statements := []string{
		`CREATE TABLE "group" (group_no TEXT, name TEXT, space_id TEXT, status INTEGER, updated_at INTEGER)`,
		`CREATE TABLE group_member (group_no TEXT, uid TEXT, status INTEGER, is_deleted INTEGER, role INTEGER)`,
		`CREATE TABLE conversation_extra (channel_id TEXT, uid TEXT, channel_type INTEGER, updated_at INTEGER)`,
		`CREATE TABLE thread (id INTEGER PRIMARY KEY, short_id TEXT, name TEXT, group_no TEXT, status INTEGER, message_count INTEGER, creator_uid TEXT, updated_at INTEGER)`,
		`CREATE TABLE space_member (space_id TEXT, uid TEXT, status INTEGER)`,
		`CREATE TABLE message (message_seq INTEGER, from_uid TEXT, channel_id TEXT, channel_type INTEGER, timestamp INTEGER, payload BLOB, is_deleted INTEGER DEFAULT 0)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("create IM schema: %v", err)
		}
	}
	if err := db.Exec(`INSERT INTO "group" VALUES ('group-a','A群','space-a',1,1),('group-c','C群','space-a',1,1),('group-b','B群','space-b',1,1)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO group_member VALUES ('group-a','actor',1,0,0),('group-a','member',1,0,0),('group-a','inactive',0,0,0),('group-c','actor',1,0,0),('group-c','member-b',1,0,0),('group-b','actor',1,0,0)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO conversation_extra VALUES ('peer-in','actor',1,2),('peer-out','actor',1,1)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO space_member VALUES ('space-a','actor',1),('space-a','member',1),('space-a','member-b',1),('space-a','peer-in',1),('space-b','peer-out',1)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSummaryWorkspaceRecentChannelUsesNewestAuthorisedMessageInSpace(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	canonicalDM := pipeline.NormalizeDMChannelID("peer-in", "actor", model.ChannelTypeDM)
	rows := []struct {
		id      string
		kind    int
		when    time.Time
		content string
	}{
		{id: "group-a", kind: model.ChannelTypeGroup, when: now.Add(-24 * time.Hour), content: "group"},
		{id: canonicalDM, kind: model.ChannelTypeDM, when: now.Add(-2 * time.Hour), content: "dm"},
		{id: "group-b", kind: model.ChannelTypeGroup, when: now.Add(-time.Hour), content: "cross-space"},
	}
	for i, row := range rows {
		if err := imDB.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, 'actor', ?, ?, ?, ?, 0)`, i+1, row.id, row.kind, row.when.Unix(), []byte(`{"content":"`+row.content+`"}`)).Error; err != nil {
			t.Fatal(err)
		}
	}
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB, messageTableCount: 1, now: func() time.Time { return now }}
	got, err := coordinator.findMostRecentAuthorizedChannel(context.Background(), "space-a", "actor", now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("find recent channel: %v", err)
	}
	if got.ChatType != "direct" || got.ChatID != canonicalDM {
		t.Fatalf("recent channel = %#v, want authorised in-space DM %q", got, canonicalDM)
	}
}

func TestSummaryWorkspaceRecentChannelDoesNotExpandPastEffectiveRange(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := imDB.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (1, 'actor', 'group-a', 2, ?, X'01', 0)`, now.Add(-8*24*time.Hour).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB, messageTableCount: 1}
	_, err := coordinator.findMostRecentAuthorizedChannel(context.Background(), "space-a", "actor", now.Add(-7*24*time.Hour), now)
	if !errors.Is(err, errSummaryWorkspaceNoRecentChannel) {
		t.Fatalf("find recent channel error = %v, want no recent channel", err)
	}
}

func TestMaterializeTemplateOnlyContextPinsRecentChannelAndOneMonth(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := imDB.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (1, 'actor', 'group-a', 2, ?, X'01', 0)`, now.Add(-time.Hour).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB, messageTableCount: 1, now: func() time.Time { return now }}
	contextValue := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{},
		Participants:      []summaryWorkspaceParticipant{},
		Template:          &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "总结进展"},
		ReferencedTaskIDs: []int64{},
	}
	got, inferred, err := coordinator.materializeWorkspaceAgentContext(context.Background(), "space-a", "actor", contextValue, WorkspaceSnapshot{}, "", service.SummaryIntentGenerate, summaryWorkspaceInputSystemIntent)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if !inferred || len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "group-a" {
		t.Fatalf("materialized source = %#v inferred=%t", got.SelectedChannels, inferred)
	}
	start, end, err := parseSummaryWorkspaceTimeRange(got.TimeRange)
	if err != nil || end.Sub(start) != time.Duration(service.AgentSummaryDefaultTimeRangeDays)*24*time.Hour {
		t.Fatalf("materialized range = %#v duration=%s err=%v", got.TimeRange, end.Sub(start), err)
	}
}

func TestMaterializeOpenScopeAgentContextPinsOneMonth(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}

	got, inferred, err := coordinator.materializeWorkspaceAgentContext(
		context.Background(), "space-a", "actor", emptySummaryWorkspaceContext(), WorkspaceSnapshot{},
		"生成一份总结", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize open-scope context: %v", err)
	}
	if inferred || len(got.SelectedChannels) != 0 {
		t.Fatalf("open-scope context unexpectedly inferred a channel: %#v inferred=%t", got.SelectedChannels, inferred)
	}
	start, end, err := parseSummaryWorkspaceTimeRange(got.TimeRange)
	if err != nil || end.Sub(start) != time.Duration(service.AgentSummaryDefaultTimeRangeDays)*24*time.Hour {
		t.Fatalf("materialized range = %#v duration=%s err=%v", got.TimeRange, end.Sub(start), err)
	}
}

func TestSummaryWorkspaceValidateSourcesEnforcesSpaceAndDMMembership(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB}
	ctx := context.Background()

	valid, err := coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}})
	if err != nil || !valid {
		t.Fatalf("same-space group valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "group-b", ChatType: "group", Name: "B群"}})
	if err != nil || valid {
		t.Fatalf("cross-space group valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "peer-in", ChatType: "direct", Name: "私聊"}})
	if err != nil || !valid {
		t.Fatalf("same-space DM peer valid=%t err=%v", valid, err)
	}
	canonical := pipeline.NormalizeDMChannelID("peer-in", "actor", model.ChannelTypeDM)
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: canonical, ChatType: "direct", Name: "私聊"}})
	if err != nil || !valid {
		t.Fatalf("canonical DM valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "peer-out", ChatType: "direct", Name: "外部私聊"}})
	if err != nil || valid {
		t.Fatalf("out-of-space DM peer valid=%t err=%v", valid, err)
	}
	if err := imDB.Exec(`UPDATE group_member SET status = 0 WHERE group_no = 'group-a' AND uid = 'actor'`).Error; err != nil {
		t.Fatal(err)
	}
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}})
	if err != nil || valid {
		t.Fatalf("inactive creator membership valid=%t err=%v", valid, err)
	}
}

func TestSummaryWorkspaceValidateTeamScopeAllowsParticipantUnionAcrossGroups(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB}
	ctx := context.Background()
	participants := []summaryWorkspaceParticipant{{UserID: "member", UserName: "成员"}}

	valid, reason, err := coordinator.validateTeamScope(ctx, []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}, participants)
	if err != nil || !valid || reason != teamScopeReasonNone {
		t.Fatalf("shared group valid=%t reason=%q err=%v", valid, reason, err)
	}
	// Union semantics: member belongs only to group-a and member-b belongs only
	// to group-c. Both are valid participants for the combined source set.
	valid, reason, err = coordinator.validateTeamScope(ctx,
		[]summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}, {ChatID: "group-c", ChatType: "group", Name: "C群"}},
		[]summaryWorkspaceParticipant{{UserID: "member"}, {UserID: "member-b"}},
	)
	if err != nil || !valid || reason != teamScopeReasonNone {
		t.Fatalf("split-membership pair must pass under union semantics: valid=%t reason=%q err=%v", valid, reason, err)
	}
	// A participant outside every selected group is rejected.
	valid, reason, err = coordinator.validateTeamScope(ctx,
		[]summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}, {ChatID: "group-c", ChatType: "group", Name: "C群"}},
		[]summaryWorkspaceParticipant{{UserID: "member"}, {UserID: "outside"}},
	)
	if err != nil || valid || reason != teamScopeReasonParticipantMissing {
		t.Fatalf("participant outside group union valid=%t reason=%q err=%v", valid, reason, err)
	}
	valid, reason, err = coordinator.validateTeamScope(ctx, []summaryWorkspaceChannel{{ChatID: "peer-in", ChatType: "direct", Name: "私聊"}}, participants)
	if err != nil || valid || reason != teamScopeReasonSourceType {
		t.Fatalf("DM team scope valid=%t reason=%q err=%v", valid, reason, err)
	}
	valid, reason, err = coordinator.validateTeamScope(ctx, []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}, []summaryWorkspaceParticipant{{UserID: "inactive"}})
	if err != nil || valid || reason != teamScopeReasonParticipantMissing {
		t.Fatalf("inactive group member valid=%t reason=%q err=%v", valid, reason, err)
	}
	tooMany := make([]summaryWorkspaceChannel, maxSummaryWorkspaceSelectedChannels+1)
	for index := range tooMany {
		tooMany[index] = summaryWorkspaceChannel{ChatID: fmt.Sprintf("group-%d", index), ChatType: "group", Name: "群"}
	}
	valid, reason, err = coordinator.validateTeamScope(ctx, tooMany, participants)
	if err != nil || valid || reason != teamScopeReasonSourceLimit {
		t.Fatalf("over-limit valid=%t reason=%q err=%v", valid, reason, err)
	}
}

// seedExtraMembership adds one active group_member row mid-test.
func seedExtraMembership(t *testing.T, db *gorm.DB, groupNo, uid string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO group_member (group_no, uid, status, is_deleted, role) VALUES (?, ?, 1, 0, 0)`, groupNo, uid).Error; err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}
