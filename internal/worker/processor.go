package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/notify"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timing"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var callbackClient = &http.Client{Timeout: 5 * time.Second}

// Processor polls the DB for pending tasks and dispatches them to the pool.
type Processor struct {
	db         *gorm.DB
	imDB       *gorm.DB
	pool       *WorkerPool
	llm        *service.LLMClient
	cfg        *config.Config
	octoClient *service.OctoSearchBatchClient // Chat-message content source.
	stopCh     chan struct{}
	triggerCh  chan model.WorkerTriggerRequest
	meta       *MetaProcessor
	createPRFn func(tx *gorm.DB, pr *model.PersonalResult) error
	// executePipelineFn, when non-nil, replaces executePipeline. Test-only hook so
	// processTask can be driven deterministically without the LLM/IM pipeline
	// (mirrors the createPRFn injection pattern). Production leaves it nil.
	executePipelineFn func(task model.SummaryTask) error
	// dispatchPersonalFn, when non-nil, replaces the p.pool.Submit(processPersonalSummary)
	// dispatch call. Test-only seam so the dispatch DECISION (which participant_ref_ids
	// get dispatched) is observable without running the LLM personal pipeline.
	// Production leaves it nil and the real pool/processPersonalSummary is used.
	dispatchPersonalFn func(taskID, participantRefID int64)
	// notifier delivers the terminal-state (Completed/Failed) IM-bot notification
	// after a task's status is durably committed. nil = disabled (OnTaskTerminal
	// is a no-op), so it is safe to call unconditionally.
	notifier *notify.Notifier
}

// NewProcessor creates a new task processor.
func NewProcessor(db, imDB *gorm.DB, pool *WorkerPool, llm *service.LLMClient, cfg *config.Config) *Processor {
	octoClient, err := newOctoSearchClientForBackend(cfg)
	if err != nil {
		log.Fatalf("[processor] %v", err)
	}
	p := &Processor{
		db:         db,
		imDB:       imDB,
		pool:       pool,
		llm:        llm,
		cfg:        cfg,
		octoClient: octoClient,
		stopCh:     make(chan struct{}),
		triggerCh:  make(chan model.WorkerTriggerRequest, 100),
	}
	p.meta = NewMetaProcessor(p)
	return p
}

func newOctoSearchClientForBackend(cfg *config.Config) (*service.OctoSearchBatchClient, error) {
	backend := strings.ToLower(strings.TrimSpace(cfg.MessageFetchBackend))
	if backend == "" {
		backend = "batch"
	}
	switch backend {
	case "batch":
		if cfg.OctoSearchURL == "" || cfg.OctoSearchToken == "" {
			return nil, fmt.Errorf("OCTO_SEARCH_URL / OCTO_SEARCH_TOKEN are required when MESSAGE_FETCH_BACKEND=batch")
		}
		if err := validateOctoSearchURL(cfg.OctoSearchURL); err != nil {
			return nil, fmt.Errorf("invalid OCTO_SEARCH_URL: %w", err)
		}
		return service.NewOctoSearchBatchClient(cfg.OctoSearchURL, cfg.OctoSearchToken), nil
	case "mysql":
		return nil, nil
	default:
		return nil, fmt.Errorf("invalid MESSAGE_FETCH_BACKEND %q (want batch or mysql)", cfg.MessageFetchBackend)
	}
}

func validateOctoSearchURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("host is required")
	}
	for _, segment := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if segment == "v1" {
			return fmt.Errorf("must be base URL without /v1")
		}
	}
	return nil
}

// SetNotifier wires the terminal-state IM-bot notifier. Optional: when unset,
// notifyTaskTerminal is a no-op.
func (p *Processor) SetNotifier(n *notify.Notifier) { p.notifier = n }

