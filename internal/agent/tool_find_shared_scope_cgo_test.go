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

// TestFindSharedChannelsDoesNotRecordScopeWithoutParticipants pins round-7 P1-2
// AT THE CALL SITE.
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
//
// Because the stored union is monotonic, one such call poisons the rest of the
// run: every later clean single-channel fetch still reports the other visible
// channels as never-fetched gaps, shipped to clients via SS-11.
func TestFindSharedChannelsDoesNotRecordScopeWithoutParticipants(t *testing.T) {
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

	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = context.WithValue(ctx, ContextKeyRunID, run.RunID)

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
		if _, err := handler(ctx, json.RawMessage(`{"participant_uids": []}`)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if got := discovered(); len(got) != 0 {
			t.Fatalf("discovered_channels = %v, want empty — with no participants this is the full visible surface, not scope", got)
		}
	})

	t.Run("omitted participant list records nothing", func(t *testing.T) {
		if _, err := handler(ctx, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if got := discovered(); len(got) != 0 {
			t.Fatalf("discovered_channels = %v, want empty", got)
		}
	})

	t.Run("a real intersection is still recorded", func(t *testing.T) {
		// user-2 shares only chan-B.
		imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES ('chan-B', 'user-2', 0, 0)`)

		if _, err := handler(ctx, json.RawMessage(`{"participant_uids": ["user-2"]}`)); err != nil {
			t.Fatalf("handler: %v", err)
		}
		got := discovered()
		if len(got) == 0 {
			t.Fatal("a genuine intersection must still be recorded as scope, got nothing")
		}
		for _, id := range got {
			if id != "chan-B" {
				t.Fatalf("discovered_channels = %v, want only the shared channel", got)
			}
		}
	})
}
