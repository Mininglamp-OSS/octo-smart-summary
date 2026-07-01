package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/ws"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var triggerClient = &http.Client{Timeout: 5 * time.Second}

// acceptReviveLeaseMinutes is the initial processing_deadline lease applied when
// Accept revives an already-Completed multi-person BY_PERSON task back to
// Processing (so the stuck-task scanner -- processor.go: status==Processing AND
// processing_deadline < now -- does not immediately reclaim it before the
// personal-worker claims and refreshes the deadline). The worker resets this the
// instant it picks the task up; this only needs to outlive the async dispatch
// hop. Kept in sync with the worker's default WORKER_TASK_LEASE_MINUTES (20).
const acceptReviveLeaseMinutes = 20

// errPersonalResultGone is a sentinel returned from the PersonalEdit transaction
// when the caller's PersonalResult row was deleted concurrently (e.g. a racing
// Leave/RemoveMember) between preload and the UPDATE, so 0 rows are affected.
// The outer handler maps it to 404/40008 rather than a 500.
var errPersonalResultGone = errors.New("personal result gone")

// errDraftAlreadySubmitted is a sentinel returned from the PersonalDraft
// transaction when the conditional UPDATE (WHERE submitted_at IS NULL) matches
// 0 rows AND a follow-up SELECT confirms the row still exists -- i.e. a racing
// Submit (manual or system back-fill) set submitted_at between PersonalDraft's
// fast-path read and the UPDATE. The outer handler maps it to 409/40016
// ("草稿已被提交"). Splitting this from errPersonalResultGone (404/40008) lets
// the front-end disambiguate "row physically gone (Leave)" from "row exists but
// already submitted (must reload to switch to /personal-edit)".
//
// 40016 was chosen because schedule.go already owns 40015 for
// errMultiPersonNotSupported; 40016 is the next free slot in the 4001x band
// (verified empty across internal/ at branch creation). If a future change
// claims 40016, shift to the next free slot in the same band and update the
// docstring + tests.
var errDraftAlreadySubmitted = errors.New("personal draft already submitted")

// errDraftPublished is a sentinel returned from the PersonalDraft transaction
// when the gate-in-tx SELECT confirms `summary_result` already exists AND
// `task.status == Completed` -- i.e. the team summary has been published
// while the PersonalDraft caller was in flight. The outer handler maps it to
// 409/40016 ("该总结已发布，请改走编辑接口"). This is the authoritative
// counterpart to the fast-path published check; it closes the residual TOCTOU
// window where the pre-tx count read observed 0 rows but
// `saveLatestResultAndCompleteTask` (scheduled_replace_helpers.go:364-419)
// commits between the fast-path and the tx start.
//
// 40016 is intentionally shared with errDraftAlreadySubmitted -- both are
// "draft window is closed" (submitted OR published), and the front-end fallback
// (reload -> switch to /personal-edit) is identical.
//
// sqlite `:memory:` FOR UPDATE caveat: our handler-level unit tests run against
// sqlite `:memory:`, which silently ignores `FOR UPDATE` (see
// members_auth_test.go:1837-1845). Wall-clock lock timing therefore cannot be
// reproduced at the handler test tier; T24 covers this branch as a
// decision-layer verification (seed a summary_result + flip task.status inside
// a tx-scoped GORM callback so the gate-in-tx SELECT sees it) rather than as a
// true race. Real lock-ordering evidence lives in production MySQL / an
// integration-test seam, which is out of scope for the "personal.go-only"
// change.
var errDraftPublished = errors.New("personal draft published")

// errDraftRegenerating is a sentinel returned from the PersonalDraft
// transaction under two paths, both of which map to 409/40009 ("内容已被重新
// 生成，请刷新后重试", matching edit.go:87/187 verbatim):
//
//	(1) gate-in-tx SELECT confirms `summary_result` already exists AND
//	    `task.status == Processing` -- i.e. the task was revived post-publish
//	    (Leave / RemoveMember / AddMembers-Accept in personal.go:1411 /
//	    :210-261) but `summary_result` was NOT deleted (revive keeps it). The
//	    row on disk is stale relative to the new roster, and drafts landing
//	    here would silently fork `personal_result` from `summary_result`.
//	(2) The RowsAffected==0 probe branch (see probe-(d) below) discovers
//	    `worker_status != Completed`. The ONLY natural production trigger for
//	    this branch is a Regenerate transaction (task.go:927-966) that
//	    committed after the draft's fast-path read and before its intra-tx
//	    UPDATE: Regenerate sets `personal_result.worker_status = Pending`,
//	    deletes `summary_result`, and resets `task.status = Pending` +
//	    participant state. The gate-in-tx already closes the primary
//	    worker-race (saveLatestResult interleaving); probe-(d) is the
//	    residual defence for the Regenerate seam. revive
//	    (personal.go:1411-1428, and its Accept inline sibling
//	    personal.go:210-261) does NOT touch worker_status, so this branch is
//	    unreachable via the revive path -- callers reading the code should
//	    not confuse the two.
//
// 40009 is intentionally shared with schedule.go ("该定时已绑定其它总结") and
// edit.go ("内容已被重新生成"): the code is an endpoint-local
// retryable-conflict signal; the front-end MUST dispatch on (endpoint, code,
// message), not on code alone. See §3 of the v2 plan for the front-end
// contract.
var errDraftRegenerating = errors.New("personal draft regenerating")

// PersonalHandler handles P2 by-person endpoints.
type PersonalHandler struct {
	db               *gorm.DB
	workerTriggerURL string
	hub              *ws.Hub
}

// NewPersonalHandler creates a new PersonalHandler.
func NewPersonalHandler(db *gorm.DB, workerTriggerURL string, hub *ws.Hub) *PersonalHandler {
	return &PersonalHandler{db: db, workerTriggerURL: workerTriggerURL, hub: hub}
}

func (h *PersonalHandler) parseTaskID(c *gin.Context) (int64, bool) {
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return 0, false
	}
	return taskID, true
}

// requireTaskInSpace enforces P1 cross-space isolation: the task must exist and
// belong to the caller's space (middleware.GetSpaceID). On a missing task OR a
// space mismatch it writes 40008/404 ("任务不存在") -- identical to a missing task so
// task existence is never leaked across spaces -- and returns (nil, false). An
// empty X-Space-Id is rejected fail-closed below before any query, sealing the
// cross-space path for the (task_id,user_id)-only personal endpoints (Accept /
// Decline / Submit / GetPersonal). Mirrors authorizeTaskAccess / GetMembers.
//
// The loaded task is returned on success so callers that need to inspect
// task-level fields (e.g. PersonalDraft's BY_PERSON mode gate) can do so
// without issuing a second SELECT. Callers that do not care about the task
// discard it via `_, taskOK := ...`.
func (h *PersonalHandler) requireTaskInSpace(c *gin.Context, taskID int64) (*model.SummaryTask, bool) {
	spaceID := middleware.GetSpaceID(c)
	// fail-closed hard gate: an empty X-Space-Id must NEVER reach the query.
	// SummaryTask.SpaceID is `not null default ''`, so tasks with space_id='' may
	// exist; querying `space_id=''` would MATCH them, leaking a cross-space read
	// (fail-open). Short-circuit to 40008/404 here, independent of data invariant.
	if spaceID == "" {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return nil, false
	}
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return nil, false
	}
	return &task, true
}

