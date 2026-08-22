package handler

// Session-Finalize v0 — the async "generate one clean summary from the whole
// conversation" entry point (POST /api/v1/summaries/agent/finalize).
//
// Unlike CreateAgentSummary (synchronous, copies one already-produced assistant
// reply into the deliverable), this endpoint:
//
//   - creates a Task(TriggerAgentFinalize, status=Pending) + a creator
//     PersonalResult(Pending) and returns 202 immediately — NO LLM call in
//     the request path;
//   - the worker poller then claims it (Pending→Processing) and runs
//     executeAgentFinalize, which CONSOLIDATES the session's already-usable
//     assistant replies into one body (see internal/worker/agent_finalize.go).
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
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

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
	ExpectedSessionRevision int `json:"expected_session_revision,omitempty"`
	// Sources and ReferencedTaskIDs are AUDIT-ONLY on this route. They are
	// validated, deduped and persisted (summary_source rows / the task's
	// referenced_task_ids column), but NO finalize code path reads them back:
	// executeFinalizeTask / executeAgentFinalize consume only the session's own
	// assistant replies and evidence. Unlike the sync route — which gates
	// ReferencedTaskIDs[0] through space + deleted_at + status + canAccessTaskDB
	// before consuming it — nothing here validates ownership, because nothing
	// consumes it. That is not an IDOR today, but it does mean the stored list is
	// caller-controlled and UNTRUSTED: any future consumer MUST validate it at
	// read time (or this route must start validating at write time) rather than
	// assuming these columns were vetted.
	Sources           []sourceReq `json:"sources,omitempty"`
	ReferencedTaskIDs []int64     `json:"referenced_task_ids,omitempty"`
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

	title := strings.TrimSpace(req.Title)

	// --- SUM-BE2 idempotency preflight (Idempotency-Key = finalize_request_id) ---
	//
	// This runs BEFORE the freeze/content check, BEFORE the in-flight guard and
	// BEFORE origin resolution. All three orderings are load-bearing:
	//
	//   - before the freeze/content check, because that check reads MUTABLE
	//     session state that another request is allowed to destroy. The sibling
	//     sync route (CreateAgentSummary) DELETEs every agent_message row for the
	//     session as part of a successful save, by design. Once that has happened,
	//     a byte-identical retry with the same key would get
	//     40004 "本次会话还没有可定稿的内容" instead of a 202 replay of the task it
	//     already owns — and since the 202 is the ONLY handle the client has on
	//     that task, it would lose it permanently. The sync route puts its
	//     preflight before every mutable-session read for exactly this reason.
	//   - before the in-flight guard, because the single most likely reason a
	//     client retries with the same key is that it never received the first
	//     202. At that moment the first task is Pending/Processing, so a guard
	//     placed first would 409 exactly the replay this preflight exists to
	//     serve, making the whole idempotency path dead code in its own window.
	//   - before origin resolution, because canonicalAgentSaveRequestHash's
	//     contract is that the hash is computed from REQUEST-OWNED fields only.
	//     resolveOriginChannelFromSession reads mutable session state and can
	//     fail transiently, so folding its result in would let a byte-identical
	//     retry hash differently and 409 a request the client never changed.
	//
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	var requestHash string
	if idempotencyKey != "" {
		if !validAgentSaveIdempotencyKey(idempotencyKey) {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "valid Idempotency-Key header is required"})
			return
		}
		requestHash = canonicalAgentSaveRequestHash(userID, finalizeHashReq(req, title))
		existing, mismatched, ok, stale, ferr := findAgentSaveIdempotentTaskWithHash(
			c.Request.Context(), h.db, spaceID, userID, idempotencyKey, requestHash)
		if ferr != nil {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "idempotency lookup failed"})
			return
		}
		if ok {
			if stale || mismatched {
				// Reuse the sync path's 40009 envelope so the client gets the
				// same reason/recovery_action contract on both save routes.
				writeAgentSaveIdempotencyResponse(c, existing, mismatched, stale)
				return
			}
			// Same-body replay: return the already-created task.
			writeFinalizeReplayResponse(c, existing)
			return
		}
	}

	// The session must have usable assistant content to consolidate, AND we
	// FREEZE the input here at save time: capture the current max assistant
	// message id as an upper bound (docs §3.4 — freeze the revision). The worker
	// only merges messages id <= this bound, so replies produced AFTER the user
	// clicked save can never contaminate this deliverable (stable, idempotent
	// output). Stored on task.AgentMessageID (BE2's audit column, repurposed as
	// the finalize freeze bound).
	var frozen struct {
		Cnt   int64
		MaxID int64
	}
	if err := h.db.WithContext(c.Request.Context()).
		Model(&model.AgentMessage{}).
		Select("COUNT(*) AS cnt, COALESCE(MAX(id),0) AS max_id").
		Where("user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''",
			userID, req.SessionID, "assistant").
		Scan(&frozen).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "check session content failed"})
		return
	}
	if frozen.Cnt == 0 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40004, Message: "本次会话还没有可定稿的内容"})
		return
	}
	frozenMessageID := frozen.MaxID

	// One active Finalize Run per session (docs §3.4): reject if a finalize task
	// for this session is not yet terminal (Pending before the poller claims it,
	// Processing while the worker runs).
	//
	// Deliberately AFTER the idempotency preflight: a same-key retry must replay
	// its own task, not be rejected by it. Soft-deleted tasks are excluded because
	// the poller skips them (processor.go), so a soft-deleted Pending finalize
	// would otherwise stay "in flight" forever and permanently 409 the session.
	var inflight int64
	if err := h.db.WithContext(c.Request.Context()).
		Model(&model.SummaryTask{}).
		Where("creator_id = ? AND agent_session_id = ? AND trigger_type = ? AND status IN ? AND deleted_at IS NULL",
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

	// Origin channel is resolved AFTER the request hash (see above): it reads
	// mutable session state, so it must not influence the replay key.
	//
	// resolveOriginChannelFromSession returns the STORAGE-layer channel_type
	// (1=DM, 2=Group, 5=Thread) recovered from the fetch_channel tool args, but
	// SummaryTask.OriginChannelType stores the APPLICATION-layer value
	// (1=Group, 2=Thread, 3=DM). Without this translation DM sessions get written
	// as Group and Thread (5) falls outside the 1..3 validation window entirely —
	// the same defect SUM-158 blocker 4 fixed on the sync path.
	//
	// Unlike the sync path this stays best-effort: origin is decoration on a
	// finalize deliverable, not a precondition, so an unrecognized type drops the
	// origin instead of rejecting a request the user can do nothing about.
	var originID string
	var originType int
	if resolvedID, resolvedType, rerr := h.resolveOriginChannelFromSession(c.Request.Context(), req.SessionID, userID); rerr == nil {
		if appOrigin, ok := storageChannelTypeToAppOrigin(resolvedType); ok {
			originID, originType = resolvedID, appOrigin
		} else {
			log.Printf("[handler] FinalizeAgentSummary: unrecognized storage channel_type=%d session=%s (channel_id=%s); dropping origin",
				resolvedType, req.SessionID, resolvedID)
		}
	}

	now := timezone.Now()
	task := model.SummaryTask{
		TaskNo:         service.GenerateTaskNo(),
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
		// Freeze bound (see above): worker merges only assistant messages with
		// id <= this, so post-save replies never leak into the deliverable.
		AgentMessageID:  frozenMessageID,
		SnapshotVersion: req.ExpectedSessionRevision,
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

		// Creator participant + a Pending PersonalResult the worker will claim
		// (Pending→Processing) and fill.
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
			// Use the shared helper, NOT RowsAffected: GORM maps DoNothing to a
			// MySQL no-op update and clientFoundRows=true reports that conflict as
			// one affected row, so the loser of a concurrent same-key race would
			// commit a SECOND task + participant + personal_result (two finalize
			// runs, two LLM calls) while the binding still points at the first.
			if berr := createAgentSaveIdempotencyBinding(tx, &binding); berr != nil {
				return berr
			}
		}
		return nil
	})
	if err != nil {
		// Lost the idempotency race: re-read and replay/mismatch.
		if errors.Is(err, errAgentSaveIdempotencyConflict) && idempotencyKey != "" {
			existing, mismatched, ok, stale, ferr := findAgentSaveIdempotentTaskWithHash(
				c.Request.Context(), h.db, spaceID, userID, idempotencyKey, requestHash)
			if ferr == nil && ok {
				if stale || mismatched {
					writeAgentSaveIdempotencyResponse(c, existing, mismatched, stale)
					return
				}
				writeFinalizeReplayResponse(c, existing)
				return
			}
		}
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "create finalize task failed"})
		return
	}

	// 202 Accepted: the worker poller claims the Pending task and runs
	// executeAgentFinalize. The client polls task status for COMPLETED.
	c.JSON(http.StatusAccepted, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"task_id": createdTaskID,
		"status":  "GENERATING",
	}})
}

