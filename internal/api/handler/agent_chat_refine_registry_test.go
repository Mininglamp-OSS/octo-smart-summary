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

// TestRefineStripsFetchToolsGatesOnAllThreeTerms pins the #206 P2-1: the strip
// decision is the behavioural change of this PR and had no test of its own.
//
// Nothing asserted that a HardNoFetch=true route actually reaches
// buildRunnerForProfile with refineNoFetch=true, so an edit to the conjunction —
// or a revert of refineHardStripEnforced — would have failed no test at all.
func TestRefineStripsFetchToolsGatesOnAllThreeTerms(t *testing.T) {
	confident := agent.RefineRoute{Intent: agent.RefineRewrite, HardNoFetch: true}
	soft := agent.RefineRoute{Intent: agent.RefineRewrite, HardNoFetch: false}
	augment := agent.RefineRoute{Intent: agent.RefineAugment, Fetch: true}

	if got := refineStripsFetchTools(true, confident); got != refineHardStripEnforced {
		t.Errorf("refine + confident rewrite: strip = %v, want %v (the enforcement constant)", got, refineHardStripEnforced)
	}
	// Every other combination must keep the tools regardless of the constant.
	for _, tc := range []struct {
		name         string
		refineActive bool
		route        agent.RefineRoute
	}{
		{"not a refine turn", false, confident},
		{"soft rewrite keeps its tools so a mis-read can recover", true, soft},
		{"an augment route fetches by definition", true, augment},
	} {
		if refineStripsFetchTools(tc.refineActive, tc.route) {
			t.Errorf("%s: must NOT strip the fetch tools", tc.name)
		}
	}
}

// TestRefineStripReachesTheRunner closes the loop the previous test cannot: that
// the decision actually changes the toolset the model receives.
func TestRefineStripReachesTheRunner(t *testing.T) {
	h := &AgentChatHandler{}
	confident := agent.RefineRoute{Intent: agent.RefineRewrite, HardNoFetch: true}

	stripped, _, err := h.buildRunnerForProfile("summary_refine", "u1", "s1", refineStripsFetchTools(true, confident))
	if err != nil {
		t.Fatalf("build stripped runner: %v", err)
	}
	kept, _, err := h.buildRunnerForProfile("summary_refine", "u1", "s1", false)
	if err != nil {
		t.Fatalf("build full runner: %v", err)
	}

	strippedHasFetch := schemaNameSet(stripped.ToolSchemas())["fetch_channel"]
	if strippedHasFetch != !refineHardStripEnforced {
		t.Errorf("confident-rewrite runner has fetch_channel = %v; with refineHardStripEnforced=%v it must be %v",
			strippedHasFetch, refineHardStripEnforced, !refineHardStripEnforced)
	}
	if !schemaNameSet(kept.ToolSchemas())["fetch_channel"] {
		t.Error("a non-stripped summary_refine runner must always keep fetch_channel")
	}
}