// notifyTaskTerminal fires the terminal-state notification for a task that just
// reached a terminal status. It reloads the committed task row so the
// notification reflects the durable state (title, creator, origin channel,
// trigger type, error_message). Best-effort: any failure is swallowed inside the
// notifier and never affects the worker's completion path. Call this ONLY after
// the terminal-status DB write has succeeded.
//
// The reloaded task.error_message is the RAW internal error string (may carry
// DSN credentials, IPs, goroutine stack heads). It is intentionally persisted
// raw in the DB so ops can root-cause from logs, but it MUST NOT reach a user
// DM.
//
// Sanitization happens at a SINGLE render point inside notify.buildText (wired
// via notify.WithErrorSanitizer(SanitizeErrorForUser) at construction in
// cmd/summary-worker/main.go). That single intercept covers BOTH the
// synchronous path here AND the asynchronous sweep/redeliver path
// (notify.go: redeliver reloads task.ErrorMessage raw from DB). Pre-PR#113 R3,
// sanitize was applied here only and the sweep path leaked raw errors to the
// user DM — see Jerry-Xin/OctoBoooot R3 review. We deliberately pass the raw
// errMsg through and let buildText scrub it once at the render boundary.
func (p *Processor) notifyTaskTerminal(taskID int64, status int) {
	if p.notifier == nil {
		return
	}
	var task model.SummaryTask
	if err := p.db.First(&task, taskID).Error; err != nil {
		log.Printf("[processor] notifyTaskTerminal: reload task %d failed: %v", taskID, err)
		return
	}
	errMsg := ""
	if task.ErrorMessage != nil {
		errMsg = *task.ErrorMessage
	}
	p.notifier.OnTaskTerminal(task, status, errMsg)
}

// TriggerCh returns the channel for worker trigger requests.
func (p *Processor) TriggerCh() chan<- model.WorkerTriggerRequest {
	return p.triggerCh
}

// Run starts the polling loop. Call Stop() to exit.
func (p *Processor) Run() {
	interval := time.Duration(p.cfg.WorkerPollInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("[processor] polling every %v", interval)
	for {
		select {
		case <-p.stopCh:
			log.Println("[processor] stopping")
			return
		case <-ticker.C:
			p.poll()
		case req := <-p.triggerCh:
			p.handleTrigger(req)
		}
	}
}

func (p *Processor) handleTrigger(req model.WorkerTriggerRequest) {
	switch req.Type {
	case "personal_summary":
		p.pool.Submit(func() {
			p.processPersonalSummary(context.Background(), req.TaskID, req.ParticipantRefID)
		})
	case "personal_regenerate":
		p.pool.Submit(func() {
			p.processPersonalSummaryAllowCompleted(context.Background(), req.TaskID, req.ParticipantRefID)
		})
	case "meta_summary":
		p.meta.TriggerMetaSummary(req.TaskID)
	default:
		log.Printf("[processor] unknown trigger type: %s", req.Type)
	}
}

// Stop signals the processor to stop polling.
func (p *Processor) Stop() {
	close(p.stopCh)
}

func (p *Processor) poll() {
	now := timezone.Now()
	deadline := now.Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute)

	// Claim tasks atomically: two-step select-then-claim per task.
	for i := 0; i < 10; i++ {
		// Step 1: find a candidate pending task
		var candidate model.SummaryTask
		if err := p.db.Where("status = ? AND retry_count < ? AND (processing_deadline IS NULL OR processing_deadline < ?) AND deleted_at IS NULL",
			model.StatusPending, p.cfg.WorkerMaxRetry, now).
			Order("id ASC").Limit(1).First(&candidate).Error; err != nil {
			return // no pending tasks
		}

		// Step 2: atomically claim it by ID (prevents race with other workers)
		result := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", candidate.ID, model.StatusPending).
			Updates(map[string]interface{}{
				"status":              model.StatusProcessing,
				"processing_deadline": deadline,
			})
		if result.Error != nil {
			log.Printf("[processor] claim task %d: %v", candidate.ID, result.Error)
			return
		}
		if result.RowsAffected == 0 {
			continue // another worker claimed it, try next
		}

		// Reload to get fresh state after claim
		var task model.SummaryTask
		if err := p.db.First(&task, candidate.ID).Error; err != nil {
			log.Printf("[processor] reload task %d: %v", candidate.ID, err)
			continue
		}

		p.pool.Submit(func() {
			p.processTask(task)
		})
	}
}

// dispatchPersonal submits the personal-summary worker for one participant.
// Routed through a single seam so tests can observe the dispatch DECISION (which
// participant_ref_ids are dispatched) without running the LLM pipeline; production
// uses the real pool + processPersonalSummary.
func (p *Processor) dispatchPersonal(taskID, participantRefID int64) {
	if p.dispatchPersonalFn != nil {
		p.dispatchPersonalFn(taskID, participantRefID)
		return
	}
	p.pool.Submit(func() {
		p.processPersonalSummary(context.Background(), taskID, participantRefID)
	})
}

