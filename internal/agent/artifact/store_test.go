//go:build cgo
// +build cgo

package artifact

import (
	"context"
	"errors"
	"testing"

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
