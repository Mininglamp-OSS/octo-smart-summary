package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/streaming"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timing"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/tokenizer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func escapeCitationMarkers(content string) string {
	return citationRe.ReplaceAllString(content, "($1)")
}

const noRelevantContentMessage = pipeline.NoRelevantContentMessage

const noSelfMessagesMessage = pipeline.NoSelfMessagesMessage

// decidePersonalMessages is a compatibility wrapper around pipeline.DecideMessages.
// It chooses which messages feed the summary after target filtering, and decides
// whether to early-return a user-facing message instead.
//
//	len(targetUIDs)==0                      → all messages, no early message (no target)
//	filtered non-empty                      → filtered messages (normal narrow)
//	filtered empty + targetUIDs==[creator]  → nil + noSelfMessagesMessage (true first-person
//	                                          query, creator never spoke → tell the user plainly)
//	filtered empty + other targets          → all messages (named someone who didn't speak in
//	                                          this source; whole source beats "no data")
func decidePersonalMessages(targetUIDs []string, creatorUID string, all, filtered []pipeline.Message) (msgs []pipeline.Message, earlyMsg string) {
	return pipeline.DecideMessages(targetUIDs, creatorUID, all, filtered)
}

// isEmptyPlaceholder reports whether content is one of the "no usable result"
// placeholder messages (no relevant content, or the creator had no messages in
// the selected source). These must not overwrite a previous valid scheduled result.
func isEmptyPlaceholder(content string) bool {
	trimmed := strings.TrimSpace(content)
	return trimmed == noRelevantContentMessage || trimmed == noSelfMessagesMessage
}

func shouldSkipScheduledPlaceholderResult(triggerType int, content string) bool {
	return triggerType == model.TriggerScheduled && isEmptyPlaceholder(content)
}

func completedPersonalResultUpdates(pr model.PersonalResult, content string, citations []model.Citation, msgCount, totalTokens int, modelVer string, genAt time.Time, skipContent bool) map[string]interface{} {
	updates := map[string]interface{}{
		"worker_status":  model.PersonalStatusCompleted,
		"workflow_stage": model.WorkflowStageGenerateSummary,
	}
	if skipContent {
		return updates
	}
	pr.SetCitations(citations)
	updates["content"] = content
	updates["citations_json"] = pr.CitationsJSON
	updates["msg_count"] = msgCount
	updates["total_token_used"] = totalTokens
	updates["model_version"] = modelVer
	updates["generated_at"] = genAt
	return updates
}

func (p *Processor) updatePersonalWorkflowStage(personalResultID int64, stage string) {
	if personalResultID == 0 || stage == "" {
		return
	}
	if err := p.db.Model(&model.PersonalResult{}).
		Where("id = ? AND worker_status = ?", personalResultID, model.PersonalStatusProcessing).
		Update("workflow_stage", stage).Error; err != nil {
		log.Printf("[personal-worker] update workflow stage pr=%d stage=%s: %v", personalResultID, stage, err)
	}
}

