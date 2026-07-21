package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/notify"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var schedulerHTTPClient = &http.Client{Timeout: 5 * time.Second}

// StartScheduler starts the 4 cron scan jobs (every 60s).
// featureTeamSchedule, when true, lets multi-person schedules through the claim path
// instead of disabling them (FEATURE_TEAM_SCHEDULE).
func StartScheduler(db *gorm.DB, imDB *gorm.DB, maxRetry int, workerTriggerURL string, maxWindowDays int, featureTeamSchedule bool, notifier *notify.Notifier) *cron.Cron {
	c := cron.New()

	c.AddFunc("@every 60s", func() { scanPendingSchedules(db, imDB, maxWindowDays, featureTeamSchedule) })
	c.AddFunc("@every 60s", func() { scanConfirmTimeouts(db) })
	c.AddFunc("@every 60s", func() { scanStuckTasks(db, maxRetry, notifier) })
	c.AddFunc("@every 60s", func() { scanStuckPersonalTasks(db, workerTriggerURL) })
	if notifier != nil {
		// Background sweep for the notify state machine. Re-attempts rows stuck
		// at status='failed' with retry budget remaining (the synchronous worker
		// path is one-shot, so without this sweep MaxNotifyAttempts collapses
		// to 1 for the common case) and reclaims rows stuck at status='pending'
		// past their lease (worker crash mid-delivery). Both branches use atomic
		// CAS, so they cannot double-send with a concurrent OnTaskTerminal call.
		c.AddFunc("@every 60s", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
			defer cancel()
			notifier.Sweep(ctx)
		})
		c.Start()
		log.Println("[scheduler] started with 5 scan jobs (every 60s)")
		return c
	}

	c.Start()
	log.Println("[scheduler] started with 4 scan jobs (every 60s)")
	return c
}

