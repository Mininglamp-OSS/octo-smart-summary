//go:build cgo
// +build cgo

package artifact

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func newArtifactTestDB(t *testing.T) *gorm.DB {
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

func pool() []pipeline.Message {
	return []pipeline.Message{
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10},
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20},
		{ChannelID: "a", MessageSeq: 3, Timestamp: 15},
	}
}

// issue A fix: the finish gate must read the artifact for the SAME run as the
// Spec. A session can hold multiple runs (one per submit); reading by session
// grabs whichever run's artifact is latest, mismatching the Spec's channel
// count → false PARTIAL. GetLatestArtifactByRun keeps both from one run.
func TestGetLatestArtifactByRunIsolatesRuns(t *testing.T) {
	db := newArtifactTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	// Same session, two runs: runOne fetched 1 channel, runFull fetched 2.
	poolOne := []pipeline.Message{{ChannelID: "a", MessageSeq: 1, Timestamp: 10}}
	poolTwo := []pipeline.Message{{ChannelID: "a", MessageSeq: 1, Timestamp: 10}, {ChannelID: "b", MessageSeq: 2, Timestamp: 20}}
	if _, _, _, err := s.FreezeFromPool(ctx, "runOne", "u1", "sess1", poolOne, FreezeMeta{}); err != nil {
		t.Fatalf("freeze runOne: %v", err)
	}
	if _, _, _, err := s.FreezeFromPool(ctx, "runFull", "u1", "sess1", poolTwo, FreezeMeta{}); err != nil {
		t.Fatalf("freeze runFull: %v", err)
	}

	// By-run returns each run's OWN artifact, regardless of freeze order.
	full, ok, err := s.GetLatestArtifactByRun(ctx, "u1", "runFull")
	if err != nil || !ok {
		t.Fatalf("byRun runFull: ok=%v err=%v", ok, err)
	}
	if full.ChannelCount != 2 {
		t.Errorf("runFull channel_count = %d, want 2", full.ChannelCount)
	}
	one, ok, err := s.GetLatestArtifactByRun(ctx, "u1", "runOne")
	if err != nil || !ok {
		t.Fatalf("byRun runOne: ok=%v err=%v", ok, err)
	}
	if one.ChannelCount != 1 {
		t.Errorf("runOne channel_count = %d, want 1", one.ChannelCount)
	}

	// Missing run → found=false, nil error.
	if _, ok, err := s.GetLatestArtifactByRun(ctx, "u1", "nope"); err != nil || ok {
		t.Errorf("missing run should be not-found: ok=%v err=%v", ok, err)
	}
}

func TestFreezeFromPoolIdempotent(t *testing.T) {
	db := newArtifactTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	art1, man1, created1, err := s.FreezeFromPool(ctx, "run1", "u1", "sess1", pool(), FreezeMeta{TimeRangeStart: 1, TimeRangeEnd: 99})
	if err != nil {
		t.Fatalf("freeze 1: %v", err)
	}
	if !created1 {
		t.Fatal("first freeze should create")
	}
	if art1.Revision != 1 || art1.MessageCount != 3 || art1.ChannelCount != 2 {
		t.Fatalf("artifact meta wrong: %+v", art1)
	}
	if man1.EntryCount != 3 {
		t.Fatalf("manifest entry_count = %d, want 3", man1.EntryCount)
	}

	// Re-freeze the SAME pool (input reordered) → idempotent, no new revision.
	reordered := []pipeline.Message{
		{ChannelID: "a", MessageSeq: 3, Timestamp: 15},
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10},
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20},
	}
	art2, _, created2, err := s.FreezeFromPool(ctx, "run1", "u1", "sess1", reordered, FreezeMeta{})
	if err != nil {
		t.Fatalf("freeze 2: %v", err)
	}
	if created2 {
		t.Fatal("re-freezing identical pool must not create a new revision")
	}
	if art2.ArtifactID != art1.ArtifactID || art2.Revision != 1 {
		t.Fatalf("idempotent freeze returned different artifact: %+v", art2)
	}

	var artCount, manCount int64
	db.Model(&model.AgentEvidenceArtifact{}).Count(&artCount)
	db.Model(&model.AgentCitationManifest{}).Count(&manCount)
	if artCount != 1 || manCount != 1 {
		t.Fatalf("duplicate rows: artifacts=%d manifests=%d", artCount, manCount)
	}
}