func (p *Processor) persistCompletedPersonalResult(task model.SummaryTask, pr model.PersonalResult, content string, citations []model.Citation, msgCount, totalTokens int, modelVer string, genAt time.Time, skipContent bool) error {
	updates := completedPersonalResultUpdates(pr, content, citations, msgCount, totalTokens, modelVer, genAt, skipContent)
	if skipContent {
		return p.db.Transaction(func(tx *gorm.DB) error {
			var lockedPR model.PersonalResult
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND task_id = ? AND user_id = ?", pr.ID, pr.TaskID, pr.UserID).
				First(&lockedPR).Error; err != nil {
				return err
			}
			var latest model.PersonalResultVersion
			if err := tx.Where("task_id = ? AND user_id = ?", lockedPR.TaskID, lockedPR.UserID).
				Order("version DESC").Order("id DESC").First(&latest).Error; err == nil {
				updates["content"] = latest.Content
				updates["citations_json"] = latest.CitationsJSON
				updates["msg_count"] = latest.MsgCount
				updates["total_token_used"] = latest.TotalTokenUsed
				updates["model_version"] = latest.ModelVersion
				updates["current_version_id"] = latest.ID
				updates["generated_at"] = latest.GeneratedAt
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			return tx.Model(&lockedPR).Updates(updates).Error
		})
	}

	return p.db.Transaction(func(tx *gorm.DB) error {
		var lockedPR model.PersonalResult
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND task_id = ? AND user_id = ?", pr.ID, pr.TaskID, pr.UserID).
			First(&lockedPR).Error; err != nil {
			return err
		}
		nextVer, err := service.GetNextPersonalVersion(tx, lockedPR.TaskID, lockedPR.UserID)
		if err != nil {
			return err
		}
		operationType := "regenerate"
		operationNote := task.Title
		if nextVer <= 1 {
			operationType = "generate"
		} else if task.TriggerType == model.TriggerScheduled {
			operationType = "scheduled_generate"
			operationNote = p.scheduledOperationNote(task)
		}

		version := model.PersonalResultVersion{
			TaskID:           lockedPR.TaskID,
			ParticipantRefID: lockedPR.ParticipantRefID,
			UserID:           lockedPR.UserID,
			Content:          content,
			MsgCount:         msgCount,
			TotalTokenUsed:   totalTokens,
			ModelVersion:     modelVer,
			Version:          nextVer,
			OperationType:    operationType,
			OperationNote:    operationNote,
			CreatedBy:        lockedPR.UserID,
			GeneratedAt:      genAt,
		}
		version.SetCitations(citations)
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		updates["current_version_id"] = version.ID
		if err := tx.Model(&lockedPR).Updates(updates).Error; err != nil {
			return err
		}
		return service.PrunePersonalResultVersions(tx, lockedPR.TaskID, lockedPR.UserID, service.PersonalResultVersionKeepLimit)
	})
}

func (p *Processor) processPersonalSummary(ctx context.Context, taskID, participantRefID int64) {
	p.processPersonalSummaryWithOptions(ctx, taskID, participantRefID, false)
}

func (p *Processor) processPersonalSummaryAllowCompleted(ctx context.Context, taskID, participantRefID int64) {
	p.processPersonalSummaryWithOptions(ctx, taskID, participantRefID, true)
}

func (p *Processor) processPersonalSummaryWithOptions(ctx context.Context, taskID, participantRefID int64, allowCompletedTask bool) {
	log.Printf("[personal-worker] start task=%d participant=%d allow_completed=%t", taskID, participantRefID, allowCompletedTask)

	// Load participant
	var participant model.SummaryParticipant
	if err := p.db.First(&participant, participantRefID).Error; err != nil {
		log.Printf("[personal-worker] participant %d not found: %v", participantRefID, err)
		return
	}

	// Load or create personal result
	var pr model.PersonalResult
	if err := p.db.Where("task_id = ? AND participant_ref_id = ?", taskID, participantRefID).First(&pr).Error; err != nil {
		log.Printf("[personal-worker] personal result not found for task=%d participant=%d: %v", taskID, participantRefID, err)
		return
	}

	// CAS: only proceed if worker_status is still Pending (prevents duplicate runs)
	now := timezone.Now()
	cas := p.db.Model(&pr).
		Where("worker_status = ?", model.PersonalStatusPending).
		Update("worker_status", model.PersonalStatusProcessing)
	if cas.RowsAffected == 0 {
		log.Printf("[personal-worker] task=%d participant=%d already processing/completed, skipping", taskID, participantRefID)
		return
	}
	p.db.Model(&participant).Updates(map[string]interface{}{
		"status":            model.ParticipantProcessing,
		"worker_started_at": now,
	})

	// CAS update task status to PROCESSING (from any earlier state).
	// personal_regenerate is allowed to run against an already-Completed task
	// without flipping the whole task back to Processing; the user will explicitly
	// submit the regenerated personal result later, and Submit revives the task for
	// meta recompute at that point.
	deadline := timezone.Now().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute)
	taskCAS := p.db.Model(&model.SummaryTask{}).
		Where("id = ? AND status IN (?, ?)", taskID, model.StatusPending, model.StatusWaitingConfirm).
		Updates(map[string]interface{}{
			"status":              model.StatusProcessing,
			"processing_deadline": deadline,
		})
	if taskCAS.Error != nil {
		log.Printf("[personal-worker] task=%d CAS update failed: %v", taskID, taskCAS.Error)
		return
	}
	if taskCAS.RowsAffected == 0 {
		var currentTask model.SummaryTask
		if err := p.db.Select("status").First(&currentTask, taskID).Error; err != nil ||
			(currentTask.Status != model.StatusProcessing && !(allowCompletedTask && currentTask.Status == model.StatusCompleted)) {
			log.Printf("[personal-worker] task=%d not in runnable state, aborting", taskID)
			return
		}
		if currentTask.Status == model.StatusProcessing {
			// Refresh deadline for already-processing task (prevents scheduler false-positive)
			p.db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
				Update("processing_deadline", deadline)
		}
	}

	// Load task
	var task model.SummaryTask
	if err := p.db.First(&task, taskID).Error; err != nil {
		log.Printf("[personal-worker] task %d not found: %v", taskID, err)
		p.markPersonalFailed(&pr, &participant, "task not found")
		return
	}

	nonNegativeMs := func(start time.Time) int64 {
		if start.IsZero() {
			return 0
		}
		d := now.Sub(start)
		if d < 0 {
			return 0
		}
		return d.Milliseconds()
	}
	taskWaitMs := nonNegativeMs(task.CreatedAt)
	prWaitMs := nonNegativeMs(pr.CreatedAt)
	participantWaitMs := nonNegativeMs(participant.CreatedAt)
	timing.GetContext(task.TaskNo).Update(func(c *timing.TaskContext) {
		c.TaskCreatedToWorkerStartMs = taskWaitMs
		c.PersonalResultCreatedToWorkerStartMs = prWaitMs
		c.ParticipantCreatedToWorkerStartMs = participantWaitMs
	})
	log.Printf("[personal-worker] pre-worker wait task=%s task_created_to_start=%dms personal_result_created_to_start=%dms participant_created_to_start=%dms",
		task.TaskNo, taskWaitMs, prWaitMs, participantWaitMs)

	workerStartAt := now
	lastWorkflowStageAt := workerStartAt
	runID := newSummaryRunID(taskID, participant.UserID)
	var streamSender *summaryStreamSender
	streamFinalized := false
	ensureStream := func() *summaryStreamSender {
		if streamSender == nil {
			streamSender = newSummaryStreamSender(ctx, p.cfg, taskID, participant.UserID, streaming.ScopePersonal, runID)
		}
		return streamSender
	}
	finishStreamDone := func(status int) {
		if streamSender != nil && !streamFinalized {
			streamSender.Done(status)
			streamFinalized = true
		}
	}
	finishStreamError := func(message string) {
		if streamSender != nil && !streamFinalized {
			streamSender.Error(message)
			streamFinalized = true
		}
	}
	defer func() {
		if streamSender != nil {
			streamSender.Close()
		}
	}()
	reportStage := func(stage string) {
		stageAt := timezone.Now()
		sinceWorkerStartMs := stageAt.Sub(workerStartAt).Milliseconds()
		deltaMs := stageAt.Sub(lastWorkflowStageAt).Milliseconds()
		if sinceWorkerStartMs < 0 {
			sinceWorkerStartMs = 0
		}
		if deltaMs < 0 {
			deltaMs = 0
		}
		lastWorkflowStageAt = stageAt
		timing.GetContext(task.TaskNo).Update(func(c *timing.TaskContext) {
			c.WorkflowStages = append(c.WorkflowStages, timing.WorkflowStageMs{
				Stage:              stage,
				SinceWorkerStartMs: sinceWorkerStartMs,
				DeltaMs:            deltaMs,
			})
		})
		log.Printf("[personal-worker] workflow stage task=%s pr=%d stage=%s since_worker_start=%dms delta=%dms",
			task.TaskNo, pr.ID, stage, sinceWorkerStartMs, deltaMs)
		p.updatePersonalWorkflowStage(pr.ID, stage)
		if stage == model.WorkflowStageGenerateSummary {
			ensureStream().Stage(stage)
		}
	}

	streamDelta := func(delta string) error {
		return ensureStream().Delta(delta)
	}

	// Execute pipeline
	content, citations, msgCount, totalTokens, modelVer, err := p.executePersonalPipeline(ctx, task, participant.UserID, reportStage, streamDelta)
	if err != nil {
		log.Printf("[personal-worker] pipeline error task=%d user=%s: %v", taskID, participant.UserID, err)
		finishStreamError("summary generation failed")
		p.markPersonalFailed(&pr, &participant, err.Error())
		return
	}
	if strings.TrimSpace(content) == "" {
		content = noRelevantContentMessage
	}
	isScheduledEmptyWindow := shouldSkipScheduledPlaceholderResult(task.TriggerType, content)

	// Best-effort check: abort early if task is no longer Processing.
	// Final safety is guaranteed by the task-level CAS in the completion path below.
	var taskCheck model.SummaryTask
	if err := p.db.Select("status").First(&taskCheck, taskID).Error; err != nil ||
		(taskCheck.Status != model.StatusProcessing && !(allowCompletedTask && taskCheck.Status == model.StatusCompleted)) {
		log.Printf("[personal-worker] task=%d no longer runnable before result write, aborting", taskID)
		finishStreamError("summary generation cancelled")
		return
	}

	genAt := timezone.Now()
	persistStart := time.Now()
	if err := p.persistCompletedPersonalResult(task, pr, content, citations, msgCount, totalTokens, modelVer, genAt, isScheduledEmptyWindow); err != nil {
		log.Printf("[personal-worker] persist personal result task=%d pr=%d: %v", taskID, pr.ID, err)
		finishStreamError("summary persistence failed")
		return
	}
	p.db.Model(&participant).Updates(map[string]interface{}{
		"status": model.ParticipantCompleted,
	})
	timing.Observe(task.TaskNo, "persist_personal_result", persistStart)

	// Send directed WS notification to the specific user
	p.sendCallback(model.TaskEvent{
		TaskID:       taskID,
		Status:       model.StatusProcessing,
		Progress:     100,
		Message:      fmt.Sprintf("personal_complete:%s", participant.UserID),
		EventType:    "PERSONAL_SUMMARY_STATUS",
		TargetUserID: participant.UserID,
	})

	// Check participant count to decide next step
	var participantCount int64
	p.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&participantCount)

	if participantCount <= 1 {
		isScheduled := task.TriggerType == model.TriggerScheduled
		if isScheduledEmptyWindow {
			if err := completeTaskWithoutNewResult(p.db, taskID); err != nil {
				if errors.Is(err, errTaskNoLongerProcessing) {
					log.Printf("[personal-worker] task %d status changed during processing (likely cancelled), skipping completion", taskID)
					finishStreamError("summary generation cancelled")
					return
				}
				log.Printf("[personal-worker] task=%d complete-without-result error: %v", taskID, err)
				finishStreamError("summary completion failed")
				return
			}
			log.Printf("[personal-worker] task %d scheduled empty-window: kept previous result, skipped prune", taskID)
		} else {
			// Single-person mode: directly create SummaryResult and complete the task.
			result := model.SummaryResult{
				TaskID:         taskID,
				Content:        content,
				TotalMsgCount:  msgCount,
				TotalTokenUsed: totalTokens,
				ModelVersion:   modelVer,
				GeneratedAt:    genAt,
			}
			result.SetCitations(citations)
			// Bug3: only scheduled tasks prune old versions in place; manual/normal
			// single-person runs keep their full version history.
			if err := saveLatestResultAndCompleteTask(p.db, taskID, &result, isScheduled, nil); err != nil {
				if errors.Is(err, errTaskNoLongerProcessing) {
					log.Printf("[personal-worker] task %d status changed during processing (likely cancelled), skipping completion", taskID)
					finishStreamError("summary generation cancelled")
					return
				}
				log.Printf("[personal-worker] save result error task=%d: %v", taskID, err)
				finishStreamError("summary completion failed")
				return
			}
		}
		p.sendCallback(model.TaskEvent{
			TaskID:   taskID,
			Status:   model.StatusCompleted,
			Progress: 100,
			Message:  "总结完成",
		})
		finishStreamDone(model.StatusCompleted)
		// Task durably reached Completed above (saveLatestResultAndCompleteTask /
		// completeTaskWithoutNewResult succeeded). Fire the terminal notification.
		p.notifyTaskTerminal(taskID, model.StatusCompleted)
		log.Printf("[personal-worker] task %d single-person completed directly", taskID)
	} else if allowCompletedTask && taskCheck.Status == model.StatusCompleted {
		finishStreamDone(model.StatusCompleted)
		// Personal-only regenerate: keep the team summary as-is until the user
		// explicitly submits this regenerated personal result. Submit will revive
		// the task and trigger meta recompute.
		log.Printf("[personal-worker] task=%d user=%s personal regenerate completed; waiting for submit", taskID, participant.UserID)
	} else {
		finishStreamDone(model.StatusProcessing)
		// Multi-person mode: trigger meta-summary to check if all participants completed.
		//
		// System back-fill of submitted_at: the meta completion gate
		// requires submitted_at IS NOT NULL, but the personal worker only sets
		// ParticipantCompleted/WorkerStatus=Completed -- it never writes submitted_at
		// (the only manual writer is /submit). A scheduled multi-person task driven by the
		// worker alone would therefore have len(submitted)==0 forever => meta dead-waits.
		// For scheduled (TriggerScheduled) multi-person tasks we back-fill submitted_at +
		// submit_source=2 (system) when the participant has confirmed (status NOT IN
		// Pending,Declined -- same gate as meta's totalAccepted), idempotently
		// (WHERE submitted_at IS NULL, so a racing manual /submit is never overwritten).
		if task.TriggerType == model.TriggerScheduled || pr.SubmitSource == model.SubmitSourceSystem {
			p.backfillSystemSubmittedAt(taskID, &pr)
		}
		p.meta.TriggerMetaSummary(taskID)
	}

	log.Printf("[personal-worker] completed task=%d user=%s msgs=%d", taskID, participant.UserID, msgCount)
}

