package agent

import (
	"context"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

// channelIDsOf projects channel ids out of a discovery result.
func channelIDsOf(channels []pipeline.ChannelInfo) []string {
	ids := make([]string, 0, len(channels))
	for _, ch := range channels {
		if ch.ChannelID != "" {
			ids = append(ids, ch.ChannelID)
		}
	}
	return ids
}

// recordDiscoveredChannels unions the channels a discovery tool just surfaced
// onto the run row, so the finish gate can detect an open-scope under-fetch.
//
// Why this exists: for an open-scope run (no UI channel picker) the Spec pins no
// channels, so the gate's expected-vs-fetched comparison had nothing to compare
// and every such run reported COMPLETE — including one that listed 12 channels
// and fetched 2. Discovery is the only place that knows what "everything in
// scope" meant.
//
// Best-effort and V2-gated, exactly like the coverage recording in fetch_channel:
// this is observability for the verdict, never a reason to fail a tool call. The
// write uses WithoutCancel for the same reason fetch_channel's does — the losses
// worth recording are often the ones that come with a canceled context.
func recordDiscoveredChannels(ctx context.Context, summaryDB *gorm.DB, uid string, channelIDs []string) {
	if !SummaryV2Enabled() || summaryDB == nil || uid == "" || len(channelIDs) == 0 {
		return
	}
	runID, _ := ctx.Value(ContextKeyRunID).(string)
	if runID == "" {
		return
	}
	if err := summaryrun.NewStore(summaryDB).RecordDiscoveredChannels(context.WithoutCancel(ctx), uid, runID, channelIDs); err != nil {
		log.Printf("[agent] record discovered channels failed run=%s count=%d: %v", runID, len(channelIDs), err)
	}
}