func TestFreezeNewContentNewRevisionAndStableOrdinals(t *testing.T) {
	db := newArtifactTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	art1, _, _, err := s.FreezeFromPool(ctx, "run1", "u1", "sess1", pool(), FreezeMeta{})
	if err != nil {
		t.Fatalf("freeze 1: %v", err)
	}
	// Ordinal of (a,1) in rev1: canonical order is (a,1)@10,(a,3)@15,(b,2)@20 → 1.
	_, entries1, err := s.LoadManifest(ctx, "u1", art1.ArtifactID)
	if err != nil {
		t.Fatalf("load manifest 1: %v", err)
	}
	ord1 := OrdinalMap(entries1)
	if ord1["a:1"] != 1 || ord1["a:3"] != 2 || ord1["b:2"] != 3 {
		t.Fatalf("rev1 ordinals wrong: %v", ord1)
	}

	// A backfill adds an EARLIER message. Under the old recompute path this
	// would shift every later ordinal; here it becomes a new revision and rev1's
	// manifest is untouched.
	bigger := append(pool(), pipeline.Message{ChannelID: "a", MessageSeq: 0, Timestamp: 5})
	art2, _, created2, err := s.FreezeFromPool(ctx, "run1", "u1", "sess1", bigger, FreezeMeta{})
	if err != nil {
		t.Fatalf("freeze 2: %v", err)
	}
	if !created2 || art2.Revision != 2 {
		t.Fatalf("new content should be revision 2: created=%v rev=%d", created2, art2.Revision)
	}

	// rev1's frozen ordinals must be unchanged despite the new earlier message.
	_, entries1Again, _ := s.LoadManifest(ctx, "u1", art1.ArtifactID)
	if OrdinalMap(entries1Again)["a:1"] != 1 {
		t.Fatal("rev1 ordinal drifted after a new revision — freezing failed")
	}
	// rev2 reflects the new earliest message at ordinal 1.
	_, entries2, _ := s.LoadManifest(ctx, "u1", art2.ArtifactID)
	if OrdinalMap(entries2)["a:0"] != 1 {
		t.Fatalf("rev2 should place the earlier message first: %v", OrdinalMap(entries2))
	}

	latest, err := s.GetLatestArtifact(ctx, "u1", "run1")
	if err != nil || latest.Revision != 2 {
		t.Fatalf("latest artifact = rev %v (err %v), want 2", latest, err)
	}

	// Hot paths adopt the first manifest even if a concurrent/later freeze
	// persisted another revision. This is the freeze-once citation contract.
	frozen, frozenEntries, found, err := s.GetFrozenManifestByRun(ctx, "u1", "run1")
	if err != nil || !found || frozen.Revision != 1 {
		t.Fatalf("frozen manifest = %#v found=%v err=%v, want revision 1", frozen, found, err)
	}
	if OrdinalMap(frozenEntries)["a:1"] != 1 {
		t.Fatalf("frozen ordinals changed after revision 2: %v", OrdinalMap(frozenEntries))
	}
}

func TestManifestOwnerScoped(t *testing.T) {
	db := newArtifactTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	art, _, _, _ := s.FreezeFromPool(ctx, "run1", "owner", "sess1", pool(), FreezeMeta{})

	if _, _, err := s.LoadManifest(ctx, "owner", art.ArtifactID); err != nil {
		t.Fatalf("owner should read manifest: %v", err)
	}
	if _, _, err := s.LoadManifest(ctx, "attacker", art.ArtifactID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("cross-user manifest read err = %v, want RecordNotFound", err)
	}
}

func TestGetFrozenManifestByRun(t *testing.T) {
	db := newArtifactTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	// Not frozen yet → found=false, no error (the freeze-once read contract).
	if _, _, found, err := s.GetFrozenManifestByRun(ctx, "u1", "run1"); err != nil || found {
		t.Fatalf("pre-freeze: found=%v err=%v, want false/nil", found, err)
	}

	s.FreezeFromPool(ctx, "run1", "u1", "sess1", pool(), FreezeMeta{})

	man, entries, found, err := s.GetFrozenManifestByRun(ctx, "u1", "run1")
	if err != nil || !found {
		t.Fatalf("post-freeze: found=%v err=%v, want true/nil", found, err)
	}
	if man.RunID != "run1" || len(entries) != 3 {
		t.Fatalf("manifest wrong: run=%s entries=%d", man.RunID, len(entries))
	}
	// Owner-scoped.
	if _, _, found, _ := s.GetFrozenManifestByRun(ctx, "attacker", "run1"); found {
		t.Fatal("cross-user GetFrozenManifestByRun should not find")
	}
}

