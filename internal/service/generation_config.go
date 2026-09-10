package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Implemented by the retrieval adapter, never by a client-provided assertion.
type GenerationSourceAuthorizer interface {
	AuthorizeGenerationSources(context.Context, string, string, []model.SummaryGenerationSource) error
}

type GenerationScheduleConfig struct {
	Enabled        bool   `json:"enabled"`
	IntervalDays   int    `json:"interval_days"`
	IntervalMonths int    `json:"interval_months"`
	RunTime        string `json:"run_time"`
	DayOfWeek      int    `json:"day_of_week"`
	DayOfMonth     int    `json:"day_of_month"`
	// Read-only compatibility field. New writes use interval recurrence.
	CronExpr string `json:"cron_expr,omitempty"`
}

type GenerationConfiguration struct {
	ContentGenerationConfig
	Spec      model.SummaryGenerationSpec `json:"spec"`
	Schedule  *GenerationScheduleConfig   `json:"schedule"`
	NextRunAt *time.Time                  `json:"next_run_at"`
}

type SaveGenerationConfigurationRequest struct {
	ExpectedConfigRevision int64                       `json:"expected_config_revision"`
	Spec                   model.SummaryGenerationSpec `json:"spec"`
	// nil preserves the existing schedule. An explicit disabled config pauses it.
	Schedule *GenerationScheduleConfig `json:"schedule,omitempty"`
	Generate *RegenerateContentRequest `json:"generate,omitempty"`
}

type SavedGenerationConfiguration struct {
	Configuration GenerationConfiguration     `json:"configuration"`
	Generation    *model.SummaryGenerationRun `json:"generation"`
}

func (s *ContentService) requireSingleGeneration(a contentAccess, target ContentTarget, actor string) error {
	if s.sourceAuthorizer == nil || s.maxWindowDays < 1 {
		return contentError("configuration_adapter_not_enabled", 503)
	}
	if a.main != target || target.Kind != ContentPersonal || target.UserID != actor ||
		a.task.CreatorID != actor || len(a.participants) != 1 || a.participants[0].UserID != actor {
		return contentError("generation_scope_not_supported", 422)
	}
	if a.participants[0].Status == model.ParticipantPending || a.participants[0].Status == model.ParticipantDeclined {
		return contentError("content_forbidden", 403)
	}
	return nil
}

// CheckExecutionTarget is a read-only route guard for the narrowly supported
// pilot. It never turns task ownership into permission on another report.
func (s *ContentService) CheckExecutionTarget(ctx context.Context, space string, taskID int64, actor, contentID string) error {
	a, target, _, err := s.resolve(ctx, space, taskID, actor, contentID)
	if err != nil {
		return err
	}
	return s.requireSingleGeneration(a, target, actor)
}

func validateGenerationSpec(spec model.SummaryGenerationSpec, actor string, maxDays int, now time.Time) error {
	if spec.SchemaVersion != 1 || spec.SummaryMode != model.ModeByPerson || spec.Collaboration != "single" ||
		len(spec.Participants) != 1 || spec.Participants[0] != actor {
		return contentError("generation_scope_not_supported", 422)
	}
	if spec.Requirement == nil || utf8.RuneCountInString(*spec.Requirement) > 2300 ||
		len(spec.Sources) == 0 || len(spec.Sources) > 50 {
		return contentError("invalid_generation_configuration", 422)
	}
	seen := map[string]bool{}
	for _, source := range spec.Sources {
		key := fmt.Sprintf("%d:%s", source.SourceType, source.SourceID)
		if source.SourceType < 1 || source.SourceType > 3 || source.SourceID == "" ||
			len(source.SourceID) > 64 || strings.TrimSpace(source.SourceID) != source.SourceID ||
			source.Confirmation != "user_confirmed" || seen[key] {
			return contentError("invalid_generation_sources", 422)
		}
		seen[key] = true
	}
	if spec.CitationRules.Policy != "message_evidence" {
		return contentError("unsupported_citation_policy", 422)
	}
	if spec.Template != nil && (spec.Template.ID == "" || len(spec.Template.ID) > 128 ||
		spec.Template.Version == "" || len(spec.Template.Version) > 128 ||
		strings.TrimSpace(spec.Template.Content) == "" || len(spec.Template.Content) > 32000) {
		return contentError("invalid_generation_template", 422)
	}
	for _, values := range [][]string{spec.Retrieval.AuthorIDs, spec.Retrieval.Keywords} {
		if len(values) > 50 {
			return contentError("invalid_retrieval_rules", 422)
		}
		seen := map[string]bool{}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 128 || seen[value] {
				return contentError("invalid_retrieval_rules", 422)
			}
			seen[value] = true
		}
	}
	_, err := ResolveGenerationTime(spec.TimeSelector, now, nil, maxDays)
	return err
}