// Accept handles POST /api/v1/summaries/:id/accept
func (h *PersonalHandler) Accept(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	// P1 (cross-space isolation): the task must exist AND live in the caller's
	// space before any (task_id,user_id) participant lookup; a space mismatch is
	// reported as 40008/404 like a missing task, so existence is not leaked and a
	// cross-space accept cannot mutate another space's participant.
	_, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}

	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "你不是该任务的参与者"})
		return
	}

	// Idempotent: already accepted or beyond → 200
	if participant.Status == model.ParticipantAccepted ||
		participant.Status == model.ParticipantProcessing ||
		participant.Status == model.ParticipantCompleted ||
		participant.Status == model.ParticipantSubmitted {
		ok(c, gin.H{"status": model.ParticipantStatusLabel(participant.Status)})
		return
	}

	// Declined cannot be undone
	if participant.Status == model.ParticipantDeclined {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "已拒绝，不能反悔"})
		return
	}

	now := timezone.Now()

	// 🔴 Unique-key 500 fix: the AUTO scheduled dispatch path may pre-create a
	// summary_personal_result row (uk_task_participant(task_id, participant_ref_id))
	// while the participant itself is still Pending. The status-only idempotency
	// guard above does not catch that case, so the old UNCONDITIONAL tx.Create(&pr)
	// violated the unique key and returned 500 "Duplicate entry" -- the user saw
	// their Accept fail / appear as a reject. Make the create idempotent: upsert with
	// DoNothing on conflict, then read back the surviving row and reuse it. This is
	// the same pattern bootstrapCreatorParticipant uses in the worker (zero DB/schema
	// change, pure application-level idempotency).
	var prCompleted bool
	err := h.db.Transaction(func(tx *gorm.DB) error {
		// Update participant to accepted
		if err := tx.Model(&participant).Updates(map[string]interface{}{
			"status":       model.ParticipantAccepted,
			"confirmed_at": now,
		}).Error; err != nil {
			return err
		}

		// Idempotent create of the personal result. On conflict with an existing
		// (task_id, participant_ref_id) row, do nothing and reuse the existing row.
		pr := model.PersonalResult{
			TaskID:           taskID,
			ParticipantRefID: participant.ID,
			UserID:           userID,
			Content:          "",
			WorkerStatus:     model.PersonalStatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}, {Name: "participant_ref_id"}},
			DoNothing: true,
		}).Create(&pr)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// A personal_result already exists (e.g. pre-created by AUTO dispatch).
			// Reuse it instead of inserting a duplicate.
			if err := tx.Where("task_id = ? AND participant_ref_id = ?", taskID, participant.ID).
				First(&pr).Error; err != nil {
				return err
			}
			// If the existing row is in a terminal state (Completed/Submitted), don't
			// reset it -- accept stays idempotent and the worker is not re-triggered
			// over a finished result. Otherwise make sure worker_status is Pending so
			// scheduledAutoDispatchTargets (Accepted && worker_status==Pending) can pick it up for (re)dispatch.
			if pr.WorkerStatus == model.PersonalStatusCompleted || pr.SubmittedAt != nil {
				prCompleted = true
			} else if pr.WorkerStatus != model.PersonalStatusPending {
				if err := tx.Model(&model.PersonalResult{}).Where("id = ?", pr.ID).
					Update("worker_status", model.PersonalStatusPending).Error; err != nil {
					return err
				}
			}
		}

		// Link personal_result_id back to participant
		if err := tx.Model(&participant).Update("personal_result_id", pr.ID).Error; err != nil {
			return err
		}

		// 🔴 "Revive" fix: a member added to an ALREADY-Completed multi-person
		// BY_PERSON task can never get their personal summary generated. AddMembers
		// puts the new member on the roster as Pending without touching task.status;
		// by the time they Accept, the old members have long since aggregated the team
		// summary and the task is Completed(status=3). Accept here creates their
		// PersonalResult(Pending) and dispatches personal_summary, but the
		// personal-worker's task CAS only moves Pending/WaitingConfirm -> Processing
		// (personal_processor.go L113). A Completed task fails that CAS, its current
		// status != Processing, and the worker aborts ("not in processing state,
		// aborting") -- the new member's summary never runs and worker_status stays Pending forever.
		//
		// Fix: when we are actually going to dispatch a NEW personal summary
		// (!prCompleted), and ONLY for a multi-person BY_PERSON task whose task is
		// currently Completed, pull the task back to Processing with a CONDITIONAL
		// UPDATE (... WHERE id=? AND status=Completed). The conditional WHERE makes
		// this race-safe and a strict no-op for any task NOT in Completed:
		//   - task still Processing (normal in-flight multi-person add) -> 0 rows,
		//     left untouched; the worker takes its existing already-Processing branch.
		//   - single-person / BY_GROUP -> guarded out below, never reset.
		//   - idempotent repeat Accept (prCompleted / already-Accepted fast-path) ->
		//     never reaches here, so no repeat reset / dispatch.
		// personal_processor, once it finishes this member, calls TriggerMetaSummary;
		// meta's completion gate (every rostered member terminal: submitted_at NOT
		// NULL OR Failed) then re-aggregates a fresh team-summary version that
		// includes the new member -- the link is self-consistent.
		if !prCompleted {
			var task model.SummaryTask
			if err := tx.Select("id", "summary_mode").First(&task, taskID).Error; err != nil {
				return err
			}
			if task.SummaryMode == model.ModeByPerson {
				var participantCount int64
				if err := tx.Model(&model.SummaryParticipant{}).
					Where("task_id = ?", taskID).Count(&participantCount).Error; err != nil {
					return err
				}
				if participantCount > 1 {
					// Race-safe revive: only a Completed task is pulled back. The worker
					// refreshes processing_deadline the moment it claims, so this initial
					// lease only has to outlive the dispatch hop; use the standard lease.
					deadline := now.Add(acceptReviveLeaseMinutes * time.Minute)
					if err := tx.Model(&model.SummaryTask{}).
						Where("id = ? AND status = ?", taskID, model.StatusCompleted).
						Updates(map[string]interface{}{
							"status":              model.StatusProcessing,
							"processing_deadline": deadline,
						}).Error; err != nil {
						return err
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		log.Printf("[personal] accept tx error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: err.Error()})
		return
	}

	// Trigger personal summary worker (async, non-blocking). Skip when the existing
	// result is already terminal so we never overwrite a finished/submitted summary.
	if !prCompleted {
		go h.triggerWorker(model.WorkerTriggerRequest{
			Type:             "personal_summary",
			TaskID:           taskID,
			ParticipantRefID: participant.ID,
		})
	}

	ok(c, gin.H{"status": "accepted"})
}

// Decline handles POST /api/v1/summaries/:id/decline
func (h *PersonalHandler) Decline(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	// P1 (cross-space isolation): task must exist in the caller's space first.
	_, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}

	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "你不是该任务的参与者"})
		return
	}

	// Only pending participants can decline
	if participant.Status != model.ParticipantPending {
		if participant.Status == model.ParticipantDeclined {
			ok(c, gin.H{"status": "declined"})
			return
		}
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "当前状态不允许拒绝"})
		return
	}

	h.db.Model(&participant).Update("status", model.ParticipantDeclined)
	ok(c, gin.H{"status": "declined"})
}

// Respond handles POST /api/v1/summaries/:id/respond
// Accepts {"action": "accept"} or {"action": "reject"}.
func (h *PersonalHandler) Respond(c *gin.Context) {
	var req struct {
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}

	switch req.Action {
	case "accept":
		h.Accept(c)
	case "reject":
		h.Decline(c)
	default:
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "action must be 'accept' or 'reject'"})
	}
}

