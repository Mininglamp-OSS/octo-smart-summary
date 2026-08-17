package agent

import (
	"context"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
)

// loadRunSpecGuidance returns the SummarySpec-derived prompt guidance for the
// run in context (SS-06), or "" when V2 is off, there is no run, or no spec is
// persisted. Injected into the Map and Reduce prompts so the model that writes
// the summaries sees the user's requirements instead of a generic prompt.
//
// Returns "" on any miss so the caller's off path keeps the exact legacy prompt
// — this is the flag gate for the generation-quality change.
func loadRunSpecGuidance(ctx context.Context) string {
	if !SummaryV2Enabled() {
		return ""
	}
	runID, _ := ctx.Value(ContextKeyRunID).(string)
	uid, _ := ctx.Value(ContextKeyUID).(string)
	if runID == "" || uid == "" {
		return ""
	}
	db, _, _, _ := GetSummaryDeps()
	if db == nil {
		return ""
	}
	spec, found, err := summaryrun.NewStore(db).GetLatestSpec(ctx, uid, runID)
	if err != nil || !found {
		return ""
	}
	return spec.BuildPromptGuidance()
}
