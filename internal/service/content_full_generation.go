package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type RegenerateContentRequest struct {
	ContentBaseline
	ExpectedConfigRevision int64  `json:"expected_config_revision"`
	IdempotencyKey         string `json:"idempotency_key"`
	// RequirementOverride, when non-nil, replaces the generation topic for this
	// single run only. It never touches summary_task.topic, generation_spec_json
	// or config_revision, and its run is excluded from the scheduled series'
	// incremental watermark (see successfulGenerationEnd). nil = use the saved
	// configuration's requirement unchanged.
	RequirementOverride *string `json:"requirement_override,omitempty"`
}

// regenerateOverrideOperation marks a one-shot ad-hoc-topic regenerate. It is
// still executor="workflow"; only the watermark query distinguishes it so an
// ad-hoc topic cannot consume the scheduled series' incremental start point.
const regenerateOverrideOperation = "regenerate_override"

type FrozenGenerationInput struct {
	Spec   model.SummaryGenerationSpec `json:"spec"`
	Time   ResolvedGenerationTime      `json:"time"`
	TaskNo string                      `json:"task_no"`
}

type FullGenerationOutput struct {
	Content   string
	Citations []model.Citation
	MsgCount  int
	Tokens    int
	Model     string
}

type FullGenerationExecutor interface {
	GenerateConfirmed(context.Context, model.SummaryGenerationRun, FrozenGenerationInput) (FullGenerationOutput, error)
}

func (s *ContentService) previousFullGeneration(space string, taskID int64, actor, contentID, key, digest string) (*model.SummaryGenerationRun, error) {
	if strings.TrimSpace(key) == "" || len(key) > 128 {
		return nil, contentError("invalid_generation_request", 400)
	}
	var run model.SummaryGenerationRun
	err := s.db.Where("idempotency_hash = ?", generationHash(space, taskID, contentID, actor, key)).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if run.RequestHash != digest {
		return nil, contentError("idempotency_conflict", 409)
	}
	return &run, nil
}

func (s *ContentService) QueueRegenerate(ctx context.Context, space string, taskID int64, actor, contentID string, request RegenerateContentRequest) (*model.SummaryGenerationRun, error) {
	var result *model.SummaryGenerationRun
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		writer := s.withDB(tx)
		l, err := writer.lockContent(ctx, space, taskID, actor, contentID)
		if err != nil {
			return err
		}
		if err := writer.requireSingleGeneration(l.access, l.target, actor); err != nil {
			return err
		}
		digest := generationHash("regenerate", request)
		result, err = writer.previousFullGeneration(space, taskID, actor, contentID, request.IdempotencyKey, digest)
		if err != nil || result != nil {
			return err
		}
		if request.ExpectedConfigRevision != l.access.task.ConfigRevision {
			return contentError("configuration_conflict", 409)
		}
		var spec model.SummaryGenerationSpec
		if json.Unmarshal(l.access.task.GenerationSpecJSON, &spec) != nil {
			return contentError("configuration_incomplete", 422)
		}
		// A one-shot topic override applies ONLY to this run's frozen spec. The
		// saved configuration (task.topic / generation_spec_json / config_revision)
		// is untouched; validateGenerationSpec inside queueFullGeneration enforces
		// the ≤2300-rune bound on the resulting requirement.
		if request.RequirementOverride != nil {
			override := strings.TrimSpace(*request.RequirementOverride)
			if override == "" {
				return contentError("invalid_generation_configuration", 422)
			}
			spec.Requirement = &override
		}
		if err := writer.sourceAuthorizer.AuthorizeGenerationSources(ctx, space, actor, spec.Sources); err != nil {
			return err
		}
		result, err = writer.queueFullGeneration(l, request, spec, nil, timezone.Now().Truncate(time.Microsecond), digest)
		return err
	})
	return result, err
}

