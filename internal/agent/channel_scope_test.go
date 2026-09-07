package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestAllowedChannelScopeIsOptInAndExact(t *testing.T) {
	if restricted, allowed := ChannelAllowedByScope(context.Background(), "group-1", 2); restricted || allowed {
		t.Fatalf("unscoped legacy context = (%v, %v), want (false, false)", restricted, allowed)
	}

	ctx := WithAllowedChannelScope(context.Background(), []ChannelScope{
		{ChannelID: "group-1", ChannelType: 2},
	})
	if restricted, allowed := ChannelAllowedByScope(ctx, "group-1", 2); !restricted || !allowed {
		t.Fatalf("selected channel = (%v, %v), want (true, true)", restricted, allowed)
	}
	for _, candidate := range []ChannelScope{
		{ChannelID: "group-2", ChannelType: 2},
		{ChannelID: "group-1", ChannelType: 5},
	} {
		if restricted, allowed := ChannelAllowedByScope(ctx, candidate.ChannelID, candidate.ChannelType); !restricted || allowed {
			t.Fatalf("unselected channel %+v = (%v, %v), want (true, false)", candidate, restricted, allowed)
		}
	}

	empty := WithAllowedChannelScope(context.Background(), nil)
	if restricted, allowed := ChannelAllowedByScope(empty, "group-1", 2); !restricted || allowed {
		t.Fatalf("empty explicit scope = (%v, %v), want (true, false)", restricted, allowed)
	}
}

func TestDiscoverableChannelScopeRequiresDeclarationBeforeRead(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = WithDiscoverableChannelScope(ctx)
	if restricted, allowed := ChannelAllowedByScope(ctx, "group-1", model.ChannelTypeGroup); !restricted || allowed {
		t.Fatalf("open discovery must still start with a deny-all read scope, got (%v,%v)", restricted, allowed)
	}

	// This models an all-chat narrowing result: returning the full candidate set
	// is still an explicit, trusted scope decision and must authorize every item.
	all := []pipeline.ChannelInfo{
		{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup, ChannelName: "项目群"},
		{ChannelID: "group-2", ChannelType: model.ChannelTypeGroup, ChannelName: "产品群"},
	}
	if !AuthorizeDiscoveredChannels(ctx, all) {
		t.Fatal("discoverable scope rejected trusted discovery result")
	}
	for _, channel := range all {
		if restricted, allowed := ChannelAllowedByScope(ctx, channel.ChannelID, channel.ChannelType); !restricted || allowed {
			t.Fatalf("undeclared channel %s = (%v,%v), want (true,false)", channel.ChannelID, restricted, allowed)
		}
	}
	if err := DeclareWorkspaceScopeChange(ctx, WorkspaceScopeChange{SourceMode: WorkspaceSourceReplace, Channels: []ChannelScope{
		{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup},
		{ChannelID: "group-2", ChannelType: model.ChannelTypeGroup},
	}}); err != nil {
		t.Fatalf("declare discovered scope: %v", err)
	}
	if got := AllowedChannelScopes(ctx); len(got) != 2 {
		t.Fatalf("declared effective scope = %#v, want 2 channels", got)
	}
}

func TestDiscoverableChannelScopeCanDeclareAcrossMultipleDiscoveryResults(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = WithDiscoverableChannelScope(ctx)
	AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup}})
	AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-2", ChannelType: model.ChannelTypeGroup}})

	if err := DeclareWorkspaceScopeChange(ctx, WorkspaceScopeChange{SourceMode: WorkspaceSourceReplace, Channels: []ChannelScope{
		{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup},
		{ChannelID: "group-2", ChannelType: model.ChannelTypeGroup},
	}}); err != nil {
		t.Fatalf("declare union of discovery results: %v", err)
	}
	for _, channelID := range []string{"group-1", "group-2"} {
		if restricted, allowed := ChannelAllowedByScope(ctx, channelID, model.ChannelTypeGroup); !restricted || !allowed {
			t.Fatalf("declared channel %s = (%v,%v), want retained union", channelID, restricted, allowed)
		}
	}
}