// GetPersonal handles GET /api/v1/summaries/:id/personal
func (h *PersonalHandler) GetPersonal(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	// P1 (cross-space isolation): the task must exist in the caller's space before
	// reading any personal_result. A space mismatch is 40008/404 like a missing
	// task (no existence leak). This runs BEFORE the "no pr -> default" fallback so
	// a cross-space caller can never coax a default-shaped 200 out of another
	// space's task; the missing-pr default behaviour for in-space callers is unchanged.
	_, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}

	var pr model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err != nil {
		// Not found → return default
		ok(c, gin.H{
			"worker_status": 0,
			"content":       "",
			"submitted_at":  nil,
			"generated_at":  nil,
			"msg_count":     0,
		})
		return
	}

	result := gin.H{
		"worker_status": pr.WorkerStatus,
		"content":       pr.Content,
		"citations":     pr.GetCitations(),
		"submitted_at":  nil,
		"generated_at":  nil,
		"msg_count":     pr.MsgCount,
	}
	if pr.SubmittedAt != nil {
		result["submitted_at"] = pr.SubmittedAt.Format(time.RFC3339)
	}
	if pr.GeneratedAt != nil {
		result["generated_at"] = pr.GeneratedAt.Format(time.RFC3339)
	}
	ok(c, result)
}

// Submit handles POST /api/v1/summaries/:id/submit
func (h *PersonalHandler) Submit(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	// P1 (cross-space isolation): task must exist in the caller's space first.
	_, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}

	var pr model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
		return
	}

	if pr.WorkerStatus != model.PersonalStatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "个人总结未完成，无法提交"})
		return
	}

	// Idempotent fast-path: already submitted (avoids a needless write). The
	// authoritative idempotency guard is the conditional UPDATE below; this is just
	// an early exit for the common case.
	if pr.SubmittedAt != nil {
		ok(c, gin.H{"status": "submitted"})
		return
	}

	now := timezone.Now()
	// 🔴 Blocker-2 fix: concurrency-safe submit. The old code did read-then-check
	// (pr.SubmittedAt==nil) then an UNCONDITIONAL Updates -- racing the system
	// back-fill (backfillScheduledSubmittedAt, submit_source=2) it could overwrite a
	// system-written row, flipping submit_source back to 1 with no CAS/transaction.
	// Use a conditional UPDATE ... WHERE submitted_at IS NULL so exactly one writer
	// (manual OR system) ever sets submitted_at; the loser sees RowsAffected==0 and
	// returns the idempotent "already submitted" response WITHOUT rewriting source.
	res := h.db.Model(&model.PersonalResult{}).
		Where("id = ? AND submitted_at IS NULL", pr.ID).
		Updates(map[string]interface{}{
			"submitted_at":  now,
			"submit_source": model.SubmitSourceManual,
		})
	if res.Error != nil {
		log.Printf("[personal] submit conditional update error: %v", res.Error)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: res.Error.Error()})
		return
	}
	if res.RowsAffected == 0 {
		// Already submitted by a concurrent manual /submit or the system back-fill.
		// Do NOT rewrite submit_source; return the same idempotent "submitted" semantics.
		ok(c, gin.H{"status": "submitted"})
		return
	}

	// Update participant status to submitted
	h.db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, userID).
		Update("status", model.ParticipantSubmitted)

	// Broadcast MEMBER_SUBMITTED event to all subscribers
	if h.hub != nil {
		h.hub.Broadcast(taskID, gin.H{
			"type": ws.EventMemberSubmitted,
			"payload": gin.H{
				"task_id": taskID,
				"user_id": userID,
			},
		})
	}

	// Trigger meta summary worker (async)
	go h.triggerWorker(model.WorkerTriggerRequest{
		Type:   "meta_summary",
		TaskID: taskID,
	})

	ok(c, gin.H{"status": "submitted"})
}