func (p *Processor) processTask(task model.SummaryTask) {
	log.Printf("[processor] processing task %d (%s)", task.ID, task.TaskNo)

	// Send progress callback
	p.sendCallback(model.TaskEvent{
		TaskID:   task.ID,
		Status:   model.StatusProcessing,
		Progress: 10,
		Message:  "开始处理",
	})

	exec := p.executePipeline
	if p.executePipelineFn != nil {
		exec = p.executePipelineFn
	}
	err := exec(task)
	if err != nil {
		log.Printf("[processor] task %d failed: %v", task.ID, err)
		errMsg := err.Error()
		newRetry := task.RetryCount + 1
		newStatus := model.StatusPending
		if newRetry >= p.cfg.WorkerMaxRetry {
			newStatus = model.StatusFailed
		}
		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"status":              newStatus,
				"retry_count":         newRetry,
				"error_message":       errMsg,
				"processing_deadline": nil,
			})
		if casResult.Error != nil {
			log.Printf("[processor] task %d update to failed/retry failed: %v", task.ID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			log.Printf("[processor] task %d status changed during processing (likely cancelled), skipping failure update", task.ID)
			return
		}
		p.sendCallback(model.TaskEvent{
			TaskID:  task.ID,
			Status:  newStatus,
			Message: errMsg,
		})
		if newStatus == model.StatusFailed {
			p.notifyTaskTerminal(task.ID, model.StatusFailed)
		}
		return
	}

	// Success — all tasks are BY_PERSON
	var participantCount int64
	if err := p.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&participantCount).Error; err != nil {
		log.Printf("[processor] task %d count participants failed: %v", task.ID, err)
		errMsg := err.Error()
		newRetry := task.RetryCount + 1
		newStatus := model.StatusPending
		if newRetry >= p.cfg.WorkerMaxRetry {
			newStatus = model.StatusFailed
		}
		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"status":              newStatus,
				"retry_count":         newRetry,
				"error_message":       errMsg,
				"processing_deadline": nil,
			})
		if casResult.Error != nil {
			log.Printf("[processor] task %d update to failed/retry failed: %v", task.ID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			log.Printf("[processor] task %d status changed during processing (likely cancelled), skipping failure update", task.ID)
			return
		}
		p.sendCallback(model.TaskEvent{
			TaskID:  task.ID,
			Status:  newStatus,
			Message: errMsg,
		})
		if newStatus == model.StatusFailed {
			p.notifyTaskTerminal(task.ID, model.StatusFailed)
		}
		return
	}

	// Defensive bootstrap: scheduled tasks (and legacy tasks reset for re-run)
	// may have no participant rows. Without at least the creator participant +
	// its personal_result, the single-participant branch below has nothing to
	// dispatch and the task is stuck in Processing forever. Create them here
	// (idempotently) so the personal pipeline can run end-to-end.
	if participantCount == 0 {
		creatorParticipantID, err := p.bootstrapCreatorParticipant(task)
		if err != nil {
			log.Printf("[processor] task %d bootstrap creator artifacts failed: %v", task.ID, err)
			errMsg := err.Error()
			newRetry := task.RetryCount + 1
			newStatus := model.StatusPending
			if newRetry >= p.cfg.WorkerMaxRetry {
				newStatus = model.StatusFailed
			}
			casResult := p.db.Model(&model.SummaryTask{}).
				Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
				Updates(map[string]interface{}{
					"status":              newStatus,
					"retry_count":         newRetry,
					"error_message":       errMsg,
					"processing_deadline": nil,
				})
			if casResult.Error != nil {
				log.Printf("[processor] task %d update to failed/retry failed: %v", task.ID, casResult.Error)
				return
			}
			if casResult.RowsAffected == 0 {
				log.Printf("[processor] task %d status changed during processing (likely cancelled), skipping failure update", task.ID)
				return
			}
			p.sendCallback(model.TaskEvent{
				TaskID:  task.ID,
				Status:  newStatus,
				Message: errMsg,
			})
			if newStatus == model.StatusFailed {
				p.notifyTaskTerminal(task.ID, model.StatusFailed)
			}
			return
		}
		participantCount = 1
		log.Printf("[processor] task %d bootstrapped creator participant %d", task.ID, creatorParticipantID)
	}

	if participantCount <= 1 {
		var participants []model.SummaryParticipant
		p.db.Where("task_id = ?", task.ID).Find(&participants)

		// Scheduled single-person tasks must not depend on the best-effort
		// processing_deadline refresh. The multi-person scheduled path already
		// dispatches before refreshing the deadline for the same reason: a racing
		// stuck-scan/claim can make the CAS miss even though this run is still
		// valid. If we return on that miss, the task stays visually stuck at the
		// first workflow step because no personal worker is ever dispatched.
		if task.TriggerType == model.TriggerScheduled {
			for _, pt := range participants {
				p.db.Model(&model.SummaryParticipant{}).Where("id = ?", pt.ID).
					Update("status", model.ParticipantAccepted)
				ptID := pt.ID
				p.dispatchPersonal(task.ID, ptID)
			}

			casResult := p.db.Model(&model.SummaryTask{}).
				Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
				Updates(map[string]interface{}{
					"processing_deadline": timezone.Now().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute),
				})
			if casResult.Error != nil {
				log.Printf("[processor] task %d scheduled single deadline refresh failed (dispatch already done): %v", task.ID, casResult.Error)
			} else if casResult.RowsAffected == 0 {
				var cur model.SummaryTask
				if err := p.db.Select("status").First(&cur, task.ID).Error; err == nil {
					log.Printf("[processor] task %d scheduled single deadline refresh missed (status=%d); dispatch already issued for %d participant(s)", task.ID, cur.Status, len(participants))
				}
			}
			p.sendCallback(model.TaskEvent{
				TaskID:   task.ID,
				Status:   model.StatusProcessing,
				Progress: 50,
				Message:  "单人模式，自动处理中",
			})
			log.Printf("[processor] task %d scheduled single participant, dispatched %d participant(s)", task.ID, len(participants))
			return
		}

		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"processing_deadline": timezone.Now().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute),
			})
		if casResult.Error != nil {
			log.Printf("[processor] task %d CAS update failed: %v", task.ID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			var cur model.SummaryTask
			if err := p.db.Select("status").First(&cur, task.ID).Error; err != nil {
				log.Printf("[processor] task %d status reload failed after CAS miss: %v", task.ID, err)
				return
			}
			if cur.Status != model.StatusProcessing {
				log.Printf("[processor] task %d no longer Processing (status=%d), skipping single dispatch", task.ID, cur.Status)
				return
			}
		}
		for _, pt := range participants {
			p.db.Model(&model.SummaryParticipant{}).Where("id = ?", pt.ID).
				Update("status", model.ParticipantAccepted)
			ptID := pt.ID
			p.dispatchPersonal(task.ID, ptID)
		}
		p.sendCallback(model.TaskEvent{
			TaskID:   task.ID,
			Status:   model.StatusProcessing,
			Progress: 50,
			Message:  "单人模式，自动处理中",
		})
		log.Printf("[processor] task %d single participant, skipping WaitingConfirm", task.ID)
	} else {
		isScheduledMultiPerson := task.TriggerType == model.TriggerScheduled

		// Scheduled dispatch must not depend on the deadline-refresh CAS.
		//
		// The AUTO scheduled multi-person dispatch used to sit
		// BEHIND a "refresh processing_deadline" CAS (WHERE status=Processing). When that
		// CAS returned RowsAffected==0 the code logged "status changed (likely cancelled),
		// skipping dispatch" and RETURNED -- without dispatching anyone AND without putting
		// the task into any terminal/recoverable state. The task stayed Processing; 20min
		// later scanStuckTasks revived it Processing->Pending (retry+1); poll() re-claimed
		// it; processTask ran again; the same CAS missed again; skip again. Permanent
		// Processing spin, retry_count climbing, nothing ever dispatched.
		//
		// The CAS can legitimately miss even for a task we still "own": a concurrent
		// scanStuckTasks revive (deadline lapse / timezone-vs-wallclock skew across
		// processes), a second worker replica, or a stale reload struct racing the row.
		// Treating "CAS missed" as "give up, leave it Processing" is the defect -- the
		// dispatch logic itself is correct; it was gated by the CAS.
		//
		// FIX (direction A + B): for EVERY scheduled multi-person task (AUTO *and*
		// CONFIRM), decide+dispatch on the MAIN path up front (idempotent:
		// scheduledAutoDispatchTargets only selects Accepted participants whose
		// personal_result.worker_status==Pending, so re-entry never re-dispatches an
		// already-running/finished personal). The deadline refresh is now best-effort
		// AFTER dispatch and a miss never strands the task. For the MANUAL (non-scheduled)
		// multi-person path we keep the original guarded CAS, but a miss re-reads the real
		// status and only returns on a genuine terminal/changed state -- otherwise it
		// falls through, and the abnormal path resets to Pending instead of leaving the task pinned in Processing.
		//
		// This branch covers scheduled CONFIRM rounds as well as AUTO rounds:
		// A scheduled CONFIRM task (confirm_policy=1/2) reaches the processor with its
		// participants ALREADY materialized as confirmed Accepted members --
		// buildScheduledTaskParticipantsConfirm only writes rows for members who already
		// confirmed, and a zero-confirmation round is soft-deleted/skipped scheduler-side
		// and never reaches here. So for a scheduled task there is no "waiting for
		// confirmation" left to do at processor time -- the round is already locked to its
		// confirmed roster. Previously these CONFIRM rounds fell into the non-dispatch
		// "participants pending confirm" branch and only got picked up ~5 min later by
		// scanStuckPersonalTasks, so users stayed on the in-progress state for minutes. Driving dispatch here is SAFE because:
		//   1. scheduledAutoDispatchTargets is idempotent -- it selects ONLY
		//      status==Accepted AND pr.worker_status==Pending, so in-flight/finished
		//      personals are never re-dispatched, and unconfirmed members do not exist as
		//      participant rows for a CONFIRM scheduled round (so they can't be mis-fired).
		//   2. MANUAL CONFIRM tasks (TriggerType != TriggerScheduled) are untouched: they
		//      still fall through to the non-scheduled branch below, where the creator was
		//      already triggered by the API handler and other members stay WaitingConfirm
		//      until they accept.
		//   3. Zero-confirmation scheduled rounds are soft-deleted scheduler-side and
		//      never reach the processor, so there is nothing to special-case here.
		if isScheduledMultiPerson {
			// Dispatch FIRST -- the whole point is that dispatch must not depend on the
			// fragile CAS. Idempotent selection makes repeated claims safe.
			targets, err := scheduledAutoDispatchTargets(p.db, task.ID)
			if err != nil {
				log.Printf("[processor] task %d select scheduled dispatch targets failed: %v", task.ID, err)
			}
			for _, refID := range targets {
				p.dispatchPersonal(task.ID, refID)
			}

			// Best-effort deadline refresh so scanStuckTasks does not revive us while the
			// personal workers run. A miss here is acceptable: dispatch already happened,
			// and re-claim is idempotent. We only need to ensure we do not stay in Processing
			// with nothing in flight, so on a miss we re-read the real status.
			casResult := p.db.Model(&model.SummaryTask{}).
				Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
				Updates(map[string]interface{}{
					"processing_deadline": timezone.Now().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute),
				})
			if casResult.Error != nil {
				log.Printf("[processor] task %d scheduled deadline refresh failed (dispatch already done): %v", task.ID, casResult.Error)
			} else if casResult.RowsAffected == 0 {
				// Status is no longer Processing. If it became terminal (Completed/Failed/
				// Cancelled) that's fine -- the run is concluding. If it was bounced back to
				// Pending (e.g. a stuck-scan revive), the next claim will re-run idempotently.
				// Either way we have already dispatched the Pending personals, so we are NOT stranded.
				var cur model.SummaryTask
				if err := p.db.Select("status").First(&cur, task.ID).Error; err == nil {
					log.Printf("[processor] task %d scheduled deadline refresh missed (status=%d); dispatch already issued for %d participant(s)", task.ID, cur.Status, len(targets))
				}
			}
			p.sendCallback(model.TaskEvent{
				TaskID:   task.ID,
				Status:   model.StatusProcessing,
				Progress: 50,
				Message:  "处理中，自动汇总",
			})
			log.Printf("[processor] task %d scheduled multi-person, dispatched %d participant(s)", task.ID, len(targets))
			return
		}

		// Manual (non-scheduled) multi-person (CONFIRM / human-confirm): Creator
		// already triggered by the API handler; other participants remain WaitingConfirm
		// until they accept. We must NOT blindly dispatch everyone here. Keep the guarded
		// deadline-refresh CAS, but make a miss recoverable and never leave the task pinned
		// in Processing. (Scheduled CONFIRM rounds never reach this branch -- they take
		// the active-dispatch path above, because their roster is already confirmed.)
		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"processing_deadline": timezone.Now().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute),
			})
		if casResult.Error != nil {
			log.Printf("[processor] task %d CAS update failed: %v", task.ID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			// Re-read the real status instead of assuming "cancelled" and giving up.
			var cur model.SummaryTask
			if err := p.db.Select("status").First(&cur, task.ID).Error; err != nil {
				log.Printf("[processor] task %d status reload failed after CAS miss: %v", task.ID, err)
				return
			}
			switch cur.Status {
			case model.StatusCompleted, model.StatusFailed, model.StatusCancelled:
				// Genuinely concluded/changed -- nothing to do.
				log.Printf("[processor] task %d reached terminal status %d, skipping dispatch", task.ID, cur.Status)
				return
			case model.StatusProcessing:
				// We still own it (a benign racing deadline write). Fall through to the waiting-for-confirm path below.
			default:
				// Bounced to Pending/WaitingConfirm by a concurrent revive. Do NOT stay in
				// Processing: let the next claim handle it. (No dispatch here -- CONFIRM
				// participants must still confirm.)
				log.Printf("[processor] task %d no longer Processing (status=%d), deferring to next claim", task.ID, cur.Status)
				return
			}
		}

		p.sendCallback(model.TaskEvent{
			TaskID:   task.ID,
			Status:   model.StatusProcessing,
			Progress: 50,
			Message:  "处理中，等待参与者确认",
		})
		log.Printf("[processor] task %d multi-person, processing (participants pending confirm)", task.ID)
	}
}

