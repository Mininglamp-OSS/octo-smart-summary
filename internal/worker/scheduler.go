package worker

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
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

// scanPendingSchedules creates tasks from due schedules.
func scanPendingSchedules(db *gorm.DB) {
	now := time.Now().UTC()
	var schedules []model.SummarySchedule
	err := db.Where("is_active = 1 AND next_run_at <= ? AND deleted_at IS NULL", now).Find(&schedules).Error
	if err != nil {
		log.Printf("[scheduler] query schedules: %v", err)
		return
	}

	for _, sched := range schedules {
		// Compute time range
		start, end := service.ComputeTimeRange(sched.TimeRangeType, now)

		// Parse source config
		var sources []struct {
			SourceType int    `json:"source_type"`
			SourceID   string `json:"source_id"`
		}
		if len(sched.SourceConfig) > 0 {
			json.Unmarshal([]byte(sched.SourceConfig), &sources)
		}

		taskNo := service.GenerateTaskNo()
		title := sched.Title
		if title == "" {
			title = "定时总结-" + taskNo[len(taskNo)-8:]
		}

		task := model.SummaryTask{
			TaskNo:         taskNo,
			SpaceID:        sched.SpaceID,
			CreatorID:      sched.CreatorID,
			Title:          title,
			SummaryMode:    model.ModeByPerson,
			TimeRangeStart: start,
			TimeRangeEnd:   end,
			Status:         model.StatusPending,
			TriggerType:    model.TriggerScheduled,
			ScheduleID:     &sched.ID,
		}

		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&task).Error; err != nil {
				return err
			}
			for _, s := range sources {
				src := model.SummarySource{
					TaskID:     task.ID,
					SourceType: s.SourceType,
					SourceID:   s.SourceID,
					SourceName: service.ResolveSourceName(s.SourceID),
				}
				if err := tx.Create(&src).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("[scheduler] create task for schedule %d: %v", sched.ID, err)
			continue
		}

		// Update schedule: last_run_at and next_run_at
		nextRun, err := service.NextRun(sched.CronExpr, now)
		if err != nil {
			log.Printf("[scheduler] compute next run for schedule %d: %v", sched.ID, err)
			continue
		}
		db.Model(&sched).Updates(map[string]interface{}{
			"last_run_at": now,
			"next_run_at": nextRun,
		})

		log.Printf("[scheduler] created task %d from schedule %d", task.ID, sched.ID)
	}
}

// scanConfirmTimeouts auto-declines participants still in WaitingConfirm
// past the task's confirm_deadline.
func scanConfirmTimeouts(db *gorm.DB) {
	now := time.Now().UTC()

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
	now := time.Now().UTC()

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
	now := time.Now().UTC()
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
