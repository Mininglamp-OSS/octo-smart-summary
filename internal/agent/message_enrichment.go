package agent

import (
	"context"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

// enrichMessagesWithMetadata populates SenderName, SourceName, and ChannelType
// on fetched messages. This fixes citation metadata loss (SUM-46 Blocker A).
//
// Rationale for tool-layer enrichment:
// - pipeline.FetchMessagesFromChannel only fills 5 fields (SenderUID, ChannelID,
//   Timestamp, SendTime, Content) to keep pipeline focused on message retrieval
// - tool layer already has accessibleChannels (from security check) containing
//   ChannelName and ChannelType
// - batch user resolution follows existing patterns (worker/processor.go:888,
//   pipeline/resolve_channel.go:604)
// - no circular dependency risk, cleaner separation of concerns
func enrichMessagesWithMetadata(
	ctx context.Context,
	messages []pipeline.Message,
	targetChannelID string,
	accessibleChannels []pipeline.ChannelInfo,
	imDB *gorm.DB,
) {
	if len(messages) == 0 {
		return
	}

	// 1. Find channel metadata from accessibleChannels (already queried for auth)
	var channelName string
	var channelType int
	for _, ch := range accessibleChannels {
		if ch.ChannelID == targetChannelID {
			channelName = ch.ChannelName
			channelType = ch.ChannelType
			break
		}
	}

	// 2. Batch resolve user names (N+1 prevention)
	// Collect unique UIDs
	uidSet := make(map[string]bool)
	for _, msg := range messages {
		if msg.SenderUID != "" {
			uidSet[msg.SenderUID] = true
		}
	}

	var uids []string
	for uid := range uidSet {
		uids = append(uids, uid)
	}

	// Batch query user table
	nameMap := make(map[string]string)
	if len(uids) > 0 && imDB != nil {
		type userRow struct {
			UID  string `gorm:"column:uid"`
			Name string `gorm:"column:name"`
		}
		var rows []userRow
		if err := imDB.WithContext(ctx).Raw(
			"SELECT uid, name FROM `user` WHERE uid IN ? AND name != ''",
			uids,
		).Scan(&rows).Error; err != nil {
			log.Printf("[agent] enrich: batch resolve user names failed: %v", err)
		} else {
			for _, r := range rows {
				nameMap[r.UID] = r.Name
			}
			log.Printf("[agent] enrich: resolved %d/%d user names", len(nameMap), len(uids))
		}
	}

	// 3. Populate all three missing fields on each message
	for i := range messages {
		// SenderName from batch-resolved map
		if name, ok := nameMap[messages[i].SenderUID]; ok {
			messages[i].SenderName = name
		}
		// SourceName and ChannelType from accessibleChannels
		messages[i].SourceName = channelName
		messages[i].ChannelType = channelType
	}

	log.Printf("[agent] enrich: populated metadata for %d messages (channel=%s, source=%s, type=%d)",
		len(messages), targetChannelID, channelName, channelType)
}