// PersonalEdit handles PUT /api/v1/summaries/:id/personal-edit
//
// need3 + need6: a participant edits THEIR OWN personal report. The caller can
// only ever touch the PersonalResult keyed by (task_id, user_id=self) -- there is
// no way to address another member's row. On success a meta_summary worker
// trigger is fired so the team summary is recomputed to incorporate the edit
// (TriggerMetaSummary has its own 100ms debounce). Reuses edit.go's content
// validation (non-empty, <=maxContentBytes, CleanUnreferencedCitations).
func (h *PersonalHandler) PersonalEdit(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "content cannot be empty"})
		return
	}
	if len(req.Content) > maxContentBytes {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "content 超过 500KB 限制"})
		return
	}

	// P1 (cross-space isolation): the task must exist AND live in the caller's
	// space BEFORE any (task_id,user_id) participant lookup -- identical ordering to
	// Accept. requireTaskInSpace also fail-closes an empty X-Space-Id. Doing this
	// FIRST seals the cross-space participant-enumeration oracle: previously a
	// non-participant in another space saw 403/40003 ("你不是该任务的参与者") while a
	// non-existent/cross-space task saw 404/40008, and that error-code difference
	// leaked both task existence and the caller's participation across spaces. Now
	// both empty-space and cross-space uniformly return 40008 ("任务不存在"), and a
	// genuine in-space non-participant gets the same 40008 "你不是该任务的参与者"
	// semantics as Accept (no differentiated leak). This also gives BE-1's revive a
	// consistent task-existence guarantee.
	_, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}

	// Authorization: the caller MUST be a participant of this task. Membership is
	// keyed on (task_id, user_id=self). Mirrors Accept: a non-participant gets the
	// same 40008 "你不是该任务的参与者" as a missing task so participation is not leaked.
	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "你不是该任务的参与者"})
		return
	}

	// Locate the caller's OWN personal result (task_id + user_id=self). No other
	// member's row is reachable from here.
	var pr model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
		return
	}

	citations := pr.GetCitations()
	cleanedCitations := service.CleanUnreferencedCitations(req.Content, citations)
	tmp := &model.PersonalResult{}
	tmp.SetCitations(cleanedCitations)
	citationsJSON := tmp.CitationsJSON

	now := timezone.Now()
	// BE-1: the personal-report Update and the Completed-task revive must run in
	// ONE transaction, mirroring Leave/RemoveMember. For an already-Completed
	// multi-person BY_PERSON task the meta worker refuses to write unless the task
	// is still Processing (meta_processor: aborts "no longer processing before
	// result write"), so without reviving the task here the edit would never make
	// it into the team summary. reviveCompletedForRecompute is race-safe + a strict
	// no-op for any task not Completed / not BY_PERSON.
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.PersonalResult{}).
			Where("id = ?", pr.ID).
			Updates(map[string]interface{}{
				"content":        req.Content,
				"citations_json": citationsJSON,
				"edited_at":      now,
			})
		if result.Error != nil {
			return result.Error
		}
		// Concurrency: a racing Leave/RemoveMember may have physically deleted the
		// preloaded row. 0 rows affected => nothing was edited; roll back and surface
		// a 404 rather than a false 200.
		if result.RowsAffected == 0 {
			return errPersonalResultGone
		}
		//续修1: only revive when there is more than one participant. A single-person
		// Completed BY_PERSON task has no other members' content to re-aggregate, so
		// reviving it would be a pointless Completed->Processing flip (noise, and out
		// of step with the Accept path). reviveCompletedForRecompute itself stays
		// count-agnostic on purpose (Leave/RemoveMember need single-person revive).
		var participantCount int64
		if err := tx.Model(&model.SummaryParticipant{}).
			Where("task_id = ?", taskID).Count(&participantCount).Error; err != nil {
			return err
		}
		if participantCount > 1 {
			return h.reviveCompletedForRecompute(tx, taskID)
		}
		return nil
	}); err != nil {
		if errors.Is(err, errPersonalResultGone) {
			c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
			return
		}
		log.Printf("[personal] personal-edit update error task=%d user=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	// need6: recompute the team summary to incorporate this edit.
	go h.triggerWorker(model.WorkerTriggerRequest{
		Type:   "meta_summary",
		TaskID: taskID,
	})

	ok(c, gin.H{"edited_at": now.Format(time.RFC3339)})
}

// PersonalDraft handles PUT /api/v1/summaries/:id/personal-draft
//
// OCT-21: a participant edits THEIR OWN personal report BEFORE submitting it,
// without triggering team recompute. The caller can only ever touch the
// PersonalResult keyed by (task_id, user_id=self) -- there is no way to address
// another member's row. This handler is intentionally split from PersonalEdit
// so the "draft" path stays free of every behaviour that belongs to the
// post-submit edit flow:
//
//   - does NOT write edited_at        (draft is not a logical "edit")
//   - does NOT revive the task        (no Completed -> Processing flip)
//   - does NOT trigger meta_summary   (team summary unaffected by drafts)
//   - does NOT touch task.status / worker_status / submitted_at / submit_source
//   - does NOT open the meta lock
//
// State-machine: write is permitted only when worker_status == Completed AND
// submitted_at IS NULL. submitted_at != nil short-circuits to 409/40016 so the
// caller knows to reload and switch to /personal-edit (which DOES trigger team
// recompute). After submit, going through this handler would silently produce
// a team summary stale relative to the participant content.
//
// Reuses edit.go's content validation (non-empty, <=maxContentBytes,
// CleanUnreferencedCitations) and personal.go's auth chain (requireTaskInSpace
// + participant lookup + (task_id,user_id=self) PR lookup).
func (h *PersonalHandler) PersonalDraft(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "content cannot be empty"})
		return
	}
	if len(req.Content) > maxContentBytes {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "content 超过 500KB 限制"})
		return
	}

	// P1 (cross-space isolation): identical ordering to PersonalEdit -- the task
	// must exist AND live in the caller's space BEFORE any (task_id,user_id)
	// participant lookup. requireTaskInSpace also fail-closes an empty
	// X-Space-Id. Note: the router group has StrictSpaceMiddleware mounted, so
	// for write methods (incl. PUT) a missing X-Space-Id is rejected at the
	// middleware with 400/40001 and this handler never runs; requireTaskInSpace
	// is the defence-in-depth.
	task, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}
	// BY_PERSON gate: PersonalDraft is only meaningful for BY_PERSON tasks.
	// The worker pickup (personal_processor.go) does not filter by summary_mode
	// and would happily materialise a PersonalResult on any mode; if the upstream
	// gates that hard-code ModeByPerson (task.go:242 / schedule.go:535) ever
	// regress, unchecked drafts would land on the wrong task type. This handler
	// tier is the defence-in-depth. Sibling early-returns of the same shape live
	// at task.go:133 (callerPlainCitationsVisible) and personal.go:1393
	// (reviveCompletedForRecompute).
	if task.SummaryMode != model.ModeByPerson {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "该任务不支持个人草稿"})
		return
	}

	// Caller MUST be a participant of this task. Same 40008 ("你不是该任务的参与者")
	// as PersonalEdit / Accept so participation is not leaked across spaces.
	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "你不是该任务的参与者"})
		return
	}

	// Locate the caller's OWN personal result (task_id + user_id=self). No other
	// member's row is reachable from here.
	var pr model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
		return
	}

	// State-machine: only allowed when the personal worker has finished and the
	// row has not yet been submitted. 40005 mirrors Submit's "个人总结未完成,
	// 无法提交" code (personal.go:382-385) so the front-end can share a single
	// toast for the "worker not done" class.
	if pr.WorkerStatus != model.PersonalStatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "个人总结未完成，无法编辑草稿"})
		return
	}
	// Fast-path 1: already submitted. Positioned before the published-check so
	// the "user tapped save after tapping submit" case pays zero extra DB
	// round-trips. The authoritative guard is the conditional UPDATE inside the
	// transaction; this early-out spares a write in the common case (front-end
	// already hides the draft entry once submitted_at is set).
	if pr.SubmittedAt != nil {
		c.JSON(http.StatusConflict, apiResponse{Code: 40016, Message: "草稿已被提交，请刷新后改走编辑接口"})
		return
	}
	// Fast-path 2 (v2, OCT-50): summary_result existence probe. If the team
	// summary has ever been published for this task, `summary_result` has at
	// least one row (worker's `saveLatestResultAndCompleteTask` is the sole
	// writer, scheduled_replace_helpers.go:402). We reject drafts against a
	// task whose team summary is already published (task.status=Completed →
	// 40016) or under post-publish revive (task.status=Processing +
	// summary_result kept → 40009), routing users to /personal-edit / retry
	// respectively.
	//
	// This is an OPTIMISATION only. The authoritative gate is inside the tx
	// (§2.4 gate-in-tx below); count==0 here can be stale by the time the tx
	// starts (worker's saveLatest may commit between the two reads). The
	// intra-tx re-check under `lockTask FOR UPDATE` closes that residual TOCTOU.
	// See errDraftPublished / errDraftRegenerating docstrings above for the
	// sqlite `:memory:` caveat that constrains our unit-test coverage of this
	// path.
	//
	// LIMIT 1 + First + errors.Is(gorm.ErrRecordNotFound) is preferred over
	// COUNT(*) so the existence probe short-circuits on the first row via the
	// `idx_task_id` index (migrations/sql/20260101-00-baseline.sql:85) and never
	// scans the table.
	var publishedProbe model.SummaryResult
	if err := h.db.Select("id").Where("task_id = ?", taskID).Limit(1).First(&publishedProbe).Error; err == nil {
		if task.Status == model.StatusProcessing {
			c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已被重新生成，请刷新后重试"})
			return
		}
		if task.Status == model.StatusCompleted {
			c.JSON(http.StatusConflict, apiResponse{Code: 40016, Message: "该总结已发布，请改走编辑接口"})
			return
		}
		// Any other task.status (Pending / Failed) with summary_result present
		// is not a shape the writers can reach today; fall through and let the
		// gate-in-tx / UPDATE + probe adjudicate authoritatively.
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("[personal] draft published-check error task=%d user=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	citations := pr.GetCitations()
	cleanedCitations := service.CleanUnreferencedCitations(req.Content, citations)
	tmp := &model.PersonalResult{}
	tmp.SetCitations(cleanedCitations)
	citationsJSON := tmp.CitationsJSON

	// 🔴 v2 B1: concurrency-safe draft. Mirroring Submit's Blocker-2 fix
	// (personal.go:396-404), the conditional UPDATE adds AND submitted_at IS NULL
	// so a racing Submit (manual or system back-fill) that sets submitted_at
	// between the fast-path read above and this UPDATE makes us lose the race
	// cleanly (RowsAffected=0) instead of clobbering the submitted content with
	// the draft -- which would leave team summary forever stale (draft does not
	// trigger meta_summary).
	//
	// RowsAffected == 0 has THREE possible causes; a single follow-up SELECT
	// inside the same transaction disambiguates them so the HTTP layer can
	// return distinct codes (front-end behaviours diverge):
	//
	//   (a) row physically gone (Leave/RemoveMember)         -> 404/40008
	//   (b) row exists AND submitted_at != NULL (race lost)  -> 409/40016
	//   (c) row exists AND submitted_at IS NULL AND none of the three columns
	//       this UPDATE writes (content, citations_json, updated_at) actually
	//       change                                           -> idempotent 200 ok
	//
	// Important nuance about (c): the UPDATE below uses a map-based .Updates(),
	// and PersonalResult.UpdatedAt carries GORM's schema.AutoUpdateTime tag
	// (see internal/model/personal_result.go) -- so GORM silently appends
	// `updated_at = <now>` to every map-based UPDATE. That means (c) is only
	// reachable when MySQL sees the new updated_at as byte-equal to the old
	// one, which in practice requires `datetime` (second precision) plus a
	// same-second repeat save against MySQL without `clientFoundRows=true`
	// (see internal/db/db.go:11-18). On sqlite (unit tests) and on MySQL with
	// `datetime(3)` (millisecond precision) the appended updated_at always
	// differs, so the driver reports RowsAffected=1 and the (c) probe branch
	// is not entered. The fix is still correct and harmless for those
	// environments -- it just means the no-op branch is exercised in the
	// real "two tabs identical body, same-second" MySQL case rather than in
	// every duplicate-body save. Treating any RowsAffected=0 as 409 would
	// still surface a misleading "草稿已被提交" toast that closes the editor
	// while the task is still pending and nobody submitted, so the probe
	// must also read content + citations_json and only escalate to 409 when
	// the row genuinely diverges.
	//
	// Wrapping in a Transaction is mildly redundant for a single UPDATE +
	// SELECT, but kept for shape-parity with PersonalEdit and to give a future
	// audit-log write a ready insertion point.
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// v2 §2.4 gate-in-tx: acquire the task-row lock BEFORE any read/write on
		// summary_result / personal_result. This mirrors worker
		// saveLatestResultAndCompleteTask (scheduled_replace_helpers.go:370-374),
		// which also takes `SELECT FOR UPDATE` on the task row before writing
		// summary_result + markTaskCompleted. Both sides use the SAME first lock
		// on the SAME row → lock order is identical, no cross-lock deadlock is
		// possible. The lock is a single-row primary-key acquire (O(1)) and is
		// held for at most 3 tiny reads + 1 UPDATE; PersonalDraft is a low-QPS
		// user-typed endpoint so the added contention is negligible.
		//
		// Residual window (honest disclosure, from v2.1 Patch 2): the gate-in-tx
		// closes the window from `saveLatestResultAndCompleteTask` tx start
		// onwards. There is still a ~100ms residual between
		// `personal_processor.go:156` (the standalone `worker_status=Completed`
		// write) and `:201` (the saveLatest tx start) where a draft that beats
		// worker to `lockTask` will see `task.status=Processing` + no
		// summary_result yet → gate-in-tx passes, UPDATE lands, then worker
		// publishes summary_result from the DRAFT-mutated personal_result → fork,
		// same shape as the original OCT-21 bug but with a 100ms window instead
		// of a permanent one. Closing this fully requires worker-side
		// atomicisation of :156 and :201 into a single tx (or moving :156 under
		// the saveLatest lock), which is a worker change and is out of scope for
		// this issue. Leader has agreed to open a follow-up for it.
		//
		// sqlite `:memory:` caveat: FOR UPDATE is silently ignored on sqlite
		// (see members_auth_test.go:1837-1845), so wall-clock lock timing is
		// NOT the property under test at the handler-test tier. The intra-tx
		// SELECT-then-UPDATE decision logic itself IS exercised by T24 via a
		// same-tx GORM callback that seeds summary_result + flips task.status
		// between fast-path and gate-in-tx.
		var lockTask model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "status").First(&lockTask, taskID).Error; err != nil {
			return err
		}

		// Intra-tx re-check of summary_result existence. Under `lockTask FOR
		// UPDATE` this is authoritative: any worker writer racing this draft
		// must either have committed before we grabbed the lock (in which case
		// we observe the row and reject) or is blocked behind us until we
		// commit/rollback (in which case draft writes and worker follows
		// afterwards -- but in that case worker's roster re-check will decide
		// against the freshly-mutated personal_result on the roster it snapshot
		// against, not against a stale one).
		var txSummaryProbe model.SummaryResult
		if err := tx.Select("id").Where("task_id = ?", taskID).Limit(1).First(&txSummaryProbe).Error; err == nil {
			if lockTask.Status == model.StatusProcessing {
				return errDraftRegenerating
			}
			return errDraftPublished
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		// UPDATE conditional on worker_status = Completed (v2 §1.2): the extra
		// clause makes probe-(d) reachable. The ONLY natural production trigger
		// for worker_status flipping away from Completed while
		// submitted_at IS NULL is the Regenerate tx (task.go:927-966), which
		// also deletes summary_result and resets task.status → Pending. If
		// Regenerate commits between the fast-path count and here, this UPDATE
		// misses (RowsAffected=0), the probe below reads worker_status, and we
		// return errDraftRegenerating → 40009. See errDraftRegenerating
		// docstring for the full seam analysis (this branch is unreachable via
		// revive: revive does not touch worker_status).
		res := tx.Model(&model.PersonalResult{}).
			Where("id = ? AND submitted_at IS NULL AND worker_status = ?",
				pr.ID, model.PersonalStatusCompleted).
			Updates(map[string]interface{}{
				"content":        req.Content,
				"citations_json": citationsJSON,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Disambiguate FOUR causes now (v2 §1.2 adds (d)). The probe is
			// read-only and stays inside the tx; we read submitted_at +
			// worker_status + content + citations_json so causes (b), (c), (d)
			// are all detectable without a second round trip.
			var probe model.PersonalResult
			if err := tx.Select("id", "submitted_at", "worker_status", "content", "citations_json").
				Where("id = ?", pr.ID).First(&probe).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errPersonalResultGone
				}
				return err
			}
			if probe.SubmittedAt != nil {
				// (b) race lost to Submit -- this is the real 409.
				return errDraftAlreadySubmitted
			}
			if probe.WorkerStatus != model.PersonalStatusCompleted {
				// (d) Regenerate tx committed worker_status: Completed→Pending
				// (and summary_result was deleted, task.status→Pending) between
				// our fast-path count read and this UPDATE. The row on disk is
				// stale relative to the new run; retry after refresh. See
				// errDraftRegenerating docstring for why this branch is
				// unreachable via revive.
				return errDraftRegenerating
			}
			// (c) row exists, not submitted, worker_status is still Completed,
			// and the conditional UPDATE matched but changed nothing. Treat as
			// idempotent success and fall through to the outer `return nil`.
			// We do NOT re-verify probe.Content == req.Content /
			// probe.CitationsJSON == citationsJSON: with submitted_at IS NULL,
			// worker_status = Completed and the UPDATE's WHERE otherwise
			// satisfied, the only way MySQL can report RowsAffected=0 here is
			// when content, citations_json AND updated_at are all byte-equal
			// to what's already on disk -- the map-based .Updates() above
			// appends updated_at via GORM's schema.AutoUpdateTime (see
			// internal/model/personal_result.go), so a genuine divergence in
			// any of the three would have produced RowsAffected=1. In practice
			// that pins (c) to MySQL `datetime` (second precision) +
			// same-second duplicate save without clientFoundRows=true; sqlite
			// and MySQL `datetime(3)` always produce a different updated_at
			// and never enter this branch.
			return nil
		}
		return nil
	}); err != nil {
		if errors.Is(err, errPersonalResultGone) {
			c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
			return
		}
		if errors.Is(err, errDraftAlreadySubmitted) {
			c.JSON(http.StatusConflict, apiResponse{Code: 40016, Message: "草稿已被提交，请刷新后改走编辑接口"})
			return
		}
		if errors.Is(err, errDraftPublished) {
			c.JSON(http.StatusConflict, apiResponse{Code: 40016, Message: "该总结已发布，请改走编辑接口"})
			return
		}
		if errors.Is(err, errDraftRegenerating) {
			c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已被重新生成，请刷新后重试"})
			return
		}
		log.Printf("[personal] personal-draft update error task=%d user=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	// Intentionally no triggerWorker(meta_summary) here -- drafts do not affect
	// the team summary until the participant explicitly submits via /submit.
	ok(c, gin.H{})
}

// GetMembers handles GET /api/v1/summaries/:id/members
func (h *PersonalHandler) GetMembers(c *gin.Context) {
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	// Authorization: only the task creator or an explicit participant may read the
	// member list. Source-group membership alone does NOT grant access. This mirrors
	// TaskHandler.authorizeTaskAccess / canAccessTask (codes 4010 / 40008 / 40003).
	userID := middleware.GetUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, apiResponse{Code: 4010, Message: "authentication required"})
		return
	}
	spaceID := middleware.GetSpaceID(c)
	// fail-closed hard gate: GET requests are NOT caught by StrictSpaceMiddleware,
	// and SummaryTask.SpaceID is `not null default ''`, so rows with space_id='' may
	// exist; querying `space_id=''` would MATCH them, leaking a cross-space roster.
	// Reject an empty X-Space-Id before any query (mirrors requireTaskInSpace).
	if spaceID == "" {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}

	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}

	if task.CreatorID != userID {
		var cnt int64
		h.db.Model(&model.SummaryParticipant{}).
			Where("task_id = ? AND user_id = ?", taskID, userID).
			Count(&cnt)
		if cnt == 0 {
			c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "无权访问此任务"})
			return
		}
	}

	var participants []model.SummaryParticipant
	h.db.Where("task_id = ?", taskID).Find(&participants)

	// Batch load personal results for submitted_at
	prMap := make(map[int64]*model.PersonalResult)
	if len(participants) > 0 {
		var prs []model.PersonalResult
		h.db.Where("task_id = ?", taskID).Find(&prs)
		for i := range prs {
			prMap[prs[i].ParticipantRefID] = &prs[i]
		}
	}

	members := make([]gin.H, 0, len(participants))
	for _, p := range participants {
		userName := p.UserName
		if p.UserID != "" {
			if resolved := service.ResolveUserName(p.UserID); resolved != p.UserID {
				userName = resolved
			}
		}
		member := gin.H{
			"user_id":      p.UserID,
			"user_name":    userName,
			"status":       model.ParticipantStatusLabel(p.Status),
			"submitted_at": nil,
		}
		if pr, exists := prMap[p.ID]; exists && pr.SubmittedAt != nil {
			member["submitted_at"] = pr.SubmittedAt.Format(time.RFC3339)
			member["content"] = pr.Content
			// 隐私收口：成员间互看个人总结只下发正文，绝不下发 citations
			//（citations 含被引用的原始聊天记录原文/上下文/跳转信息）。content
			// 保留供互看，citations 从源头不出网。自己看自己（p.UserID == userID）
			// 则额外下发 citations，让引用可点开看详情（与 GetPersonal 一致）。
			if p.UserID == userID {
				member["citations"] = pr.GetCitations()
			}
		}
		members = append(members, member)
	}

	ok(c, gin.H{"members": members})
}