// scanPendingSchedules requeues bound tasks from due schedules.
func scanPendingSchedules(db *gorm.DB, imDB *gorm.DB, maxWindowDays int, featureTeamSchedule bool) {
	now := timezone.Now()
	var schedules []model.SummarySchedule
	err := db.Where("is_active = 1 AND next_run_at <= ? AND deleted_at IS NULL", now).Find(&schedules).Error
	if err != nil {
		log.Printf("[scheduler] query schedules: %v", err)
		return
	}

	for _, sched := range schedules {
		taskID, claimed, err := claimAndRequeueScheduledTask(db, imDB, sched, now, maxWindowDays, featureTeamSchedule)
		if err != nil {
			log.Printf("[scheduler] create task for schedule %d: %v", sched.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		if taskID == 0 {
			continue
		}
		log.Printf("[scheduler] requeued task %d from schedule %d", taskID, sched.ID)
	}
}

func claimAndRequeueScheduledTask(db *gorm.DB, imDB *gorm.DB, sched model.SummarySchedule, now time.Time, maxWindowDays int, featureTeamSchedule bool) (int64, bool, error) {
	if sched.NextRunAt == nil {
		return 0, false, nil
	}

	var runTaskID int64
	claimed := false
	requeued := false

	// The schedule claim (FOR UPDATE re-read + next_run advance) and task requeue share
	// one transaction. Scheduled runs are modeled as new versions of the bound summary
	// task, not as new user-visible tasks: the left list keeps one item, and the result
	// history grows via scheduled_generate versions on that same task.
	if err := db.Transaction(func(tx *gorm.DB) error {
		var lockedSched model.SummarySchedule
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND deleted_at IS NULL", sched.ID).
			First(&lockedSched).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if lockedSched.IsActive != 1 || lockedSched.NextRunAt == nil || !lockedSched.NextRunAt.Equal(*sched.NextRunAt) || lockedSched.NextRunAt.After(now) {
			return nil
		}

		nextRun, err := service.NextRunScheduledAdvance(lockedSched.CronExpr, lockedSched.IntervalDays, lockedSched.IntervalMonths, lockedSched.RunTime, lockedSched.DayOfWeek, lockedSched.DayOfMonth, lockedSched.AnchorDOM, *lockedSched.NextRunAt, now)
		if err != nil {
			log.Printf("[scheduler] ALERT schedule %d has invalid recurrence (%v); disabling to stop repeated re-scan/cost", lockedSched.ID, err)
			return tx.Model(&model.SummarySchedule{}).
				Where("id = ?", lockedSched.ID).
				Update("is_active", 0).Error
		}
		start, end, err := service.ComputeTimeRange(lockedSched.TimeRangeType, now, lockedSched.LastRunAt, lockedSched.CronExpr, lockedSched.IntervalDays, lockedSched.IntervalMonths, maxWindowDays)
		if err != nil {
			log.Printf("[scheduler] ALERT schedule %d has invalid time range (%v); disabling to stop repeated re-scan/cost", lockedSched.ID, err)
			return tx.Model(&model.SummarySchedule{}).
				Where("id = ?", lockedSched.ID).
				Update("is_active", 0).Error
		}
		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Update("next_run_at", nextRun).Error; err != nil {
			return err
		}
		claimed = true

		acceptedCount := scheduledAcceptedParticipantCount(lockedSched, lockedSched.CreatorID)
		if acceptedCount == 0 {
			log.Printf("[scheduler] schedule %d: zero confirmed participants; skipping round without changing task (last_run_at preserved)", lockedSched.ID)
			return nil
		}

		if multiPerson, err := scheduleConfigMultiPerson(lockedSched); err != nil {
			return err
		} else if multiPerson && !featureTeamSchedule {
			log.Printf("[scheduler] ALERT schedule %d participant config is multi-person; scheduled summary not supported for team tasks (FEATURE_TEAM_SCHEDULE off), disabling + notifying creator", lockedSched.ID)
			return tx.Model(&model.SummarySchedule{}).
				Where("id = ?", lockedSched.ID).
				Update("is_active", 0).Error
		}

		var task model.SummaryTask
		createdTask := false
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("schedule_id = ? AND deleted_at IS NULL", lockedSched.ID).
			Order("id DESC").
			First(&task).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				task = model.SummaryTask{
					TaskNo:         service.GenerateTaskNo(),
					SpaceID:        lockedSched.SpaceID,
					CreatorID:      lockedSched.CreatorID,
					Title:          lockedSched.Title,
					Topic:          lockedSched.Title,
					SummaryMode:    lockedSched.SummaryMode,
					TimeRangeStart: start,
					TimeRangeEnd:   end,
					Status:         model.StatusPending,
					TriggerType:    model.TriggerScheduled,
					ScheduleID:     &lockedSched.ID,
					CreatedAt:      now,
					UpdatedAt:      now,
				}
				if err := tx.Create(&task).Error; err != nil {
					return err
				}
				createdTask = true
			} else {
				return err
			}
		}

		if !createdTask && (task.Status == model.StatusPending || task.Status == model.StatusWaitingConfirm || task.Status == model.StatusProcessing) {
			log.Printf("[scheduler] schedule %d task %d is non-terminal (status=%d); skipping overlapping run (last_run_at preserved)", lockedSched.ID, task.ID, task.Status)
			return nil
		}

		if err := preservePersonalResultVersionsBeforeRequeue(tx, task.ID); err != nil {
			return err
		}
		if err := tx.Where("task_id = ?", task.ID).Delete(&model.SummarySource{}).Error; err != nil {
			return err
		}
		if err := tx.Where("task_id = ?", task.ID).Delete(&model.PersonalResult{}).Error; err != nil {
			return err
		}
		if err := tx.Where("task_id = ?", task.ID).Delete(&model.SummaryNotification{}).Error; err != nil {
			return err
		}
		if err := tx.Where("task_id = ?", task.ID).Delete(&model.SummaryParticipant{}).Error; err != nil {
			return err
		}
		if err := buildScheduledTaskSources(tx, imDB, task.ID, lockedSched.SourceConfig); err != nil {
			return err
		}
		materialized, err := buildScheduledTaskParticipants(tx, task, lockedSched.ParticipantConfig, lockedSched.ConfirmPolicy, now)
		if err != nil {
			return err
		}
		if materialized == 0 {
			log.Printf("[scheduler] schedule %d: zero participants materialized; skipping round without running task %d", lockedSched.ID, task.ID)
			return nil
		}

		updates := map[string]interface{}{
			"title":               lockedSched.Title,
			"topic":               lockedSched.Title,
			"summary_mode":        lockedSched.SummaryMode,
			"time_range_start":    start,
			"time_range_end":      end,
			"status":              model.StatusPending,
			"trigger_type":        model.TriggerScheduled,
			"retry_count":         0,
			"error_message":       nil,
			"processing_deadline": nil,
		}
		if err := tx.Model(&model.SummaryTask{}).
			Where("id = ?", task.ID).
			Updates(updates).Error; err != nil {
			return err
		}

		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Update("last_run_at", now).Error; err != nil {
			return err
		}

		runTaskID = task.ID
		requeued = true
		return nil
	}); err != nil {
		return 0, false, err
	}

	if !claimed {
		return 0, false, nil
	}
	if !requeued {
		return 0, true, nil
	}
	return runTaskID, true, nil
}

