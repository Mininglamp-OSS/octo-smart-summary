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
type ErrReferenceUnavailable struct {
	Reason string // "not_found" / "forbidden" / "not_completed" / "deleted" / "no_visible_content"
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
	// Uses the same predicate as checkReferenceableFast for consistency (P2-1).
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

// maxReferencedTaskIDs caps the number of referenced task IDs accepted by
// the chat path to prevent unbounded query fanout (P1-3).
const maxReferencedTaskIDs = 20
