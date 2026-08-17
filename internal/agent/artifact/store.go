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
			m, err := loadManifestTx(tx, userID, existing.ArtifactID, false)
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
		// Owner-scoped like every other read in this store, and a LOCKING
		// read: under MySQL REPEATABLE READ a plain consistent read would use
		// the transaction snapshot, so two concurrent freezers could both see
		// the same max and allocate the same revision; FOR UPDATE reads the
		// latest committed row and serializes the allocation per run.
		// uk_artifact_run_rev is the schema backstop if the lock is ever
		// bypassed.
		var maxRev struct{ Max *int }
		if err := tx.Model(&model.AgentEvidenceArtifact{}).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("MAX(revision) as max").
			Where("run_id = ? AND user_id = ?", runID, userID).
			Scan(&maxRev).Error; err != nil {
			return err
		}
		revision := 1
		if maxRev.Max != nil {
			revision = *maxRev.Max + 1
		}

		ts := now()

		// Insert the artifact, retrying on a lost REVISION race. Two distinct
		// losers are possible with OnConflict{DoNothing} (any unique key →
		// RowsAffected 0):
		//   (a) another tx froze the SAME content first → adopt its row;
		//   (b) another tx froze DIFFERENT content and claimed our revision
		//       (uk_artifact_run_rev rejected us) → re-read max and retry.
		// Without the retry, case (b) would surface as a spurious freeze error.
		const revisionRetries = 3
		won := false
		for attempt := 0; attempt <= revisionRetries; attempt++ {
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
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(art)
			if res.Error != nil {
				return res.Error
			}

			// Do not infer insert-vs-conflict from RowsAffected. GORM maps
			// DoNothing to ON DUPLICATE KEY UPDATE on MySQL, and a DSN with
			// clientFoundRows=true reports the conflict as one affected row.
			// Read the unique content key back instead: our UUID means we won;
			// another UUID means an identical-content writer won; no row means
			// the conflict was only on (run_id, revision) and we must retry.
			var persisted model.AgentEvidenceArtifact
			perr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("run_id = ? AND content_hash = ?", runID, hash).
				First(&persisted).Error
			if perr == nil && persisted.ArtifactID == art.ArtifactID {
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
				won = true
				break
			}
			if perr == nil {
				m, err := loadManifestTx(tx, userID, persisted.ArtifactID, true)
				if err != nil {
					return err
				}
				artifact, manifest, created = &persisted, m, false
				return nil
			}
			if !errors.Is(perr, gorm.ErrRecordNotFound) {
				return perr
			}

			// Lost a race. Locking read for the recovery: under REPEATABLE
			// READ a plain First would reuse the transaction snapshot taken
			// before the winner committed and return ErrRecordNotFound,
			// making the freeze fail silently.
			var winner model.AgentEvidenceArtifact
			werr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("run_id = ? AND content_hash = ?", runID, hash).
				First(&winner).Error
			if werr == nil {
				m, err := loadManifestTx(tx, userID, winner.ArtifactID, true)
				if err != nil {
					return err
				}
				artifact, manifest, created = &winner, m, false
				return nil
			}
			if !errors.Is(werr, gorm.ErrRecordNotFound) {
				return werr
			}
			// Case (b): revision race — someone else owns our revision with
			// different content. Re-read the max (locking, so the retry sees
			// the newly committed row) and try the next revision.
			if attempt == revisionRetries {
				break
			}
			var nextMax struct{ Max *int }
			if err := tx.Model(&model.AgentEvidenceArtifact{}).
				Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("MAX(revision) as max").
				Where("run_id = ? AND user_id = ?", runID, userID).
				Scan(&nextMax).Error; err != nil {
				return err
			}
			revision = 1
			if nextMax.Max != nil {
				revision = *nextMax.Max + 1
			}
		}
		if !won {
			return fmt.Errorf("freeze: lost revision race for run %s after %d retries", runID, revisionRetries)
		}
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
	man, err := loadManifestTx(s.db.WithContext(ctx), userID, artifactID, false)
	if err != nil {
		return nil, nil, err
	}
	entries, err := DecodeEntries(man.Entries)
	if err != nil {
		return nil, nil, err
	}
	return man, entries, nil
}

func loadManifestTx(tx *gorm.DB, userID, artifactID string, locking bool) (*model.AgentCitationManifest, error) {
	var man model.AgentCitationManifest
	query := tx.Where("artifact_id = ? AND user_id = ?", artifactID, userID)
	if locking {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.First(&man).Error; err != nil {
		return nil, fmt.Errorf("load manifest for artifact %s: %w", artifactID, err)
	}
	return &man, nil
}

// GetFrozenManifestByRun returns the first (lowest-revision) manifest for a run and its
// decoded entries. found=false (nil error) when the run has no manifest yet —
// the caller then freezes one. Owner-scoped by user_id.
//
// This is the freeze-once read: concurrent first-freezes may persist later
// artifact revisions, but all hot paths adopt revision 1 so evidence growth
// cannot renumber citations already emitted.
func (s *Store) GetFrozenManifestByRun(ctx context.Context, userID, runID string) (*model.AgentCitationManifest, []StableID, bool, error) {
	var man model.AgentCitationManifest
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND user_id = ?", runID, userID).
		Order("revision ASC").
		First(&man).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	entries, err := DecodeEntries(man.Entries)
	if err != nil {
		return nil, nil, false, err
	}
	return &man, entries, true, nil
}

// GetLatestArtifactBySession returns the highest-revision artifact for a
// (user, session), owner-scoped. found=false (nil error) when none exists.
// Used by the finish gate to read coverage facts (truncated, channel_count,
// failed_channels) at finalize time.
func (s *Store) GetLatestArtifactBySession(ctx context.Context, userID, sessionID string) (*model.AgentEvidenceArtifact, bool, error) {
	var art model.AgentEvidenceArtifact
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("revision DESC").
		First(&art).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &art, true, nil
}

// GetLatestBySession returns the manifest of the highest-revision artifact for a
// (user, session), plus its decoded entries. found=false (nil error) when the
// session has no frozen artifact. Owner-scoped by user_id.
//
// This is retained as SS-08 scaffolding and for migration tests only; no
// production path calls it. Save-time adoption is request/run scoped through
// GetFrozenManifestByRun, so the shipped schema intentionally carries no
// session lookup index for this dormant helper.
//
// Ordering is created_at DESC with artifact_id as a deterministic tiebreaker.
// The previous revision DESC ordering was wrong: revision is allocated PER RUN
// (max+1 scoped by run_id in FreezeFromPool), so every run's first freeze is
// revision 1 — in any session with ≥2 runs the old ordering tied at rev 1 and
// picked an arbitrary run's manifest, and an old run that re-fetched (rev ≥2)
// deterministically beat the just-completed run.
//
// Callers must still prove the manifest belongs to THEIR request: the
// save-time path resolves the run via request_id and reads
// GetFrozenManifestByRun instead, because a request that froze nothing must
// not adopt an earlier run's manifest.
func (s *Store) GetLatestBySession(ctx context.Context, userID, sessionID string) (*model.AgentCitationManifest, []StableID, bool, error) {
	var art model.AgentEvidenceArtifact
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("created_at DESC, artifact_id DESC").
		First(&art).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	man, entries, err := s.LoadManifest(ctx, userID, art.ArtifactID)
	if err != nil {
		return nil, nil, false, err
	}
	return man, entries, true, nil
}

func countChannels(entries []StableID) int {
	seen := make(map[string]bool)
	for _, e := range entries {
		seen[e.ChannelID] = true
	}
	return len(seen)
}
