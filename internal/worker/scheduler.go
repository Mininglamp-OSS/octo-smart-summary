package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var schedulerHTTPClient = &http.Client{Timeout: 5 * time.Second}

// StartScheduler starts the 4 cron scan jobs (every 60s).
func StartScheduler(db *gorm.DB, maxRetry int, workerTriggerURL string) *cron.Cron {
	c := cron.New()

	c.AddFunc("@every 60s", func() { scanPendingSchedules(db) })
	c.AddFunc("@every 60s", func() { scanConfirmTimeouts(db) })
	c.AddFunc("@every 60s", func() { scanStuckTasks(db, maxRetry) })
	c.AddFunc("@every 60s", func() { scanStuckPersonalTasks(db, workerTriggerURL) })

	c.Start()
	log.Println("[scheduler] started with 4 scan jobs (every 60s)")
	return c
}

// scanPendingSchedules requeues bound tasks from due schedules.
func scanPendingSchedules(db *gorm.DB) {
	now := timezone.Now()
	var schedules []model.SummarySchedule
	err := db.Where("is_active = 1 AND next_run_at <= ? AND deleted_at IS NULL", now).Find(&schedules).Error
	if err != nil {
		log.Printf("[scheduler] query schedules: %v", err)
		return
	}

	for _, sched := range schedules {
		taskID, claimed, err := claimAndCreateScheduledTask(db, sched, now)
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

func claimAndCreateScheduledTask(db *gorm.DB, sched model.SummarySchedule, now time.Time) (int64, bool, error) {
	if sched.NextRunAt == nil {
		return 0, false, nil
	}

	var task model.SummaryTask
	claimed := false
	requeued := false

	// The schedule claim (FOR UPDATE re-read + next_run advance) and the task reset share
	// ONE transaction: a reset failure rolls back the next_run_at advance too (60s scan
	// retries), avoiding a dropped cycle. Business skips (no bound task, overlapping
	// Processing, multi-person) still commit the advanced next_run_at to avoid re-scanning
	// forever; only real errors roll back.
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
			// The due snapshot was deactivated, retimed or already claimed after scan().
			// Skip instead of using stale schedule fields from the pre-scan snapshot.
			return nil
		}

		// Recompute from the FOR UPDATE re-read row so claim sees the latest config
		// even when non-recurrence fields changed without bumping next_run_at.
		nextRun, err := service.NextRunScheduledAdvance(lockedSched.CronExpr, lockedSched.IntervalDays, lockedSched.IntervalMonths, lockedSched.RunTime, lockedSched.DayOfWeek, lockedSched.DayOfMonth, *lockedSched.NextRunAt, now)
		if err != nil {
			log.Printf("[scheduler] ALERT schedule %d has invalid recurrence (%v); disabling to stop repeated re-scan/cost", lockedSched.ID, err)
			if err := tx.Model(&model.SummarySchedule{}).
				Where("id = ?", lockedSched.ID).
				Update("is_active", 0).Error; err != nil {
				return err
			}
			return nil
		}
		start, end := service.ComputeTimeRange(lockedSched.TimeRangeType, now)
		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Updates(map[string]interface{}{
				"last_run_at": now,
				"next_run_at": nextRun,
			}).Error; err != nil {
			return err
		}
		claimed = true

		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("schedule_id = ? AND deleted_at IS NULL", lockedSched.ID).
			Order("id DESC").
			First(&task).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				log.Printf("[scheduler] ALERT schedule %d claimed but no bound task found; skipping overwrite", lockedSched.ID)
				// Business skip: keep the advanced next_run_at (commit), no reset.
				return nil
			}
			return err
		}

		if task.Status == model.StatusProcessing {
			log.Printf("[scheduler] schedule %d task %d still processing; skipping overlapping overwrite", lockedSched.ID, task.ID)
			// Business skip (overlap protection): commit advanced next_run_at.
			return nil
		}

		// Method A guard: scheduled summary is single-person only this version. Multi-person
		// (team) tasks are driven by the human meta flow and must NOT be reset here. The claim
		// already advanced next_run_at, so a multi-person row is simply skipped (and re-claimed
		// + re-skipped next cycle) until team scheduling ships -- expected, not a bug.
		var participantCount int64
		if err := tx.Model(&model.SummaryParticipant{}).
			Where("task_id = ?", task.ID).
			Count(&participantCount).Error; err != nil {
			return err
		}
		if participantCount > 1 {
			log.Printf("[scheduler] schedule %d bound task %d is multi-person (%d participants); scheduled summary not supported for team tasks this version, skipping (next_run_at already advanced; expected re-skip next cycle)", lockedSched.ID, task.ID, participantCount)
			// Business skip (multi-person guard): commit advanced next_run_at.
			return nil
		}

		requeued = true
		if err := syncScheduledTaskConfig(tx, lockedSched, task, now); err != nil {
			return err
		}

		if err := tx.Model(&model.SummaryParticipant{}).
			Where("task_id = ?", task.ID).
			Updates(map[string]interface{}{
				"status":            model.ParticipantAccepted,
				"confirmed_at":      now,
				"worker_started_at": nil,
			}).Error; err != nil {
			return err
		}

		if err := tx.Model(&model.PersonalResult{}).
			Where("task_id = ?", task.ID).
			Updates(map[string]interface{}{
				"worker_status":    model.PersonalStatusPending,
				"content":          "",
				"citations_json":   "",
				"msg_count":        0,
				"total_token_used": 0,
				"model_version":    "",
				"error_message":    nil,
				"submitted_at":     nil,
				"generated_at":     nil,
				"edited_at":        nil,
			}).Error; err != nil {
			return err
		}

		if err := tx.Model(&model.SummaryTask{}).
			Where("id = ?", task.ID).
			Updates(map[string]interface{}{
				"time_range_start":    start,
				"time_range_end":      end,
				"status":              model.StatusPending,
				"trigger_type":        model.TriggerScheduled,
				"retry_count":         0,
				"error_message":       nil,
				"processing_deadline": nil,
				"confirm_deadline":    nil,
				"schedule_id":         lockedSched.ID,
			}).Error; err != nil {
			return err
		}

		return nil
	}); err != nil {
		// Technical failure: whole tx (including the next_run_at advance) rolled
		// back. next_run_at stays in the past so the next 60s scan retries.
		return 0, false, err
	}

	if !claimed {
		return 0, false, nil
	}

	// Only report a real requeue when we actually reset the task. Skipped cases
	// (no bound task, still processing, or multi-person guard) return taskID=0 so
	// scanPendingSchedules does not log a misleading "requeued task" line.
	if !requeued {
		return 0, true, nil
	}
	return task.ID, true, nil
}

