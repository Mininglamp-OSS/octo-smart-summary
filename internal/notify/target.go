package notify

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// deliveryTarget is the resolved destination of a notification.
type deliveryTarget struct {
	ChannelID   string
	ChannelType int // WireChannelDM / WireChannelGroup / WireChannelThread
	// TargetUID is the user the bot must EnsureFriend with before a DM is
	// deliverable. Set only for DM delivery; empty for group/thread回发.
	TargetUID string
	// SpaceID (SummaryTask.SpaceID) is passed to ensureFriend so octo-server can
	// build the space-prefixed whitelist channel (s{spaceID}_{uid}).
	SpaceID string
}

// resolveTarget decides where a terminal-state notification for `task` is sent.
//
// 本期口径 (OCT-4, this phase): we ALWAYS fall back to a DM to the task creator.
// The task carries OriginChannelID/OriginChannelType (model.OriginChannel*:
// 0=Global, 1=Group, 2=Thread, 3=DM), but 原群/thread 回发 is an explicit
// follow-up enhancement and is intentionally NOT depended on here. Whether the
// origin is DM, empty, or Global, we route to ChannelTypePerson with
// channel_id = CreatorID. Group/Thread origins also fall back to a creator DM
// this phase (we do not yet post back into the originating group/thread).
//
// The switch is written out so the future enhancement (route Group/Thread back
// to the origin channel) is a localized change, but every branch currently
// resolves to the DM fallback.
func resolveTarget(task model.SummaryTask) (deliveryTarget, bool) {
	creatorDM := func() (deliveryTarget, bool) {
		if task.CreatorID == "" {
			return deliveryTarget{}, false
		}
		return deliveryTarget{
			ChannelID:   task.CreatorID,
			ChannelType: WireChannelDM,
			TargetUID:   task.CreatorID,
			SpaceID:     task.SpaceID,
		}, true
	}

	switch task.OriginChannelType {
	case model.OriginChannelDM, model.OriginChannelGlobal:
		// DM origin or no/global origin → creator DM (本期默认兜底).
		return creatorDM()
	case model.OriginChannelGroup, model.OriginChannelThread:
		// 原群/thread 回发：后续增强，本期不做依赖 → 退回 creator DM 兜底.
		return creatorDM()
	default:
		return creatorDM()
	}
}