func (s *ContentService) GenerationConfiguration(ctx context.Context, space string, taskID int64, actor, contentID string) (GenerationConfiguration, error) {
	a, target, _, err := s.resolve(ctx, space, taskID, actor, contentID)
	if err != nil {
		return GenerationConfiguration{}, err
	}
	if err := s.requireSingleGeneration(a, target, actor); err != nil {
		return GenerationConfiguration{}, err
	}
	return s.configurationOf(ctx, a.task, actor)
}

func (s *ContentService) configurationOf(ctx context.Context, task model.SummaryTask, actor string) (GenerationConfiguration, error) {
	out, err := s.configurationSeed(task, actor)
	if err != nil {
		return out, err
	}
	if len(task.GenerationSpecJSON) == 0 {
		var rows []model.SummarySource
		if err := s.db.WithContext(ctx).Where("task_id = ? AND derived = ?", task.ID, false).Find(&rows).Error; err != nil {
			return out, err
		}
		for _, row := range rows {
			out.Spec.Sources = append(out.Spec.Sources, model.SummaryGenerationSource{
				SourceID: row.SourceID, SourceType: row.SourceType, Confirmation: "legacy_unconfirmed",
			})
		}
	}
	if task.ScheduleID != nil {
		var schedule model.SummarySchedule
		err := s.db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", *task.ScheduleID).First(&schedule).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return out, err
		}
		if err == nil {
			if schedule.SpaceID != task.SpaceID || schedule.CreatorID != actor {
				return out, contentError("configuration_forbidden", 403)
			}
			out.Schedule = &GenerationScheduleConfig{Enabled: schedule.IsActive == 1, IntervalDays: schedule.IntervalDays,
				IntervalMonths: schedule.IntervalMonths, RunTime: schedule.RunTime, DayOfWeek: schedule.DayOfWeek, DayOfMonth: schedule.DayOfMonth, CronExpr: schedule.CronExpr}
			out.NextRunAt = schedule.NextRunAt
		}
	}
	return out, nil
}

// Read-only status shared with list projection. Sources and requirements never
// leave the service in the lightweight list DTO.
func (s *ContentService) configurationSeed(task model.SummaryTask, actor string) (GenerationConfiguration, error) {
	out := GenerationConfiguration{ContentGenerationConfig: ContentGenerationConfig{
		State: "incomplete", Revision: task.ConfigRevision, MissingFields: []string{},
	}}
	if len(task.GenerationSpecJSON) > 0 {
		if err := json.Unmarshal(task.GenerationSpecJSON, &out.Spec); err != nil {
			return out, contentError("configuration_repair_required", 409)
		}
		if err := validateGenerationSpec(out.Spec, actor, s.maxWindowDays, timezone.Now()); err != nil {
			out.State = "unavailable"
			code := err.Error()
			out.UnavailableReason = &code
		} else {
			out.State = "complete"
		}
	} else {
		out.Spec = model.SummaryGenerationSpec{
			SchemaVersion: 1, SummaryMode: model.ModeByPerson, Collaboration: "single", Participants: []string{actor},
			TimeSelector: model.SummaryTimeSelector{Mode: "relative", Days: 7, Timezone: timezone.Name},
			Sources:      []model.SummaryGenerationSource{}, CitationRules: model.SummaryCitationRules{Policy: "message_evidence"},
			Retrieval:    model.SummaryRetrievalRules{AuthorIDs: []string{}, Keywords: []string{}},
			FieldSources: map[string]string{"time_selector": "confirmation_required"},
		}
		// Never substitute title or a legacy Agent title-derived topic as a requirement.
		out.MissingFields = []string{"sources_confirmation", "requirement", "time_selector_confirmation"}
		out.Spec.FieldSources["sources"] = "legacy_explicit_sources"
	}
	return out, nil
}

