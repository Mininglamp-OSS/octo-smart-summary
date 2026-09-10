package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RefineContentRequest struct {
	ContentBaseline
	IdempotencyKey string `json:"idempotency_key"`
	Feedback       string `json:"feedback"`
}

// FrozenRefineInput contains only evidence the initiating actor may use. It is
// private server-side input, never serialized as part of a public run DTO.
type FrozenRefineInput struct {
	Content                string               `json:"content"`
	Citations              []model.Citation     `json:"citations"`
	TeamCitations          []model.TeamCitation `json:"team_citations"`
	GenerationSpecSnapshot model.JSON           `json:"generation_spec_snapshot"`
	Feedback               string               `json:"feedback"`
	MsgCount               int                  `json:"msg_count"`
	BaseTokenUsed          int                  `json:"base_token_used"`
}

func generationHash(parts ...any) string {
	encoded, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

// QueueRefine commits durable admission before any executor is woken. This
// method does not call a model and is independent of the HTTP request lifetime.
func (s *ContentService) QueueRefine(ctx context.Context, space string, taskID int64, actor, contentID string, request RefineContentRequest) (*model.SummaryGenerationRun, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 ||
		strings.TrimSpace(request.Feedback) == "" || utf8.RuneCountInString(request.Feedback) > 2000 {
		return nil, contentError("invalid_generation_request", 400)
	}
	var result *model.SummaryGenerationRun
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		writer := s.withDB(tx)
		l, err := writer.lockContent(ctx, space, taskID, actor, contentID)
		if err != nil {
			return err
		}
		key := generationHash(space, taskID, contentID, actor, request.IdempotencyKey)
		digest := generationHash("refine", request.ContentBaseline, request.Feedback)
		var previous model.SummaryGenerationRun
		err = tx.Where("idempotency_hash = ?", key).First(&previous).Error
		if err == nil {
			if previous.RequestHash != digest {
				return contentError("idempotency_conflict", 409)
			}
			result = &previous
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := l.checkBaseline(request.ContentBaseline); err != nil {
			return err
		}
		if err := l.checkEditableStage(); err != nil {
			return err
		}
		now := timezone.Now().Truncate(time.Microsecond)
		if err := l.normalize(actor, now); err != nil {
			return err
		}
		visible := *l.current
		writer.filterVersion(l.access, l.target, actor, &visible)
		if visible.CitationVisibility == "permission_hidden" {
			// Existing private citation payload is not input to a team rewrite.
			// Remove its markers too, so the model cannot claim hidden evidence.
			visible.Content = formalCitationMarker.ReplaceAllStringFunc(visible.Content, func(marker string) string {
				if strings.HasPrefix(marker, "[P") {
					return marker
				}
				return ""
			})
		}
		frozen := FrozenRefineInput{
			Content: visible.Content, Citations: visible.Citations, TeamCitations: visible.TeamCitations,
			GenerationSpecSnapshot: l.current.GenerationSpecSnapshot, Feedback: request.Feedback,
		}
		if l.pr != nil {
			frozen.MsgCount, frozen.BaseTokenUsed = l.pr.MsgCount, l.pr.TotalTokenUsed
		} else {
			id, _ := ParseContentVersionID(l.current.VersionID, l.target)
			var row model.SummaryResult
			if err := tx.First(&row, id.RowID).Error; err != nil {
				return err
			}
			frozen.MsgCount, frozen.BaseTokenUsed = row.TotalMsgCount, row.TotalTokenUsed
		}
		input, err := json.Marshal(frozen)
		if err != nil {
			return err
		}
		run := model.SummaryGenerationRun{
			ID: uuid.NewString(), SpaceID: space, TaskID: taskID, ContentID: contentID, ActorID: actor,
			OperationType: "refine", Executor: "refine", Scope: "content",
			IdempotencyHash: key, RequestHash: digest, EffectiveAt: now,
			BaseVersionID: l.current.VersionID, BaseContentRevision: l.current.ContentRevision,
			ConfigRevision: l.access.task.ConfigRevision, InputJSON: input, Status: "pending", Stage: "queued",
			CreatedAt: now, UpdatedAt: now,
		}
		if err := writer.reserveGeneration(&run); err != nil {
			return err
		}
		result = &run
		return nil
	})
	return result, err
}

