package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
)

func schemaNameSet(schemas []agent.Tool) map[string]bool {
	m := make(map[string]bool, len(schemas))
	for _, s := range schemas {
		m[s.Function.Name] = true
	}
	return m
}

// TestRefineRewriteRegistryExcludesFetch verifies SS-08b: a confident-rewrite
// registry physically omits the data-fetching tools, so a pure rewrite cannot
// pull new messages ("纯格式零 fetch"), while the full summary registry keeps them.
func TestRefineRewriteRegistryExcludesFetch(t *testing.T) {
	h := &AgentChatHandler{}

	rewriteReg, err := h.buildRegistryWithUID("u1", "s1", refineRewriteToolNames)
	if err != nil {
		t.Fatalf("build rewrite registry: %v", err)
	}
	fullReg, err := h.buildSummaryRegistryWithUID("u1", "s1")
	if err != nil {
		t.Fatalf("build full registry: %v", err)
	}

	rewriteNames := schemaNameSet(rewriteReg.Schemas())
	fullNames := schemaNameSet(fullReg.Schemas())

	// Fetch/discovery tools must be ABSENT from the rewrite registry, PRESENT in full.
	forbidden := []string{
		"fetch_channel", "peek_channel", "search_messages", "filter_relevant",
		"list_channels", "narrow_channels_by_topic", "find_shared_channels",
	}
	for _, name := range forbidden {
		if rewriteNames[name] {
			t.Errorf("rewrite registry must NOT contain %q", name)
		}
		if !fullNames[name] {
			t.Errorf("full summary registry SHOULD contain %q", name)
		}
	}

	// Time + local summarize/merge tools remain available for rewrite.
	for _, name := range []string{"get_current_time", "extract_time_range", "summarize_chunk", "merge_summaries"} {
		if !rewriteNames[name] {
			t.Errorf("rewrite registry should keep %q", name)
		}
	}
}