// writeFinalizeReplayResponse answers a same-key, same-body replay with the
// task the client already owns.
//
// status is read from the TASK, not hardcoded to "GENERATING". The replay can
// legitimately arrive after the worker has finished (or failed) the run — a
// client that lost the first 202 has no idea how much time passed — and telling
// it "GENERATING" for a Completed task makes a polling client wait for a
// transition that already happened, or wait forever on a Failed one. The sync
// route already returns the real status on replay; this aligns the two.
// replayed:true likewise mirrors the sync envelope.
func writeFinalizeReplayResponse(c *gin.Context, task model.SummaryTask) {
	c.JSON(http.StatusAccepted, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"task_id":  task.ID,
		"task_no":  task.TaskNo,
		"status":   task.Status,
		"replayed": true,
	}})
}

// finalizeHashReq projects a finalize request onto the canonical save-request
// shape that canonicalAgentSaveRequestHash consumes after #202 collapsed the
// old positional signature into (userID, createAgentSummaryReq).
//
// ExpectedSessionRevision has no field on createAgentSummaryReq, so it rides in
// SnapshotVersion: both are optimistic-concurrency tokens and neither path
// populates the other, so the hash stays injective per route.
func finalizeHashReq(req finalizeAgentSummaryReq, title string) createAgentSummaryReq {
	// finalizeRouteMarker discriminates this route inside the shared idempotency
	// namespace. summary_agent_save_idempotency is keyed only on
	// (space_id, user_id, idempotency_key), and finalizeHashReq leaves fields the
	// sync path legally leaves empty too (AgentMessageID=0, RequestID="",
	// Participants=nil), so without a marker a client reusing one key across both
	// endpoints gets a CROSS-ROUTE replay: /finalize returning 202 for a Completed
	// sync task no worker will touch, or /summaries/agent returning a queued async
	// task with no content.
	const finalizeRouteMarker = "agent-finalize/v0"
	return createAgentSummaryReq{
		SessionID: req.SessionID,
		Title:     title,
		Sources:   req.Sources,
		// RequestID is request-owned and unused by the finalize route, so it is
		// free to carry the route discriminator into the canonical payload.
		RequestID:         finalizeRouteMarker,
		ReferencedTaskIDs: req.ReferencedTaskIDs,
		SnapshotVersion:   req.ExpectedSessionRevision,
	}
}
