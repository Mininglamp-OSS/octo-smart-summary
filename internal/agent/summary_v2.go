package agent

import (
	"os"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

// SS-03 rollout flag. AGENT_SUMMARY_V2_MODE gates the SummaryRun / SummarySpec
// persistence path so the whole Stage-2 contract can ship dark and be enabled
// per-environment without touching the live answer.
//
//   - off    (default): byte-identical to pre-SS-03 behavior. No run is created,
//     no new query runs, agent_chat takes exactly the old path.
//   - shadow: currently identical to on, including user-visible profile guards
//     and frozen-manifest behavior; the distinct name is reserved for later.
//   - on:     enables the same Stage-2 behavior as shadow today.
//
// Kept as an env reader mirroring HistoryWindow() so wiring it in needs no change
// to NewAgentChatHandler's signature or router.go — off therefore stays a true
// no-op. config.Config surfaces the same value (AgentSummaryV2Mode); both read
// paths go through config.NormalizeAgentSummaryV2Mode so they can never disagree.
const (
	V2ModeOff    = config.AgentSummaryV2Off
	V2ModeShadow = config.AgentSummaryV2Shadow
	V2ModeOn     = config.AgentSummaryV2On

	// DefaultSummaryV2Mode is the safe default: the new path is dark until an
	// operator explicitly opts in.
	DefaultSummaryV2Mode = V2ModeOff
)

// SummaryV2Mode reads AGENT_SUMMARY_V2_MODE and normalizes it through the
// single shared normalizer (fail safe, never fail into the new path).
func SummaryV2Mode() string {
	return config.NormalizeAgentSummaryV2Mode(os.Getenv("AGENT_SUMMARY_V2_MODE"))
}

// SummaryV2Enabled reports whether any non-off mode is active.
func SummaryV2Enabled() bool { return SummaryV2Mode() != V2ModeOff }