// backfillSystemSubmittedAt system-back-fills submitted_at for a scheduled
// or task-level-regenerated multi-person personal result so the meta gate (submitted_at IS NOT NULL) can
// progress without a manual /submit (§4.4-2). It runs in a single transaction:
//   - re-reads the participant's current status under the same tx (the confirm
//     gate must reflect committed state, not the in-memory pr loaded earlier);
//   - only back-fills when the participant has confirmed: status NOT IN
//     (Pending, Declined) -- the same set meta's totalAccepted counts;
//   - is idempotent and race-safe via WHERE submitted_at IS NULL, so a concurrent
//     manual /submit (submit_source=1) is never overwritten;
//   - sets submit_source=2 to mark the system origin.
//
// participantCount>1 is the caller's branch invariant (this is only reached from
// the multi-person path), so it is not re-checked here.
func (p *Processor) backfillSystemSubmittedAt(taskID int64, pr *model.PersonalResult) {
	now := timezone.Now()
	err := p.db.Transaction(func(tx *gorm.DB) error {
		var status int
		if err := tx.Model(&model.SummaryParticipant{}).
			Select("status").
			Where("id = ?", pr.ParticipantRefID).
			Scan(&status).Error; err != nil {
			return err
		}
		// Use the same accepted-participant gate as meta aggregation.
		if status == model.ParticipantPending || status == model.ParticipantDeclined {
			return nil
		}
		res := tx.Model(&model.PersonalResult{}).
			Where("id = ? AND submitted_at IS NULL", pr.ID).
			Updates(map[string]interface{}{
				"submitted_at":  now,
				"submit_source": model.SubmitSourceSystem,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			log.Printf("[personal-worker] task=%d participant=%d system back-filled submitted_at (submit_source=2)", taskID, pr.ParticipantRefID)
		}
		return nil
	})
	if err != nil {
		log.Printf("[personal-worker] task=%d participant=%d back-fill submitted_at failed: %v", taskID, pr.ParticipantRefID, err)
	}
}

func (p *Processor) markPersonalFailed(pr *model.PersonalResult, participant *model.SummaryParticipant, errMsg string) {
	sanitized := sanitizeErrorForUser(errMsg)

	// Retryable personal failures are returned to Pending; exhausted attempts are
	// marked terminal for that participant.
	//
	// The old markPersonalFailed unconditionally set worker_status=Failed and reset the
	// participant to Accepted. But the retry scanner (scanStuckPersonalTasks) only
	// re-triggers personal_result rows whose worker_status==Pending -- a Failed row was
	// NEVER re-run. Meanwhile meta's totalAccepted still counts that Accepted participant
	// (status NOT IN Pending,Declined), and the multi-person path (participantCount>1)
	// does NOT propagate task-level failure, so meta would wait forever for a submit that
	// never comes -> task stuck until the lease times out.
	//
	// Retry behavior:
	//   - retry_count < WorkerMaxRetry: increment retry_count and set worker_status back
	//     to Pending (participant stays Accepted). scanStuckPersonalTasks' M3 sweep (and
	//     the AUTO dispatch path) then naturally re-runs it -- a transient failure heals.
	//   - retry_count >= WorkerMaxRetry: set worker_status=Failed (terminal). Propagate so
	//     meta never dead-waits:
	//       * single-person: fail the task (unchanged behavior).
	//       * multi-person: Decline the failed participant so it drops out of meta's
	//         totalAccepted (status NOT IN Pending,Declined), letting the remaining
	//         members aggregate; then kick meta to re-evaluate. This keeps the existing
	//         meta gate intact.
	maxRetry := p.cfg.WorkerMaxRetry
	if maxRetry < 1 {
		maxRetry = 1
	}

	var shouldNotify bool
	var willRetry bool
	var permanentMultiDeclined bool
	txErr := p.db.Transaction(func(tx *gorm.DB) error {
		// 🟠 Atomic retry_count increment (no lost updates).
		//
		// The old code did a plain Select(retry_count) -> newRetry=current+1 -> write-back
		// inside the tx, with no FOR UPDATE / no CAS. Two concurrent failure workers (e.g.
		// after the stuck-scan resets a timed-out Processing row and a second worker is
		// pulled up) could read the same retry_count and both write back the same newRetry,
		// so one failure is silently swallowed and WorkerMaxRetry never trips -> infinite
		// re-runs. Fix: increment in the database atomically (retry_count = retry_count + 1)
		// then SELECT the post-increment value, so newRetry is the real DB-derived count,
		// not an application-layer +1. Each concurrent failure strictly accumulates.
		if err := tx.Model(&model.PersonalResult{}).
			Where("id = ?", pr.ID).
			Update("retry_count", gorm.Expr("retry_count + 1")).Error; err != nil {
			return err
		}
		var current model.PersonalResult
		if err := tx.Select("retry_count").First(&current, pr.ID).Error; err != nil {
			return err
		}
		newRetry := current.RetryCount
		willRetry = newRetry < maxRetry

		if willRetry {
			// Transient failure: reset to Pending so the retry scanner re-runs it.
			// retry_count was already atomically incremented above; only the remaining
			// columns are written here (do not re-set retry_count to an app-layer value).
			if err := tx.Model(pr).Updates(map[string]interface{}{
				"worker_status":  model.PersonalStatusPending,
				"workflow_stage": "",
				"error_message":  &sanitized,
			}).Error; err != nil {
				return err
			}
			// Participant returns to Accepted, clear worker_started_at so the stuck-scan
			// lease check (worker_started_at < leaseTimeout) does not mis-handle it.
			if err := tx.Model(participant).Updates(map[string]interface{}{
				"status":            model.ParticipantAccepted,
				"worker_started_at": nil,
			}).Error; err != nil {
				return err
			}
			return nil
		}

		// Permanent failure: terminal Failed. retry_count already atomically incremented
		// above (the DB-derived newRetry >= maxRetry), so it is not re-written here.
		if err := tx.Model(pr).Updates(map[string]interface{}{
			"worker_status": model.PersonalStatusFailed,
			"error_message": &sanitized,
		}).Error; err != nil {
			return err
		}

		var participantCount int64
		if err := tx.Model(&model.SummaryParticipant{}).Where("task_id = ?", pr.TaskID).Count(&participantCount).Error; err != nil {
			return err
		}
		if participantCount <= 1 {
			// Single-person: keep prior behavior -- reset participant to Accepted and propagate failure to the task.
			if err := tx.Model(participant).Update("status", model.ParticipantAccepted).Error; err != nil {
				return err
			}
			result := tx.Model(&model.SummaryTask{}).
				Where("id = ? AND status = ?", pr.TaskID, model.StatusProcessing).
				Updates(map[string]interface{}{
					"status":        model.StatusFailed,
					"error_message": &sanitized,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				log.Printf("[personal-worker] task=%d CAS update skipped (not in Processing state)", pr.TaskID)
			} else {
				shouldNotify = true
			}
			return nil
		}

		// Multi-person: Decline the permanently-failed participant so meta's totalAccepted
		// (status NOT IN Pending,Declined) excludes it and the remaining members can
		// aggregate. This keeps single/manual paths untouched (they take the branch above).
		if err := tx.Model(participant).Update("status", model.ParticipantDeclined).Error; err != nil {
			return err
		}
		permanentMultiDeclined = true
		return nil
	})

	if txErr != nil {
		log.Printf("[personal-worker] markPersonalFailed transaction failed: task=%d err=%v", pr.TaskID, txErr)
		return
	}

	if willRetry {
		log.Printf("[personal-worker] task=%d participant=%d personal failed, reset to Pending for retry (retry<%d), msg=%s",
			pr.TaskID, pr.ParticipantRefID, maxRetry, sanitized)
		// Re-trigger immediately rather than waiting for the 5-minute stuck scan.
		if p.pool != nil {
			ptID := pr.ParticipantRefID
			taskID := pr.TaskID
			p.pool.Submit(func() {
				p.processPersonalSummary(context.Background(), taskID, ptID)
			})
		}
		return
	}

	if permanentMultiDeclined {
		// A member is permanently out: re-evaluate meta so the task does not dead-wait on the now-Declined member.
		if p.meta != nil {
			p.meta.TriggerMetaSummary(pr.TaskID)
		}
		log.Printf("[personal-worker] task=%d participant=%d permanently failed (multi-person), declined + re-kicked meta, msg=%s",
			pr.TaskID, pr.ParticipantRefID, sanitized)
		return
	}

	if shouldNotify {
		p.sendCallback(model.TaskEvent{
			TaskID:   pr.TaskID,
			Status:   model.StatusFailed,
			Progress: 0,
			Message:  sanitized,
		})
		// shouldNotify is set only when the task-level CAS to StatusFailed actually
		// flipped the row (single-person terminal failure). Fire the notification.
		p.notifyTaskTerminal(pr.TaskID, model.StatusFailed)
	}
	log.Printf("[personal-worker] task=%d marked failed (terminal), sanitizedMsg=%s", pr.TaskID, sanitized)
}

func (p *Processor) executePersonalPipeline(ctx context.Context, task model.SummaryTask, userID string, reportStage func(string), streamDelta func(string) error) (string, []model.Citation, int, int, string, error) {
	totalStart := time.Now()
	taskNo := task.TaskNo
	defer func() {
		timing.Observe(taskNo, "personal_pipeline_total", totalStart)
		// Boss request: one consolidated per-run report — how many LLM calls,
		// what each was for, time + tokens. Flushed at run end (success or error).
		timing.FlushReport(taskNo, time.Since(totalStart).Milliseconds(), nil)
	}()

	// Load sources
	var sources []model.SummarySource
	if err := p.db.Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("load sources: %w", err)
	}

	specifiedSources := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		specifiedSources = append(specifiedSources, map[string]interface{}{
			"source_id":   s.SourceID,
			"source_type": s.SourceType,
			"source_name": s.SourceName,
		})
	}

	// Unified LLM tool-call callback (shared by all Function Call sites).
	// purpose is derived from forceFn so the report says what each call did
	// (extract_time_range / resolve_channel_scope / resolve_topic_target).
	toolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		callStart := time.Now()
		args, tokens, err := p.llm.CallWithTools(ctx, messages, tools, forceFn, p.cfg.LLMTemperature)
		purpose := "检索预处理(tool-call)"
		if forceFn != "" {
			purpose = "检索预处理: " + forceFn
		}
		timing.RecordLLMSince(taskNo, purpose, callStart, tokens)
		return args, err
	}

	// Legacy callback for PostRetrievalNarrow (still uses CallRaw)
	llmFn := func(ctx context.Context, prompt string) (string, error) {
		callStart := time.Now()
		out, err := p.llm.CallRaw(ctx, prompt)
		timing.RecordLLMSince(taskNo, "检索后裁剪 PostRetrievalNarrow", callStart, 0)
		return out, err
	}

	// Fetch messages via personal pipeline (Layer 0-5)
	var channelScopeOpts *pipeline.ChannelScopeOptions
	if p.cfg.ChannelScopeEnabled {
		channelScopeOpts = &pipeline.ChannelScopeOptions{
			Enabled: true,
		}
	}

	fetchStart := time.Now()
	messages, intentResult, err := pipeline.ResolveAndFetchMessagesForPersonal(
		ctx, userID, nil, nil, specifiedSources, task.EffectiveTopic(),
		task.TimeRangeStart, task.TimeRangeEnd,
		p.imDB, p.octoClient, p.cfg.MessageFetchBackend, toolCallFn, llmFn,
		p.cfg.MsgTableCount, p.cfg.MaxMessagesPerChannel, p.cfg.FetchConcurrency, p.cfg.OctoSearchPollSec,
		channelScopeOpts, reportStage,
	)
	timing.Observe(taskNo, "fetch_messages", fetchStart)
	if err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("fetch messages: %w", err)
	}

	// Record message retrieval stats
	tctx := timing.GetContext(taskNo)
	channelSet := make(map[string]struct{})
	for _, m := range messages {
		channelSet[m.ChannelID] = struct{}{}
	}
	tctx.Update(func(c *timing.TaskContext) {
		c.MessagesRetrieved = len(messages)
		c.ChannelCount = len(channelSet)
	})

	// Resolve sender names (for display in summary)
	resolveStart := time.Now()
	nameMap := p.batchResolveUserNames(messages)
	timing.Observe(taskNo, "resolve_user_names", resolveStart)
	log.Printf("[personal-worker] batchResolveUserNames took %dms (%d names)",
		time.Since(resolveStart).Milliseconds(), len(nameMap))

	// Get target person info from unified intent recognition (returned from fetch)
	var targetUIDs []string
	if intentResult != nil && !intentResult.Skipped {
		tctx.Update(func(c *timing.TaskContext) {
			c.IntentSkipped = false
			c.IntentLLMCalls = 1 // unified call
		})
		targetUIDs = intentResult.TargetPersons.UIDs
		if intentResult.TargetPersons.IncludeSelf && userID != "" {
			// Ensure creator is in target list if include_self is true
			hasCreator := false
			for _, uid := range targetUIDs {
				if uid == userID {
					hasCreator = true
					break
				}
			}
			if !hasCreator {
				targetUIDs = append(targetUIDs, userID)
			}
		}
		log.Printf("[personal-worker] topic target from intent: %v (creator=%s)", targetUIDs, userID)
	} else if intentResult != nil && intentResult.Skipped {
		tctx.Update(func(c *timing.TaskContext) {
			c.IntentSkipped = true
			c.IntentSkipReason = intentResult.SkipReason
		})
		timing.RecordSkip(taskNo, "intent_recognition", intentResult.SkipReason)
		log.Printf("[personal-worker] topic target resolution skipped (%s)", intentResult.SkipReason)
	}

	// Fallback: the unified intent can miss a named person two ways — its pre-fetch
	// member roster is capped at 500 (sorted by UID), and the shortcut may skip
	// intent entirely (e.g. simple_channel_constraint when memberMap is empty so the
	// member-name guard can't fire). Either way targetUIDs ends up empty and the
	// summary would over-widen to ALL messages. So re-resolve against the actual
	// post-fetch senders (nameMap, untruncated) whenever we have no target and the
	// topic is not purely generic (pure_generic_topic by definition names no one).
	if topic := task.EffectiveTopic(); len(targetUIDs) == 0 && intentResult.SkipReason != "pure_generic_topic" && topic != "" {
		if fallback := pipeline.ResolveTopicTarget(ctx, topic, nameMap, userID, toolCallFn); len(fallback) > 0 {
			targetUIDs = fallback
			log.Printf("[personal-worker] target resolved via post-fetch fallback: %v (creator=%s)", targetUIDs, userID)
		}
	}

	// Record target person stats
	tctx.Update(func(c *timing.TaskContext) {
		c.TargetPersonCount = len(targetUIDs)
	})

	// Apply context window filter (signature changed: userID → targetUIDs)
	filterStart := time.Now()
	var userMessages []pipeline.Message
	var earlyMsg string
	if len(targetUIDs) > 0 {
		filtered := pipeline.FilterWithContext(messages, targetUIDs, p.cfg.ContextWindow)
		log.Printf("[personal-worker] FilterWithContext took %dms (%d → %d messages, targets=%v)",
			time.Since(filterStart).Milliseconds(), len(messages), len(filtered), targetUIDs)
		tctx.Update(func(c *timing.TaskContext) {
			c.FilteredCount = len(filtered)
		})
		userMessages, earlyMsg = decidePersonalMessages(targetUIDs, userID, messages, filtered)
		if earlyMsg != "" {
			// True first-person query and the creator has no messages in the selected
			// source(s): tell the user plainly instead of falling back to the whole source.
			log.Printf("[personal-worker] creator %s had no messages in selected source(s), returning self-empty notice", userID)
			return earlyMsg, nil, 0, 0, p.llm.ModelVersion(), nil
		}
		if len(filtered) == 0 {
			// Named other person(s) didn't speak in this source → whole source beats "no data".
			log.Printf("[personal-worker] target(s) %v had no messages in selected source(s), falling back to all %d messages",
				targetUIDs, len(messages))
		}
	} else {
		userMessages = messages
		log.Printf("[personal-worker] no specific target, using all %d messages (took %dms)",
			len(messages), time.Since(filterStart).Milliseconds())
	}
	if len(userMessages) == 0 {
		return noRelevantContentMessage, nil, 0, 0, p.llm.ModelVersion(), nil
	}

	// Record final message count
	tctx.Update(func(c *timing.TaskContext) {
		c.MessagesFinal = len(userMessages)
	})

	for i := range userMessages {
		if name, ok := nameMap[userMessages[i].SenderUID]; ok {
			userMessages[i].SenderName = name
		} else {
			userMessages[i].SenderName = userMessages[i].SenderUID
		}
	}

	// Assign CitationIndex to all messages (evidence pool)
	citIdx := 1
	targetMsgCount := 0
	for i := range userMessages {
		userMessages[i].CitationIndex = citIdx
		citIdx++
		if userMessages[i].IsTargetUser {
			targetMsgCount++
		}
	}
	// F1: untrimmed / fallback paths never set IsTargetUser, so targetMsgCount stays 0
	// even though every message is effectively in scope. Normalize to the full count so
	// Reduce and persistence see the real number. True-narrow paths have ≥1 IsTargetUser
	// and are unaffected.
	if targetMsgCount == 0 {
		targetMsgCount = len(userMessages)
	}

	if reportStage != nil {
		reportStage(model.WorkflowStageAnalyzeChatContent)
	}

	// Create tokenizer for token counting
	tokCfg := tokenizer.Config{
		CharsPerTokenCJK:   p.cfg.ResolveCharsPerTokenCJK(),
		CharsPerTokenASCII: p.cfg.CharsPerTokenASCII,
		KimiAPIKey:         p.cfg.KimiAPIKey,
		HTTPTimeout:        p.cfg.TokenizerHTTPTimeout,
	}
	tok := tokenizer.New(p.cfg.LLMModel, tokCfg)

	// System prompt overhead (same as used in chunking)
	const systemPromptTokens = 3000

	// Calculate total tokens for all messages
	var allContent strings.Builder
	for _, m := range userMessages {
		allContent.WriteString(m.Content)
		allContent.WriteString("\n")
	}
	// Account for per-message formatting overhead: "[idx][time] sender: " prefix
	// Typical overhead: ~15 tokens per message (RFC3339 time + brackets + sender name)
	const perMessageFormattingOverhead = 15
	estimatedTotalTokens := tok.Count(allContent.String()) + len(userMessages)*perMessageFormattingOverhead

	// Check if we can skip Map-Reduce
	// Use the minimum of skipThreshold and mapMaxTokens, then subtract system prompt overhead
	// to ensure we don't exceed the per-model context budget
	skipThreshold := p.cfg.ResolveSkipMapReduceThreshold()
	mapMaxTokens := p.cfg.ResolveMapMaxTokens()
	if mapMaxTokens > 0 && mapMaxTokens < skipThreshold {
		skipThreshold = mapMaxTokens
	}
	effectiveSkipThreshold := skipThreshold - systemPromptTokens
	skipMapReduce := tok.IsExact() && estimatedTotalTokens <= effectiveSkipThreshold
	if skipMapReduce {
		log.Printf("[personal-worker] skip Map-Reduce: totalTokens=%d <= threshold=%d (exact=%v)",
			estimatedTotalTokens, effectiveSkipThreshold, tok.IsExact())
	}

	// Token-aware chunking — resolve budget via explicit config / per-model default / global fallback
	maxTokens := p.cfg.ResolveMapMaxTokens()
	if maxTokens < 10000 {
		log.Printf("[config] resolved MapMaxTokens=%d too small, using default 100000", maxTokens)
		maxTokens = 100000
	}
	effectiveMax := maxTokens - systemPromptTokens

	var chunks [][]pipeline.Message
	var currentChunk []pipeline.Message
	currentTokens := 0

	for _, m := range userMessages {
		msgTokens := tok.Estimate(m.Content)
		if msgTokens > effectiveMax {
			log.Printf("[chunking] WARNING: single message exceeds token budget: %d > %d", msgTokens, effectiveMax)
		}
		if len(currentChunk) > 0 && currentTokens+msgTokens > effectiveMax {
			chunks = append(chunks, currentChunk)
			currentChunk = nil
			currentTokens = 0
		}
		currentChunk = append(currentChunk, m)
		currentTokens += msgTokens
		// Force flush if this single message already exceeds budget
		if msgTokens > effectiveMax {
			chunks = append(chunks, currentChunk)
			currentChunk = nil
			currentTokens = 0
		}
	}
	if len(currentChunk) > 0 {
		chunks = append(chunks, currentChunk)
	}
	log.Printf("[personal-worker] Chunking: %d messages → %d chunks (maxTokens=%d)",
		len(userMessages), len(chunks), effectiveMax)

	// Record Map-Reduce stats
	tctx.Update(func(c *timing.TaskContext) {
		c.ChunkCount = len(chunks)
		c.UsedMapReduce = len(chunks) > 1 && !skipMapReduce
	})

	startTime := task.TimeRangeStart.Format("2006-01-02 15:04")
	endTime := task.TimeRangeEnd.Format("2006-01-02 15:04")
	sourceName := "多来源"
	if len(sources) == 1 {
		sourceName = sources[0].SourceName
	}

	// Determine userName: use target's name when topic points to someone else
	var userName string
	if len(targetUIDs) == 1 && targetUIDs[0] != userID {
		userName = nameMap[targetUIDs[0]]
	}
	if userName == "" {
		userName = nameMap[userID]
	}
	if userName == "" {
		userName = userID
	}

	generationTopic := p.generationTopic(task)

	var finalContent string
	var totalTokens int

	if skipMapReduce {
		// Skip Map-Reduce: call Map once with all messages (single chunk)
		var formatted []string
		for _, m := range userMessages {
			formatted = append(formatted, fmt.Sprintf("[%d][%s] %s: %s",
				m.CitationIndex, m.SendTime, m.SenderName,
				escapeCitationMarkers(m.Content)))
		}

		if reportStage != nil {
			reportStage(model.WorkflowStageGenerateSummary)
		}

		mapStart := time.Now()
		mapCallStart := time.Now()
		var err error
		finalContent, totalTokens, err = p.llm.CallMapStream(ctx,
			joinStrings(formatted), sourceName, 0, len(userMessages),
			startTime, endTime, generationTopic, userName, streamDelta,
		)
		timing.RecordLLMSince(taskNo, "Map: 单次总结(跳过Map-Reduce)", mapCallStart, totalTokens)
		timing.Observe(taskNo, "llm_map_summary", mapStart)
		if err != nil {
			return "", nil, 0, 0, "", fmt.Errorf("map (skip map-reduce): %w", err)
		}
		// Check for failed marker - CallMap returns marker string with nil error after retries exhausted
		if strings.Contains(finalContent, service.MapFailedMarker) || finalContent == "" {
			return "", nil, 0, 0, "", fmt.Errorf("map (skip map-reduce): LLM call failed")
		}
		log.Printf("[personal-worker] Skip Map-Reduce: single Map call took %dms (tokens=%d)",
			time.Since(mapStart).Milliseconds(), totalTokens)
	} else {
		// Normal Map-Reduce flow
		type chunkResult struct {
			summary string
			tokens  int
			failed  bool
			fatal   bool
		}

		concurrency := p.cfg.WorkerMapConcurrency
		if concurrency <= 0 {
			concurrency = 1
		}

		results := make([]chunkResult, len(chunks))
		mapSem := make(chan struct{}, concurrency)
		var mapWg sync.WaitGroup

		if len(chunks) == 1 && reportStage != nil {
			// Single-chunk fast path: the Map call produces the final user-facing summary.
			// Attribute its latency to generate_summary instead of analyze_chat_content.
			reportStage(model.WorkflowStageGenerateSummary)
		}

		mapStart := time.Now()
		for i, chunk := range chunks {
			mapWg.Add(1)
			go func(idx int, c []pipeline.Message) {
				defer mapWg.Done()

				select {
				case mapSem <- struct{}{}:
				case <-ctx.Done():
					results[idx] = chunkResult{failed: true}
					return
				}
				defer func() { <-mapSem }()

				var formatted []string
				for _, m := range c {
					formatted = append(formatted, fmt.Sprintf("[%d][%s] %s: %s",
						m.CitationIndex, m.SendTime, m.SenderName,
						escapeCitationMarkers(m.Content)))
				}

				callStart := time.Now()
				var summary string
				var tokens int
				var err error
				if len(chunks) == 1 {
					summary, tokens, err = p.llm.CallMapStream(ctx,
						joinStrings(formatted), sourceName, idx, len(c),
						startTime, endTime, generationTopic, userName, streamDelta,
					)
				} else {
					summary, tokens, err = p.llm.CallMap(ctx,
						joinStrings(formatted), sourceName, idx, len(c),
						startTime, endTime, generationTopic, userName,
					)
				}
				timing.RecordLLMSince(taskNo, fmt.Sprintf("Map: 分块总结 chunk#%d", idx), callStart, tokens)
				if err != nil {
					log.Printf("[personal-worker] Map chunk %d failed: %v", idx, err)
					isFatal := strings.Contains(err.Error(), "reasoning budget exhausted")
					results[idx] = chunkResult{failed: true, fatal: isFatal}
				} else {
					results[idx] = chunkResult{summary: summary, tokens: tokens}
				}
			}(i, chunk)
		}
		mapWg.Wait()
		timing.Observe(taskNo, "llm_map_summary", mapStart)
		log.Printf("[personal-worker] Map phase took %dms (%d chunks, concurrency=%d)",
			time.Since(mapStart).Milliseconds(), len(chunks), concurrency)

		if ctx.Err() != nil {
			return "", nil, 0, 0, "", fmt.Errorf("map phase cancelled: %w", ctx.Err())
		}

		var fatalChunks []int
		for i, r := range results {
			if r.fatal {
				fatalChunks = append(fatalChunks, i)
			}
		}
		if len(fatalChunks) > 0 {
			return "", nil, 0, 0, "", fmt.Errorf(
				"Map phase aborted: reasoning budget exhausted on chunk(s) %v", fatalChunks)
		}

		var chunkSummaries []string
		for _, r := range results {
			if !r.failed && !strings.Contains(r.summary, service.MapFailedMarker) {
				chunkSummaries = append(chunkSummaries, r.summary)
				totalTokens += r.tokens
			}
		}
		if len(chunkSummaries) == 0 && len(results) > 0 {
			return "", nil, 0, 0, "", fmt.Errorf("all %d chunk(s) failed during Map phase (LLM unreachable)", len(results))
		}

		if len(chunks) > 1 && reportStage != nil {
			reportStage(model.WorkflowStageGenerateSummary)
		}

		// Reduce phase
		reduceStart := time.Now()
		var reduceTokens int

		if len(chunkSummaries) == 1 {
			// Single chunk fast path: skip Reduce, use Map result directly
			finalContent = chunkSummaries[0]
			reduceTokens = 0
			log.Printf("[pipeline] single chunk — skipping Reduce")
		} else {
			// Multiple chunks: execute Reduce to merge
			var err error
			reduceCallStart := time.Now()
			finalContent, reduceTokens, err = p.llm.CallReduceStream(ctx,
				chunkSummaries, sourceName, startTime, endTime, targetMsgCount, generationTopic, streamDelta,
			)
			timing.RecordLLMSince(taskNo, "Reduce: 合并分块总结", reduceCallStart, reduceTokens)
			if err != nil {
				return "", nil, 0, 0, "", fmt.Errorf("reduce: %w", err)
			}
		}
		totalTokens += reduceTokens
		timing.Observe(taskNo, "llm_reduce_summary", reduceStart)
		log.Printf("[personal-worker] Reduce phase took %dms", time.Since(reduceStart).Milliseconds())
	}

	// Build citations from final content
	citationStart := time.Now()
	citations := buildCitations(finalContent, userMessages, messages, nameMap)
	finalContent, citations = dedupCitations(finalContent, citations)
	finalContent = stripOrphanCitations(finalContent, citations)
	timing.Observe(taskNo, "build_citations", citationStart)
	log.Printf("[personal-worker] Citation build took %dms (%d citations)",
		time.Since(citationStart).Milliseconds(), len(citations))

	log.Printf("[personal-worker] Total executePersonalPipeline took %dms",
		time.Since(totalStart).Milliseconds())

	return finalContent, citations, targetMsgCount, totalTokens, p.llm.ModelVersion(), nil
}

