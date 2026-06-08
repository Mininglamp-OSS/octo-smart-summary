package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

var errTaskNoLongerProcessing = errors.New("task no longer processing")

type scheduleSourceConfig struct {
	SourceType int    `json:"source_type"`
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
}

type scheduleParticipantConfig struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

func syncScheduledTaskConfig(tx *gorm.DB, sched model.SummarySchedule, task model.SummaryTask, now time.Time) error {
	if err := syncScheduledTaskSources(tx, task.ID, sched.SourceConfig); err != nil {
		return err
	}
	if err := syncScheduledTaskParticipants(tx, task, sched.ParticipantConfig, now); err != nil {
		return err
	}
	return nil
}

func syncScheduledTaskSources(tx *gorm.DB, taskID int64, raw model.JSON) error {
	if len(raw) == 0 {
		return nil
	}

	var sources []scheduleSourceConfig
	if err := json.Unmarshal(raw, &sources); err != nil {
		return service.NewBizError(40000, "定时来源配置无效", http.StatusBadRequest)
	}
	if err := tx.Where("task_id = ?", taskID).Delete(&model.SummarySource{}).Error; err != nil {
		return err
	}
	for _, src := range sources {
		if src.SourceID == "" {
			return fmt.Errorf("scheduled source_id is required")
		}
		sourceName := src.SourceName
		if sourceName == "" {
			sourceName = service.ResolveSourceNameWithType(src.SourceID, src.SourceType, nil)
		}
		if err := tx.Create(&model.SummarySource{
			TaskID:     taskID,
			SourceType: src.SourceType,
			SourceID:   src.SourceID,
			SourceName: sourceName,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func syncScheduledTaskParticipants(tx *gorm.DB, task model.SummaryTask, raw model.JSON, now time.Time) error {
	if len(raw) == 0 {
		return nil
	}

	var participants []scheduleParticipantConfig
	if err := json.Unmarshal(raw, &participants); err != nil {
		return service.NewBizError(40000, "定时参与者配置无效", http.StatusBadRequest)
	}

	desired := make([]scheduleParticipantConfig, 0, len(participants)+1)
	seen := make(map[string]struct{}, len(participants)+1)
	appendUser := func(userID, userName string) {
		if userID == "" {
			return
		}
		if _, ok := seen[userID]; ok {
			return
		}
		seen[userID] = struct{}{}
		if userName == "" {
			userName = service.ResolveUserName(userID)
		}
		desired = append(desired, scheduleParticipantConfig{
			UserID:   userID,
			UserName: userName,
		})
	}

	appendUser(task.CreatorID, "")
	for _, participant := range participants {
		appendUser(participant.UserID, participant.UserName)
	}

	if err := tx.Where("task_id = ?", task.ID).Delete(&model.PersonalResult{}).Error; err != nil {
		return err
	}
	if err := tx.Where("task_id = ?", task.ID).Delete(&model.SummaryParticipant{}).Error; err != nil {
		return err
	}

	for _, participant := range desired {
		row := model.SummaryParticipant{
			TaskID:      task.ID,
			UserID:      participant.UserID,
			UserName:    participant.UserName,
			Status:      model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}

		pr := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: row.ID,
			UserID:           participant.UserID,
			WorkerStatus:     model.PersonalStatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := tx.Create(&pr).Error; err != nil {
			return err
		}
		if err := tx.Model(&row).Update("personal_result_id", pr.ID).Error; err != nil {
			return err
		}
	}

	return nil
}

func markTaskCompleted(tx *gorm.DB, taskID int64) error {
	casResult := tx.Model(&model.SummaryTask{}).
		Where("id = ? AND status = ?", taskID, model.StatusProcessing).
		Updates(map[string]interface{}{
			"status":              model.StatusCompleted,
			"error_message":       nil,
			"processing_deadline": nil,
		})
	if casResult.Error != nil {
		return casResult.Error
	}
	if casResult.RowsAffected == 0 {
		return errTaskNoLongerProcessing
	}
	return nil
}

func completeTaskWithoutNewResult(db *gorm.DB, taskID int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return markTaskCompleted(tx, taskID)
	})
}

// saveLatestResultAndCompleteTask inserts the new result and marks the task Completed.
// isScheduled gates version retention: scheduled runs keep only the latest result (the bound
// task is overwritten in place each cycle); manual/normal/team-meta keep full version history.
func saveLatestResultAndCompleteTask(db *gorm.DB, taskID int64, result *model.SummaryResult, isScheduled bool) error {
	return db.Transaction(func(tx *gorm.DB) error {
		nextVer, err := service.GetNextVersion(tx, taskID)
		if err != nil {
			return err
		}
		result.TaskID = taskID
		result.Version = nextVer
		if err := tx.Create(result).Error; err != nil {
			return err
		}
		if isScheduled {
			// Scheduled-only: prune stale auto-generated prior-cycle versions after
			// the replacement result is durably inserted. Hand-edited rows
			// (edited_at IS NOT NULL) are retained permanently as user data, even
			// across later scheduled cycles.
			if err := tx.Where("task_id = ? AND id <> ? AND edited_at IS NULL", taskID, result.ID).Delete(&model.SummaryResult{}).Error; err != nil {
				return err
			}
			// summary_chunk currently has no version column, so cleanup must happen
			// only after the replacement result is durably inserted.
			if err := tx.Where("task_id = ?", taskID).Delete(&model.SummaryChunk{}).Error; err != nil {
				return err
			}
		}
		return markTaskCompleted(tx, taskID)
	})
}
