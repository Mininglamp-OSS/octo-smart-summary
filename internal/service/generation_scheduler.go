package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// QueueDueGenerations is the managed branch of the existing Schedule engine.
// It never rebuilds personal rows or advances a success watermark at admission.
func (s *ContentService) QueueDueGenerations(ctx context.Context, now time.Time, spaces []string) error {
	if len(spaces) == 0 || s.sourceAuthorizer == nil {
		return nil
	}
	var schedules []model.SummarySchedule
	column := "summary_schedule.space_id"
	if s.db.Dialector.Name() == "mysql" {
		column = "BINARY summary_schedule.space_id"
	}
	if err := s.db.WithContext(ctx).Where(column+" IN ? AND deleted_at IS NULL AND is_active = ? AND next_run_at <= ?", spaces, 1, now).
		Where("EXISTS (SELECT 1 FROM summary_task WHERE summary_task.schedule_id = summary_schedule.id AND summary_task.deleted_at IS NULL AND summary_task.content_protocol_version > 0 AND summary_task.generation_spec_json IS NOT NULL)").
		Order("next_run_at ASC").Limit(100).Find(&schedules).Error; err != nil {
		return err
	}
	for _, schedule := range schedules {
		if _, err := s.QueueScheduledGeneration(ctx, schedule.ID, now); err != nil {
			// One unavailable source/configuration must not starve other schedules.
			var ce *ContentError
			if !errors.As(err, &ce) {
				return err
			}
		}
	}
	return nil
}

func (s *ContentService) QueueScheduledGeneration(ctx context.Context, scheduleID int64, now time.Time) (*model.SummaryGenerationRun, error) {
	var out *model.SummaryGenerationRun
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		writer := s.withDB(tx)
		var schedule model.SummarySchedule
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted_at IS NULL", scheduleID).First(&schedule).Error; err != nil {
			return err
		}
		if schedule.IsActive != 1 || schedule.NextRunAt == nil || schedule.NextRunAt.After(now) {
			return nil
		}
		var tasks []model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("schedule_id = ? AND deleted_at IS NULL", schedule.ID).Order("id DESC").Limit(1).Find(&tasks).Error; err != nil {
			return err
		}
		if len(tasks) != 1 || tasks[0].ContentProtocolVersion == 0 || len(tasks[0].GenerationSpecJSON) == 0 {
			return nil
		}
		task := tasks[0]
		target := ContentTarget{SpaceID: task.SpaceID, TaskID: task.ID, Kind: ContentPersonal, UserID: task.CreatorID}
		l, err := writer.lockContent(ctx, task.SpaceID, task.ID, task.CreatorID, target.ID())
		if err != nil {
			return err
		}
		if schedule.SpaceID != task.SpaceID || schedule.CreatorID != task.CreatorID {
			return contentError("configuration_forbidden", 403)
		}
		if err := writer.requireSingleGeneration(l.access, target, task.CreatorID); err != nil {
			return err
		}
		if l.current == nil {
			return contentError("content_not_found", 404)
		}
		slot, next, err := ResolveGenerationSlot(schedule, now)
		if err != nil {
			return err
		}
		// Busy slots are coalesced/skipped, not run later under a different anchor.
		var active int64
		if err := tx.Model(&model.SummaryGenerationRun{}).Where("task_id = ? AND active_slot IS NOT NULL", task.ID).Count(&active).Error; err != nil {
			return err
		}
		if active == 0 {
			var spec model.SummaryGenerationSpec
			if json.Unmarshal(task.GenerationSpecJSON, &spec) != nil {
				return contentError("configuration_repair_required", 409)
			}
			if err := writer.sourceAuthorizer.AuthorizeGenerationSources(ctx, task.SpaceID, task.CreatorID, spec.Sources); err != nil {
				return err
			}
			request := RegenerateContentRequest{ContentBaseline: ContentBaseline{ExpectedCurrentVersionID: l.current.VersionID, ExpectedContentRevision: l.current.ContentRevision},
				ExpectedConfigRevision: task.ConfigRevision, IdempotencyKey: fmt.Sprintf("schedule:%d:%s", schedule.ID, slot.Format(time.RFC3339Nano))}
			out, err = writer.queueFullGeneration(l, request, spec, &schedule, slot, generationHash("scheduled_generate", schedule.ID, slot))
			if err != nil {
				return err
			}
		}
		return tx.Model(&schedule).Update("next_run_at", next).Error
	})
	return out, err
}
