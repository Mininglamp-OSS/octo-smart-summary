package handler

import (
	"context"
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// ReferencedSummaryArtifact is the unified product of the reference resolver.
// It carries whatever the caller is allowed to see for this task, plus
// metadata about the artifact type for API responses.
type ReferencedSummaryArtifact struct {
	TaskID   int64
	Task     model.SummaryTask
	Type     string // "team_result" or "personal_result"
	Content  string
	Citations []model.Citation
	TeamCitations []model.TeamCitation
	Snapshot *model.Snapshot
	// Sources and historical metadata for traditional summaries without snapshots
	Sources      []model.SummarySource
	HasSnapshot  bool
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
		if !callerPlainCitationsVisible(db.WithContext(ctx), &task, userID, &result) {
			plainCitations = []model.Citation{}
		}
		// Load sources for historical metadata.
		var sources []model.SummarySource
		db.WithContext(ctx).Where("task_id = ?", task.ID).Find(&sources)

		return &ReferencedSummaryArtifact{
			TaskID:        task.ID,
			Task:          task,
			Type:          "team_result",
			Content:       result.Content,
			Citations:     plainCitations,
			TeamCitations: result.GetTeamCitations(),
			Snapshot:      nil,
			Sources:       sources,
			HasSnapshot:   false,
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
		var sources []model.SummarySource
		db.WithContext(ctx).Where("task_id = ?", task.ID).Find(&sources)

		return &ReferencedSummaryArtifact{
			TaskID:        task.ID,
			Task:          task,
			Type:          "personal_result",
			Content:       pr.Content,
			Citations:     pr.GetCitations(),
			TeamCitations: []model.TeamCitation{},
			Snapshot:      pr.GetSnapshot(),
			Sources:       sources,
			HasSnapshot:   pr.GetSnapshot() != nil,
		}, nil
	}

	// 6. No visible product.
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
		if refErr, ok := err.(*ErrReferenceUnavailable); ok {
			return false, "", refErr.Reason
		}
		return false, "", "error"
	}
	return true, art.Type, ""
}