// reserveGeneration requires the task row lock. Task-wide runs exclude
// independent content runs; admitted child runs share their parent's round but
// still exclude two children writing the same content.
func (s *ContentService) reserveGeneration(run *model.SummaryGenerationRun) error {
	var active []model.SummaryGenerationRun
	if err := s.db.Where("task_id = ? AND active_slot IS NOT NULL", run.TaskID).Find(&active).Error; err != nil {
		return err
	}
	if run.Scope != "task" && run.Scope != "content" {
		return contentError("invalid_generation_scope", 400)
	}
	parentFound := run.ParentGenerationID == nil
	for _, other := range active {
		if run.ParentGenerationID != nil && other.ID == *run.ParentGenerationID &&
			other.Scope == "task" && other.SpaceID == run.SpaceID && !other.CancelRequested {
			parentFound = true
			continue
		}
		sameRound := run.ParentGenerationID != nil && other.ParentGenerationID != nil &&
			*run.ParentGenerationID == *other.ParentGenerationID
		if run.Scope == "task" || other.Scope == "task" || other.ContentID == run.ContentID ||
			(run.ParentGenerationID != nil && !sameRound) {
			return contentError("generation_busy", 409)
		}
	}
	if !parentFound || (run.Scope == "task" && run.ParentGenerationID != nil) {
		return contentError("generation_parent_inactive", 409)
	}
	slot := generationHash(run.SpaceID, run.TaskID, run.Scope, run.ContentID)
	if run.Scope == "task" {
		slot = generationHash(run.SpaceID, run.TaskID, "task")
	}
	run.ActiveSlot = &slot
	return s.db.Create(run).Error
}

// withRun follows the same lock order as content writes, including callbacks.
func (s *ContentService) withRun(ctx context.Context, id string, fn func(*ContentService, *model.SummaryGenerationRun) error) error {
	var hint model.SummaryGenerationRun
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&hint).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return contentError("generation_not_found", 404)
		}
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", hint.TaskID).First(&task).Error; err != nil {
			return err
		}
		var run model.SummaryGenerationRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&run).Error; err != nil {
			return err
		}
		if run.ID != id || task.SpaceID != run.SpaceID {
			return contentError("generation_not_found", 404)
		}
		return fn(s.withDB(tx), &run)
	})
}

// RecoverableGenerations is a durable queue scan, not an in-memory job list.
// An expired execution stays in its active slot until reclaimed or cancelled.
func (s *ContentService) RecoverableGenerations(ctx context.Context, spaces []string, limit int) ([]model.SummaryGenerationRun, error) {
	if limit < 1 || limit > 100 {
		return nil, contentError("invalid_page_limit", 400)
	}
	runs := []model.SummaryGenerationRun{}
	if len(spaces) == 0 {
		return runs, nil
	}
	spaceColumn := "space_id"
	if s.db.Dialector.Name() == "mysql" {
		spaceColumn = "BINARY space_id"
	}
	executors := []string{"refine"}
	if s.sourceAuthorizer != nil {
		executors = append(executors, "workflow")
	}
	err := s.db.WithContext(ctx).Where(spaceColumn+" IN ? AND executor IN ? AND cancel_requested = ? AND active_slot IS NOT NULL",
		spaces, executors, false).
		Where("status = ? OR (status = ? AND lease_until <= ?)", "pending", "running", timezone.Now()).
		Order("created_at ASC").Limit(limit).Find(&runs).Error
	return runs, err
}

func (s *ContentService) ClaimGeneration(ctx context.Context, id string, lease time.Duration) (*model.SummaryGenerationRun, error) {
	if lease < time.Second || lease > 30*time.Minute {
		return nil, contentError("invalid_generation_lease", 400)
	}
	var claimed *model.SummaryGenerationRun
	var rejected error
	err := s.withRun(ctx, id, func(writer *ContentService, run *model.SummaryGenerationRun) error {
		now := timezone.Now()
		if run.CancelRequested || run.ActiveSlot == nil ||
			(run.Status != "pending" && !(run.Status == "running" && run.LeaseUntil != nil && !run.LeaseUntil.After(now))) {
			return contentError("generation_not_claimable", 409)
		}
		// Revoked membership/deleted tasks cannot be executed after a restart.
		l, err := writer.lockContent(ctx, run.SpaceID, run.TaskID, run.ActorID, run.ContentID)
		if err != nil {
			return writer.rejectUnauthorizedRun(run, err, &rejected)
		}
		if err := l.checkEditableStage(); err != nil {
			return writer.rejectUnauthorizedRun(run, err, &rejected)
		}
		until := now.Add(lease)
		run.ExecutionToken++
		run.LeaseUntil, run.Status, run.Stage = &until, "running", "refining"
		if run.Executor == "workflow" {
			if err := writer.requireSingleGeneration(l.access, l.target, run.ActorID); err != nil {
				return writer.rejectUnauthorizedRun(run, contentError("content_forbidden", 403), &rejected)
			}
			run.Stage = "generating"
		}
		if err := writer.db.Save(run).Error; err != nil {
			return err
		}
		claimed = run
		return nil
	})
	if err == nil && rejected != nil {
		return nil, rejected
	}
	return claimed, err
}

