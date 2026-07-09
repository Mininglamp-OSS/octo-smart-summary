package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
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
		taskID, claimed, err := claimAndCreateScheduledTask(db, imDB, sched, now, maxWindowDays, featureTeamSchedule)
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

func claimAndCreateScheduledTask(db *gorm.DB, imDB *gorm.DB, sched model.SummarySchedule, now time.Time, maxWindowDays int, featureTeamSchedule bool) (int64, bool, error) {
	if sched.NextRunAt == nil {
		return 0, false, nil
	}

	var newTask model.SummaryTask
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
		nextRun, err := service.NextRunScheduledAdvance(lockedSched.CronExpr, lockedSched.IntervalDays, lockedSched.IntervalMonths, lockedSched.RunTime, lockedSched.DayOfWeek, lockedSched.DayOfMonth, lockedSched.AnchorDOM, *lockedSched.NextRunAt, now)
		if err != nil {
			log.Printf("[scheduler] ALERT schedule %d has invalid recurrence (%v); disabling to stop repeated re-scan/cost", lockedSched.ID, err)
			if err := tx.Model(&model.SummarySchedule{}).
				Where("id = ?", lockedSched.ID).
				Update("is_active", 0).Error; err != nil {
				return err
			}
			return nil
		}
		start, end, err := service.ComputeTimeRange(lockedSched.TimeRangeType, now, lockedSched.LastRunAt, lockedSched.CronExpr, lockedSched.IntervalDays, lockedSched.IntervalMonths, maxWindowDays)
		if err != nil {
			log.Printf("[scheduler] ALERT schedule %d has invalid time range (%v); disabling to stop repeated re-scan/cost", lockedSched.ID, err)
			if err := tx.Model(&model.SummarySchedule{}).
				Where("id = ?", lockedSched.ID).
				Update("is_active", 0).Error; err != nil {
				return err
			}
			return nil
		}
		// Advance ONLY next_run_at here (the scheduling cadence) so a due schedule
		// is not re-scanned every 60s. last_run_at (the type=4 incremental window
		// anchor) must NOT advance at claim time: it is advanced later, only when we
		// actually requeue the task to run (see below). Coupling them here was the
		// bug -- a business skip would commit last_run_at=now without producing a
		// summary, permanently dropping the skipped window for type=4.
		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Updates(map[string]interface{}{
				"next_run_at": nextRun,
			}).Error; err != nil {
			return err
		}
		claimed = true

		// 1->N model: find the LATEST task under this schedule only to enforce the
		// overlap guard. We no longer reuse/reset it -- each run INSERTs a brand-new
		// task. ErrRecordNotFound (no task yet) is fine: the very first run has no
		// predecessor, so there is nothing to overlap with.
		var latest model.SummaryTask
		hasLatest := false
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("schedule_id = ? AND deleted_at IS NULL", lockedSched.ID).
			Order("id DESC").
			First(&latest).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		} else {
			hasLatest = true
		}

		// Overlap guard: skip creating a new run if ANY non-terminal task already
		// exists under this schedule (status IN Pending/WaitingConfirm/Processing,
		// not soft-deleted). Previously this only checked whether the LATEST task was
		// Processing -- but scanStuckTasks revives an expired Processing run back to
		// Pending, so the latest could be Pending (not Processing) while still being a
		// live placeholder; the old guard then created a SECOND executable task for
		// the same schedule/window (duplicate-execution race, no DB unique key).
		// Treating every non-terminal status as an occupying placeholder closes that
		// gap. Skip WITHOUT advancing last_run_at (same transient-overlap semantics):
		// the window is preserved and covered by the next cycle once the in-flight run
		// reaches a terminal state. A task hitting WorkerMaxRetry is marked Failed
		// (terminal) by scanStuckTasks, which naturally releases this skip -- so it
		// never blocks the schedule permanently.
		var liveCount int64
		if err := tx.Model(&model.SummaryTask{}).
			Where("schedule_id = ? AND deleted_at IS NULL AND status IN ?",
				lockedSched.ID,
				[]int{model.StatusPending, model.StatusWaitingConfirm, model.StatusProcessing}).
			Count(&liveCount).Error; err != nil {
			return err
		}
		if liveCount > 0 {
			log.Printf("[scheduler] schedule %d has %d non-terminal task(s); skipping overlapping new task (last_run_at preserved)", lockedSched.ID, liveCount)
			return nil
		}

		// Scheduled summary is single-person only unless FEATURE_TEAM_SCHEDULE is on.
		// If the schedule's participant config went multi-person and the flag is OFF, it
		// is structurally unsupported for scheduling: disable + alert. When the flag is
		// ON, multi-person schedules are allowed through to the multi-person execution
		// path (children built by buildScheduledTaskChildren, all participants Accepted
		// under AUTO; meta aggregation via the submit-gate back-fill).
		if multiPerson, err := scheduleConfigMultiPerson(lockedSched); err != nil {
			return err
		} else if multiPerson && !featureTeamSchedule {
			log.Printf("[scheduler] ALERT schedule %d participant config is multi-person; scheduled summary not supported for team tasks (FEATURE_TEAM_SCHEDULE off), disabling + notifying creator", lockedSched.ID)
			if err := tx.Model(&model.SummarySchedule{}).
				Where("id = ?", lockedSched.ID).
				Update("is_active", 0).Error; err != nil {
				return err
			}
			// NOTE: creator notification is a follow-up.
			return nil
		}

		requeued = true

		// INSERT a brand-new task for this run (1->N). Title/origin/space carry over
		// from the schedule; the rest is a fresh Pending scheduled task.
		newTask = model.SummaryTask{
			TaskNo:         service.GenerateTaskNo(),
			SpaceID:        lockedSched.SpaceID,
			CreatorID:      lockedSched.CreatorID,
			Title:          lockedSched.Title,
			SummaryMode:    lockedSched.SummaryMode,
			TimeRangeStart: start,
			TimeRangeEnd:   end,
			Status:         model.StatusPending,
			TriggerType:    model.TriggerScheduled,
			ScheduleID:     &lockedSched.ID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if hasLatest {
			// Preserve origin channel from the previous run so the new summary keeps
			// the same delivery context.
			newTask.OriginChannelID = latest.OriginChannelID
			newTask.OriginChannelType = latest.OriginChannelType
		}
		if err := tx.Create(&newTask).Error; err != nil {
			return err
		}

		// Rebuild the three subtables (source / participant / personal_result) from
		// the schedule config -- the new task_id starts with no children. The returned
		// count is the number of Accepted participants materialized this round.
		//
		// V5 §4.3/Q2: for a CONFIRM schedule where NOBODY (creator included) has
		// confirmed yet, no children are built (acceptedCount==0). The round is skipped:
		// the freshly-created placeholder task is SOFT-DELETED (deleted_at set) so it
		// produces no result, never surfaces in any deleted_at IS NULL query (list/
		// detail), does not occupy the overlap guard, and is not picked up by the
		// processor. The next cycle re-checks the confirm list (already-advanced
		// next_run_at avoids a busy re-scan). last_run_at is NOT advanced for a skipped
		// round so a type=4 incremental window is preserved.
		acceptedCount, err := buildScheduledTaskChildren(tx, imDB, lockedSched, newTask, now)
		if err != nil {
			return err
		}
		if acceptedCount == 0 {
			log.Printf("[scheduler] schedule %d: CONFIRM round has zero confirmed participants; skipping round (placeholder task %d soft-deleted, no result, last_run_at preserved)", lockedSched.ID, newTask.ID)
			// Soft-delete the placeholder task instead of marking it Cancelled, so it
			// never surfaces in any deleted_at IS NULL query (list/detail). DeletedAt is
			// a plain *time.Time (not gorm.DeletedAt), so set it explicitly with now to
			// stay transaction-consistent. The overlap guard ignores it (deleted_at non-
			// null), next_run_at is already advanced, last_run_at stays put: the plan
			// silently rolls to the next time point.
			// Clean up the summary_source child rows already materialized this round.
			// buildScheduledTaskChildren builds sources BEFORE counting accepted
			// participants, so on a zero-confirm round the source rows exist while the
			// parent task is only soft-deleted -- they would become invisible orphans
			// accumulating every skipped round. SummarySource has no DeletedAt, so this
			// is a physical delete (correct: they are valueless, unreferenced garbage).
			if err := tx.Where("task_id = ?", newTask.ID).Delete(&model.SummarySource{}).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.SummaryTask{}).
				Where("id = ?", newTask.ID).
				Update("deleted_at", now).Error; err != nil {
				return err
			}
			requeued = false
			return nil
		}

		// Advance last_run_at ONLY now that the new task is actually created to run.
		// The type=4 incremental window anchor moves only on a real run, never on a
		// business skip, so a skipped interval's messages are still covered by the
		// next window instead of being permanently dropped.
		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Update("last_run_at", now).Error; err != nil {
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

	// Only report a real requeue when we actually created a new task. Skipped cases
	// (still processing or multi-person guard) return taskID=0 so scanPendingSchedules
	// does not log a misleading "requeued task" line.
	if !requeued {
		return 0, true, nil
	}
	return newTask.ID, true, nil
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