func TestGetLatestBySession(t *testing.T) {
	db := newArtifactTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	if _, _, found, err := s.GetLatestBySession(ctx, "u1", "sess1"); err != nil || found {
		t.Fatalf("pre-freeze session: found=%v err=%v", found, err)
	}

	s.FreezeFromPool(ctx, "run1", "u1", "sess1", pool(), FreezeMeta{})

	_, entries, found, err := s.GetLatestBySession(ctx, "u1", "sess1")
	if err != nil || !found || len(entries) != 3 {
		t.Fatalf("session lookup: found=%v err=%v entries=%d", found, err, len(entries))
	}
	if _, _, found, _ := s.GetLatestBySession(ctx, "attacker", "sess1"); found {
		t.Fatal("cross-user GetLatestBySession should not find")
	}
}

// TestGetLatestBySessionMultiRun: revision is allocated PER RUN, so two runs
// in one session both freeze at revision 1 and the old revision-DESC ordering
// tied between them arbitrarily — and an older run that re-fetched (rev 2)
// deterministically beat the just-completed run. The fixed ordering
// (created_at DESC, artifact_id DESC) must always return the most recently
// FROZEN manifest.
func TestGetLatestBySessionMultiRun(t *testing.T) {
	db := newArtifactTestDB(t)
	if db == nil {
		return
	}
	s := NewStore(db)
	ctx := context.Background()

	// Pin the clock so the ordering assertion does not depend on wall time.
	oldNow := now
	defer func() { now = oldNow }()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	tick := 0
	now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }

	// Turn 1: run A freezes its pool → revision 1 (created_at T1).
	poolA := []pipeline.Message{
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10},
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20},
	}
	_, _, _, err := s.FreezeFromPool(ctx, "runA", "u1", "sess1", poolA, FreezeMeta{})
	if err != nil {
		t.Fatalf("freeze A: %v", err)
	}

	// Run A re-fetches a CHANGED pool → revision 2 (created_at T2). Without
	// the fix this row is the revision-DESC winner forever, even after a
	// newer run freezes.
	poolA2 := append([]pipeline.Message{}, poolA...)
	poolA2 = append(poolA2, pipeline.Message{ChannelID: "c", MessageSeq: 9, Timestamp: 25})
	_, _, _, err = s.FreezeFromPool(ctx, "runA", "u1", "sess1", poolA2, FreezeMeta{})
	if err != nil {
		t.Fatalf("freeze A2: %v", err)
	}

	// Turn 2: run B freezes its pool → ALSO revision 1, but newest by
	// created_at (T3).
	poolB := []pipeline.Message{
		{ChannelID: "a", MessageSeq: 1, Timestamp: 10},
		{ChannelID: "b", MessageSeq: 2, Timestamp: 20},
		{ChannelID: "d", MessageSeq: 4, Timestamp: 40},
		{ChannelID: "e", MessageSeq: 5, Timestamp: 50},
	}
	_, manB, _, err := s.FreezeFromPool(ctx, "runB", "u1", "sess1", poolB, FreezeMeta{})
	if err != nil {
		t.Fatalf("freeze B: %v", err)
	}
	if manB.Revision != 1 {
		t.Fatalf("run B first freeze should be revision 1, got %d", manB.Revision)
	}

	man, entries, found, err := s.GetLatestBySession(ctx, "u1", "sess1")
	if err != nil || !found {
		t.Fatalf("session lookup: found=%v err=%v", found, err)
	}
	if man.ArtifactID != manB.ArtifactID {
		t.Fatalf("session lookup returned artifact %s (rev %d), want run B's %s — the just-frozen manifest must win over run A's higher revision", man.ArtifactID, man.Revision, manB.ArtifactID)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want run B's 4-message pool", len(entries))
	}
}