func preservePersonalResultVersionsBeforeRequeue(tx *gorm.DB, taskID int64) error {
	var rows []model.PersonalResult
	if err := tx.Where("task_id = ?", taskID).Find(&rows).Error; err != nil {
		return err
	}
	for _, pr := range rows {
		if strings.TrimSpace(pr.Content) == "" {
			continue
		}
		var latest model.PersonalResultVersion
		err := tx.Where("task_id = ? AND user_id = ?", pr.TaskID, pr.UserID).
			Order("version DESC").Order("id DESC").First(&latest).Error
		if err == nil && latest.Content == pr.Content && latest.CitationsJSON == pr.CitationsJSON {
			if pr.CurrentVersionID == nil {
				if err := tx.Model(&model.PersonalResult{}).Where("id = ?", pr.ID).Update("current_version_id", latest.ID).Error; err != nil {
					return err
				}
			}
			continue
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		nextVer, err := service.GetNextPersonalVersion(tx, pr.TaskID, pr.UserID)
		if err != nil {
			return err
		}
		operationType := "generate"
		var parentID *int64
		if latest.ID > 0 {
			operationType = "manual_edit"
			parentID = &latest.ID
		}
		generatedAt := timezone.Now()
		if pr.EditedAt != nil {
			generatedAt = *pr.EditedAt
		} else if pr.GeneratedAt != nil {
			generatedAt = *pr.GeneratedAt
		} else if !pr.CreatedAt.IsZero() {
			generatedAt = pr.CreatedAt
		}
		snapshot := model.PersonalResultVersion{
			TaskID:           pr.TaskID,
			ParticipantRefID: pr.ParticipantRefID,
			UserID:           pr.UserID,
			Content:          pr.Content,
			CitationsJSON:    pr.CitationsJSON,
			MsgCount:         pr.MsgCount,
			TotalTokenUsed:   pr.TotalTokenUsed,
			ModelVersion:     pr.ModelVersion,
			Version:          nextVer,
			OperationType:    operationType,
			ParentVersionID:  parentID,
			CreatedBy:        pr.UserID,
			GeneratedAt:      generatedAt,
		}
		if err := tx.Create(&snapshot).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.PersonalResult{}).Where("id = ?", pr.ID).Update("current_version_id", snapshot.ID).Error; err != nil {
			return err
		}
		if err := service.PrunePersonalResultVersions(tx, pr.TaskID, pr.UserID, service.PersonalResultVersionKeepLimit); err != nil {
			return err
		}
	}
	return nil
}

// claimAndCreateScheduledTask is kept as a compatibility wrapper for older tests.
// Scheduled runs now requeue the bound task and append a new result version.
func claimAndCreateScheduledTask(db *gorm.DB, imDB *gorm.DB, sched model.SummarySchedule, now time.Time, maxWindowDays int, featureTeamSchedule bool) (int64, bool, error) {
	return claimAndRequeueScheduledTask(db, imDB, sched, now, maxWindowDays, featureTeamSchedule)
}

func scheduledAcceptedParticipantCount(sched model.SummarySchedule, creatorID string) int {
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	if sched.ConfirmPolicy == model.SchedConfirmAuto {
		return len(cfg.EffectiveUserIDs(creatorID))
	}
	cfg.EnsureCreatorEntry(creatorID)
	count := 0
	for _, uid := range cfg.EffectiveUserIDs(creatorID) {
		if cfg.IsConfirmed(uid) {
			count++
		}
	}
	return count
}