// scanConfirmTimeouts auto-declines participants still in WaitingConfirm
// past the task's confirm_deadline.
func scanConfirmTimeouts(db *gorm.DB) {
	now := timezone.Now()

	// Find tasks with confirm_deadline passed that still have WaitingConfirm participants
	var taskIDs []int64
	db.Model(&model.SummaryTask{}).
		Where("confirm_deadline < ? AND confirm_deadline IS NOT NULL AND deleted_at IS NULL AND status NOT IN (?, ?, ?)",
			now, model.StatusCompleted, model.StatusFailed, model.StatusCancelled).
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
func scanStuckTasks(db *gorm.DB, maxRetry int) {
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

	// Fail tasks that exceeded max retries (also handle NULL deadline)
	failResult := db.Model(&model.SummaryTask{}).
		Where("status = ? AND (processing_deadline IS NULL OR processing_deadline < ?) AND retry_count >= ?",
			model.StatusProcessing, now, maxRetry-1).
		Updates(map[string]interface{}{
			"status":              model.StatusFailed,
			"processing_deadline": nil,
			"retry_count":         gorm.Expr("retry_count + 1"),
			"error_message":       "exceeded max retries",
		})
	if failResult.RowsAffected > 0 {
		log.Printf("[scheduler] failed %d stuck tasks (max retries exceeded)", failResult.RowsAffected)
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
			Update("worker_status", model.PersonalStatusPending)
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
