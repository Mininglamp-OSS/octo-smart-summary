package agent

import (
	"os"
	"strings"
)

// SS-03 rollout flag. AGENT_SUMMARY_V2_MODE gates the SummaryRun / SummarySpec
// persistence path so the whole Stage-2 contract can ship dark and be enabled
// per-environment without touching the live answer.
//
//   - off    (default): byte-identical to pre-SS-03 behavior. No run is created,
//     no new query runs, agent_chat takes exactly the old path.
//   - shadow: create + persist the Run/Spec for observability, but the reply
//     path is unchanged (nothing user-visible depends on it yet).
//   - on:     same as shadow for SS-03; later stages (SS-05+) make the tools read
//     the persisted Spec.
//
// Kept as an env reader mirroring HistoryWindow() so wiring it in needs no change
// to NewAgentChatHandler's signature or router.go — off therefore stays a true
// no-op. config.Config also surfaces the value (AgentSummaryV2Mode) so DEP-01 /
// CONFIGURATION.md have a documented home; both read the same env var.
const (
	V2ModeOff    = "off"
	V2ModeShadow = "shadow"
	V2ModeOn     = "on"

	// DefaultSummaryV2Mode is the safe default: the new path is dark until an
	// operator explicitly opts in.
	DefaultSummaryV2Mode = V2ModeOff
)

// SummaryV2Mode reads AGENT_SUMMARY_V2_MODE, normalizing case/whitespace and
// falling back to off for unset or unrecognized values (fail safe, never fail
// into the new path).
func SummaryV2Mode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_SUMMARY_V2_MODE"))) {
	case V2ModeShadow:
		return V2ModeShadow
	case V2ModeOn:
		return V2ModeOn
	default:
		return V2ModeOff
	}
}

// SummaryV2Enabled reports whether any non-off mode is active.
func SummaryV2Enabled() bool { return SummaryV2Mode() != V2ModeOff }