// scheduleConfirmPolicyIsAuto reports whether the task's bound schedule uses the
// AUTO confirm policy (confirm_policy==0, the legacy single-person default). The confirm policy
// lives on summary_schedule, not summary_task, so it is read via task.ScheduleID.
//
// Default is FALSE (NOT auto) on every uncertain path -- safe side. Only an
// explicitly read schedule.confirm_policy == SchedConfirmAuto returns true.
// Rationale: returning true on uncertainty would dispatch all participants for a task that may be
// CONFIRM, firing personal workers without the required human confirmation and
// breaking the manual-confirm semantics. Failing closed (no auto-dispatch) is
// recoverable -- the task waits / the 5-minute stuck scan can still pick up a
// genuinely AUTO run -- whereas failing open mis-triggers CONFIRM tasks. So both
// ScheduleID==nil and a failed lookup return false. (AUTO tasks always carry a
// ScheduleID, bound at claim time, so the normal AUTO path is unaffected.)
func (p *Processor) scheduleConfirmPolicyIsAuto(task model.SummaryTask) bool {
	if task.ScheduleID == nil {
		// No bound schedule: cannot confirm AUTO. Fail closed (do not auto-dispatch).
		return false
	}
	var sched model.SummarySchedule
	if err := p.db.Select("confirm_policy").First(&sched, *task.ScheduleID).Error; err != nil {
		// Schedule lookup failed (deleted, etc.): fail closed. Do NOT default to AUTO --
		// blindly dispatching a possibly-CONFIRM task would break human-confirmation
		// semantics. Better to not run than to mis-trigger.
		log.Printf("[processor] task %d confirm_policy lookup failed (defaulting NOT-auto, safe side): %v", task.ID, err)
		return false
	}
	return sched.ConfirmPolicy == model.SchedConfirmAuto
}

