package agent

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestWorkspaceScopeSchemasHidePlanningToolsForClosedScope(t *testing.T) {
	reg, err := BuildRegistry([]string{
		"get_current_time", "list_channels", "set_summary_scope",
		"fetch_channel", "summarize_chunk", "emit_summary_response",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithAllowedChannelScope(context.Background(), []ChannelScope{{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup}})
	got := schemaNames(workspaceScopeSchemas(ctx, reg.Schemas(), reg))
	for _, hidden := range []string{"get_current_time", "list_channels", "set_summary_scope"} {
		if got[hidden] {
			t.Fatalf("locked scope still exposes %q: %#v", hidden, got)
		}
	}
	for _, visible := range []string{"fetch_channel", "summarize_chunk", "emit_summary_response"} {
		if !got[visible] {
			t.Fatalf("locked scope hid execution tool %q: %#v", visible, got)
		}
	}
}

func TestWorkspaceScopeSchemasLockAfterDeclaration(t *testing.T) {
	reg, err := BuildRegistry([]string{"list_channels", "set_summary_scope", "fetch_channel"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = WithDiscoverableChannelScope(ctx)
	if got := schemaNames(workspaceScopeSchemas(ctx, reg.Schemas(), reg)); !got["set_summary_scope"] || !got["list_channels"] {
		t.Fatalf("open scope lost planning tools: %#v", got)
	}
	AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup}})
	if err := DeclareWorkspaceScopeChange(ctx, WorkspaceScopeChange{
		SourceMode: WorkspaceSourceReplace,
		Channels:   []ChannelScope{{ChannelID: "group-1", ChannelType: model.ChannelTypeGroup}},
	}); err != nil {
		t.Fatal(err)
	}
	got := schemaNames(workspaceScopeSchemas(ctx, reg.Schemas(), reg))
	if got["set_summary_scope"] || got["list_channels"] || !got["fetch_channel"] {
		t.Fatalf("declared scope schemas = %#v", got)
	}
}

func schemaNames(schemas []Tool) map[string]bool {
	names := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		names[schema.Function.Name] = true
	}
	return names
}
