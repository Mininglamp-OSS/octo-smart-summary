// Package summaryrun persists and serializes agent-summary runs (SS-03).
//
// Two correctness guarantees live here, both enforced at the database layer
// rather than in application logic:
//
//   - Run-row idempotency: CreateOrGetRun relies on UNIQUE(user_id,
//     session_id, request_id). A retried submit (e.g. an SSE stream that
//     failed and downgraded to the non-streaming endpoint reusing the same
//     request_id) gets back the SAME run row instead of creating a second
//     one, and its spec is not re-persisted. We INSERT and let the unique
//     key reject duplicates — never "SELECT then INSERT", which races.
//
//     Scope, stated plainly: the dedup is at the run-row level only. A
//     replay still re-runs the answer path (fetch + summarize) in this
//     stage — skipping the work on replay is deferred. And because the
//     replay reuses the original run's frozen citation manifest, evidence
//     that appeared after the first attempt is not citable under the
//     replayed answer and is dropped from the chunk input.
//
//   - Serialized updates: run mutations go through an optimistic compare-and-swap
//     on Version, so two concurrent requests cannot fork run state.
package summaryrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// ErrConcurrentUpdate is returned when an optimistic CAS update finds the run
// already advanced by another writer (version mismatch).
var ErrConcurrentUpdate = errors.New("summaryrun: concurrent update (version mismatch)")

// now is indirected so tests can pin time; production uses time.Now.
var now = time.Now

// Store is the gorm-backed run/spec repository.
type Store struct {
	db *gorm.DB
}

// NewStore builds a Store over the given DB (the summary/application DB).
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// CreateOrGetRun idempotently returns the run for (userID, sessionID,
// requestID). If one already exists it is returned with created=false and no
// new work is scheduled; otherwise a fresh run is inserted (created=true).
//
// The dedup is a real INSERT guarded by the unique key, not a check-then-act,
// so concurrent first-time requests with the same request_id converge on one
// row.
func (s *Store) CreateOrGetRun(ctx context.Context, userID, sessionID, requestID, scopePolicy string) (*model.AgentSummaryRun, bool, error) {
	if scopePolicy != model.ScopePolicyOpen {
		scopePolicy = model.ScopePolicyClosed
	}
	ts := now()
	run := &model.AgentSummaryRun{
		RunID:       uuid.NewString(),
		UserID:      userID,
		SessionID:   sessionID,
		RequestID:   requestID,
		ScopePolicy: scopePolicy,
		Status:      model.RunStatusCreated,
		Version:     0,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}

	// GORM maps DoNothing to ON DUPLICATE KEY UPDATE on MySQL. Do not use
	// RowsAffected to distinguish insert from conflict: clientFoundRows=true
	// changes that value. Read the unique tuple back and compare UUIDs instead.
	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(run)
	if res.Error != nil {
		return nil, false, res.Error
	}
	existing, err := s.GetByRequest(ctx, userID, sessionID, requestID)
	if err != nil {
		return nil, false, err
	}
	return existing, existing.RunID == run.RunID, nil
}