// Returning an authorization error from the transaction would roll back the
// cancellation and leave a revoked run occupying its slot forever. Commit the
// terminal state first, then return the rejection to the caller.
func (s *ContentService) rejectUnauthorizedRun(run *model.SummaryGenerationRun, err error, rejected *error) error {
	var ce *ContentError
	if !errors.As(err, &ce) {
		return err
	}
	if ce.HTTPStatus == 403 || ce.HTTPStatus == 404 {
		run.Status, run.ErrorCode, run.CancelRequested = "cancelled", "permission_revoked", true
	} else if ce.Code == "content_repair_required" || ce.Code == "version_repair_required" ||
		ce.Code == "configuration_repair_required" || ce.Code == "content_not_editable" {
		run.Status, run.ErrorCode = "failed", ce.Code
	} else {
		return err
	}
	run.Stage, run.ActiveSlot, run.LeaseUntil = "finished", nil, nil
	*rejected = err
	return s.db.Save(run).Error
}

func checkGenerationLease(run *model.SummaryGenerationRun, token int64, now time.Time) error {
	if run.CancelRequested || run.Status != "running" || run.ActiveSlot == nil ||
		run.ExecutionToken != token || run.LeaseUntil == nil || !run.LeaseUntil.After(now) {
		return contentError("generation_lease_lost", 409)
	}
	return nil
}

// CompleteRefine never fetches current evidence to reconstruct the input.
// A revision conflict saves one candidate; it does not clobber the current row.
func (s *ContentService) CompleteRefine(ctx context.Context, id string, token int64, body, usedModel string, tokens int) (*model.SummaryGenerationRun, error) {
	if err := validateContentBody(body); err != nil {
		return nil, err
	}
	if tokens < 0 || len(usedModel) > 50 {
		return nil, contentError("invalid_generation_output", 400)
	}
	var result *model.SummaryGenerationRun
	var rejected error
	err := s.withRun(ctx, id, func(writer *ContentService, run *model.SummaryGenerationRun) error {
		if run.Status == "completed" || run.Status == "conflict" {
			if run.ExecutionToken != token {
				return contentError("generation_lease_lost", 409)
			}
			result = run
			return nil
		}
		now := timezone.Now()
		if err := checkGenerationLease(run, token, now); err != nil {
			return err
		}
		if run.OperationType != "refine" || run.Executor != "refine" {
			return contentError("invalid_generation_operation", 409)
		}
		l, err := writer.lockContent(ctx, run.SpaceID, run.TaskID, run.ActorID, run.ContentID)
		if err != nil {
			return writer.rejectUnauthorizedRun(run, err, &rejected)
		}
		if err := l.checkEditableStage(); err != nil {
			return writer.rejectUnauthorizedRun(run, err, &rejected)
		}
		var input FrozenRefineInput
		if err := json.Unmarshal(run.InputJSON, &input); err != nil {
			return contentError("generation_input_invalid", 409)
		}
		if err := validateContentMarkers(body, input.Citations, input.TeamCitations); err != nil {
			return err
		}
		applied := l.current != nil && l.current.VersionID == run.BaseVersionID &&
			l.current.ContentRevision == run.BaseContentRevision
		v, err := l.appendRefinement(run, input, body, usedModel, tokens, now)
		if err != nil {
			return err
		}
		if applied {
			v.ContentRevision = l.current.ContentRevision + 1
			if err := l.applyVersion(v); err != nil {
				return err
			}
		}
		run.OutputVersionID, run.Applied = v.VersionID, applied
		run.ActiveSlot, run.LeaseUntil, run.Stage, run.Status = nil, nil, "finished", "completed"
		if !applied {
			run.Status, run.ConflictReason = "conflict", "content_conflict"
		}
		if err := writer.db.Save(run).Error; err != nil {
			return err
		}
		result = run
		return nil
	})
	if err == nil && rejected != nil {
		return nil, rejected
	}
	return result, err
}

