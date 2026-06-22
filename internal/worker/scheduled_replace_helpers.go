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

func syncScheduledTaskConfig(tx *gorm.DB, imDB *gorm.DB, sched model.SummarySchedule, task model.SummaryTask, now time.Time) error {
	if err := syncScheduledTaskSources(tx, imDB, task.ID, sched.SourceConfig); err != nil {
		return err
	}
	if err := syncScheduledTaskParticipants(tx, task, sched.ParticipantConfig, now); err != nil {
		return err
	}
	return nil
}

// buildScheduledTaskChildren creates the source / participant / personal_result
// subtable rows for a freshly INSERTed scheduled task. Under the 1->N model every
// run is a brand-new task_id, so its three subtables start empty and must be
// rebuilt from the schedule config (the source of truth). This is the same shape
// as syncScheduledTaskConfig but assumes no pre-existing children (no deletes) and
// always materializes the creator participant even when ParticipantConfig is empty.
//
// Returns the number of Accepted participants materialized. For a V5 CONFIRM
// schedule this is the count of already-confirmed members this round; 0 means the
// whole round must be skipped (nobody confirmed, including the creator — §4.3/Q2),
// and the caller terminates the task without producing a result.
func buildScheduledTaskChildren(tx *gorm.DB, imDB *gorm.DB, sched model.SummarySchedule, task model.SummaryTask, now time.Time) (int, error) {
	if err := buildScheduledTaskSources(tx, imDB, task.ID, sched.SourceConfig); err != nil {
		return 0, err
	}
	return buildScheduledTaskParticipants(tx, task, sched.ParticipantConfig, sched.ConfirmPolicy, now)
}

