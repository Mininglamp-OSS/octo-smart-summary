package handler

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// ReferencedSummaryArtifact is the unified product of the reference resolver.
// It carries whatever the caller is allowed to see for this task, plus
// metadata about the artifact type for API responses.
type ReferencedSummaryArtifact struct {
	Task          model.SummaryTask
	Type          string // "team_result" or "personal_result"
	Content       string
	Citations     []model.Citation
	TeamCitations []model.TeamCitation
	Snapshot      *model.Snapshot
	// Sources and historical metadata for traditional summaries without snapshots
	Sources []model.SummarySource
}

// ErrReferenceUnavailable is a structured rejection reason used when a
// referenced task cannot provide visible content to the caller.
//
// The Reason enum lists the values actually emitted by the resolver and
// referenceableFromLoaded (P2-5 fix, R4 yj):
//   - "not_found"            (task missing, soft-deleted, or caller unauthorized)
//   - "not_completed"        (authorized caller, task exists but not completed)
//   - "no_visible_content"   (authorized + completed but nothing to show)
//   - "error"                (transient DB failure loading the task itself)
//
// "forbidden" and "deleted" are collapsed into "not_found" at :63/:76 by
// design (existence-oracle avoidance, see P3) and MUST NOT be added back.
type ErrReferenceUnavailable struct {
	Reason string
	TaskID int64
}

func (e *ErrReferenceUnavailable) Error() string {
	return fmt.Sprintf("reference unavailable for task %d: %s", e.TaskID, e.Reason)
}

// resolveReferencedArtifact applies the unified resolution rules from the
// design doc: space isolation → not deleted → canAccessTaskDB → status=completed
// → prefer current_result_id → highest-version summary_result → fall back to
// caller-owned PersonalResult (never cross-user).
//
// Authorization is checked BEFORE status so that unauthorized callers cannot
// distinguish "exists but not completed" from "does not exist" — both return
// an opaque "not_found" reason (P3 fix).
//
// "Visible" is consistent with detail-page citation visibility: BY_PERSON does
// not leak other participants' PersonalResult.
func resolveReferencedArtifact(
	ctx context.Context,
	db *gorm.DB,
	taskID int64,
	spaceID string,
	userID string,
) (*ReferencedSummaryArtifact, error) {
	// 1. Load task, space-scoped, not deleted.
	var task model.SummaryTask
	err := db.WithContext(ctx).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).
		First(&task).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &ErrReferenceUnavailable{Reason: "not_found", TaskID: taskID}
		}
		// P2-5: Distinguish real DB errors from not-found. A transient DB
		// failure (connection, timeout) should not be reported as "not_found"
		// which would mislead the caller into thinking the task doesn't exist.
		log.Printf("[reference] DB error loading task %d: %v", taskID, err)
		return nil, &ErrReferenceUnavailable{Reason: "error", TaskID: taskID}
	}

	// 2. Access check BEFORE status check (P3): unauthorized callers get
	// opaque "not_found" regardless of task status, preventing existence
	// oracle via not_completed vs not_found distinction.
	if !canAccessTaskDB(db.WithContext(ctx), userID, task.ID, task.CreatorID) {
		return nil, &ErrReferenceUnavailable{Reason: "not_found", TaskID: taskID}
	}

	// 3. Completed check (only reachable after authorization).
	if task.Status != model.StatusCompleted {
		return nil, &ErrReferenceUnavailable{Reason: "not_completed", TaskID: taskID}
	}

	// 4. Try team-level SummaryResult first (covers manual, scheduled, bot, and
	// agent team results). Use queryDisplayResult which respects
	// current_result_id and falls back to highest version.
	result, err := queryDisplayResult(db.WithContext(ctx), task.ID)
	if err == nil && result.ID != 0 && result.Content != "" {
		// Determine citations visibility per detail-page privacy rules.
		plainCitations := result.GetCitations()
		citationsVisible := callerPlainCitationsVisible(db.WithContext(ctx), &task, userID, &result)
		if !citationsVisible {
			plainCitations = []model.Citation{}
		}
		// Load sources for historical metadata.
		var sources []model.SummarySource
		if err := db.WithContext(ctx).Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
			log.Printf("[reference] DB error loading sources for task %d: %v", task.ID, err)
			sources = nil // fail closed: no sources rather than silently incomplete
		}

		return &ReferencedSummaryArtifact{
			Task:          task,
			Type:          "team_result",
			Content:       result.Content,
			Citations:     plainCitations,
			TeamCitations: result.GetTeamCitations(),
			Snapshot:      nil,
			Sources:       sources,
		}, nil
	}

	// 5. Fall back to caller's own PersonalResult (agent summaries, or
	// BY_PERSON single-participant results). No cross-user fallback.
	// Uses the same content!='' predicate as hasNonEmptyPersonalResult /
	// nonEmptyPersonalResultTaskIDs so the list/detail referenceable flag
	// and this resolver step agree on what "visible" means (P1-1, R4 yj).
	var pr model.PersonalResult
	err = db.WithContext(ctx).
		Where("task_id = ? AND user_id = ? AND content != ?", task.ID, userID, "").
		Order("id DESC").
		First(&pr).Error
	if err == nil && pr.ID != 0 {
		// Only load sources if there is no snapshot — sources are rendered
		// only in the no-snapshot (traditional summary) branch (P2-4).
		var sources []model.SummarySource
		if pr.GetSnapshot() == nil {
			if err := db.WithContext(ctx).Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
				log.Printf("[reference] DB error loading sources for task %d: %v", task.ID, err)
				sources = nil
			}
		}

		return &ReferencedSummaryArtifact{
			Task:          task,
			Type:          "personal_result",
			Content:       pr.Content,
			Citations:     pr.GetCitations(),
			TeamCitations: []model.TeamCitation{},
			Snapshot:      pr.GetSnapshot(),
			Sources:       sources,
		}, nil
	}

	// 6. No visible product.
	log.Printf("[reference] no visible content for task %d, user %s", taskID, userID)
	return nil, &ErrReferenceUnavailable{Reason: "no_visible_content", TaskID: taskID}
}

