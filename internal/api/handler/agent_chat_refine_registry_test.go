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
// or a change to the enforcement switch — would have failed no test at all.
func TestRefineStripsFetchToolsGatesOnAllThreeTerms(t *testing.T) {
	confident := agent.RefineRoute{Intent: agent.RefineRewrite, HardNoFetch: true}
	soft := agent.RefineRoute{Intent: agent.RefineRewrite, HardNoFetch: false}
	augment := agent.RefineRoute{Intent: agent.RefineAugment, Fetch: true}

	enableHardStrip(t)
	if !refineStripsFetchTools(true, confident) {
		t.Error("refine + confident rewrite + enforcement on: must strip the fetch tools")
	}
	// Every other combination must keep the tools even with enforcement ON.
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

// TestRefineStripIsOffByDefault pins the rollout contract: enforcement is opt-in
// at runtime, and `shadow` stays a true observe-only stage (classify + log,
// never strip). A confident-rewrite verdict is computed identically in every
// case below — only whether it is ACTED ON changes.
func TestRefineStripIsOffByDefault(t *testing.T) {
	confident := agent.RefineRoute{Intent: agent.RefineRewrite, HardNoFetch: true}

	for _, tc := range []struct {
		name, mode, strip string
	}{
		{"unset env with v2 on", "on", ""},
		{"explicitly disabled", "on", "false"},
		{"malformed value fails safe", "on", "yes-please"},
		{"shadow is observe-only even when asked to strip", "shadow", "true"},
		{"off mode never strips", "off", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_SUMMARY_V2_MODE", tc.mode)
			t.Setenv(agent.HardStripEnvVar, tc.strip)
			if refineStripsFetchTools(true, confident) {
				t.Error("the tool strip must not fire; it is opt-in and requires mode=on")
			}
		})
	}
}

// enableHardStrip turns enforcement on for one test, restored automatically.
func enableHardStrip(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_SUMMARY_V2_MODE", agent.V2ModeOn)
	t.Setenv(agent.HardStripEnvVar, "true")
}

// TestRefineStripReachesTheRunner closes the loop the previous tests cannot:
// that the decision actually changes the toolset the model receives.
func TestRefineStripReachesTheRunner(t *testing.T) {
	h := &AgentChatHandler{}
	confident := agent.RefineRoute{Intent: agent.RefineRewrite, HardNoFetch: true}

	enableHardStrip(t)
	stripped, _, err := h.buildRunnerForProfile("summary_refine", "u1", "s1", refineStripsFetchTools(true, confident))
	if err != nil {
		t.Fatalf("build stripped runner: %v", err)
	}
	if schemaNameSet(stripped.ToolSchemas())["fetch_channel"] {
		t.Error("enforcement is on and the verdict is a confident rewrite: fetch_channel must be gone")
	}

	kept, _, err := h.buildRunnerForProfile("summary_refine", "u1", "s1", false)
	if err != nil {
		t.Fatalf("build full runner: %v", err)
	}
	if !schemaNameSet(kept.ToolSchemas())["fetch_channel"] {
		t.Error("a non-stripped summary_refine runner must always keep fetch_channel")
	}

	// And with enforcement off — the default — the same verdict leaves the
	// toolset intact, which is what makes the observation window safe.
	t.Setenv(agent.HardStripEnvVar, "false")
	observeOnly, _, err := h.buildRunnerForProfile("summary_refine", "u1", "s1", refineStripsFetchTools(true, confident))
	if err != nil {
		t.Fatalf("build observe-only runner: %v", err)
	}
	if !schemaNameSet(observeOnly.ToolSchemas())["fetch_channel"] {
		t.Error("with enforcement off the verdict is recorded only; fetch_channel must remain")
	}
}