func buildScheduledTaskSources(tx *gorm.DB, imDB *gorm.DB, taskID int64, raw model.JSON) error {
	if len(raw) == 0 {
		return nil
	}
	var sources []scheduleSourceConfig
	if err := json.Unmarshal(raw, &sources); err != nil {
		return service.NewBizError(40000, "定时来源配置无效", http.StatusBadRequest)
	}
	for _, src := range sources {
		if src.SourceID == "" {
			return fmt.Errorf("scheduled source_id is required")
		}
		// Always resolve the canonical source name from the IM DB; never trust a
		// client-supplied source_name (the schedule-management UI can submit a raw
		// group_no/thread id as the "name", e.g. "groupNo____shortId"). Resolving
		// unconditionally keeps the stored name consistent with the instant-summary
		// path, which already drops source_name and lets the backend look it up.
		sourceName := service.ResolveSourceNameWithType(src.SourceID, src.SourceType, imDB)
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

// buildScheduledTaskParticipants materializes the participant + personal_result
// rows for a freshly INSERTed scheduled task. Behaviour is driven by the
// schedule's confirm_policy:
//
//   - AUTO (confirm_policy==SchedConfirmAuto): every participant (creator AND
//     configured others) is pre-Accepted with ConfirmedAt set. There is no human
//     confirmation step, so the AUTO dispatch path picks up the whole roster.
//     (Unchanged from the pre-V5 single-person fix.)
//
//   - CONFIRM (V5 one-time / schedule-level confirm, any non-AUTO policy): the
//     schedule's participant_config carries an embedded confirm state per member
//     (model.ParseScheduleParticipantConfig). This round materializes ONLY members
//     that have already confirmed (creator included — Q2: the creator is NOT
//     auto-accepted anymore, it must confirm too) as Accepted + personal_result
//     (方案乙). Un-confirmed members (including an un-confirmed creator) are NOT
//     materialized this round: no participant row, no personal_result, no PR, no
//     dispatch, not counted in meta. If ZERO members are confirmed (creator
//     included) the whole round is skipped — no children are built and the caller
//     terminates the task without producing a result (§4.3 / Q2).
//
// Returns the number of Accepted participants actually materialized so the caller
// can detect a zero-confirmed CONFIRM round and skip it.
func buildScheduledTaskParticipants(tx *gorm.DB, task model.SummaryTask, raw model.JSON, confirmPolicy int, now time.Time) (int, error) {
	autoAccept := confirmPolicy == model.SchedConfirmAuto

	if autoAccept {
		return buildScheduledTaskParticipantsAuto(tx, task, raw, now)
	}
	return buildScheduledTaskParticipantsConfirm(tx, task, raw, now)
}

// buildScheduledTaskParticipantsAuto pre-accepts the whole roster (creator +
// configured others). The participant_config may be in the legacy bare-array
// shape or the V5 object shape; both normalize through
// model.ParseScheduleParticipantConfig.
func buildScheduledTaskParticipantsAuto(tx *gorm.DB, task model.SummaryTask, raw model.JSON, now time.Time) (int, error) {
	cfg := model.ParseScheduleParticipantConfig(raw)
	roster := cfg.EffectiveUserIDs(task.CreatorID) // creator always included
	nameOf := buildParticipantNameLookup(cfg)

	count := 0
	for _, uid := range roster {
		if err := materializeAcceptedParticipant(tx, task, uid, nameOf(uid), now); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// buildScheduledTaskParticipantsConfirm implements the V5 CONFIRM 方案乙 dispatch:
// only already-confirmed members (creator included) are materialized as Accepted
// this round; un-confirmed members are skipped entirely.
func buildScheduledTaskParticipantsConfirm(tx *gorm.DB, task model.SummaryTask, raw model.JSON, now time.Time) (int, error) {
	cfg := model.ParseScheduleParticipantConfig(raw)
	// Make sure the creator is part of the confirm roster (Q2). EnsureCreatorEntry
	// only mutates the in-memory copy; the persisted schedule config is the source
	// of truth and is reset/updated via the confirm API / UpdateSchedule.
	cfg.EnsureCreatorEntry(task.CreatorID)
	nameOf := buildParticipantNameLookup(cfg)

	count := 0
	for _, uid := range cfg.EffectiveUserIDs(task.CreatorID) {
		if !cfg.IsConfirmed(uid) {
			// 方案乙: un-confirmed member (incl. un-confirmed creator) is NOT part of
			// this round — no participant, no personal_result, no dispatch, no meta.
			continue
		}
		if err := materializeAcceptedParticipant(tx, task, uid, nameOf(uid), now); err != nil {
			return count, err
		}
		count++
	}
	// count==0 (§4.3 / Q2): nobody confirmed (creator included) — the caller skips
	// the whole round (terminates the task without a result).
	return count, nil
}

// buildParticipantNameLookup returns a resolver that prefers the config-supplied
// user_name and falls back to service.ResolveUserName.
func buildParticipantNameLookup(cfg model.ScheduleParticipantConfig) func(string) string {
	names := make(map[string]string, len(cfg.Participants))
	for _, p := range cfg.Participants {
		if p.UserName != "" {
			names[p.UserID] = p.UserName
		}
	}
	return func(uid string) string {
		if n, ok := names[uid]; ok && n != "" {
			return n
		}
		return service.ResolveUserName(uid)
	}
}

// materializeAcceptedParticipant creates one Accepted participant + its pending
// personal_result and back-links personal_result_id (the shared shape used by
// both the AUTO and CONFIRM build paths).
func materializeAcceptedParticipant(tx *gorm.DB, task model.SummaryTask, userID, userName string, now time.Time) error {
	if userID == "" {
		return nil
	}
	row := model.SummaryParticipant{
		TaskID:      task.ID,
		UserID:      userID,
		UserName:    userName,
		Status:      model.ParticipantAccepted,
		ConfirmedAt: &now,
	}
	if err := tx.Create(&row).Error; err != nil {
		return err
	}
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: row.ID,
		UserID:           userID,
		WorkerStatus:     model.PersonalStatusPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := tx.Create(&pr).Error; err != nil {
		return err
	}
	return tx.Model(&row).Update("personal_result_id", pr.ID).Error
}

func syncScheduledTaskSources(tx *gorm.DB, imDB *gorm.DB, taskID int64, raw model.JSON) error {
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
		// Always resolve the canonical source name from the IM DB; never trust a
		// client-supplied source_name (see buildScheduledTaskSources).
		sourceName := service.ResolveSourceNameWithType(src.SourceID, src.SourceType, imDB)
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

	// V5 §3.1: participant_config is the object form
	// {"participants":[...],"confirm_gate_passed":...}. Normalize through the single
	// parser so the V5 object form (and every legacy array shape) is handled; the
	// old bare-array Unmarshal failed on the V5 object form and returned a
	// "定时参与者配置无效" error for any V5 schedule.
	cfg := model.ParseScheduleParticipantConfig(raw)
	roster := cfg.EffectiveUserIDs(task.CreatorID) // creator always included, deduped
	nameOf := buildParticipantNameLookup(cfg)

	desired := make([]scheduleParticipantConfig, 0, len(roster))
	for _, uid := range roster {
		name := nameOf(uid)
		if name == "" {
			name = service.ResolveUserName(uid)
		}
		desired = append(desired, scheduleParticipantConfig{
			UserID:   uid,
			UserName: name,
		})
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
