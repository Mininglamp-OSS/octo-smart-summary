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
// design doc: space isolation → not deleted → status=completed → canAccessTaskDB
// → prefer current_result_id → highest-version summary_result → fall back to
// caller-owned PersonalResult (never cross-user).
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
		log.Printf("[reference] DB error loading task %d: %v", taskID, err)
		return nil, &ErrReferenceUnavailable{Reason: "not_found", TaskID: taskID}
	}

	// 2. Completed check.
	if task.Status != model.StatusCompleted {
		return nil, &ErrReferenceUnavailable{Reason: "not_completed", TaskID: taskID}
	}

	// 3. Access check: creator or explicit participant.
	if !canAccessTaskDB(db.WithContext(ctx), userID, task.ID, task.CreatorID) {
		return nil, &ErrReferenceUnavailable{Reason: "forbidden", TaskID: taskID}
	}

	// 4. Try team-level SummaryResult first (covers manual, scheduled, bot, and
	// agent team results). Use queryDisplayResult which respects
	// current_result_id and falls back to highest version.
	result, err := queryDisplayResult(db.WithContext(ctx), task.ID)
	if err == nil && result.ID != 0 {
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
	var pr model.PersonalResult
	err = db.WithContext(ctx).
		Where("task_id = ? AND user_id = ?", task.ID, userID).
		Order("id DESC").
		First(&pr).Error
	if err == nil && pr.ID != 0 && pr.Content != "" {
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

// checkReferenceable is a lightweight read-only check used by list/detail
// handlers to populate the `referenceable`, `reference_artifact_type`, and
// `reference_unavailable_reason` fields in API responses.
func checkReferenceable(
	ctx context.Context,
	db *gorm.DB,
	taskID int64,
	spaceID string,
	userID string,
) (referenceable bool, artifactType string, unavailableReason string) {
	art, err := resolveReferencedArtifact(ctx, db, taskID, spaceID, userID)
	if err != nil {
		var refErr *ErrReferenceUnavailable
		if errors.As(err, &refErr) {
			return false, "", refErr.Reason
		}
		return false, "", "error"
	}
	return true, art.Type, ""
}

// checkReferenceableFast is a lightweight check that determines whether a
// task is referenceable WITHOUT loading sources, content, citations, or
// snapshot. It only checks: task exists in space, not deleted, completed,
// caller has access, and a visible product (team result or personal result)
// exists. This avoids the N+1 query problem on list endpoints where we only
// need the boolean referenceable flag and artifact type, not the full content.
//
// For the list endpoint, callers should prefer this over checkReferenceable
// to avoid loading unnecessary sources/content per row.
func checkReferenceableFast(
	ctx context.Context,
	db *gorm.DB,
	taskID int64,
	spaceID string,
	userID string,
) (referenceable bool, artifactType string, unavailableReason string) {
	// 1. Load task, space-scoped, not deleted — only the columns we need.
	var task model.SummaryTask
	err := db.WithContext(ctx).
		Select("id", "space_id", "creator_id", "status", "deleted_at").
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).
		First(&task).Error
	if err != nil {
		return false, "", "not_found"
	}

	// 2. Completed check.
	if task.Status != model.StatusCompleted {
		return false, "", "not_completed"
	}

	// 3. Access check.
	if !canAccessTaskDB(db.WithContext(ctx), userID, task.ID, task.CreatorID) {
		return false, "", "forbidden"
	}

	// 4. Check if a team-level SummaryResult exists (current_result or highest
	// version) — just existence, no content loading.
	var resultCount int64
	if err := db.WithContext(ctx).Model(&model.SummaryResult{}).
		Where("task_id = ?", task.ID).
		Count(&resultCount).Error; err != nil {
		log.Printf("[reference-fast] DB error counting results for task %d: %v", task.ID, err)
		return false, "", "error"
	}
	if resultCount > 0 {
		// Verify the display result actually has content.
		result, err := queryDisplayResult(db.WithContext(ctx), task.ID)
		if err == nil && result.ID != 0 {
			return true, "team_result", ""
		}
	}

	// 5. Check caller's own PersonalResult — just existence, no content loading.
	var prCount int64
	if err := db.WithContext(ctx).Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ? AND content != ?", task.ID, userID, "").
		Count(&prCount).Error; err != nil {
		log.Printf("[reference-fast] DB error counting personal results for task %d: %v", task.ID, err)
		return false, "", "error"
	}
	if prCount > 0 {
		return true, "personal_result", ""
	}

	log.Printf("[reference-fast] no visible content for task %d, user %s", taskID, userID)
	return false, "", "no_visible_content"
}