// SanitizeErrorForUser is the canonical whitelist that maps a raw internal
// error string to a user-safe Chinese string suitable for an IM DM.
//
// Exported so the notify package can wire it as the single render-point
// sanitizer in Notifier.buildText (covers both the synchronous worker path
// AND the sweep/redeliver path that reads task.ErrorMessage raw from the DB).
// See PR#113 Jerry-Xin/OctoBoooot R3: sanitizing only at the worker call
// sites left the sweep path leaking DSN/IP/stack to the user DM on retry.
func SanitizeErrorForUser(errMsg string) string {
	return sanitizeErrorForUser(errMsg)
}

func sanitizeErrorForUser(errMsg string) string {
	switch {
	case strings.Contains(errMsg, "LLM API error"):
		return "AI 服务暂时不可用，请稍后重试"
	case strings.Contains(errMsg, "context deadline exceeded"):
		return "AI 处理超时，请稍后重试"
	case strings.Contains(errMsg, "all") && strings.Contains(errMsg, "chunk(s) failed"):
		return "AI 服务暂时不可用，所有分片处理失败"
	default:
		// Do not leak raw internal errors (may contain DSN, IPs, stack traces).
		// Raw error is already logged by the caller via log.Printf.
		return "AI 处理失败，请稍后重试"
	}
}