func (h *PersonalHandler) triggerWorker(req model.WorkerTriggerRequest) {
	if h.workerTriggerURL == "" {
		return
	}
	body, err := json.Marshal(req)
	if err != nil {
		log.Printf("[personal] marshal trigger: %v", err)
		return
	}
	resp, err := triggerClient.Post(h.workerTriggerURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[personal] trigger worker POST failed: %v", err)
		return
	}
	resp.Body.Close()
}

// addMembersReq is the body for POST /api/v1/summaries/:id/members.
type addMembersReq struct {
	UserIDs []string `json:"user_ids"`
}

// AddMembers handles POST /api/v1/summaries/:id/members
//
// need7 (corrected semantics): the task CREATOR adds new collaborators to a
// multi-person task by putting them on the roster as UNCONFIRMED / PENDING.
// Only the NEW members need to confirm; previously-confirmed members are left
// completely untouched (V5 Q3: "member change marks only new members
// unconfirmed"). Per NEW member, inside one transaction:
//   - if the task has a bound schedule, the member is added to
//     schedule.participant_config as confirmed=false / confirmed_at=null
//     (V5 embedded confirm state, "pending"); existing confirmed members keep
//     their state -- never reset.
//   - a summary_participant(Pending) row is created. NO PersonalResult is
//     materialized and NO personal_summary is dispatched here.
//
// The new member must later hit POST /summaries/:id/accept (PersonalHandler.
// Accept) themselves, which flips them to Accepted, creates the PersonalResult
// and dispatches personal_summary; on completion personal_processor fires
// TriggerMetaSummary, folding them into the team summary.
//
// Idempotent: a uid that is already a participant is skipped (no error, no duplicate, no state reset).
func (h *PersonalHandler) AddMembers(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	var req addMembersReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}

	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, middleware.GetSpaceID(c)).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}
	if task.CreatorID != userID {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "仅创建者可添加成员"})
		return
	}
	// Reject terminal tasks: new members would be stuck Pending forever (no revive
	// path — Accept revive CAS is WHERE status=Completed, worker CAS also misses).
	// Only Failed/Cancelled are blocked; Pending/WaitingConfirm/Processing/Completed
	// still allow adding (Completed is this PR's revive-recompute scenario).
	if task.Status == model.StatusFailed || task.Status == model.StatusCancelled {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "任务已结束，无法添加成员"})
		return
	}

	// Dedup + drop blanks from the request roster.
	seen := make(map[string]struct{}, len(req.UserIDs))
	want := make([]string, 0, len(req.UserIDs))
	for _, uid := range req.UserIDs {
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		want = append(want, uid)
	}

	addedCount := 0

	err := h.db.Transaction(func(tx *gorm.DB) error {
		// Load existing participants once for idempotency.
		var existing []model.SummaryParticipant
		if err := tx.Where("task_id = ?", taskID).Find(&existing).Error; err != nil {
			return err
		}
		existingByUID := make(map[string]bool, len(existing))
		for _, p := range existing {
			existingByUID[p.UserID] = true
		}

		// Load + mutate the schedule participant_config once (if bound). F3: take a
		// row lock (FOR UPDATE) while reading so concurrent AddMembers calls on the
		// same schedule serialize the read-modify-write of participant_config and
		// cannot lose each other's JSON edits. Same pattern as V5 UpdateSchedule's lockedSched (schedule.go).
		var sched model.SummarySchedule
		hasSched := false
		if task.ScheduleID != nil {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND deleted_at IS NULL", *task.ScheduleID).First(&sched).Error; err == nil {
				hasSched = true
			}
		}
		cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
		schedDirty := false

		for _, uid := range want {
			if existingByUID[uid] {
				// Already a member (any state) -> skip entirely. Never reset an
				// existing/confirmed member (V5 Q3: only NEW members go pending).
				continue
			}

			name := service.ResolveUserName(uid)
			if name == "" {
				name = uid // fall back to uid when unresolved
			}

			// ① schedule participant_config: NEW member is added UNCONFIRMED
			//    (confirmed=false / confirmed_at=null). Existing entries are left as-is.
			if hasSched {
				if cfg.FindParticipant(uid) == nil {
					cfg.Participants = append(cfg.Participants, model.ScheduleParticipantEntry{
						UserID:    uid,
						UserName:  name,
						Confirmed: false,
					})
					schedDirty = true
				}
			}

			// ② create a PENDING participant. No PersonalResult, no dispatch -- the
			//    member must Accept themselves to generate their personal summary.
			//    F2: concurrency-safe create. Two racing AddMembers calls could both
			//    miss the existence check above and both Create -> unique-key collision
			//    on uk(task_id,user_id) -> 500. Upsert with DoNothing on conflict, then
			//    only count it as newly-added (RowsAffected==1) when this call actually
			//    inserted the row. A conflict means another writer already added them:
			//    idempotent skip, no error.
			row := model.SummaryParticipant{
				TaskID:   taskID,
				UserID:   uid,
				UserName: name,
				Status:   model.ParticipantPending,
			}
			res := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "task_id"}, {Name: "user_id"}},
				DoNothing: true,
			}).Create(&row)
			if res.Error != nil {
				return res.Error
			}
			existingByUID[uid] = true
			if res.RowsAffected == 0 {
				// Lost the race: a concurrent writer already inserted this member.
				// Idempotent -- do not count, do not re-touch.
				continue
			}
			addedCount++
		}

		if hasSched && schedDirty {
			// A newly-added unconfirmed member means the schedule gate is no longer
			// fully passed; recompute it so it reflects the pending member.
			cfg.RecomputeGate(task.CreatorID)
			raw, mErr := cfg.Marshal()
			if mErr != nil {
				return mErr
			}
			updates := map[string]interface{}{"participant_config": raw}
			// consent-bypass P1 (reviewer yujiawei): if this schedule is currently AUTO,
			// adding an UNCONFIRMED new member (schedDirty) must flip it to CONFIRM.
			// Otherwise the next scheduled run goes through buildScheduledTaskParticipantsAuto,
			// which pre-accepts the WHOLE roster (EffectiveUserIDs) ignoring per-member
			// Confirmed flags -> the new member is auto-dispatched and summarized without
			// ever Accepting. Flipping to CONFIRM routes the next run through the CONFIRM
			// path so it waits for the newcomer to Accept (mirrors UpdateSchedule's
			// AUTO->CONFIRM handling in schedule.go). Strict: only when the schedule is
			// currently AUTO and a new unconfirmed member was actually added. Written in
			// the SAME tx Update as participant_config (one round-trip, no partial state).
			if sched.ConfirmPolicy == model.SchedConfirmAuto {
				updates["confirm_policy"] = model.SchedConfirmRequire
				// keep the in-memory sched in sync in case later logic reads it.
				sched.ConfirmPolicy = model.SchedConfirmRequire
			}
			if err := tx.Model(&model.SummarySchedule{}).
				Where("id = ?", sched.ID).
				Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("[personal] add-members tx error task=%d: %v", taskID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	// No dispatch here: each new member generates their personal summary only after
	// they Accept (POST /summaries/:id/accept), which then auto-folds into the team
	// summary via personal_processor.TriggerMetaSummary.
	ok(c, gin.H{"added": addedCount})
}

// Leave handles POST /api/v1/summaries/:id/leave
//
// A participant (NOT the creator) leaves a multi-person collaboration. The
// creator cannot leave -- they must delete the task instead. On success the
// caller's summary_participant row AND their summary_personal_result row are
// PHYSICALLY removed (no schema/soft-delete column on the subtables) inside one
// transaction, then a meta_summary recompute is dispatched so the team summary
// is re-aggregated WITHOUT the departed member.
func (h *PersonalHandler) Leave(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}

	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, middleware.GetSpaceID(c)).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}

	// The creator cannot leave their own collaboration -- they must delete it.
	if task.CreatorID == userID {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "创建者不能退出，请使用删除"})
		return
	}

	// The caller must actually be a participant.
	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "你不是该任务的参与者"})
		return
	}

	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// Global lock order is schedule -> task (matches UpdateSchedule/DeleteSchedule/
		// scheduler). Acquire the schedule row lock FIRST when this task is schedule-
		// bound, THEN the task row lock, so this removal cannot deadlock against a
		// concurrent schedule-side tx that already holds schedule and wants task.
		// The task row lock additionally serializes this removal against a concurrent
		// meta-save (saveLatestResultAndCompleteTask) on the same task, closing the
		// residual roster-vs-merge TOCTOU window.
		//
		// FIX5: task.ScheduleID was read OUTSIDE this tx (auth check). A concurrent
		// CreateSchedule (manual->scheduled) can rebind nil->non-nil in between, which
		// would make the conditional schedule lock + stripScheduleParticipant below get
		// skipped, leaving the departed member in participant_config (re-materialized next
		// scheduled round). Re-peek the live schedule_id (unlocked; preserves schedule->task
		// order) and bail out retryable on mismatch; the retried call observes the real
		// binding and strips correctly.
		livePeek, err := peekTaskScheduleID(tx, middleware.GetSpaceID(c), middleware.GetUserID(c), taskID)
		if err != nil {
			return err
		}
		if !int64PtrEqual(livePeek, task.ScheduleID) {
			return errRebindConcurrentModified
		}
		if task.ScheduleID != nil {
			if _, err := lockOptionalScheduleForUpdate(tx, *task.ScheduleID); err != nil {
				return err
			}
		}
		var lockTask model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").First(&lockTask, taskID).Error; err != nil {
			return err
		}
		// FIX-SCHEDULE-ALLROUNDS: under a schedule, the same schedule_id spawns
		// multiple rounds of summary_task (first round trigger_type=1 + each
		// scheduled round trigger_type=2), and the departing member has a
		// participant / personal_result row PER ROUND (per task_id). Deleting only
		// the clicked round's row leaves the member in the OTHER rounds, so the
		// collapsed-by-schedule list (ListSummaries) still matches them and the
		// summary keeps showing -- "leave does nothing". So for a schedule-bound
		// task we delete the member's rows across ALL non-deleted rounds of the
		// SAME schedule within the SAME space (single IN(SELECT ...) subquery, no
		// N+1). For a manual task (ScheduleID == nil) behavior is unchanged: delete
		// only this task_id's rows.
		if task.ScheduleID != nil {
			subTaskIDs := tx.Model(&model.SummaryTask{}).
				Select("id").
				Where("schedule_id = ? AND space_id = ? AND deleted_at IS NULL", *task.ScheduleID, task.SpaceID)
			if err := tx.Where("user_id = ? AND task_id IN (?)", userID, subTaskIDs).
				Delete(&model.SummaryParticipant{}).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ? AND task_id IN (?)", userID, subTaskIDs).
				Delete(&model.PersonalResult{}).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Where("task_id = ? AND user_id = ?", taskID, userID).
				Delete(&model.SummaryParticipant{}).Error; err != nil {
				return err
			}
			if err := tx.Where("task_id = ? AND user_id = ?", taskID, userID).
				Delete(&model.PersonalResult{}).Error; err != nil {
				return err
			}
		}

		// FIX3: the departed member must also be stripped from the bound
		// schedule's participant_config. Otherwise the next scheduled round
		// re-materializes them from cfg.EffectiveUserIDs (scheduled_replace_helpers)
		// and the member "comes back" after leaving. Lock the schedule row
		// FOR UPDATE (same pattern as AddMembers) so concurrent roster edits
		// serialize their read-modify-write of the JSON.
		if task.ScheduleID != nil {
			if err := h.stripScheduleParticipant(tx, *task.ScheduleID, userID, task.CreatorID); err != nil {
				return err
			}
		}

		// FIX1: a multi-person BY_PERSON task that has already aggregated its
		// team summary is Completed. The meta worker only writes when the task
		// is still Processing (meta_processor: aborts if "no longer processing
		// before result write"), so a plain meta_summary trigger on a Completed
		// task is a no-op and the departed member's content lingers in the team
		// summary. Mirror Accept's revive: conditionally pull a Completed
		// BY_PERSON task back to Processing so the dispatched recompute can run.
		// The WHERE status=Completed makes this race-safe + a strict no-op for any task not Completed.
		if err := h.reviveCompletedForRecompute(tx, taskID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if isScheduleRetryableConflict(err) {
			writeRetryableRebindConflict(c)
			return
		}
		log.Printf("[personal] leave tx error task=%d user=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	// Recompute the team summary so the departed member is dropped.
	go h.triggerWorker(model.WorkerTriggerRequest{
		Type:   "meta_summary",
		TaskID: taskID,
	})

	ok(c, gin.H{"left": true})
}

// RemoveMember handles DELETE /api/v1/summaries/:id/members?uid=<uid>
//
// The task CREATOR removes another member from a multi-person collaboration.
// Only the creator may remove members; the creator cannot be removed (neither
// by uid nor by self). On success the target member's summary_participant row
// AND their summary_personal_result row are PHYSICALLY removed inside one
// transaction, then a meta_summary recompute is dispatched.
func (h *PersonalHandler) RemoveMember(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "缺少 uid 参数"})
		return
	}

	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, middleware.GetSpaceID(c)).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}

	// Only the creator may remove members.
	if task.CreatorID != userID {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "仅创建者可移除成员"})
		return
	}

	// The creator cannot be removed (whether addressed by uid or as self).
	if uid == task.CreatorID || uid == userID {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "不能移除创建者"})
		return
	}

	// The target must actually be a participant.
	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, uid).First(&participant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "该成员不存在"})
		return
	}

	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// Global lock order is schedule -> task (matches UpdateSchedule/DeleteSchedule/
		// scheduler). Acquire the schedule row lock FIRST when this task is schedule-
		// bound, THEN the task row lock, so this removal cannot deadlock against a
		// concurrent schedule-side tx that already holds schedule and wants task.
		// The task row lock additionally serializes this removal against a concurrent
		// meta-save (saveLatestResultAndCompleteTask) on the same task, closing the
		// residual roster-vs-merge TOCTOU window.
		//
		// FIX5: task.ScheduleID was read OUTSIDE this tx (auth check). A concurrent
		// CreateSchedule (manual->scheduled) can rebind nil->non-nil in between, which
		// would make the conditional schedule lock + stripScheduleParticipant below get
		// skipped, leaving the removed member in participant_config (re-materialized next
		// scheduled round). Re-peek the live schedule_id (unlocked; preserves schedule->task
		// order) and bail out retryable on mismatch; the retried call observes the real
		// binding and strips correctly.
		livePeek, err := peekTaskScheduleID(tx, middleware.GetSpaceID(c), middleware.GetUserID(c), taskID)
		if err != nil {
			return err
		}
		if !int64PtrEqual(livePeek, task.ScheduleID) {
			return errRebindConcurrentModified
		}
		if task.ScheduleID != nil {
			if _, err := lockOptionalScheduleForUpdate(tx, *task.ScheduleID); err != nil {
				return err
			}
		}
		var lockTask model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").First(&lockTask, taskID).Error; err != nil {
			return err
		}
		// FIX-SCHEDULE-ALLROUNDS: see Leave for the full rationale. A schedule
		// spawns multiple rounds (task_id) under one schedule_id and the removed
		// member has a participant / personal_result row per round. Deleting only
		// the clicked round leaves them in the other rounds, so the collapsed list
		// still shows them -- "remove does nothing". For a schedule-bound task,
		// delete the member's rows across ALL non-deleted rounds of the SAME
		// schedule in the SAME space (single IN(SELECT ...) subquery, no N+1). For a
		// manual task (ScheduleID == nil) behavior is unchanged.
		if task.ScheduleID != nil {
			subTaskIDs := tx.Model(&model.SummaryTask{}).
				Select("id").
				Where("schedule_id = ? AND space_id = ? AND deleted_at IS NULL", *task.ScheduleID, task.SpaceID)
			if err := tx.Where("user_id = ? AND task_id IN (?)", uid, subTaskIDs).
				Delete(&model.SummaryParticipant{}).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ? AND task_id IN (?)", uid, subTaskIDs).
				Delete(&model.PersonalResult{}).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Where("task_id = ? AND user_id = ?", taskID, uid).
				Delete(&model.SummaryParticipant{}).Error; err != nil {
				return err
			}
			if err := tx.Where("task_id = ? AND user_id = ?", taskID, uid).
				Delete(&model.PersonalResult{}).Error; err != nil {
				return err
			}
		}

		// FIX3: strip the removed member from the bound schedule's
		// participant_config so the next scheduled round does not re-materialize
		// them. (creator is guarded out above, so we never delete the creator's config entry.)
		if task.ScheduleID != nil {
			if err := h.stripScheduleParticipant(tx, *task.ScheduleID, uid, task.CreatorID); err != nil {
				return err
			}
		}

		// FIX1: revive an already-Completed multi-person BY_PERSON task back to
		// Processing so the dispatched meta_summary recompute can actually write
		// (see Leave for the full rationale). No-op for non-Completed tasks.
		if err := h.reviveCompletedForRecompute(tx, taskID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if isScheduleRetryableConflict(err) {
			writeRetryableRebindConflict(c)
			return
		}
		log.Printf("[personal] remove-member tx error task=%d uid=%s: %v", taskID, uid, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	// Recompute the team summary so the removed member is dropped.
	go h.triggerWorker(model.WorkerTriggerRequest{
		Type:   "meta_summary",
		TaskID: taskID,
	})

	ok(c, gin.H{"removed": true})
}

// reviveCompletedForRecompute conditionally pulls an already-Completed
// multi-person BY_PERSON task back to Processing so that a freshly-dispatched
// meta_summary recompute (after a member leaves / is removed) can actually
// write its result. The meta worker refuses to write unless the task is still
// Processing, so without this a Completed task's team summary would never be
// re-aggregated and the departed member's content would linger.
//
// Race-safe by construction: the UPDATE carries WHERE status=Completed, so it is
// a strict no-op for any task that is NOT Completed (still Processing, Pending,
// Failed, single-person, etc.). Only ByPerson tasks are revived. Must run inside
// the caller's delete transaction, after the participant/personal_result rows are removed.
func (h *PersonalHandler) reviveCompletedForRecompute(tx *gorm.DB, taskID int64) error {
	var task model.SummaryTask
	if err := tx.Select("id", "summary_mode").First(&task, taskID).Error; err != nil {
		return err
	}
	if task.SummaryMode != model.ModeByPerson {
		return nil
	}
	// Revive regardless of remaining participant count: even when only the
	// creator remains, the team summary still has to be re-aggregated to drop the departed member's content.
	deadline := timezone.Now().Add(acceptReviveLeaseMinutes * time.Minute)
	return tx.Model(&model.SummaryTask{}).
		Where("id = ? AND status = ?", taskID, model.StatusCompleted).
		Updates(map[string]interface{}{
			"status":              model.StatusProcessing,
			"processing_deadline": deadline,
		}).Error
}

// stripScheduleParticipant removes targetUID from the bound schedule's
// participant_config and recomputes its confirm gate, so the next scheduled
// round does not re-materialize a member who has left / been removed. The
// schedule row is locked FOR UPDATE (same pattern as AddMembers) so concurrent
// roster edits serialize their read-modify-write of the JSON. Must run inside
// the caller's delete transaction.
func (h *PersonalHandler) stripScheduleParticipant(tx *gorm.DB, scheduleID int64, targetUID, creatorID string) error {
	var sched model.SummarySchedule
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", scheduleID).First(&sched).Error; err != nil {
		// Schedule gone / soft-deleted -> nothing to strip.
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		return err
	}

	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	out := cfg.Participants[:0]
	removed := false
	for _, p := range cfg.Participants {
		if p.UserID == targetUID {
			removed = true
			continue
		}
		out = append(out, p)
	}
	if !removed {
		return nil // target not in config -> no write needed
	}
	cfg.Participants = out
	cfg.RecomputeGate(creatorID)

	raw, err := cfg.Marshal()
	if err != nil {
		return err
	}
	return tx.Model(&model.SummarySchedule{}).
		Where("id = ?", sched.ID).
		Update("participant_config", raw).Error
}