// Watermarks come only from successfully APPLIED full runs of this confirmed
// configuration. Edits/restores/refine, admission, failure and stale candidates
// cannot move them. No mutable schedule.last_run_at is treated as evidence.
func (s *ContentService) successfulGenerationEnd(taskID, revision int64) (*time.Time, error) {
	var runs []model.SummaryGenerationRun
	err := s.db.Where("task_id = ? AND config_revision = ? AND executor = ? AND operation_type <> ? AND applied = ? AND status = ?",
		taskID, revision, "workflow", regenerateOverrideOperation, true, "completed").Order("effective_at DESC").Limit(1).Find(&runs).Error
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	var input FrozenGenerationInput
	if json.Unmarshal(runs[0].InputJSON, &input) != nil {
		return nil, contentError("generation_input_invalid", 409)
	}
	return &input.Time.End, nil
}

func (s *ContentService) queueFullGeneration(l *lockedContent, request RegenerateContentRequest, spec model.SummaryGenerationSpec, schedule *model.SummarySchedule, effectiveAt time.Time, digest string) (*model.SummaryGenerationRun, error) {
	if previous, err := s.previousFullGeneration(l.target.SpaceID, l.target.TaskID, l.target.UserID, l.target.ID(), request.IdempotencyKey, digest); err != nil || previous != nil {
		return previous, err
	}
	if err := l.checkBaseline(request.ContentBaseline); err != nil {
		return nil, err
	}
	if err := l.checkEditableStage(); err != nil {
		return nil, err
	}
	if err := validateGenerationSpec(spec, l.target.UserID, s.maxWindowDays, effectiveAt); err != nil {
		return nil, err
	}
	successEnd, err := s.successfulGenerationEnd(l.target.TaskID, l.access.task.ConfigRevision)
	if err != nil {
		return nil, err
	}
	resolved, err := ResolveGenerationTime(spec.TimeSelector, effectiveAt, successEnd, s.maxWindowDays)
	if err != nil {
		return nil, err
	}
	if err := l.normalize(l.target.UserID, timezone.Now()); err != nil {
		return nil, err
	}
	input, err := json.Marshal(FrozenGenerationInput{Spec: spec, Time: resolved, TaskNo: l.access.task.TaskNo})
	if err != nil {
		return nil, err
	}
	now := timezone.Now()
	run := model.SummaryGenerationRun{
		ID: uuid.NewString(), SpaceID: l.target.SpaceID, TaskID: l.target.TaskID, ContentID: l.target.ID(), ActorID: l.target.UserID,
		OperationType: "regenerate", Executor: "workflow", Scope: "task", EffectiveAt: resolved.EffectiveAt,
		IdempotencyHash: generationHash(l.target.SpaceID, l.target.TaskID, l.target.ID(), l.target.UserID, request.IdempotencyKey), RequestHash: digest,
		BaseVersionID: l.current.VersionID, BaseContentRevision: l.current.ContentRevision, ConfigRevision: l.access.task.ConfigRevision,
		InputJSON: input, Status: "pending", Stage: "queued", CreatedAt: now, UpdatedAt: now,
	}
	if schedule != nil {
		run.ScheduleID, run.ScheduledFor, run.OperationType = &schedule.ID, &resolved.EffectiveAt, "scheduled_generate"
	} else if request.RequirementOverride != nil {
		run.OperationType = regenerateOverrideOperation
	}
	if err := s.reserveGeneration(&run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *ContentService) ExecuteFullGeneration(ctx context.Context, id string, executor FullGenerationExecutor) (*model.SummaryGenerationRun, error) {
	if executor == nil {
		return nil, contentError("generation_executor_unavailable", 503)
	}
	run, err := s.ClaimGeneration(ctx, id, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	var input FrozenGenerationInput
	if run.Executor != "workflow" || json.Unmarshal(run.InputJSON, &input) != nil ||
		validateGenerationSpec(input.Spec, run.ActorID, s.maxWindowDays, run.EffectiveAt) != nil ||
		input.Time.Boundary != "[start,end)" || !input.Time.Start.Before(input.Time.End) {
		_ = s.FailGeneration(ctx, id, run.ExecutionToken, "invalid_output")
		return nil, contentError("generation_input_invalid", 409)
	}
	generationCtx, cancel := context.WithTimeout(ctx, 29*time.Minute)
	defer cancel()
	output, err := executor.GenerateConfirmed(generationCtx, *run, input)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if failure := s.FailGeneration(ctx, id, run.ExecutionToken, "model_failed"); failure != nil {
			return nil, failure
		}
		return nil, contentError("generation_execution_failed", 502)
	}
	result, err := s.CompleteFullGeneration(ctx, id, run.ExecutionToken, output)
	if err != nil {
		var ce *ContentError
		if errors.As(err, &ce) && (ce.Code == "invalid_content" || ce.Code == "invalid_generation_output" || ce.Code == "invalid_citation_reference") {
			_ = s.FailGeneration(ctx, id, run.ExecutionToken, "invalid_output")
		}
	}
	return result, err
}

func (s *ContentService) CompleteFullGeneration(ctx context.Context, id string, token int64, output FullGenerationOutput) (*model.SummaryGenerationRun, error) {
	if err := validateContentBody(output.Content); err != nil {
		return nil, err
	}
	if output.Tokens < 0 || output.MsgCount < 0 || len(output.Model) > 50 {
		return nil, contentError("invalid_generation_output", 400)
	}
	evidence, _ := json.Marshal(output.Citations)
	if len(output.Citations) > 0 && !validMessageEvidence(string(evidence)) {
		return nil, contentError("invalid_generation_output", 400)
	}
	if err := validateContentMarkers(output.Content, output.Citations, nil); err != nil {
		return nil, err
	}
	var result *model.SummaryGenerationRun
	var rejected error
	err := s.withRun(ctx, id, func(writer *ContentService, run *model.SummaryGenerationRun) error {
		if run.Executor != "workflow" {
			return contentError("invalid_generation_operation", 409)
		}
		if run.Status == "completed" || run.Status == "conflict" {
			if run.ExecutionToken != token {
				return contentError("generation_lease_lost", 409)
			}
			result = run
			return nil
		}
		if err := checkGenerationLease(run, token, timezone.Now()); err != nil {
			return err
		}
		l, err := writer.lockContent(ctx, run.SpaceID, run.TaskID, run.ActorID, run.ContentID)
		if err != nil {
			return writer.rejectUnauthorizedRun(run, err, &rejected)
		}
		if err := writer.requireSingleGeneration(l.access, l.target, run.ActorID); err != nil {
			return writer.rejectUnauthorizedRun(run, contentError("content_forbidden", 403), &rejected)
		}
		var input FrozenGenerationInput
		if json.Unmarshal(run.InputJSON, &input) != nil || input.Spec.Requirement == nil {
			return contentError("generation_input_invalid", 409)
		}
		if err := l.checkEditableStage(); err != nil {
			return writer.rejectUnauthorizedRun(run, err, &rejected)
		}
		if err := writer.sourceAuthorizer.AuthorizeGenerationSources(ctx, run.SpaceID, run.ActorID, input.Spec.Sources); err != nil {
			return writer.rejectUnauthorizedRun(run, contentError("content_forbidden", 403), &rejected)
		}
		snapshot, _ := json.Marshal(model.SummaryGenerationSnapshot{Spec: input.Spec, ConfigRevision: run.ConfigRevision,
			EffectiveAt: input.Time.EffectiveAt.Format(time.RFC3339Nano), ResolvedStart: input.Time.Start.Format(time.RFC3339Nano),
			ResolvedEnd: input.Time.End.Format(time.RFC3339Nano), Boundary: input.Time.Boundary, DataGapTruncated: input.Time.DataGapTruncated,
			Executor: run.Executor, Model: output.Model, GenerationID: run.ID})
		v, err := l.appendRefinement(run, FrozenRefineInput{Citations: output.Citations, GenerationSpecSnapshot: snapshot,
			Feedback: *input.Spec.Requirement, MsgCount: output.MsgCount}, output.Content, output.Model, output.Tokens, timezone.Now())
		if err != nil {
			return err
		}
		applied := l.current.VersionID == run.BaseVersionID && l.current.ContentRevision == run.BaseContentRevision
		if applied {
			v.ContentRevision = l.current.ContentRevision + 1
			if err := l.applyVersion(v); err != nil {
				return err
			}
		}
		run.OutputVersionID, run.Applied, run.Status, run.Stage = v.VersionID, applied, "completed", "finished"
		run.ActiveSlot, run.LeaseUntil = nil, nil
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
