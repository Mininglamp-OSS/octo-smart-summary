package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// now is indirected so tests can pin time.
var now = time.Now

// FreezeMeta carries the coverage facts a fetch knows but the message pool
// alone does not (truncation, per-channel failures, the requested window).
type FreezeMeta struct {
	TimeRangeStart int64
	TimeRangeEnd   int64
	Truncated      bool
	FailedChannels []string
}

// Store persists evidence artifacts and citation manifests.
type Store struct {
	db *gorm.DB
}

// NewStore builds a Store over the summary/application DB.
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// FreezeFromPool idempotently freezes the message pool for a run into an
// immutable artifact revision + a citation manifest, returning them and whether
// they were newly created.
//
// Idempotency: keyed by (run_id, content_hash). Re-freezing the same pool
// returns the existing artifact/manifest with created=false and writes nothing
// — so a retried fetch does not spawn a new revision. A pool with different
// messages hashes differently and becomes a new revision (max+1).
func (s *Store) FreezeFromPool(ctx context.Context, runID, userID, sessionID string, messages []pipeline.Message, meta FreezeMeta) (*model.AgentEvidenceArtifact, *model.AgentCitationManifest, bool, error) {
	entries := CanonicalOrder(messages)
	hash, err := ContentHash(entries)
	if err != nil {
		return nil, nil, false, err
	}
	entriesJSON, err := EncodeEntries(entries)
	if err != nil {
		return nil, nil, false, err
	}
	channelCount := countChannels(entries)
	timeRangeJSON, err := json.Marshal(map[string]int64{"start": meta.TimeRangeStart, "end": meta.TimeRangeEnd})
	if err != nil {
		return nil, nil, false, err
	}
	failed := meta.FailedChannels
	if failed == nil {
		failed = []string{}
	}
	failedJSON, err := json.Marshal(failed)
	if err != nil {
		return nil, nil, false, err
	}

	var (
		artifact *model.AgentEvidenceArtifact
		manifest *model.AgentCitationManifest
		created  bool
	)

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Idempotent short-circuit: same pool already frozen for this run.
		var existing model.AgentEvidenceArtifact
		found := tx.Where("run_id = ? AND content_hash = ?", runID, hash).First(&existing).Error
		if found == nil {
			m, err := loadManifestTx(tx, userID, existing.ArtifactID)
			if err != nil {
				return err
			}
			artifact, manifest, created = &existing, m, false
			return nil
		}
		if !errors.Is(found, gorm.ErrRecordNotFound) {
			return found
		}

		// New content → new revision (max existing + 1, computed in-tx).
		var maxRev struct{ Max *int }
		if err := tx.Model(&model.AgentEvidenceArtifact{}).
			Select("MAX(revision) as max").
			Where("run_id = ?", runID).
			Scan(&maxRev).Error; err != nil {
			return err
		}
		revision := 1
		if maxRev.Max != nil {
			revision = *maxRev.Max + 1
		}

		ts := now()
		art := &model.AgentEvidenceArtifact{
			ArtifactID:      uuid.NewString(),
			RunID:           runID,
			UserID:          userID,
			SessionID:       sessionID,
			Revision:        revision,
			ContentHash:     hash,
			MessageCount:    len(entries),
			ChannelCount:    channelCount,
			ActualTimeRange: string(timeRangeJSON),
			FailedChannels:  string(failedJSON),
			Truncated:       meta.Truncated,
			CreatedAt:       ts,
		}
		// Guard against a concurrent freezer racing the same content_hash.
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(art)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Lost the race: another tx inserted this content first. Reload it.
			var winner model.AgentEvidenceArtifact
			if err := tx.Where("run_id = ? AND content_hash = ?", runID, hash).First(&winner).Error; err != nil {
				return err
			}
			m, err := loadManifestTx(tx, userID, winner.ArtifactID)
			if err != nil {
				return err
			}
			artifact, manifest, created = &winner, m, false
			return nil
		}

		man := &model.AgentCitationManifest{
			ManifestID: uuid.NewString(),
			ArtifactID: art.ArtifactID,
			RunID:      runID,
			UserID:     userID,
			Revision:   revision,
			Entries:    entriesJSON,
			EntryCount: len(entries),
			CreatedAt:  ts,
		}
		if err := tx.Create(man).Error; err != nil {
			return err
		}
		artifact, manifest, created = art, man, true
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return artifact, manifest, created, nil
}

// GetLatestArtifact returns the highest-revision artifact for a run,
// owner-scoped by user_id.
func (s *Store) GetLatestArtifact(ctx context.Context, userID, runID string) (*model.AgentEvidenceArtifact, error) {
	var art model.AgentEvidenceArtifact
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND user_id = ?", runID, userID).
		Order("revision DESC").
		First(&art).Error
	if err != nil {
		return nil, err
	}
	return &art, nil
}

// LoadManifest returns the manifest for an artifact plus its decoded ordinal
// entries. Owner-scoped by user_id.
func (s *Store) LoadManifest(ctx context.Context, userID, artifactID string) (*model.AgentCitationManifest, []StableID, error) {
	man, err := loadManifestTx(s.db.WithContext(ctx), userID, artifactID)
	if err != nil {
		return nil, nil, err
	}
	entries, err := DecodeEntries(man.Entries)
	if err != nil {
		return nil, nil, err
	}
	return man, entries, nil
}

func loadManifestTx(tx *gorm.DB, userID, artifactID string) (*model.AgentCitationManifest, error) {
	var man model.AgentCitationManifest
	if err := tx.Where("artifact_id = ? AND user_id = ?", artifactID, userID).First(&man).Error; err != nil {
		return nil, fmt.Errorf("load manifest for artifact %s: %w", artifactID, err)
	}
	return &man, nil
}

func countChannels(entries []StableID) int {
	seen := make(map[string]bool)
	for _, e := range entries {
		seen[e.ChannelID] = true
	}
	return len(seen)
}
