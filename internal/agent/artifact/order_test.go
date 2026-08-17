package artifact

import (
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// referenceOrder replicates the EXACT dedup + sort the two live citation sites
// use (getSessionMessagePool / buildCitationsForSession). CanonicalOrder must
// match it byte-for-byte or SS-05's manifest swap would renumber citations.
func referenceOrder(msgs []pipeline.Message) []StableID {
	seen := map[string]bool{}
	var kept []pipeline.Message
	for _, m := range msgs {
		k := StableID{ChannelID: m.ChannelID, MessageSeq: m.MessageSeq}.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		kept = append(kept, m)
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].Timestamp != kept[j].Timestamp {
			return kept[i].Timestamp < kept[j].Timestamp
		}
		if kept[i].ChannelID != kept[j].ChannelID {
			return kept[i].ChannelID < kept[j].ChannelID
		}
		return kept[i].MessageSeq < kept[j].MessageSeq
	})
	out := make([]StableID, len(kept))
	for i, m := range kept {
		out[i] = StableID{ChannelID: m.ChannelID, MessageSeq: m.MessageSeq}
	}
	return out
}

func TestCanonicalOrderMatchesCitationSort(t *testing.T) {
	msgs := []pipeline.Message{
		{ChannelID: "b", MessageSeq: 2, Timestamp: 100},
		{ChannelID: "a", MessageSeq: 5, Timestamp: 100}, // same ts, channel a < b
		{ChannelID: "a", MessageSeq: 1, Timestamp: 50},  // earliest ts
		{ChannelID: "b", MessageSeq: 2, Timestamp: 100}, // dup of first
		{ChannelID: "a", MessageSeq: 3, Timestamp: 100}, // same ts+channel, seq 3 < 5
	}
	got := CanonicalOrder(msgs)
	want := referenceOrder(msgs)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (dedup mismatch)", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Explicit expected sequence: (a,1)@50, (a,3)@100, (a,5)@100, (b,2)@100.
	exp := []StableID{{"a", 1}, {"a", 3}, {"a", 5}, {"b", 2}}
	for i := range exp {
		if got[i] != exp[i] {
			t.Fatalf("order[%d] = %+v, want %+v", i, got[i], exp[i])
		}
	}
}

func TestContentHashIdempotentAndSensitive(t *testing.T) {
	a := []pipeline.Message{
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10},
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20},
	}
	// Same messages, different INPUT order → same canonical order → same hash.
	b := []pipeline.Message{
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20},
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10},
	}
	ha, err := ContentHash(CanonicalOrder(a))
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := ContentHash(CanonicalOrder(b))
	if ha != hb {
		t.Fatalf("hash not input-order-insensitive: %s != %s", ha, hb)
	}

	// One extra message → different content → different hash.
	c := append([]pipeline.Message{}, a...)
	c = append(c, pipeline.Message{ChannelID: "c", MessageSeq: 9, Timestamp: 30})
	hc, _ := ContentHash(CanonicalOrder(c))
	if hc == ha {
		t.Fatal("different pool must hash differently")
	}
}

func TestOrdinalMap(t *testing.T) {
	entries := []StableID{{"a", 1}, {"a", 3}, {"b", 2}}
	m := OrdinalMap(entries)
	if m["a:1"] != 1 || m["a:3"] != 2 || m["b:2"] != 3 {
		t.Fatalf("ordinal map wrong: %v", m)
	}
}

func TestEncodeDecodeEntriesRoundtrip(t *testing.T) {
	entries := []StableID{{"a", 1}, {"b", 2}}
	enc, err := EncodeEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeEntries(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0] != entries[0] || back[1] != entries[1] {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
}
