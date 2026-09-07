package agent

import (
	"context"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
)

// recordDeclaredChannels records the final model-declared channel scope on
// the run row, so the finish gate can detect an open-scope under-fetch.
//
// Why this exists: for an open-scope run (no UI channel picker) the Spec pins no
// channels, so the gate's expected-vs-fetched comparison had nothing to compare
// and every such run reported COMPLETE — including one that selected 12
// channels and fetched 2. Discovery candidates are deliberately not recorded:
// only set_summary_scope owns the final selection and therefore the expected
// coverage set.
//
// Best-effort and V2-gated, exactly like the coverage recording in fetch_channel:
// this is observability for the verdict, never a reason to fail a tool call. The
// write uses WithoutCancel for the same reason fetch_channel's does — the losses
// worth recording are often the ones that come with a canceled context.
func recordDeclaredChannels(ctx context.Context, uid string, channelIDs []string) {
	if !SummaryV2Enabled() || uid == "" {
		return
	}
	summaryDB, _, _, _ := GetSummaryDeps()
	if summaryDB == nil {
		return
	}
	runID, _ := ctx.Value(ContextKeyRunID).(string)
	if runID == "" {
		return
	}
	if err := summaryrun.NewStore(summaryDB).SetDeclaredChannels(context.WithoutCancel(ctx), uid, runID, channelIDs); err != nil {
		log.Printf("[agent] record declared channels failed run=%s count=%d: %v", runID, len(channelIDs), err)
	}
}

func channelScopeIDsOf(channels []ChannelScope) []string {
	ids := make([]string, 0, len(channels))
	for _, channel := range channels {
		if channel.ChannelID != "" {
			ids = append(ids, channel.ChannelID)
		}
	}
	return ids
}