// scheduleConfigMultiPerson reports whether a schedule's participant config
// resolves to more than one distinct user (the implicit creator counts once).
// Scheduled summaries are single-person this version.
func scheduleConfigMultiPerson(sched model.SummarySchedule) (bool, error) {
	if len(sched.ParticipantConfig) == 0 {
		return false, nil
	}
	// V5 §3.1: participant_config is now an object form
	// {"participants":[...],"confirm_gate_passed":...}. Use the single
	// normalizer so the V5 object form (and every legacy array shape) is parsed
	// consistently; the old bare-array Unmarshal failed on the V5 object form and
	// silently returned false, bypassing the FEATURE_TEAM_SCHEDULE-off guard.
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	return len(cfg.EffectiveUserIDs(sched.CreatorID)) > 1, nil
}

// scanConfirmTimeouts auto-declines participants still in WaitingConfirm
// past the task's confirm_deadline.
//
// V5/Q5: scheduled runs no longer use a per-round confirm window (confirm is a
// one-time, schedule-level event and scheduled tasks never write
// confirm_deadline), so this scan is restricted to MANUAL tasks. A scheduled
// task can never produce a WaitingConfirm participant under V5, so excluding
// trigger_type=scheduled here is a no-op safety net that also ignores any legacy
// scheduled row that might still carry a confirm_deadline.
func scanConfirmTimeouts(db *gorm.DB) {
	now := timezone.Now()

	// Find MANUAL tasks with confirm_deadline passed that still have WaitingConfirm participants
	var taskIDs []int64
	db.Model(&model.SummaryTask{}).
		Where("confirm_deadline < ? AND confirm_deadline IS NOT NULL AND deleted_at IS NULL AND trigger_type = ? AND status NOT IN (?, ?, ?)",
			now, model.TriggerManual, model.StatusCompleted, model.StatusFailed, model.StatusCancelled).
		Pluck("id", &taskIDs)

	if len(taskIDs) == 0 {
		return
	}

	// Auto-decline timed-out participants
	result := db.Model(&model.SummaryParticipant{}).
		Where("task_id IN ? AND status = ?", taskIDs, model.ParticipantPending).
		Update("status", model.ParticipantDeclined)
	if result.RowsAffected > 0 {
		log.Printf("[scheduler] auto-declined %d timed-out participants", result.RowsAffected)
	}
}

// scanStuckTasks resets tasks stuck in processing past their deadline.
// Increments retry_count; if max retries exceeded, marks as Failed.
// notifier (optional) is fired once per task that this sweep transitions to
// Failed, AFTER the terminal-status write has committed.
func scanStuckTasks(db *gorm.DB, maxRetry int, notifier *notify.Notifier) {
	now := timezone.Now()

	// Reset tasks that can still retry (also handle NULL deadline for legacy data)
	result := db.Model(&model.SummaryTask{}).
		Where("status = ? AND (processing_deadline IS NULL OR processing_deadline < ?) AND retry_count < ?",
			model.StatusProcessing, now, maxRetry-1).
		Updates(map[string]interface{}{
			"status":              model.StatusPending,
			"processing_deadline": nil,
			"retry_count":         gorm.Expr("retry_count + 1"),
		})
	if result.RowsAffected > 0 {
		log.Printf("[scheduler] reset %d stuck tasks (retry incremented)", result.RowsAffected)
	}

	// Fail tasks that exceeded max retries. Two-step (snapshot IDs, then per-row
	// CAS UPDATE) so we can attribute exactly which IDs *this* sweep flipped to
	// Failed — a bulk UPDATE cannot return the rows, and a snapshot-then-bulk
	// shape would miss any task that crossed the deadline between the snapshot
	// and the UPDATE (review feedback P2-2: snapshot-then-update race could
	// drop the notification for a late-crosser since on the next sweep its
	// status is already Failed). The per-row CAS guards on the same predicates
	// the snapshot used, so concurrent transitions are absorbed (RowsAffected==0
	// means another worker / cancel got there first; we skip).
	var toFail []int64
	db.Model(&model.SummaryTask{}).
		Where("status = ? AND (processing_deadline IS NULL OR processing_deadline < ?) AND retry_count >= ?",
			model.StatusProcessing, now, maxRetry-1).
		Pluck("id", &toFail)

	var failedIDs []int64
	for _, id := range toFail {
		res := db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ? AND (processing_deadline IS NULL OR processing_deadline < ?) AND retry_count >= ?",
				id, model.StatusProcessing, now, maxRetry-1).
			Updates(map[string]interface{}{
				"status":              model.StatusFailed,
				"processing_deadline": nil,
				"retry_count":         gorm.Expr("retry_count + 1"),
				"error_message":       "exceeded max retries",
			})
		if res.Error == nil && res.RowsAffected == 1 {
			failedIDs = append(failedIDs, id)
		}
	}
	if len(failedIDs) > 0 {
		log.Printf("[scheduler] failed %d stuck tasks (max retries exceeded)", len(failedIDs))
	}

	// Notify exactly the tasks this sweep transitioned. Reload each so the
	// notification reflects the committed terminal row.
	if notifier != nil {
		for _, id := range failedIDs {
			var task model.SummaryTask
			if err := db.First(&task, id).Error; err != nil {
				continue
			}
			if task.Status != model.StatusFailed {
				continue
			}
			errMsg := ""
			if task.ErrorMessage != nil {
				errMsg = *task.ErrorMessage
			}
			notifier.OnTaskTerminal(task, model.StatusFailed, errMsg)
		}
	}
}

