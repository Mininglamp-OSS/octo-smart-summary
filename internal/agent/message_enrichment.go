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
//   - pipeline.FetchMessagesFromChannel only fills 5 fields (SenderUID, ChannelID,
//     Timestamp, SendTime, Content) to keep pipeline focused on message retrieval
//   - tool layer already has accessibleChannels (from security check) containing
//     ChannelName and ChannelType
//   - batch user resolution follows existing patterns (worker/processor.go:888,
//     pipeline/resolve_channel.go:604)
//   - no circular dependency risk, cleaner separation of concerns
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

	// Batch query user table — select robot flag alongside name so a single
	// roundtrip covers both fields. SenderIsBot is filled from the union of
	// (user.robot=1) OR (uid IN robot table), keeping judgement identical to
	// the candidates API (internal/api/handler/candidates.go) and the worker
	// path (worker.batchResolveUserNames). Two IM sources are unioned so
	// system bots (BotFather etc.) with robot=0 on the user row are still
	// flagged via their robot table membership.
	nameMap := make(map[string]string)
	botSet := make(map[string]bool)
	if len(uids) > 0 && imDB != nil {
		type userRow struct {
			UID   string `gorm:"column:uid"`
			Name  string `gorm:"column:name"`
			Robot int    `gorm:"column:robot"`
		}
		var rows []userRow
		if err := imDB.WithContext(ctx).Raw(
			"SELECT uid, name, robot FROM `user` WHERE uid IN ?",
			uids,
		).Scan(&rows).Error; err != nil {
			log.Printf("[agent] enrich: batch resolve user names failed: %v", err)
		} else {
			for _, r := range rows {
				// Bot classification must NOT depend on name presence:
				// programmatically provisioned bots can have an empty
				// name row but still be robot=1. Gating botSet on the
				// name filter caused an agent-vs-worker mismatch (see
				// PR #237 review by Jerry-Xin, P1 blocking).
				if r.Name != "" {
					nameMap[r.UID] = r.Name
				}
				if r.Robot == 1 {
					botSet[r.UID] = true
				}
			}
			log.Printf("[agent] enrich: resolved %d/%d user names", len(nameMap), len(uids))
		}

		// Union with the robot table so system bots with robot=0 on the
		// user row are still flagged. Non-fatal: on error we keep the
		// partial botSet from the user query above.
		var robotIDs []string
		if err := imDB.WithContext(ctx).Raw(
			"SELECT robot_id FROM `robot` WHERE robot_id IN ?",
			uids,
		).Scan(&robotIDs).Error; err != nil {
			log.Printf("[agent] enrich: batch resolve robot table failed: %v", err)
		} else {
			for _, rid := range robotIDs {
				botSet[rid] = true
			}
		}
	}

	// 3. Populate all fields on each message
	for i := range messages {
		// SenderName from batch-resolved map
		if name, ok := nameMap[messages[i].SenderUID]; ok {
			messages[i].SenderName = name
		}
		// SenderIsBot from batch-resolved bot set (missing UID => false,
		// same default as candidates.go's exclusion behaviour).
		messages[i].SenderIsBot = botSet[messages[i].SenderUID]
		// SourceName and ChannelType from accessibleChannels
		messages[i].SourceName = channelName
		messages[i].ChannelType = channelType
	}

	log.Printf("[agent] enrich: populated metadata for %d messages (channel=%s, source=%s, type=%d, bots=%d)",
		len(messages), targetChannelID, channelName, channelType, len(botSet))
}
