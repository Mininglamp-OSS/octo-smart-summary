package handler

// Session-Finalize v0 — the async "generate one clean summary from the whole
// conversation" entry point (POST /api/v1/summaries/agent/finalize).
//
// Unlike CreateAgentSummary (synchronous, copies one already-produced assistant
// reply into the deliverable), this endpoint:
//
//   - creates a Task(TriggerAgentFinalize, status=Processing) + a creator
//     PersonalResult(Processing) and returns 202 immediately — NO LLM call in
//     the request path;
//   - the worker poller then claims it and runs executeAgentFinalize, which
//     CONSOLIDATES the session's already-usable assistant replies into one body
//     (see internal/worker/agent_finalize.go).
//
// Safety is reused verbatim from SUM-BE2 (agent_summary_save.go): the
// Idempotency-Key preflight + canonical request-hash give same-body replay /
// different-body 409, and a per-session in-flight guard enforces 3.4's "one
// active Finalize Run per session at a time".
//
// P0 concerns from docs §3.4 are absorbed here without a message binding:
// idempotency = finalize_request_id (Idempotency-Key header), concurrency =
// in-flight guard, revision = expected_session_revision folded into the hash.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

type finalizeAgentSummaryReq struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	// ExpectedSessionRevision is an optimistic-concurrency token (docs §3.4):
	// it does NOT let the user pick a message. v0 folds it into the request hash
	// so a retry with a different revision 409s; a full revision CAS is a
	// forward-compatible follow-up.
	ExpectedSessionRevision int         `json:"expected_session_revision,omitempty"`
	Sources                 []sourceReq `json:"sources,omitempty"`
	ReferencedTaskIDs       []int64     `json:"referenced_task_ids,omitempty"`
}