// lockConfiguration preserves schedule -> task ordering. No LLM/retrieval call
// occurs in this transaction; the authorizer only checks source permissions.
func (s *ContentService) lockConfiguration(ctx context.Context, space string, taskID int64, actor, contentID string, hint model.SummaryTask) (*lockedContent, error) {
	if hint.ScheduleID != nil {
		var schedule model.SummarySchedule
		if err := s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted_at IS NULL", *hint.ScheduleID).First(&schedule).Error; err != nil {
			return nil, contentError("configuration_repair_required", 409)
		}
		if schedule.SpaceID != space || schedule.CreatorID != actor {
			return nil, contentError("configuration_forbidden", 403)
		}
	}
	l, err := s.lockContent(ctx, space, taskID, actor, contentID)
	if err != nil {
		return nil, err
	}
	a, b := hint.ScheduleID, l.access.task.ScheduleID
	if (a == nil) != (b == nil) || (a != nil && b != nil && *a != *b) {
		return nil, contentError("configuration_conflict", 409)
	}
	return l, s.requireSingleGeneration(l.access, l.target, actor)
}

func (s *ContentService) SaveGenerationConfiguration(ctx context.Context, space string, taskID int64, actor, contentID string, request SaveGenerationConfigurationRequest) (SavedGenerationConfiguration, error) {
	var out SavedGenerationConfiguration
	if request.ExpectedConfigRevision < 0 {
		return out, contentError("configuration_baseline_required", 400)
	}
	if err := validateGenerationSpec(request.Spec, actor, s.maxWindowDays, timezone.Now()); err != nil {
		return out, err
	}
	// Read the lock-order hint OUTSIDE the transaction. A plain SELECT before
	// the row locks would establish a stale InnoDB REPEATABLE READ snapshot.
	var hint model.SummaryTask
	if err := s.db.WithContext(ctx).Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, space).First(&hint).Error; err != nil {
		return out, contentError("content_not_found", 404)
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		writer := s.withDB(tx)
		l, err := writer.lockConfiguration(ctx, space, taskID, actor, contentID, hint)
		if err != nil {
			return err
		}
		if l.current == nil {
			return contentError("content_not_found", 404)
		}
		if err := l.checkEditableStage(); err != nil {
			return err
		}
		if request.Generate != nil {
			previous, err := writer.previousFullGeneration(space, taskID, actor, contentID, request.Generate.IdempotencyKey, generationHash("configure_generate", request))
			if err != nil {
				return err
			}
			if previous != nil {
				out.Generation = previous
				out.Configuration, err = writer.configurationOf(ctx, l.access.task, actor)
				return err
			}
		}
		if l.access.task.ConfigRevision != request.ExpectedConfigRevision {
			return contentError("configuration_conflict", 409)
		}
		if err := writer.sourceAuthorizer.AuthorizeGenerationSources(ctx, space, actor, request.Spec.Sources); err != nil {
			return err
		}
		spec := request.Spec
		spec.FieldSources = map[string]string{"sources": "user_confirmed", "requirement": "user_confirmed", "time_selector": "user_confirmed", "template": "user_confirmed", "retrieval": "user_confirmed"}
		encoded, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		// Configuration edits do not touch current content, revision or run input.
		if err := tx.Model(&model.SummaryTask{}).Where("id = ?", taskID).Updates(map[string]any{
			"generation_spec_json": model.JSON(encoded), "config_revision": request.ExpectedConfigRevision + 1, "content_protocol_version": ContentContractVersion,
			"topic": *spec.Requirement,
		}).Error; err != nil {
			return err
		}
		l.access.task.GenerationSpecJSON, l.access.task.ConfigRevision = encoded, request.ExpectedConfigRevision+1
		if err := writer.projectGenerationConfig(l, spec, request.Schedule); err != nil {
			return err
		}
		if request.Generate != nil {
			out.Generation, err = writer.queueFullGeneration(l, *request.Generate, spec, nil, timezone.Now().Truncate(time.Microsecond), generationHash("configure_generate", request))
			if err != nil {
				return err
			}
		}
		out.Configuration, err = writer.configurationOf(ctx, l.access.task, actor)
		return err
	})
	return out, err
}

