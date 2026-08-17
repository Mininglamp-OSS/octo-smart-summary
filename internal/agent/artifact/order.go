// Package artifact freezes the evidence pool and the citation ordinal mapping
// for a run (SS-04), so citation numbers stop drifting between the mid-run and
// save-time citation passes.
//
// The freezing logic here is deliberately the single source of truth for
// "canonical citation order". It reproduces, byte-for-byte, the dedup + sort
// the two live citation sites use today (getSessionMessagePool and
// buildCitationsForSession): dedup by (channel_id, message_seq), then sort by
// (timestamp, channel_id, message_seq). When SS-05 swaps those sites to read a
// frozen manifest, the ordinals must match what they produced before — so the
// ordering rule cannot diverge.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// StableID identifies a message independently of its position in any pool.
type StableID struct {
	ChannelID  string `json:"channel_id"`
	MessageSeq int64  `json:"message_seq"`
}

// Key is the stable de-dup / lookup key, matching the "channel:seq" form used
// by the citation sites.
func (s StableID) Key() string { return fmt.Sprintf("%s:%d", s.ChannelID, s.MessageSeq) }

// CanonicalOrder dedups messages by (channel_id, message_seq) and returns the
// frozen stable-id list in citation order. It MUST stay byte-identical to the
// dedup + sort in getSessionMessagePool / buildCitationsForSession:
//
//	dedup by channel_id+message_seq (first occurrence wins)
//	sort by timestamp asc, then channel_id asc, then message_seq asc
//
// The 1-based ordinal of an entry is its index+1.
func CanonicalOrder(messages []pipeline.Message) []StableID {
	seen := make(map[string]bool, len(messages))
	type entry struct {
		id StableID
		ts int64
	}
	entries := make([]entry, 0, len(messages))
	for _, m := range messages {
		id := StableID{ChannelID: m.ChannelID, MessageSeq: m.MessageSeq}
		k := id.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		entries = append(entries, entry{id: id, ts: m.Timestamp})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ts != entries[j].ts {
			return entries[i].ts < entries[j].ts
		}
		if entries[i].id.ChannelID != entries[j].id.ChannelID {
			return entries[i].id.ChannelID < entries[j].id.ChannelID
		}
		return entries[i].id.MessageSeq < entries[j].id.MessageSeq
	})

	out := make([]StableID, len(entries))
	for i, e := range entries {
		out[i] = e.id
	}
	return out
}

// ContentHash is the sha256 hex of the ordered stable-id list. Two fetches
// yielding the same messages (same ids, same order) hash identically, which is
// the artifact idempotency key.
func ContentHash(entries []StableID) (string, error) {
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal entries for hash: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// OrdinalMap maps each stable id's Key() to its 1-based citation ordinal.
func OrdinalMap(entries []StableID) map[string]int {
	m := make(map[string]int, len(entries))
	for i, e := range entries {
		m[e.Key()] = i + 1
	}
	return m
}

// EncodeEntries serializes the frozen entries for the manifest column.
func EncodeEntries(entries []StableID) (string, error) {
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal entries: %w", err)
	}
	return string(b), nil
}

// DecodeEntries parses the manifest column back into stable ids.
func DecodeEntries(raw string) ([]StableID, error) {
	var out []StableID
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("unmarshal entries: %w", err)
	}
	return out, nil
}