// scheduledAutoDispatchTargets returns the participant_ref_ids that must be
// dispatched for an AUTO scheduled multi-person task: every participant whose
// status is Accepted and whose personal_result.worker_status is still Pending
// (i.e. not yet started by a personal worker). This is the explicit replacement
// for relying on the 5-minute scanStuckPersonalTasks fallback. It is a pure
// query (no side effects) so the dispatch decision is unit-testable without the
// LLM pipeline; the caller performs the actual p.pool.Submit.
func scheduledAutoDispatchTargets(db *gorm.DB, taskID int64) ([]int64, error) {
	var refIDs []int64
	err := db.Model(&model.SummaryParticipant{}).
		Joins("JOIN summary_personal_result pr ON pr.participant_ref_id = summary_participant.id").
		Where("summary_participant.task_id = ? AND summary_participant.status = ? AND pr.worker_status = ?",
			taskID, model.ParticipantAccepted, model.PersonalStatusPending).
		Pluck("summary_participant.id", &refIDs).Error
	return refIDs, err
}

func (p *Processor) bootstrapCreatorParticipant(task model.SummaryTask) (int64, error) {
	now := timezone.Now()
	creatorName := service.ResolveUserName(task.CreatorID)
	var participant model.SummaryParticipant

	err := p.db.Transaction(func(tx *gorm.DB) error {
		participant = model.SummaryParticipant{
			TaskID:      task.ID,
			UserID:      task.CreatorID,
			UserName:    creatorName,
			Status:      model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}, {Name: "user_id"}},
			DoNothing: true,
		}).Create(&participant)
		if result.Error != nil {
			return fmt.Errorf("upsert participant: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			if err := tx.Where("task_id = ? AND user_id = ?", task.ID, task.CreatorID).First(&participant).Error; err != nil {
				return fmt.Errorf("load existing participant: %w", err)
			}
		}

		participantUpdates := map[string]interface{}{}
		if participant.UserName == "" {
			participantUpdates["user_name"] = creatorName
		}
		if participant.Status != model.ParticipantAccepted {
			participantUpdates["status"] = model.ParticipantAccepted
		}
		if participant.ConfirmedAt == nil {
			participantUpdates["confirmed_at"] = now
		}
		if len(participantUpdates) > 0 {
			if err := tx.Model(&model.SummaryParticipant{}).Where("id = ?", participant.ID).Updates(participantUpdates).Error; err != nil {
				return fmt.Errorf("normalize participant: %w", err)
			}
		}

		pr := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: participant.ID,
			UserID:           task.CreatorID,
			WorkerStatus:     model.PersonalStatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		result = tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}, {Name: "participant_ref_id"}},
			DoNothing: true,
		})
		if p.createPRFn != nil {
			if err := p.createPRFn(result, &pr); err != nil {
				return fmt.Errorf("upsert personal_result: %w", err)
			}
		} else {
			result = result.Create(&pr)
			if err := result.Error; err != nil {
				return fmt.Errorf("upsert personal_result: %w", err)
			}
		}
		if result.RowsAffected == 0 {
			if err := tx.Where("task_id = ? AND participant_ref_id = ?", task.ID, participant.ID).First(&pr).Error; err != nil {
				return fmt.Errorf("load existing personal_result: %w", err)
			}
		}

		if participant.PersonalResultID == nil || *participant.PersonalResultID != pr.ID {
			if err := tx.Model(&model.SummaryParticipant{}).
				Where("id = ? AND (personal_result_id IS NULL OR personal_result_id <> ?)", participant.ID, pr.ID).
				Update("personal_result_id", pr.ID).Error; err != nil {
				return fmt.Errorf("link participant personal_result: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}
	return participant.ID, nil
}

func (p *Processor) executePipeline(task model.SummaryTask) error {
	pipelineStart := time.Now()
	defer func() { timing.Observe(task.TaskNo, "execute_pipeline_total", pipelineStart) }()
	ctx := context.Background()

	// Load sources
	var sources []model.SummarySource
	if err := p.db.Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
		return fmt.Errorf("load sources: %w", err)
	}

	// Build specified sources for pipeline
	specifiedSources := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		specifiedSources = append(specifiedSources, map[string]interface{}{
			"source_id":   s.SourceID,
			"source_type": s.SourceType,
			"source_name": s.SourceName,
		})
	}

	// Fetch messages via pipeline. Tool-call / raw LLM uses in this (fetch) path
	// are accounted under the same task_no, so they appear in the same per-run
	// report that personal_pipeline flushes at the end.
	toolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		callStart := time.Now()
		args, tokens, err := p.llm.CallWithTools(ctx, messages, tools, forceFn, p.cfg.LLMTemperature)
		purpose := "检索预处理(tool-call)"
		if forceFn != "" {
			purpose = "检索预处理: " + forceFn
		}
		timing.RecordLLMSince(task.TaskNo, purpose, callStart, tokens)
		return args, err
	}
	llmFn := func(ctx context.Context, prompt string) (string, error) {
		callStart := time.Now()
		out, err := p.llm.CallRaw(ctx, prompt)
		timing.RecordLLMSince(task.TaskNo, "检索后裁剪 PostRetrievalNarrow", callStart, 0)
		return out, err
	}

	var messages []pipeline.Message
	var err error

	// Load participants for this task
	var participants []model.SummaryParticipant
	if err := p.db.Where("task_id = ?", task.ID).Find(&participants).Error; err != nil {
		return fmt.Errorf("load participants: %w", err)
	}
	var participantUIDs []string
	var participantNames []string
	for _, pt := range participants {
		if pt.UserID != task.CreatorID {
			participantUIDs = append(participantUIDs, pt.UserID)
			participantNames = append(participantNames, pt.UserName)
		}
	}

	var channelScopeOpts *pipeline.ChannelScopeOptions
	if p.cfg.ChannelScopeEnabled {
		channelScopeOpts = &pipeline.ChannelScopeOptions{
			Enabled: true,
		}
	}

	fetchStart := time.Now()
	messages, _, err = pipeline.ResolveAndFetchMessagesForPersonal(
		ctx, task.CreatorID, participantUIDs, participantNames, specifiedSources, task.EffectiveTopic(),
		task.TimeRangeStart, task.TimeRangeEnd,
		p.imDB, p.octoClient, p.cfg.MessageFetchBackend, toolCallFn, llmFn, p.cfg.MsgTableCount, p.cfg.MaxMessagesPerChannel, p.cfg.FetchConcurrency, p.cfg.OctoSearchPollSec,
		channelScopeOpts, nil,
	)
	timing.Observe(task.TaskNo, "fetch_messages", fetchStart)
	if err != nil {
		return fmt.Errorf("fetch messages: %w", err)
	}

	if len(messages) == 0 {
		log.Printf("[processor] task %d: 0 messages fetched", task.ID)
		return nil
	}

	// Personal summaries will be generated by personal_processor.
	// NOTE: total message count is persisted on SummaryResult.TotalMsgCount
	// by personal_processor; summary_task has no total_msg_count column.
	log.Printf("[processor] task %d: %d messages fetched, personal_processor will handle summaries", task.ID, len(messages))
	return nil
}

