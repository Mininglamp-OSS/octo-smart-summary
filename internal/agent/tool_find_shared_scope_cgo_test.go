//go:build cgo

package agent

import (
	"context"
	"encoding/json"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// TestFindSharedChannelsOnlyRecordsDeclaredScope pins the separation between
// discovery candidates and the final fetchable/coverage scope at the call site.
//
// It has to be pinned here and not on IntersectParticipantChannels: mutation
// verification showed the guard in tool_find_shared.go could be deleted with
// every other test still green, which is the same silent-revert hole the round-6
// 摘要 removal had.
//
// IntersectParticipantChannels returns creatorChannels UNCHANGED when
// participant_uids is empty, and creatorChannels is the same
// pipeline.GetUserChannels call list_channels makes — so {"participant_uids": []}
// recorded every visible channel as the run's committed scope, re-entering
// through a sibling handler the exact input list_channels was deliberately
// stopped from recording two rounds ago. `required` in the tool schema is
// advisory metadata sent to the model, not validation.
func TestFindSharedChannelsOnlyRecordsDeclaredScope(t *testing.T) {
	imDB := setupAgentImDB(t)
	// user-1 can see three channels.
	for _, id := range []string{"chan-A", "chan-B", "chan-C"} {
		imDB.Exec(`INSERT INTO "group" (group_no, name, space_id, status, creator) VALUES (?, ?, 'test-space', 1, 'user-1')`, id, id)
		imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES (?, 'user-1', 0, 0)`, id)
	}

	summaryDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
	}
	if err := summaryDB.AutoMigrate(&model.AgentSummaryRun{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	SetSummaryDeps(summaryDB, imDB, nil, config.Config{})
	defer SetSummaryDeps(nil, nil, nil, config.Config{})
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	store := summaryrun.NewStore(summaryDB)
	run, _, err := store.CreateOrGetRun(context.Background(), "user-1", "sess-1", "req-1", model.ScopePolicyOpen)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	openContext := func() context.Context {
		ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
		ctx = context.WithValue(ctx, ContextKeyRunID, run.RunID)
		ctx = WithWorkspaceSpaceID(ctx, "test-space")
		return WithDiscoverableChannelScope(ctx)
	}

	_, handler := FindSharedChannelsTool()

	discovered := func() []string {
		t.Helper()
		var got model.AgentSummaryRun
		if err := summaryDB.Where("run_id = ?", run.RunID).First(&got).Error; err != nil {
			t.Fatalf("reload run: %v", err)
		}
		if got.DiscoveredChannels == "" {
			return nil
		}
		var ids []string
		if err := json.Unmarshal([]byte(got.DiscoveredChannels), &ids); err != nil {
			t.Fatalf("decode discovered_channels %q: %v", got.DiscoveredChannels, err)
		}
		return ids
	}

	t.Run("empty participant list records nothing", func(t *testing.T) {
		ctx := openContext()
		if _, err := handler(ctx, json.RawMessage(`{"participant_uids": []}`)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if err := DeclareWorkspaceScopeChange(ctx, WorkspaceScopeChange{SourceMode: WorkspaceSourceReplace, Channels: []ChannelScope{{ChannelID: "chan-A", ChannelType: model.ChannelTypeGroup}}}); err == nil {
			t.Fatal("empty participant query authorized the full visible surface")
		}
	})

	t.Run("omitted participant list records nothing", func(t *testing.T) {
		ctx := openContext()
		if _, err := handler(ctx, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if err := DeclareWorkspaceScopeChange(ctx, WorkspaceScopeChange{SourceMode: WorkspaceSourceReplace, Channels: []ChannelScope{{ChannelID: "chan-A", ChannelType: model.ChannelTypeGroup}}}); err == nil {
			t.Fatal("omitted participant query authorized the full visible surface")
		}
	})

	t.Run("a real intersection is recorded only after declaration", func(t *testing.T) {
		// user-2 shares only chan-B.
		imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES ('chan-B', 'user-2', 0, 0)`)

		ctx := openContext()
		if _, err := handler(ctx, json.RawMessage(`{"participant_uids": ["user-2"]}`)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if got := discovered(); len(got) != 0 {
			t.Fatalf("discovery alone recorded final coverage scope: %v", got)
		}
		_, setScope := SetSummaryScopeTool()
		if _, err := setScope(ctx, json.RawMessage(`{"source_mode":"replace","channels":[{"channel_id":"chan-B","channel_type":2}]}`)); err != nil {
			t.Fatalf("declare shared channel: %v", err)
		}
		got := discovered()
		if len(got) == 0 {
			t.Fatal("declared shared intersection must be recorded as final scope")
		}
		for _, id := range got {
			if id != "chan-B" {
				t.Fatalf("discovered_channels = %v, want only the shared channel", got)
			}
		}
	})
}
