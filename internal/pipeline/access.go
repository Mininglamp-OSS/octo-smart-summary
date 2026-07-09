package pipeline

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// Source-type encoding used by summary/schedule sourceReq:
//   1 = group, 2 = thread, 3 = DM
// pipeline ChannelType encoding used by GetUserChannels:
//   1 = DM, 2 = group, 5 = thread
// The mapping stays private to this file.

// SourceRef is the pipeline-side view of a summary/schedule source entry.
// It exists so callers (handlers) don't have to expose their private request
// struct just to run the access check.
type SourceRef struct {
	SourceType int // 1=group, 2=thread, 3=DM
	SourceID   string
	SourceName string
}

// sourceKey normalizes a SourceRef into a lookup key comparable against the
// keys produced from GetUserChannels output. DM ids are folded through
// NormalizeDMChannelID so peer/peer@self/self@peer all collapse to one key.
func (s SourceRef) sourceKey(selfUID string) (channelType int, channelID string) {
	switch s.SourceType {
	case 1: // group -> pipeline group
		return 2, s.SourceID
	case 2: // thread -> pipeline thread
		return 5, s.SourceID
	case 3: // DM -> pipeline DM (normalize peer id)
		return 1, NormalizeDMChannelID(s.SourceID, selfUID, 1)
	default:
		return 0, s.SourceID
	}
}

// ValidateUserAccessibleSources checks each entry in sources against the
// authoritative user-accessible channel set (GetUserChannels). Returns the
// subset that is NOT accessible; callers turn a non-empty missing list into
// a 40017 business error.
//
// imDB==nil is a permissive fallback (mirrors ResolveSourceNameWithType) so
// unit-test paths that skip the IM DB stay green. sources empty -> no work,
// nil missing.
func ValidateUserAccessibleSources(ctx context.Context, uid string, imDB *gorm.DB, sources []SourceRef) ([]SourceRef, error) {
	if imDB == nil || len(sources) == 0 {
		return nil, nil
	}

	// Pass explicitly-selected thread ids to GetUserChannels so an archived
	// thread the user is legitimately editing (e.g. keeping it in a schedule)
	// still shows up in the accessible set. Non-thread entries are ignored
	// by the underlying query.
	var selectedThreadIDs []string
	for _, s := range sources {
		if s.SourceType == 2 && s.SourceID != "" {
			selectedThreadIDs = append(selectedThreadIDs, s.SourceID)
		}
	}

	// Write path is fail-closed: any IM query failure short-circuits to an
	// error (surfaced as HTTP 500 by callers) so a partial visibility set can't
	// masquerade as "no access" and cause false 403/40017. Read paths keep the
	// permissive GetUserChannels intact (see reviewer thread e0640d10).
	channels, err := getUserChannelsStrict(ctx, uid, imDB, selectedThreadIDs...)
	if err != nil {
		return nil, err
	}

	// (channel_type, channel_id) set of what the user can see.
	allowed := make(map[[2]string]struct{}, len(channels))
	for _, ch := range channels {
		key := [2]string{itoa2(ch.ChannelType), ch.ChannelID}
		allowed[key] = struct{}{}
	}

	var missing []SourceRef
	for _, s := range sources {
		ct, cid := s.sourceKey(uid)
		if ct == 0 {
			// Unknown source_type is treated as inaccessible; handler-side
			// validation should reject unknown types earlier, but fail closed
			// here rather than silently pass.
			missing = append(missing, s)
			continue
		}
		if _, ok := allowed[[2]string{itoa2(ct), cid}]; !ok {
			missing = append(missing, s)
		}
	}
	return missing, nil
}

// itoa2 is a stdlib-free tiny int-to-string for our fixed set of channel
// types (1/2/5). Avoids pulling strconv just for one call site.
func itoa2(n int) string {
	switch n {
	case 1:
		return "1"
	case 2:
		return "2"
	case 5:
		return "5"
	default:
		return "?"
	}
}

