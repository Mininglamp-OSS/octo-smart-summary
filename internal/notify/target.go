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

// resolveTargets decides where a terminal-state notification for `task` is sent.
// It returns a list so a by-person task can fan out one DM per recipient; an
// empty list means "no deliverable target" (caller skips).
//
// by-person (SummaryMode==ModeByPerson): union of every summary_participant's
// UserID with the task creator, deduped. Each uid becomes an independent creator
// DM target so the notify state machine dedups/retries each recipient on its own
// (task, kind, uid) row.
//
// A participant-lookup error is returned (not swallowed): silently degrading to
// just the creator would mark completed as delivered, never build participant
// rows, and leave the miss invisible to the sweep — participants would be lost
// forever. The caller skips this run so a later trigger/sweep can retry.
//
// non-by-person: a single creator DM (本期口径). The task carries
// OriginChannelID/OriginChannelType but 原群/thread 回发 is an explicit follow-up
// enhancement and is intentionally NOT depended on here — every origin routes to
// the creator DM fallback this phase.
func (n *Notifier) resolveTargets(task model.SummaryTask) ([]deliveryTarget, error) {
	mk := func(uid string) deliveryTarget {
		return deliveryTarget{
			ChannelID:   uid,
			ChannelType: WireChannelDM,
			TargetUID:   uid,
			SpaceID:     task.SpaceID,
		}
	}

	if task.SummaryMode == model.ModeByPerson {
		// Union participant UIDs with the creator, dedup, skip empties.
		seen := make(map[string]struct{})
		var targets []deliveryTarget
		add := func(uid string) {
			if uid == "" {
				return
			}
			if _, ok := seen[uid]; ok {
				return
			}
			seen[uid] = struct{}{}
			targets = append(targets, mk(uid))
		}
		if n != nil && n.db != nil {
			var parts []model.SummaryParticipant
			if err := n.db.Where("task_id = ?", task.ID).Find(&parts).Error; err != nil {
				return nil, err
			}
			for _, p := range parts {
				add(p.UserID)
			}
		}
		add(task.CreatorID) // creator always in the completed fan-out
		return targets, nil
	}

	// Non-by-person: single creator DM (empty creator → no target).
	if task.CreatorID == "" {
		return nil, nil
	}
	return []deliveryTarget{mk(task.CreatorID)}, nil
}

// creatorTarget resolves the single creator DM target used for the failed path
// (failed notifications go only to the creator, never fanned out) and for the
// sweep redeliver path which reconstructs a target for a specific recipient_uid.
func (n *Notifier) creatorTarget(task model.SummaryTask) (deliveryTarget, bool) {
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

// targetForUID rebuilds a DM target for an explicit recipient uid using the
// task's SpaceID. Used by the sweep redeliver path where the recipient is read
// back from the stored notification row rather than re-derived from the task.
func targetForUID(task model.SummaryTask, uid string) (deliveryTarget, bool) {
	if uid == "" {
		return deliveryTarget{}, false
	}
	return deliveryTarget{
		ChannelID:   uid,
		ChannelType: WireChannelDM,
		TargetUID:   uid,
		SpaceID:     task.SpaceID,
	}, true
}
