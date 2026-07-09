package pipeline

import (
	"context"

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

	channels, err := GetUserChannels(ctx, uid, imDB, selectedThreadIDs...)
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
