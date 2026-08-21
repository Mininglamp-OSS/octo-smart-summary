package agent

import (
	"os"
	"strconv"
	"strings"

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

// HardStripEnvVar is the runtime kill switch for the SS-08b tool strip.
const HardStripEnvVar = "AGENT_SUMMARY_HARD_STRIP"

// RefineHardStripEnabled reports whether a HardNoFetch verdict may be ACTED ON
// (fetch tools physically removed) rather than merely computed and logged.
//
// Default false, and deliberately an ENV read rather than a compile-time const:
// the strip is the one decision in this contract that a runtime mistake cannot
// undo — buildRunnerForProfile removes list/narrow/find/peek/fetch/search/filter
// outright, so a misclassified request for new data becomes impossible to satisfy
// with no recovery path, and it is silent (fetch_expected=0 on the rewrite path,
// so the finish gate has nothing to flag). Reverting it must not require a
// rebuild.
//
// It additionally requires mode == on, so `shadow` stays a true observe-only
// stage: classify + persist + log tools_stripped=, never strip. That is the
// rollout order this work committed to — measure the false-positive rate on real
// traffic, then flip — rather than arguing it from a pinned example set. Three
// consecutive review rounds each found a NEW systematic false-positive family
// that the pinned suite did not predict, which is the empirical case for keeping
// the switch off by default.
func RefineHardStripEnabled() bool {
	if SummaryV2Mode() != V2ModeOn {
		return false
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(HardStripEnvVar)))
	return err == nil && enabled
}
