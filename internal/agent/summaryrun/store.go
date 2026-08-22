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
	return s.CreateOrGetRunWithFetchExpectation(ctx, userID, sessionID, requestID, scopePolicy, true)
}

// CreateOrGetRunWithFetchExpectation is CreateOrGetRun with the turn's fetch
// expectation stated explicitly. fetchExpected=false marks a turn that is
// fetch-free by design (SS-08b confident rewrite), so the finish gate does not
// disclose "coverage not measured" for a turn that was never allowed to measure.
func (s *Store) CreateOrGetRunWithFetchExpectation(ctx context.Context, userID, sessionID, requestID, scopePolicy string, fetchExpected bool) (*model.AgentSummaryRun, bool, error) {
	if scopePolicy != model.ScopePolicyOpen {
		scopePolicy = model.ScopePolicyClosed
	}
	ts := now()
	run := &model.AgentSummaryRun{
		RunID:              uuid.NewString(),
		UserID:             userID,
		SessionID:          sessionID,
		RequestID:          requestID,
		ScopePolicy:        scopePolicy,
		Status:             model.RunStatusCreated,
		AttemptedChannels:  "[]",
		SucceededChannels:  "[]",
		FailedChannels:     "[]",
		DiscoveredChannels: "[]",
		FetchExpected:      fetchExpected,
		Version:            0,
		CreatedAt:          ts,
		UpdatedAt:          ts,
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

// SetStatus force-sets the run's task-flow status without an optimistic version
// check. Used by the SS-07b tool-error hook to mark a run failed when a fatal
// tool error occurs mid-run (called concurrently from the tool worker pool, so
// it must not depend on a read-modify-write of Version).
func (s *Store) SetStatus(ctx context.Context, userID, runID, status string) error {
	return s.db.WithContext(ctx).Model(&model.AgentSummaryRun{}).
		Where("run_id = ? AND user_id = ?", runID, userID).
		Updates(map[string]interface{}{"status": status, "updated_at": now()}).Error
}

// ClearFailedStatusForReplay resets a run whose status was latched to failed by a
// PREVIOUS attempt under the same idempotency tuple, so a fresh attempt starts
// from a clean slate.
//
// Why this exists: the SS-07b fatal-tool marker lives in the per-runner hook
// closure, so OnToolSuccess can only clear a marker set by the SAME runner. An
// SSE downgrade (or any client retry) that reuses request_id resolves to the
// existing run row via CreateOrGetRun, builds a NEW runner with an empty fatal
// set, and therefore can never clear the failed status written by the previous
// attempt — a clean regenerated summary was then disclosed as FAILED by the
// finish gate. The run row is the only state shared across attempts, so the
// reset has to happen here, at the moment a replay is recognised.
//
// Scoped deliberately: only failed → running, only for the owner's run. A run
// already finished is left alone, and a genuine fatal error in the NEW attempt
// re-marks it through the hook as usual.
func (s *Store) ClearFailedStatusForReplay(ctx context.Context, userID, runID string) error {
	return s.db.WithContext(ctx).Model(&model.AgentSummaryRun{}).
		Where("run_id = ? AND user_id = ? AND status = ?", runID, userID, model.RunStatusFailed).
		Updates(map[string]interface{}{"status": model.RunStatusRunning, "updated_at": now()}).Error
}

// SetFinishStatus records the finish-gate verdict (COMPLETE/PARTIAL/FAILED) on a
// run. This is a terminal write (no optimistic CAS): the verdict is computed once
// at finalize and does not race concurrent status transitions.
func (s *Store) SetFinishStatus(ctx context.Context, userID, runID, finishStatus string) error {
	res := s.db.WithContext(ctx).Model(&model.AgentSummaryRun{}).
		Where("run_id = ? AND user_id = ?", runID, userID).
		Updates(map[string]interface{}{
			"finish_status": finishStatus,
			"updated_at":    now(),
		})
	return res.Error
}

// RecordChannelFetch records the latest outcome of one channel fetch. The row
// lock prevents parallel tool calls from losing each other's channel ids. A
// successful retry clears a previous failure for that channel; a later failure
// clears success so the final state describes the latest attempt.
func (s *Store) RecordChannelFetch(ctx context.Context, userID, runID, channelID string, succeeded, truncated bool) error {
	if channelID == "" {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run model.AgentSummaryRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("run_id = ? AND user_id = ?", runID, userID).
			First(&run).Error; err != nil {
			return err
		}

		attempted, err := decodeChannels(run.AttemptedChannels)
		if err != nil {
			return fmt.Errorf("decode attempted_channels: %w", err)
		}
		succeededChannels, err := decodeChannels(run.SucceededChannels)
		if err != nil {
			return fmt.Errorf("decode succeeded_channels: %w", err)
		}
		failed, err := decodeChannels(run.FailedChannels)
		if err != nil {
			return fmt.Errorf("decode failed_channels: %w", err)
		}

		attempted = addChannel(attempted, channelID)
		if succeeded {
			succeededChannels = addChannel(succeededChannels, channelID)
			failed = removeChannel(failed, channelID)
		} else {
			failed = addChannel(failed, channelID)
			succeededChannels = removeChannel(succeededChannels, channelID)
		}

		attemptedJSON, _ := json.Marshal(attempted)
		succeededJSON, _ := json.Marshal(succeededChannels)
		failedJSON, _ := json.Marshal(failed)
		return tx.Model(&model.AgentSummaryRun{}).
			Where("run_id = ? AND user_id = ?", runID, userID).
			Updates(map[string]interface{}{
				"coverage_measured":  true,
				"attempted_channels": string(attemptedJSON),
				"succeeded_channels": string(succeededJSON),
				"failed_channels":    string(failedJSON),
				"coverage_truncated": run.CoverageTruncated || truncated,
				"updated_at":         now(),
			}).Error
	})
}

// MarkOutputTruncated latches the fact that a model completion on this run's
// answer path was cut off by finish_reason=length.
//
// LATCH, never clear. The Reduce may be retried, and the planner emits several
// completions per run; a later untruncated call does not undo the fact that an
// earlier one was truncated, because the truncated text may already have been
// folded into what the user receives. Writing `false` would let a subsequent
// clean call erase the disclosure — the exact silent-completeness failure this
// path exists to prevent. Hence the guarded UPDATE below only ever flips 0 -> 1.
//
// Idempotent and lock-free: a single conditional UPDATE, so concurrent writers
// (parallel Map/Reduce tool calls) converge without the row lock that
// RecordChannelFetch needs for its read-modify-write JSON sets.
func (s *Store) MarkOutputTruncated(ctx context.Context, userID, runID string) error {
	if userID == "" || runID == "" {
		return nil
	}
	return s.db.WithContext(ctx).Model(&model.AgentSummaryRun{}).
		Where("run_id = ? AND user_id = ? AND output_truncated = ?", runID, userID, false).
		Updates(map[string]interface{}{
			"output_truncated": true,
			"updated_at":       now(),
		}).Error
}

// AddDroppedMessages atomically records messages discarded before the model.
// It is owner-scoped and additive because summarize_chunk calls may run in
// parallel for distinct handles.
func (s *Store) AddDroppedMessages(ctx context.Context, userID, runID string, count int) error {
	if count <= 0 {
		return nil
	}
	return s.db.WithContext(ctx).Model(&model.AgentSummaryRun{}).
		Where("run_id = ? AND user_id = ?", runID, userID).
		Updates(map[string]interface{}{
			"dropped_messages": gorm.Expr("dropped_messages + ?", count),
			"updated_at":       now(),
		}).Error
}

// RecordDiscoveredChannels unions channel ids the run learned are in scope.
//
// For an open-scope run the Spec pins nothing, so this is the only way the gate
// can tell "fetched everything in scope" from "found 12 channels and fetched 2".
// Union rather than replace: discovery happens across several tool calls
// (list_channels, narrow_channels_by_topic, find_shared_channels) and each sees
// only its own slice.
func (s *Store) RecordDiscoveredChannels(ctx context.Context, userID, runID string, channelIDs []string) error {
	if len(channelIDs) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run model.AgentSummaryRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("run_id = ? AND user_id = ?", runID, userID).
			First(&run).Error; err != nil {
			return err
		}
		discovered, err := decodeChannels(run.DiscoveredChannels)
		if err != nil {
			return fmt.Errorf("decode discovered_channels: %w", err)
		}
		before := len(discovered)
		for _, id := range channelIDs {
			if id != "" {
				discovered = addChannel(discovered, id)
			}
		}
		if len(discovered) == before {
			return nil
		}
		discoveredJSON, _ := json.Marshal(discovered)
		return tx.Model(&model.AgentSummaryRun{}).
			Where("run_id = ? AND user_id = ?", runID, userID).
			Updates(map[string]interface{}{
				"discovered_channels": string(discoveredJSON),
				"updated_at":          now(),
			}).Error
	})
}

func decodeChannels(raw string) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	var channels []string
	if err := json.Unmarshal([]byte(raw), &channels); err != nil {
		return nil, err
	}
	if channels == nil {
		channels = []string{}
	}
	return channels, nil
}

func addChannel(channels []string, channelID string) []string {
	for _, channel := range channels {
		if channel == channelID {
			return channels
		}
	}
	return append(channels, channelID)
}

func removeChannel(channels []string, channelID string) []string {
	out := channels[:0]
	for _, channel := range channels {
		if channel != channelID {
			out = append(out, channel)
		}
	}
	return out
}