func (l *lockedContent) appendRefinement(run *model.SummaryGenerationRun, input FrozenRefineInput, body, usedModel string, tokens int, now time.Time) (*FormalContentVersion, error) {
	parent, err := ParseContentVersionID(run.BaseVersionID, l.target)
	if err != nil || parent.Provisional {
		return nil, contentError("generation_input_invalid", 409)
	}
	// Preserve the content's source configuration while recording this actual
	// rewrite's model/engine/run. The original input remains frozen in the run.
	snapshot := map[string]any{}
	if len(input.GenerationSpecSnapshot) > 0 {
		if err := json.Unmarshal(input.GenerationSpecSnapshot, &snapshot); err != nil || snapshot == nil {
			return nil, contentError("generation_input_invalid", 409)
		}
	}
	snapshot["executor"], snapshot["model"], snapshot["generation_id"] = run.Executor, usedModel, run.ID
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	if l.target.Kind == ContentResult {
		version, err := GetNextVersion(l.service.db, l.target.TaskID)
		if err != nil {
			return nil, err
		}
		row := model.SummaryResult{
			TaskID: l.target.TaskID, Content: body, Version: version,
			OperationType: run.OperationType, OperationNote: input.Feedback, ParentResultID: &parent.RowID,
			BaseContentRevision: run.BaseContentRevision, GenerationID: &run.ID,
			GenerationSpecSnapshot: snapshotJSON, ModelVersion: usedModel,
			TotalMsgCount: input.MsgCount, TotalTokenUsed: input.BaseTokenUsed + tokens, CreatedBy: run.ActorID, GeneratedAt: now,
		}
		row.SetCitations(input.Citations)
		row.SetTeamCitations(input.TeamCitations)
		if err := l.service.db.Create(&row).Error; err != nil {
			return nil, err
		}
		v := (resultContentRepository{target: l.target}).dto(row)
		return &v, nil
	}
	version, err := GetNextPersonalVersion(l.service.db, l.target.TaskID, l.target.UserID)
	if err != nil {
		return nil, err
	}
	row := model.PersonalResultVersion{
		TaskID: l.target.TaskID, UserID: l.target.UserID, ParticipantRefID: l.pr.ParticipantRefID,
		Content: body, Version: version, OperationType: run.OperationType, OperationNote: input.Feedback,
		ParentVersionID: &parent.RowID, BaseContentRevision: run.BaseContentRevision, GenerationID: &run.ID,
		GenerationSpecSnapshot: snapshotJSON, ModelVersion: usedModel, TotalTokenUsed: input.BaseTokenUsed + tokens, MsgCount: input.MsgCount,
		CreatedBy: run.ActorID, GeneratedAt: now,
	}
	row.SetCitations(input.Citations)
	if err := l.service.db.Create(&row).Error; err != nil {
		return nil, err
	}
	v := (personalContentRepository{target: l.target}).dto(row)
	return &v, nil
}

func (l *lockedContent) applyVersion(v *FormalContentVersion) error {
	id, err := ParseContentVersionID(v.VersionID, l.target)
	if err != nil || id.Provisional {
		return contentError("version_not_found", 404)
	}
	var table any = &model.SummaryResult{}
	if l.pr != nil {
		table = &model.PersonalResultVersion{}
	}
	if err := l.service.db.Model(table).Where("id = ?", id.RowID).Update("content_revision", v.ContentRevision).Error; err != nil {
		return err
	}
	if err := l.setCurrent(v, true); err != nil {
		return err
	}
	if l.pr != nil {
		var row model.PersonalResultVersion
		if err := l.service.db.First(&row, id.RowID).Error; err != nil {
			return err
		}
		if err := l.service.db.Model(&model.PersonalResult{}).Where("id = ?", l.pr.ID).
			Updates(map[string]any{"generated_at": row.GeneratedAt, "msg_count": row.MsgCount,
				"total_token_used": row.TotalTokenUsed, "model_version": row.ModelVersion}).Error; err != nil {
			return err
		}
	}
	v.IsCurrent = true
	l.current = v
	return nil
}