// scanStuckPersonalTasks resets personal summaries stuck in processing
// and detects accepted participants with PENDING personal_result that were never triggered.
func scanStuckPersonalTasks(db *gorm.DB, workerTriggerURL string) {
	now := timezone.Now()
	leaseTimeout := now.Add(-10 * time.Minute)

	// Find participants stuck in processing
	var stuck []model.SummaryParticipant
	db.Where("status = ? AND worker_started_at < ?",
		model.ParticipantProcessing, leaseTimeout).Find(&stuck)

	for _, p := range stuck {
		// Reset personal_result to PENDING
		db.Model(&model.PersonalResult{}).
			Where("participant_ref_id = ? AND worker_status = ?", p.ID, model.PersonalStatusProcessing).
			Updates(map[string]interface{}{
				"worker_status":  model.PersonalStatusPending,
				"workflow_stage": "",
			})
		// Reset participant to accepted
		db.Model(&p).Updates(map[string]interface{}{
			"status":            model.ParticipantAccepted,
			"worker_started_at": nil,
		})
		log.Printf("[scheduler] reset stuck personal task for participant %d", p.ID)

		// Re-trigger personal worker
		schedulerTriggerWorker(workerTriggerURL, model.WorkerTriggerRequest{
			Type:             "personal_summary",
			TaskID:           p.TaskID,
			ParticipantRefID: p.ID,
		})
	}

	// M3: Detect accepted participants with PENDING personal_result > 5 minutes
	stuckTimeout := now.Add(-5 * time.Minute)
	var acceptedStuck []model.SummaryParticipant
	db.Where("status = ? AND personal_result_id IS NOT NULL",
		model.ParticipantAccepted).Find(&acceptedStuck)

	for _, p := range acceptedStuck {
		var pr model.PersonalResult
		if err := db.Where("participant_ref_id = ? AND worker_status = ? AND created_at < ?",
			p.ID, model.PersonalStatusPending, stuckTimeout).First(&pr).Error; err != nil {
			continue
		}
		log.Printf("[scheduler] re-triggering stuck accepted participant %d (personal_result PENDING > 5min)", p.ID)
		schedulerTriggerWorker(workerTriggerURL, model.WorkerTriggerRequest{
			Type:             "personal_summary",
			TaskID:           p.TaskID,
			ParticipantRefID: p.ID,
		})
	}
}

func schedulerTriggerWorker(workerTriggerURL string, req model.WorkerTriggerRequest) {
	if workerTriggerURL == "" {
		return
	}
	body, err := json.Marshal(req)
	if err != nil {
		log.Printf("[scheduler] marshal trigger: %v", err)
		return
	}
	resp, err := schedulerHTTPClient.Post(workerTriggerURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[scheduler] trigger worker POST failed: %v", err)
		return
	}
	resp.Body.Close()
}