// getUserChannelsStrict is the write-path variant of GetUserChannels: any
// group/DM/thread query failure returns an error instead of the permissive
// log-and-continue used by read paths. Kept in this file rather than folded
// back into GetUserChannels because the five existing read-path callers rely
// on the fail-open degradation to tolerate transient IM outages; changing that
// contract would ripple far outside this fix (reviewer thread e0640d10).
//
// Semantics otherwise mirror GetUserChannels verbatim (same SQL, same
// selectedThreadIDs scoping for archived threads, same DM normalization).
func getUserChannelsStrict(ctx context.Context, uid string, imDB *gorm.DB, selectedThreadIDs ...string) ([]ChannelInfo, error) {
	if imDB == nil {
		return nil, fmt.Errorf("IM database not available")
	}

	var channels []ChannelInfo

	// Groups
	type groupRow struct {
		ChannelID   string `gorm:"column:channel_id"`
		ChannelType int    `gorm:"column:channel_type"`
		ChannelName string `gorm:"column:channel_name"`
		SpaceID     string `gorm:"column:space_id"`
	}
	var groups []groupRow
	err := imDB.WithContext(ctx).Raw(`
		SELECT g.group_no AS channel_id,
		       2 AS channel_type,
		       g.name AS channel_name,
		       COALESCE(g.space_id, '') AS space_id
		FROM `+"`group`"+` g
		INNER JOIN group_member gm ON g.group_no = gm.group_no
		WHERE gm.uid = ?
		  AND gm.is_deleted = 0
		  AND g.status = 1
		ORDER BY g.updated_at DESC
	`, uid).Scan(&groups).Error
	if err != nil {
		return nil, fmt.Errorf("query groups: %w", err)
	}
	for _, g := range groups {
		channels = append(channels, ChannelInfo{
			ChannelID:   g.ChannelID,
			ChannelType: g.ChannelType,
			ChannelName: g.ChannelName,
			SpaceID:     g.SpaceID,
		})
	}

	// DM channels — write path fails closed (contrast: GetUserChannels logs+continues).
	type dmRow struct {
		ChannelID string `gorm:"column:channel_id"`
	}
	var dms []dmRow
	err = imDB.WithContext(ctx).Raw(`
		SELECT channel_id
		FROM conversation_extra
		WHERE uid = ? AND channel_type = 1
		GROUP BY channel_id
		ORDER BY MAX(updated_at) DESC
		LIMIT 200
	`, uid).Scan(&dms).Error
	if err != nil {
		return nil, fmt.Errorf("query DM channels: %w", err)
	}
	for _, d := range dms {
		peerUID := getPeerUID(d.ChannelID, uid)
		normalized := NormalizeDMChannelID(d.ChannelID, uid, 1)
		channels = append(channels, ChannelInfo{
			ChannelID:   normalized,
			ChannelType: 1,
			ChannelName: fmt.Sprintf("私聊-%s", peerUID),
			PeerUID:     peerUID,
		})
	}

	// Thread channels (channelType=5) — write path fails closed.
	type threadRow struct {
		ChannelID   string `gorm:"column:channel_id"`
		ChannelType int    `gorm:"column:channel_type"`
		ChannelName string `gorm:"column:channel_name"`
		SpaceID     string `gorm:"column:space_id"`
	}
	var threadChannels []threadRow
	threadStatusCond := "t.status = 1"
	threadArgs := []interface{}{uid}
	if len(selectedThreadIDs) > 0 {
		threadStatusCond = "(t.status = 1 OR (t.status = 2 AND CONCAT(t.group_no, '____', t.short_id) IN ?))"
		threadArgs = append(threadArgs, selectedThreadIDs)
	}
	threadQuery := `
		SELECT CONCAT(t.group_no, '____', t.short_id) AS channel_id,
		       5 AS channel_type,
		       CONCAT(t.name, ' · ', g.name) AS channel_name,
		       COALESCE(g.space_id, '') AS space_id
		FROM thread t
		INNER JOIN ` + "`group`" + ` g ON g.group_no COLLATE utf8mb4_unicode_ci = t.group_no
		INNER JOIN thread_member tm ON tm.thread_id = t.id
		WHERE tm.uid = ?
		  AND ` + threadStatusCond + `
		  AND g.status = 1
		ORDER BY t.updated_at DESC
	`
	err = imDB.WithContext(ctx).Raw(threadQuery, threadArgs...).Scan(&threadChannels).Error
	if err != nil {
		return nil, fmt.Errorf("query thread channels: %w", err)
	}
	for _, tc := range threadChannels {
		channels = append(channels, ChannelInfo{
			ChannelID:   tc.ChannelID,
			ChannelType: 5,
			ChannelName: tc.ChannelName,
			SpaceID:     tc.SpaceID,
		})
	}

	return channels, nil
}
