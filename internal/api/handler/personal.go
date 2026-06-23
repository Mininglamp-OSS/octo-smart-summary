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
// task existence is never leaked across spaces -- and returns false. An empty
// X-Space-Id is rejected fail-closed below before any query, sealing the
// cross-space path for the (task_id,user_id)-only personal endpoints (Accept /
// Decline / Submit / GetPersonal). Mirrors authorizeTaskAccess / GetMembers.
func (h *PersonalHandler) requireTaskInSpace(c *gin.Context, taskID int64) bool {
	spaceID := middleware.GetSpaceID(c)
	// fail-closed hard gate: an empty X-Space-Id must NEVER reach the query.
	// SummaryTask.SpaceID is `not null default ''`, so tasks with space_id='' may
	// exist; querying `space_id=''` would MATCH them, leaking a cross-space read
	// (fail-open). Short-circuit to 40008/404 here, independent of data invariant.
	if spaceID == "" {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return false
	}
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return false
	}
	return true
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
	if !h.requireTaskInSpace(c, taskID) {
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
	if !h.requireTaskInSpace(c, taskID) {
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
	if !h.requireTaskInSpace(c, taskID) {
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
	if !h.requireTaskInSpace(c, taskID) {
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

	// Authorization: the caller MUST be a participant of this task. Membership is
	// keyed on (task_id, user_id=self); a non-participant gets 403.
	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "你不是该任务的参与者"})
		return
	}

	// BE-3: space isolation + task existence prerequisite. The task must live in
	// the caller's space; a space mismatch is reported as 40008 ("任务不存在")
	// exactly like a missing task, so existence is not leaked across spaces. This
	// also gives BE-1's revive a consistent task-existence guarantee.
	spaceID := middleware.GetSpaceID(c)
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
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
			if err := tx.Model(&model.SummarySchedule{}).
				Where("id = ?", sched.ID).
				Update("participant_config", raw).Error; err != nil {
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
		if err := tx.Where("task_id = ? AND user_id = ?", taskID, userID).
			Delete(&model.SummaryParticipant{}).Error; err != nil {
			return err
		}
		if err := tx.Where("task_id = ? AND user_id = ?", taskID, userID).
			Delete(&model.PersonalResult{}).Error; err != nil {
			return err
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
		if err := tx.Where("task_id = ? AND user_id = ?", taskID, uid).
			Delete(&model.SummaryParticipant{}).Error; err != nil {
			return err
		}
		if err := tx.Where("task_id = ? AND user_id = ?", taskID, uid).
			Delete(&model.PersonalResult{}).Error; err != nil {
			return err
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