// referenceableFromLoaded computes referenceable status from data already
// loaded by the list/detail handler, avoiding redundant per-row queries (P1-3).
//
// Parameters:
//   - task: the full task row (already space-scoped and deleted_at-filtered)
//   - hasResult: whether a display SummaryResult exists (from pickDisplayResult)
//   - resultContent: the display result's content (for empty-content guard)
//   - isParticipant: whether the caller is creator or participant
//   - hasPersonalResult: whether the caller has a PersonalResult for this task
//     (P1-1: agent summaries write PersonalResult, not SummaryResult)
func referenceableFromLoaded(
	task model.SummaryTask,
	hasResult bool,
	resultContent string,
	isParticipant bool,
	hasPersonalResult bool,
) (referenceable bool, artifactType string, unavailableReason string) {
	// Authorization (P3: before status).
	if !isParticipant {
		return false, "", "not_found"
	}

	if task.Status != model.StatusCompleted {
		return false, "", "not_completed"
	}

	// Team result with non-empty content (P2-2: guard against empty content).
	if hasResult && resultContent != "" {
		return true, "team_result", ""
	}

	// P1-1: For agent summaries (no team SummaryResult), check the caller's
	// own PersonalResult. If one exists, the task is referenceable as a
	// personal_result — this is exactly what SUM-19 needs to unify across
	// summary types (agent summaries write PersonalResult, not SummaryResult).
	if hasPersonalResult {
		return true, "personal_result", ""
	}

	return false, "", "no_visible_content"
}

// maxReferencedTaskIDs caps the number of *distinct* referenced task IDs
// accepted by the chat path to prevent unbounded query fanout (P1-3).
//
// Callers MUST dedupReferencedTaskIDs first and validate the *deduped*
// length against this cap; otherwise a payload of {999×25, 1, 2, 3} would
// exhaust the budget on duplicates and drop the unique IDs (R4 yj P2-2,
// R5 yj P3, R5 ms P2-4, R5 jx non-blocking #1 — all four reviewers
// converged on "dedup-then-check at the binding layer").
const maxReferencedTaskIDs = 20

// dedupReferencedTaskIDs preserves first-seen order and drops repeats.
// Exported at package scope so both the chat handlers (Chat / ChatStream)
// and the context builder (buildReferencedSummariesContext) can share one
// definition — this is the single source of truth for the referenced-ID
// contract, so the dedup semantics cannot drift between validate-at-binding
// and consume-at-build.
func dedupReferencedTaskIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// hasNonEmptyPersonalResult reports whether the caller owns a PersonalResult
// for taskID with non-empty content. This is the single-task form of the
// shared predicate used by the reference resolver, the detail endpoint's
// referenceable flag, and the list endpoint's batch build. All three MUST
// agree on what "visible" means to prevent the list/detail vs resolver
// divergence flagged in R3/R4 (yj/ms/jx). Returns (false, err) on real DB
// errors so callers can distinguish "no visible content" from "lookup
// failed" (P1-1 R4 yj: the batch call in ListSummaries silently swallowed
// its .Error, hiding every personal-result-only referenceable=true).
func hasNonEmptyPersonalResult(db *gorm.DB, taskID int64, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var count int64
	err := db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ? AND content != ?", taskID, userID, "").
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// nonEmptyPersonalResultTaskIDs is the batch form of hasNonEmptyPersonalResult.
// Returns the set of taskIDs (from the given slice) where the caller owns at
// least one PersonalResult with non-empty content. Selects only task_id so a
// list of up to page_size=100 tasks does not materialize the medium-text
// Content / SnapshotJSON columns just to populate a boolean map (P2-3 R4 yj).
func nonEmptyPersonalResultTaskIDs(db *gorm.DB, taskIDs []int64, userID string) (map[int64]bool, error) {
	out := make(map[int64]bool, len(taskIDs))
	if len(taskIDs) == 0 || userID == "" {
		return out, nil
	}
	var ids []int64
	err := db.Model(&model.PersonalResult{}).
		Where("task_id IN ? AND user_id = ? AND content != ?", taskIDs, userID, "").
		Distinct().
		Pluck("task_id", &ids).Error
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
