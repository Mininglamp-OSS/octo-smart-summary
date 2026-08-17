//go:build cgo
// +build cgo

package agent

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func newManifestTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil
	}
	if err := db.AutoMigrate(&model.AgentEvidenceArtifact{}, &model.AgentCitationManifest{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func idx(pool []pipeline.Message) map[string]int {
	m := map[string]int{}
	for _, msg := range pool {
		m[msg.ChannelID] = msg.CitationIndex // channels are unique per message here
	}
	return m
}

// TestApplyFrozenManifestFreezeOnceStable is the SS-05 B1 drift-fix proof: once a
// run's manifest is frozen, a later citation pass over a CHANGED pool (an earlier
// message inserted) reads the original frozen ordinals instead of renumbering —
// the exact drift the old recompute path caused.
func TestApplyFrozenManifestFreezeOnceStable(t *testing.T) {
	db := newManifestTestDB(t)
	if db == nil {
		return
	}
	SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	ctx := context.Background()
	const uid, sess, run = "u1", "sess1", "run1"

	// Mid-run pool (already citation-ordered by an earlier getSessionMessagePool):
	// a@10 → 1, b@20 → 2, c@30 → 3.
	pool1 := []pipeline.Message{
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10, CitationIndex: 1},
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20, CitationIndex: 2},
		{ChannelID: "c", MessageSeq: 3, Timestamp: 30, CitationIndex: 3},
	}
	got1 := applyFrozenManifest(ctx, uid, sess, run, pool1)
	m1 := idx(got1)
	if m1["a"] != 1 || m1["b"] != 2 || m1["c"] != 3 {
		t.Fatalf("first pass ordinals wrong: %v", m1)
	}

	// A backfill inserts an EARLIER message (z@5). Under the old recompute path
	// this shifts a/b/c to 2/3/4. With the frozen manifest, z is post-freeze →
	// dropped, and a/b/c keep 1/2/3.
	pool2 := []pipeline.Message{
		{ChannelID: "z", MessageSeq: 9, Timestamp: 5, CitationIndex: 999},
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10, CitationIndex: 999},
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20, CitationIndex: 999},
		{ChannelID: "c", MessageSeq: 3, Timestamp: 30, CitationIndex: 999},
	}
	got2 := applyFrozenManifest(ctx, uid, sess, run, pool2)
	m2 := idx(got2)
	if m2["a"] != 1 || m2["b"] != 2 || m2["c"] != 3 {
		t.Fatalf("frozen ordinals drifted on second pass: %v", m2)
	}
	if _, present := m2["z"]; present {
		t.Fatal("post-freeze message z should be dropped from the citable set")
	}
	if len(got2) != 3 {
		t.Fatalf("citable count = %d, want 3", len(got2))
	}
}

// TestApplyFrozenManifestNoRunIsNoop verifies the guard: an empty run_id (V2 off
// path) returns the pool untouched.
func TestApplyFrozenManifestNoRunIsNoop(t *testing.T) {
	db := newManifestTestDB(t)
	if db == nil {
		return
	}
	SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	pool := []pipeline.Message{{ChannelID: "a", MessageSeq: 1, Timestamp: 10, CitationIndex: 7}}
	got := applyFrozenManifest(context.Background(), "u1", "sess1", "", pool)
	if len(got) != 1 || got[0].CitationIndex != 7 {
		t.Fatalf("empty run_id must be a no-op, got %+v", got)
	}
}
