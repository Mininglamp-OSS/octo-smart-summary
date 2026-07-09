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
// keys produced from the accessible-channel query output. DM ids are folded
// through NormalizeDMChannelID so peer/peer@self/self@peer all collapse to
// one key.
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

// ValidateUserAccessibleSources rejects entries the user cannot see. Write
// path only: fail-closed on any error, including imDB==nil. Read paths keep
// the permissive GetUserChannels (see reviewer thread e0640d10).
//
// Empty sources short-circuits to (nil, nil). Otherwise returns the subset
// of sources that are NOT accessible; callers turn a non-empty missing list
// into a 40017 business error, or on err into an HTTP 500.
func ValidateUserAccessibleSources(ctx context.Context, uid string, imDB *gorm.DB, sources []SourceRef) ([]SourceRef, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	// Fail-closed on missing IM DB: production wires imDB=nil when the IM
	// connection failed at startup (cmd/summary-api/main.go:61-64). A permissive
	// fallback here would let any Update/Create bypass the access check whenever
	// the IM DB happens to be down — the exact IDOR this whole change closes.
	// Callers surface this as HTTP 500 (not 40017), so an outage looks like an
	// outage rather than a false "no access" verdict.
	if imDB == nil {
		return nil, fmt.Errorf("access check unavailable: IM DB not connected")
	}

	// Gather the two id sets we need to probe:
	//   * selectedThreadIDs -> archived-thread relaxation for the thread query
	//   * dmIDs             -> targeted DM existence check (avoids the LIMIT 200
	//                          truncation that a broad "list all my DMs" query
	//                          would inherit for heavy users)
	var selectedThreadIDs []string
	var dmIDs []string
	for _, s := range sources {
		switch s.SourceType {
		case 2:
			if s.SourceID != "" {
				selectedThreadIDs = append(selectedThreadIDs, s.SourceID)
			}
		case 3:
			if s.SourceID != "" {
				dmIDs = append(dmIDs, NormalizeDMChannelID(s.SourceID, uid, 1))
			}
		}
	}

	channels, err := getUserChannelsStrict(ctx, uid, imDB, selectedThreadIDs, dmIDs)
	if err != nil {
		return nil, err
	}

	// (channel_type, channel_id) set of what the user can see.
	allowed := make(map[[2]string]struct{}, len(channels))
	for _, ch := range channels {
		allowed[[2]string{itoa2(ch.ChannelType), ch.ChannelID}] = struct{}{}
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
// log-and-continue used by read paths. Kept private here (rather than folded
// back into GetUserChannels) because the five existing read-path callers rely
// on the fail-open degradation to tolerate transient IM outages; changing
// that contract would ripple far outside this fix (reviewer thread e0640d10).
//
// Semantics deliberately diverge from GetUserChannels on two axes to match
// the picker contract and avoid write-path false negatives:
//   - Thread accessibility joins group_member (not thread_member) so any
//     thread inside a user's group counts as accessible even if the user has
//     never posted in it — mirroring candidates.go:188-190 (the picker the
//     frontend actually shows). thread_member was too strict and produced
//     false 40017 on a user's first source pick.
//   - DM accessibility is a targeted existence check on the request's DM ids
//     (no LIMIT), instead of listing the top-N most-recent DMs. The LIMIT
//     200 in GetUserChannels' DM query is fine for read-path candidate
//     surfacing but broke write-path validation for users with more than
//     200 DMs (a legitimate DM #201+ would get judged missing).
func getUserChannelsStrict(ctx context.Context, uid string, imDB *gorm.DB, selectedThreadIDs, dmIDs []string) ([]ChannelInfo, error) {
	if imDB == nil {
		return nil, fmt.Errorf("IM database not available")
	}

	var channels []ChannelInfo

	// Groups — pull all groups the user is an active member of.
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

	// DM channels — targeted existence check on the request's DM ids, no LIMIT.
	// See the function-doc note on why this diverges from GetUserChannels.
	if len(dmIDs) > 0 {
		type dmRow struct {
			ChannelID string `gorm:"column:channel_id"`
		}
		var dms []dmRow
		err = imDB.WithContext(ctx).Raw(`
			SELECT channel_id
			FROM conversation_extra
			WHERE uid = ?
			  AND channel_type = 1
			  AND channel_id IN ?
			GROUP BY channel_id
		`, uid, dmIDs).Scan(&dms).Error
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
	}

	// Thread channels (channelType=5) — joined via group_member so any thread
	// inside a group the user belongs to counts (see function-doc note aligning
	// with picker candidates.go:188-190). Archived threads only surface when
	// selectedThreadIDs explicitly names them (same relaxation as GetUserChannels).
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
		INNER JOIN group_member gm ON gm.group_no COLLATE utf8mb4_unicode_ci = t.group_no
		WHERE gm.uid = ?
		  AND gm.is_deleted = 0
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
