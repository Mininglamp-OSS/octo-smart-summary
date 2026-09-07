package agent

// Regression: messages fetched after the citation manifest was frozen must
// not reach the model with CitationIndex=0. Under V2 the frozen manifest is
// the single source of truth for ordinals; a message not in it is dropped
// from the chunk input and counted, never handed to the LLM as [0].
// Under legacy (v2=false) the pool is the full session so every message is
// in the map; a miss keeps the message unchanged (historical behavior).

import (
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func frozenMap(entries map[string]int) map[string]int {
	m := make(map[string]int, len(entries))
	for k, v := range entries {
		m[k] = v
	}
	return m
}

func TestAssignCitationIndexes_V2DropsManifestMiss(t *testing.T) {
	// Frozen manifest ordinals: a→1, b→2, c→3. Message d was fetched after
	// the freeze and is not in the manifest.
	citMap := frozenMap(map[string]int{
		"ch1:100": 1,
		"ch1:101": 2,
		"ch1:102": 3,
	})
	msgs := []pipeline.Message{
		{ChannelID: "ch1", MessageSeq: 100, CitationIndex: 0},
		{ChannelID: "ch1", MessageSeq: 101, CitationIndex: 0},
		{ChannelID: "ch1", MessageSeq: 102, CitationIndex: 0},
		{ChannelID: "ch1", MessageSeq: 999, CitationIndex: 0}, // post-freeze
	}

	kept, misses := assignCitationIndexes(msgs, citMap, true)

	if misses != 1 {
		t.Fatalf("manifestMisses = %d, want 1", misses)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d messages, want 3 (post-freeze message must be dropped)", len(kept))
	}
	for i, want := range []int{1, 2, 3} {
		if kept[i].CitationIndex != want {
			t.Errorf("kept[%d].CitationIndex = %d, want %d", i, kept[i].CitationIndex, want)
		}
	}
	// The dropped message must not appear anywhere in kept.
	for _, m := range kept {
		if m.MessageSeq == 999 {
			t.Fatal("post-freeze message (seq 999) leaked into kept — it would reach the model as [0]")
		}
	}
}

func TestAssignCitationIndexes_V2AllMissDropsEverything(t *testing.T) {
	citMap := frozenMap(map[string]int{"ch1:1": 1})
	msgs := []pipeline.Message{
		{ChannelID: "ch1", MessageSeq: 2, CitationIndex: 0},
		{ChannelID: "ch1", MessageSeq: 3, CitationIndex: 0},
	}
	kept, misses := assignCitationIndexes(msgs, citMap, true)
	if misses != 2 {
		t.Fatalf("manifestMisses = %d, want 2", misses)
	}
	if len(kept) != 0 {
		t.Fatalf("kept %d messages, want 0", len(kept))
	}
}

func TestAssignCitationIndexes_LegacyKeepsMiss(t *testing.T) {
	// Legacy: the pool is the full session, so a miss is not expected in
	// practice, but the historical behavior is to keep the message with
	// whatever index it already had (never drop).
	citMap := frozenMap(map[string]int{"ch1:100": 1})
	msgs := []pipeline.Message{
		{ChannelID: "ch1", MessageSeq: 100, CitationIndex: 0},
		{ChannelID: "ch1", MessageSeq: 200, CitationIndex: 7}, // not in map
	}
	kept, misses := assignCitationIndexes(msgs, citMap, false)
	if misses != 0 {
		t.Fatalf("manifestMisses = %d, want 0 (legacy never counts)", misses)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d messages, want 2 (legacy keeps everything)", len(kept))
	}
	if kept[0].CitationIndex != 1 {
		t.Errorf("kept[0].CitationIndex = %d, want 1", kept[0].CitationIndex)
	}
	if kept[1].CitationIndex != 7 {
		t.Errorf("kept[1].CitationIndex = %d, want 7 (unchanged)", kept[1].CitationIndex)
	}
}

func TestAssignCitationIndexes_V2NoMissKeepsAll(t *testing.T) {
	citMap := frozenMap(map[string]int{"ch1:1": 1, "ch1:2": 2})
	msgs := []pipeline.Message{
		{ChannelID: "ch1", MessageSeq: 1, CitationIndex: 0},
		{ChannelID: "ch1", MessageSeq: 2, CitationIndex: 0},
	}
	kept, misses := assignCitationIndexes(msgs, citMap, true)
	if misses != 0 {
		t.Fatalf("manifestMisses = %d, want 0", misses)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d messages, want 2", len(kept))
	}
}

func TestAssignCitationIndexes_DoesNotMutateCachedSlice(t *testing.T) {
	cache := newMessageCache()
	original := []pipeline.Message{
		{ChannelID: "ch1", MessageSeq: 50},  // miss before hit: used to compact in place
		{ChannelID: "ch1", MessageSeq: 100}, // hit
	}
	handle := cache.Store(original, "u1", "session-1")
	wantCached := append([]pipeline.Message(nil), original...)

	kept, misses := assignCitationIndexes(cache.Retrieve(handle, "u1", "session-1"), map[string]int{"ch1:100": 1}, true)
	if misses != 1 || len(kept) != 1 || kept[0].MessageSeq != 100 || kept[0].CitationIndex != 1 {
		t.Fatalf("filtered result = %#v, misses=%d; want seq 100 at citation 1 and one miss", kept, misses)
	}
	if got := cache.Retrieve(handle, "u1", "session-1"); !reflect.DeepEqual(got, wantCached) {
		t.Fatalf("cached slice mutated: got %#v, want %#v", got, wantCached)
	}
}