// GetByRequest loads a run by its idempotency tuple. Owner-scoped by user_id.
func (s *Store) GetByRequest(ctx context.Context, userID, sessionID, requestID string) (*model.AgentSummaryRun, error) {
	var run model.AgentSummaryRun
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND request_id = ?", userID, sessionID, requestID).
		First(&run).Error
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// GetByID loads a run by run_id, owner-scoped by user_id so a guessed run_id
// from another user resolves to not-found.
func (s *Store) GetByID(ctx context.Context, userID, runID string) (*model.AgentSummaryRun, error) {
	var run model.AgentSummaryRun
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND user_id = ?", runID, userID).
		First(&run).Error
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// GetLatestSpec loads the highest-version Spec for a run and decodes it into a
// summaryspec.Spec. found=false (nil error) when the run has no spec yet.
// Owner-scoped: the spec is only returned if the run belongs to userID.
//
// This is the read that closes the "user requirement never reaches Map/Reduce"
// defect: the generation tools load the persisted Spec here instead of running
// a generic, request-blind prompt.
func (s *Store) GetLatestSpec(ctx context.Context, userID, runID string) (summaryspec.Spec, bool, error) {
	// Owner-scope through the run so a guessed run_id cannot read another user's
	// spec (agent_summary_spec has no user_id column).
	if _, err := s.GetByID(ctx, userID, runID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return summaryspec.Spec{}, false, nil
		}
		return summaryspec.Spec{}, false, err
	}

	var row model.AgentSummarySpec
	err := s.db.WithContext(ctx).
		Where("run_id = ?", runID).
		Order("version DESC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return summaryspec.Spec{}, false, nil
	}
	if err != nil {
		return summaryspec.Spec{}, false, err
	}

	var spec summaryspec.Spec
	if err := json.Unmarshal([]byte(row.SpecJSON), &spec); err != nil {
		return summaryspec.Spec{}, false, fmt.Errorf("decode spec_json: %w", err)
	}
	return spec, true, nil
}

// SaveSpec persists a new immutable Spec version for the run and CAS-advances
// the run's current spec pointer + status in one transaction. expectedVersion
// is the run's Version the caller observed; a mismatch means another writer
// advanced the run first and yields ErrConcurrentUpdate.
func (s *Store) SaveSpec(ctx context.Context, run *model.AgentSummaryRun, expectedVersion int, spec summaryspec.Spec, sources summaryspec.FieldSources, userRequest string) (*model.AgentSummarySpec, error) {
	specJSON, err := spec.JSON()
	if err != nil {
		return nil, err
	}
	srcJSON, err := sources.JSON()
	if err != nil {
		return nil, err
	}
	hash, err := spec.Hash()
	if err != nil {
		return nil, err
	}

	newSpecVersion := run.SpecVersion + 1
	row := &model.AgentSummarySpec{
		SpecID:       uuid.NewString(),
		RunID:        run.RunID,
		Version:      newSpecVersion,
		SpecHash:     hash,
		Objective:    spec.Objective,
		Topic:        spec.Topic,
		Audience:     spec.Audience,
		Language:     spec.Language,
		DetailLevel:  spec.DetailLevel,
		SpecJSON:     specJSON,
		FieldSources: srcJSON,
		UserRequest:  userRequest,
		CreatedAt:    now(),
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(row).Error; err != nil {
			return err
		}
		res := tx.Model(&model.AgentSummaryRun{}).
			Where("run_id = ? AND user_id = ? AND version = ?", run.RunID, run.UserID, expectedVersion).
			Updates(map[string]interface{}{
				"spec_id":      row.SpecID,
				"spec_version": newSpecVersion,
				"status":       model.RunStatusRunning,
				"version":      expectedVersion + 1,
				"updated_at":   now(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrConcurrentUpdate
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	run.SpecID = row.SpecID
	run.SpecVersion = newSpecVersion
	run.Status = model.RunStatusRunning
	run.Version = expectedVersion + 1
	return row, nil
}

// UpdateStatusCAS advances the run status under an optimistic version check.
// Owner-scoped by user_id to match every other read in this store — run_id
// is a server UUID so this is defense-in-depth, but the asymmetry would
// otherwise be copied by the next writer.
func (s *Store) UpdateStatusCAS(ctx context.Context, userID, runID string, expectedVersion int, status string) error {
	res := s.db.WithContext(ctx).Model(&model.AgentSummaryRun{}).
		Where("run_id = ? AND user_id = ? AND version = ?", runID, userID, expectedVersion).
		Updates(map[string]interface{}{
			"status":     status,
			"version":    expectedVersion + 1,
			"updated_at": now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrConcurrentUpdate
	}
	return nil
}

// GetLatestRunBySession returns the most recently updated run for a (user,
// session), owner-scoped. found=false (nil error) when none exists. Used at
// finalize time to attach the finish verdict to the run that just completed.
func (s *Store) GetLatestRunBySession(ctx context.Context, userID, sessionID string) (*model.AgentSummaryRun, bool, error) {
	var run model.AgentSummaryRun
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("updated_at DESC").
		First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &run, true, nil
}

// SetFinishStatus records the finish-gate verdict (COMPLETE/PARTIAL/FAILED) on a
// run. This is a terminal write (no optimistic CAS): the verdict is computed once
// at finalize and does not race concurrent status transitions.
func (s *Store) SetFinishStatus(ctx context.Context, runID, finishStatus string) error {
	res := s.db.WithContext(ctx).Model(&model.AgentSummaryRun{}).
		Where("run_id = ?", runID).
		Updates(map[string]interface{}{
			"finish_status": finishStatus,
			"updated_at":    now(),
		})
	return res.Error
}