func (s *ContentService) projectGenerationConfig(l *lockedContent, spec model.SummaryGenerationSpec, scheduleConfig *GenerationScheduleConfig) error {
	tx, task := s.db, l.access.task
	if err := tx.Where("task_id = ?", task.ID).Delete(&model.SummarySource{}).Error; err != nil {
		return err
	}
	for _, source := range spec.Sources {
		row := model.SummarySource{TaskID: task.ID, SourceType: source.SourceType, SourceID: source.SourceID}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
	}
	if scheduleConfig == nil && task.ScheduleID == nil {
		return nil
	}
	var schedule model.SummarySchedule
	if task.ScheduleID != nil {
		if err := tx.First(&schedule, *task.ScheduleID).Error; err != nil {
			return err
		}
	} else {
		if scheduleConfig == nil || !scheduleConfig.Enabled {
			return nil
		}
		schedule = model.SummarySchedule{SpaceID: task.SpaceID, CreatorID: task.CreatorID, Title: task.Title, SummaryMode: model.ModeByPerson, ConfirmPolicy: model.SchedConfirmAuto}
	}
	schedule.GenerationInstruction = *spec.Requirement
	sourceJSON, _ := json.Marshal(spec.Sources)
	schedule.SourceConfig = sourceJSON
	participants, _ := json.Marshal([]map[string]string{{"user_id": task.CreatorID}})
	schedule.ParticipantConfig = participants
	if scheduleConfig != nil {
		// Pausing a historical cron schedule must not require converting its
		// recurrence first. Its fields are preserved on this explicit pause.
		if !scheduleConfig.Enabled && schedule.ID != 0 {
			schedule.IsActive = 0
			return tx.Save(&schedule).Error
		}
		// Editing only the requirements/sources must not restart the schedule
		// from wall-clock time or erase an overdue slot. This also preserves an
		// existing cron projection without allowing creation of new cron rules.
		unchanged := schedule.ID != 0 && schedule.IsActive == 1 &&
			scheduleConfig.CronExpr == schedule.CronExpr &&
			scheduleConfig.IntervalDays == schedule.IntervalDays && scheduleConfig.IntervalMonths == schedule.IntervalMonths &&
			scheduleConfig.RunTime == schedule.RunTime && scheduleConfig.DayOfWeek == schedule.DayOfWeek &&
			scheduleConfig.DayOfMonth == schedule.DayOfMonth
		if unchanged {
			return tx.Save(&schedule).Error
		}
		if scheduleConfig.CronExpr != "" {
			return contentError("invalid_schedule_recurrence", 422)
		}
		if err := ValidateIntervalForWrite("", scheduleConfig.IntervalDays, scheduleConfig.IntervalMonths); err != nil {
			return contentError("invalid_schedule_recurrence", 422)
		}
		if scheduleConfig.RunTime == "" {
			return contentError("invalid_schedule_recurrence", 422)
		}
		for _, err := range []error{ValidateRunTime(scheduleConfig.RunTime), ValidateDayOfWeek(scheduleConfig.DayOfWeek), ValidateDayOfMonth(scheduleConfig.DayOfMonth)} {
			if err != nil {
				return contentError("invalid_schedule_recurrence", 422)
			}
		}
		schedule.CronExpr, schedule.IntervalDays, schedule.IntervalMonths = "", scheduleConfig.IntervalDays, scheduleConfig.IntervalMonths
		schedule.RunTime, schedule.DayOfWeek, schedule.DayOfMonth = scheduleConfig.RunTime, scheduleConfig.DayOfWeek, scheduleConfig.DayOfMonth
		schedule.AnchorDOM = scheduleConfig.DayOfMonth
		schedule.IsActive = 0
		if scheduleConfig.Enabled {
			schedule.IsActive = 1
			next, err := NextRunInitial("", schedule.IntervalDays, schedule.IntervalMonths, schedule.RunTime, schedule.DayOfWeek, schedule.DayOfMonth, timezone.Now())
			if err != nil {
				return contentError("invalid_schedule_recurrence", 422)
			}
			schedule.NextRunAt = &next
		}
	}
	// Legacy time_range_type is only a projection. Managed executors always
	// resolve the canonical selector, never this lossy compatibility value.
	schedule.TimeRangeType = 2
	if err := tx.Save(&schedule).Error; err != nil {
		return err
	}
	if task.ScheduleID == nil {
		if err := tx.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Update("schedule_id", schedule.ID).Error; err != nil {
			return err
		}
		l.access.task.ScheduleID = &schedule.ID
	}
	return nil
}