func (p *Processor) sendCallback(event model.TaskEvent) {
	body, err := json.Marshal(event)
	if err != nil {
		log.Printf("[processor] marshal callback: %v", err)
		return
	}

	resp, err := callbackClient.Post(p.cfg.WorkerCallbackURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[processor] callback POST failed: %v", err)
		// Fallback: save event to DB
		p.db.Create(&model.SummaryEvent{
			TaskID:   event.TaskID,
			Status:   event.Status,
			Progress: event.Progress,
			Message:  event.Message,
		})
		return
	}
	resp.Body.Close()
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += "\n"
		}
		result += s
	}
	return result
}

// batchResolveUserNames queries the IM DB for display names of the given message senders.
// Returns a map from UID to display name. UIDs that cannot be resolved are omitted.
func (p *Processor) batchResolveUserNames(messages []pipeline.Message) map[string]string {
	nameMap := make(map[string]string)
	if p.imDB == nil || len(messages) == 0 {
		return nameMap
	}

	// Collect unique UIDs
	uidSet := make(map[string]bool)
	for _, msg := range messages {
		if msg.SenderUID != "" {
			uidSet[msg.SenderUID] = true
		}
	}
	if len(uidSet) == 0 {
		return nameMap
	}

	uids := make([]string, 0, len(uidSet))
	for uid := range uidSet {
		uids = append(uids, uid)
	}

	// Batch query from user table
	type userRow struct {
		UID  string `gorm:"column:uid"`
		Name string `gorm:"column:name"`
	}
	var rows []userRow
	if err := p.imDB.Raw("SELECT uid, name FROM `user` WHERE uid IN ?", uids).Scan(&rows).Error; err != nil {
		log.Printf("[processor] batch resolve user names: %v", err)
		return nameMap
	}
	for _, r := range rows {
		if r.Name != "" {
			nameMap[r.UID] = r.Name
		}
	}
	log.Printf("[processor] resolved %d/%d user names", len(nameMap), len(uids))
	return nameMap
}