// FinalizeAgentSummary handles POST /api/v1/summaries/agent/finalize.
func (h *AgentSummaryHandler) FinalizeAgentSummary(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)

	var req finalizeAgentSummaryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: err.Error()})
		return
	}
	if req.SessionID == "" || !sessionIDPattern.MatchString(req.SessionID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 缺失或不符合正则 ^[A-Za-z0-9_-]{1,128}$"})
		return
	}

	req.ReferencedTaskIDs = dedupReferencedTaskIDs(req.ReferencedTaskIDs)
	if len(req.ReferencedTaskIDs) > maxReferencedTaskIDs {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: fmt.Sprintf("too many referenced task IDs: %d distinct (max %d)", len(req.ReferencedTaskIDs), maxReferencedTaskIDs)})
		return
	}

	// The session must have usable assistant content to consolidate — reject
	// early with a clear error instead of enqueuing a task that will fail in the
	// worker. Also implicitly rejects a still-generating session that has not
	// produced any assistant reply yet.
	var replyCount int64
	if err := h.db.WithContext(c.Request.Context()).
		Model(&model.AgentMessage{}).
		Where("user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''",
			userID, req.SessionID, "assistant").
		Count(&replyCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "check session content failed"})
		return
	}
	if replyCount == 0 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40004, Message: "本次会话还没有可定稿的内容"})
		return
	}

	// One active Finalize Run per session (docs §3.4): reject if a finalize task
	// for this session is not yet terminal (Pending before the poller claims it,
	// Processing while the worker runs).
	var inflight int64
	if err := h.db.WithContext(c.Request.Context()).
		Model(&model.SummaryTask{}).
		Where("creator_id = ? AND agent_session_id = ? AND trigger_type = ? AND status IN ?",
			userID, req.SessionID, model.TriggerAgentFinalize,
			[]int{model.StatusPending, model.StatusProcessing}).
		Count(&inflight).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "check in-flight finalize failed"})
		return
	}
	if inflight > 0 {
		c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "本次会话的定稿正在生成中,请稍候"})
		return
	}

	// Best-effort origin channel resolution from the session (same helper the
	// sync path uses); a failure just leaves origin empty — the deliverable does
	// not depend on it.
	var originID string
	var originType int
	if resolvedID, resolvedType, rerr := h.resolveOriginChannelFromSession(c.Request.Context(), req.SessionID, userID); rerr == nil {
		originID, originType = resolvedID, resolvedType
	}

	title := strings.TrimSpace(req.Title)

	// --- SUM-BE2 idempotency preflight (Idempotency-Key = finalize_request_id) ---
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	var requestHash string
	if idempotencyKey != "" {
		if !validAgentSaveIdempotencyKey(idempotencyKey) {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "valid Idempotency-Key header is required"})
			return
		}
		requestHash = canonicalAgentSaveRequestHash(
			req.SessionID, title, originID, originType,
			0, req.ExpectedSessionRevision, req.Sources, req.ReferencedTaskIDs,
		)
		existing, mismatched, ok, ferr := findAgentSaveIdempotentTaskWithHash(
			c.Request.Context(), h.db, spaceID, userID, idempotencyKey, requestHash)
		if ferr != nil {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "idempotency lookup failed"})
			return
		}
		if ok {
			if mismatched {
				c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "same Idempotency-Key with a different request"})
				return
			}
			// Same-body replay: return the already-created task.
			c.JSON(http.StatusAccepted, apiResponse{Code: 0, Message: "ok", Data: gin.H{
				"task_id": existing.ID,
				"status":  "GENERATING",
			}})
			return
		}
	}

	now := timezone.Now()
	task := model.SummaryTask{
		SpaceID:        spaceID,
		CreatorID:      userID,
		Title:          title,
		Topic:          title,
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		// Pending so the worker POLLER claims it (Pending→Processing→dispatch);
		// AgentSummaryHandler has no workerTriggerURL for the HTTP fast-path, and
		// the poller is the reliable async pickup either way.
		Status:            model.StatusPending,
		TriggerType:       model.TriggerAgentFinalize,
		OriginChannelID:   originID,
		OriginChannelType: originType,
		ReferencedTaskIDs: serializeReferencedTaskIDs(req.ReferencedTaskIDs),
		AgentSessionID:    req.SessionID,
		SnapshotVersion:   req.ExpectedSessionRevision,
	}

	var createdTaskID int64
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&task).Error; err != nil {
			return fmt.Errorf("create summary_task: %w", err)
		}
		createdTaskID = task.ID

		// Sources (deduped by (type,id)) — mirrors the sync path.
		seenSrc := make(map[string]struct{}, len(req.Sources))
		for _, s := range req.Sources {
			if s.SourceID == "" {
				continue
			}
			key := fmt.Sprintf("%d:%s", s.SourceType, s.SourceID)
			if _, dup := seenSrc[key]; dup {
				continue
			}
			seenSrc[key] = struct{}{}
			src := model.SummarySource{
				TaskID:     createdTaskID,
				SourceType: s.SourceType,
				SourceID:   s.SourceID,
				SourceName: service.ResolveSourceNameWithType(s.SourceID, s.SourceType, h.imDB),
			}
			if err := tx.Create(&src).Error; err != nil {
				return fmt.Errorf("create summary_source: %w", err)
			}
		}

		// Creator participant + a Processing PersonalResult the worker will fill.
		creatorP := model.SummaryParticipant{
			TaskID:      createdTaskID,
			UserID:      userID,
			UserName:    service.ResolveUserName(userID),
			Status:      model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := tx.Create(&creatorP).Error; err != nil {
			return fmt.Errorf("create creator participant: %w", err)
		}
		creatorPR := model.PersonalResult{
			TaskID:           createdTaskID,
			ParticipantRefID: creatorP.ID,
			UserID:           userID,
			WorkerStatus:     model.PersonalStatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := tx.Create(&creatorPR).Error; err != nil {
			return fmt.Errorf("create creator personal_result: %w", err)
		}
		if err := tx.Model(&creatorP).Update("personal_result_id", creatorPR.ID).Error; err != nil {
			return fmt.Errorf("link participant to personal_result: %w", err)
		}

		// SUM-BE2 idempotency binding (same tx as the task).
		if idempotencyKey != "" {
			binding := model.SummaryAgentSaveIdempotency{
				SpaceID:        spaceID,
				UserID:         userID,
				IdempotencyKey: idempotencyKey,
				RequestHash:    requestHash,
				TaskID:         task.ID,
				CreatedAt:      now,
			}
			insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&binding)
			if insert.Error != nil {
				return fmt.Errorf("create agent save idempotency: %w", insert.Error)
			}
			if insert.RowsAffected == 0 {
				return errAgentSaveIdempotencyConflict
			}
		}
		return nil
	})
	if err != nil {
		// Lost the idempotency race: re-read and replay/mismatch.
		if errors.Is(err, errAgentSaveIdempotencyConflict) && idempotencyKey != "" {
			existing, mismatched, ok, ferr := findAgentSaveIdempotentTaskWithHash(
				c.Request.Context(), h.db, spaceID, userID, idempotencyKey, requestHash)
			if ferr == nil && ok {
				if mismatched {
					c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "same Idempotency-Key with a different request"})
					return
				}
				c.JSON(http.StatusAccepted, apiResponse{Code: 0, Message: "ok", Data: gin.H{
					"task_id": existing.ID,
					"status":  "GENERATING",
				}})
				return
			}
		}
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "create finalize task failed"})
		return
	}

	// 202 Accepted: the worker poller claims the Processing task and runs
	// executeAgentFinalize. The client polls task status for COMPLETED.
	c.JSON(http.StatusAccepted, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"task_id": createdTaskID,
		"status":  "GENERATING",
	}})
}