// ApplyCandidate is intentionally distinct from Restore: it switches to an
// existing generated candidate, while Restore overwrites the current version.
func (s *ContentService) ApplyCandidate(ctx context.Context, space string, taskID int64, actor, contentID, generationID string, base ContentBaseline) (*FormalContentVersion, error) {
	return s.mutate(ctx, space, taskID, actor, contentID, base, func(l *lockedContent, now time.Time) error {
		var run model.SummaryGenerationRun
		if err := l.service.db.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND task_id = ?", generationID, taskID).First(&run).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return contentError("generation_not_found", 404)
			}
			return err
		}
		if run.SpaceID != space || run.ContentID != contentID || run.ID != generationID {
			return contentError("generation_not_found", 404)
		}
		if run.Status != "conflict" || run.Applied || run.CancelRequested {
			return contentError("generation_not_applicable", 409)
		}
		id, err := ParseContentVersionID(run.OutputVersionID, l.target)
		if err != nil || id.Provisional {
			return contentError("version_not_found", 404)
		}
		v, err := l.repo.version(id.RowID)
		if err != nil {
			return err
		}
		if err := l.validateStoredEvidence(id.RowID); err != nil {
			return err
		}
		v.ContentRevision = l.current.ContentRevision + 1
		if err := l.applyVersion(v); err != nil {
			return err
		}
		run.Applied, run.Status, run.ConflictReason = true, "completed", ""
		if err := l.service.db.Save(&run).Error; err != nil {
			return err
		}
		return l.audit(actor, "apply_candidate", v.VersionID, v.ContentRevision, now)
	})
}

func (s *ContentService) Generation(ctx context.Context, space string, taskID int64, actor, contentID, id string) (*model.SummaryGenerationRun, error) {
	if _, _, _, err := s.resolve(ctx, space, taskID, actor, contentID); err != nil {
		return nil, err
	}
	var run model.SummaryGenerationRun
	err := s.db.WithContext(ctx).Where("id = ? AND task_id = ?", id, taskID).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && (run.SpaceID != space || run.ContentID != contentID || run.ID != id)) {
		return nil, contentError("generation_not_found", 404)
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *ContentService) CancelGeneration(ctx context.Context, space string, taskID int64, actor, contentID, id string) error {
	return s.withRun(ctx, id, func(writer *ContentService, run *model.SummaryGenerationRun) error {
		if run.SpaceID != space || run.TaskID != taskID || run.ContentID != contentID {
			return contentError("generation_not_found", 404)
		}
		if _, err := writer.lockContent(ctx, space, taskID, actor, contentID); err != nil {
			return err
		}
		if run.Status == "cancelled" {
			return nil
		}
		if run.Status != "pending" && run.Status != "running" {
			return contentError("generation_already_finished", 409)
		}
		// Cancel admitted children atomically too; their tokens can no longer
		// submit outputs after a task-wide cancellation.
		if err := writer.db.Model(&model.SummaryGenerationRun{}).
			Where("parent_generation_id = ? AND active_slot IS NOT NULL", run.ID).
			Updates(map[string]any{"status": "cancelled", "cancel_requested": true, "active_slot": nil, "lease_until": nil}).Error; err != nil {
			return err
		}
		run.Status, run.Stage, run.CancelRequested, run.ActiveSlot, run.LeaseUntil = "cancelled", "finished", true, nil, nil
		return writer.db.Save(run).Error
	})
}

func (s *ContentService) FailGeneration(ctx context.Context, id string, token int64, code string) error {
	// Persist a bounded code, never raw provider output, prompts or credentials.
	if code != "model_failed" && code != "invalid_output" && code != "execution_timeout" {
		return contentError("invalid_generation_error", 400)
	}
	return s.withRun(ctx, id, func(writer *ContentService, run *model.SummaryGenerationRun) error {
		if err := checkGenerationLease(run, token, timezone.Now()); err != nil {
			return err
		}
		run.Status, run.Stage, run.ErrorCode, run.ActiveSlot, run.LeaseUntil = "failed", "finished", code, nil, nil
		return writer.db.Save(run).Error
	})
}