func TestDeclaredChannelScopeFreezesDiscovery(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = WithDiscoverableChannelScope(ctx, []ChannelScope{{ChannelID: "group-a", ChannelType: model.ChannelTypeGroup}})
	AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-b", ChannelType: model.ChannelTypeGroup}})
	if err := DeclareWorkspaceScopeChange(ctx, WorkspaceScopeChange{
		SourceMode: WorkspaceSourceReplace,
		Channels:   []ChannelScope{{ChannelID: "group-b", ChannelType: model.ChannelTypeGroup}},
	}); err != nil {
		t.Fatalf("declare scope: %v", err)
	}
	if AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-c", ChannelType: model.ChannelTypeGroup}}) {
		t.Fatal("discovery grant succeeded after final scope declaration")
	}
	if restricted, allowed := ChannelAllowedByScope(ctx, "group-c", model.ChannelTypeGroup); !restricted || allowed {
		t.Fatalf("post-declaration channel = (%v,%v), want denied", restricted, allowed)
	}
}

func TestClosedChannelScopeCannotBeExpandedByDiscovery(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = WithAllowedChannelScope(ctx, []ChannelScope{{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup}})
	if AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-2", ChannelType: model.ChannelTypeGroup}}) {
		t.Fatal("closed UI scope must reject discovery grants")
	}
	visible := RestrictDiscoveredChannels(ctx, []pipeline.ChannelInfo{
		{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup},
		{ChannelID: "group-2", ChannelType: model.ChannelTypeGroup},
	})
	if len(visible) != 1 || visible[0].ChannelID != "group-1" {
		t.Fatalf("closed-scope discovery leaked channels: %#v", visible)
	}
}

func TestClosedDiscoveryFilterCanonicalizesDMIDs(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "self-user")
	ctx = WithAllowedChannelScope(ctx, []ChannelScope{{ChannelID: "peer-user", ChannelType: model.ChannelTypeDM}})
	visible := RestrictDiscoveredChannels(ctx, []pipeline.ChannelInfo{{
		ChannelID: "self-user@peer-user", ChannelType: model.ChannelTypeDM,
	}})
	if len(visible) != 1 {
		t.Fatalf("logical/canonical DM comparison dropped selected channel: %#v", visible)
	}
}

func TestAllowedTimeRangeOverridesModelArguments(t *testing.T) {
	trustedStart := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	trustedEnd := time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)
	ctx := WithAllowedTimeRange(context.Background(), trustedStart, trustedEnd)
	gotStart, gotEnd := ResolveAllowedTimeRange(ctx, time.Time{}, time.Now())
	if !gotStart.Equal(trustedStart) || !gotEnd.Equal(trustedEnd) {
		t.Fatalf("resolved range = %s..%s, want %s..%s", gotStart, gotEnd, trustedStart, trustedEnd)
	}
}

func TestAllowedChannelScopeCanonicalizesLogicalDMID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "self-user")
	canonical := pipeline.NormalizeDMChannelID("peer-user", "self-user", model.ChannelTypeDM)

	ctx = WithAllowedChannelScope(ctx, []ChannelScope{{ChannelID: canonical, ChannelType: model.ChannelTypeDM}})
	if restricted, allowed := ChannelAllowedByScope(ctx, "peer-user", model.ChannelTypeDM); !restricted || !allowed {
		t.Fatalf("logical DM id did not match selected canonical id: (%v, %v), canonical=%q", restricted, allowed, canonical)
	}
}

func TestChannelReadToolsRejectOutsideSelectedScopeBeforeLookup(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = WithAllowedChannelScope(ctx, []ChannelScope{{ChannelID: "group-1", ChannelType: 2}})

	_, fetch := FetchChannelTool()
	if _, err := fetch(ctx, []byte(`{"channel_id":"group-2","time_start":"2026-08-20T00:00:00Z","time_end":"2026-08-27T00:00:00Z"}`)); err == nil || !strings.Contains(err.Error(), "channel_type is required") {
		t.Fatalf("missing channel type must remain a repairable argument error, got %v", err)
	}
	if _, err := fetch(ctx, []byte(`{"channel_id":"group-2","channel_type":2,"time_start":"2026-08-20T00:00:00Z","time_end":"2026-08-27T00:00:00Z"}`)); err == nil || !strings.Contains(err.Error(), "outside the selected summary scope") {
		t.Fatalf("fetch outside scope error = %v", err)
	} else {
		var outside *ErrChannelOutsideSelectedScope
		if !errors.As(err, &outside) || outside.ChannelID != "group-2" || outside.ChannelType != 2 {
			t.Fatalf("fetch error type = %T %+v", err, err)
		}
	}

	_, peek := PeekChannelTool()
	if _, err := peek(ctx, []byte(`{"channel_id":"group-1","channel_type":5}`)); err == nil || !strings.Contains(err.Error(), "outside the selected summary scope") {
		t.Fatalf("peek with mismatched type error = %v", err)
	}
}
