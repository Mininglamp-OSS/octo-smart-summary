package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AgentSummaryHandler persists the deliverable produced by the agent
// conversational entry (POST /api/v1/summaries/agent).
//
// Design (see docs/agent-deliverable-persistence.md and issue SUM-15):
//
//   - The task is born status=Completed + trigger_type=Agent + worker_status=Completed:
//     content is filled synchronously from the agent's already-produced reply on
//     the given session_id; no worker is dispatched, no LLM call happens here.
//   - This handler is Path A minimum-viable ("骨架 + 落 content"): citations are
//     persisted as an empty array in v1 and will be wired to structured Citation
//     objects in the follow-up PR that changes summarize_chunk / merge_summaries
//     to emit indexed [n] plus the message pool needed by worker.BuildCitations.
//   - creator_id / space_id are taken from the auth middleware only (StrictAuth +
//     StrictSpace); accepting them from the request body would break the identity
//     boundary the Chat handler already enforces.
type AgentSummaryHandler struct {
	db           *gorm.DB
	imDB         *gorm.DB
	llmApiURL    string
	llmApiKey    string
	llmModel     string
	llmTimeout   int
	llmMaxTokens int
	store        agentHistoryStore
	// documentClient fetches summarize-ready document content (see document_source.go).
	// Used by the document AI 速览 preview.
	documentClient documentSourceClient
	// runnerFactory is an optional test-only hook for injecting a fake agent
	// runner without going through the real LLM. When nil (production path),
	// newRunner falls back to buildRunner with handler's LLM config.
	// Returns refineRunner (an interface, so tests can plug in a fake struct)
	// rather than *agent.Runner (a concrete type whose dependencies are
	// unexported).
	// Not exposed via NewAgentSummaryHandler — tests assign this field
	// directly using same-package access.
	runnerFactory func(profile, uid string) (refineRunner, string, error)
	// beforeDraftLoad is a test-only synchronization hook used to exercise
	// the race where another request commits after idempotency preflight.
	beforeDraftLoad func()
}

// refineRunner is the minimal subset of *agent.Runner used by RefineAgentSummary.
// Declared as an interface (not a concrete type) so tests can inject a fake
// without depending on unexported types in the agent package.
type refineRunner interface {
	RunWithHistory(ctx context.Context, system string, history []agent.Message, userInput string) (string, []agent.Message, error)
}

func NewAgentSummaryHandler(db, imDB *gorm.DB, llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) *AgentSummaryHandler {
	return &AgentSummaryHandler{
		db:             db,
		imDB:           imDB,
		llmApiURL:      llmApiURL,
		llmApiKey:      llmApiKey,
		llmModel:       llmModel,
		llmTimeout:     llmTimeout,
		llmMaxTokens:   llmMaxTokens,
		store:          newAgentMessageRepo(db),
		documentClient: newDefaultDocumentSourceClient(),
	}
}

// createAgentSummaryReq mirrors the SUM-24 v1.0 contract where origin_channel
// fields are now optional. OriginChannelID is a pointer to distinguish between
// "not provided" (nil) and "explicitly provided as empty string" (non-nil pointing to "").
//
// SUM-BE2 additions (all optional during the FE-2 rollout window; once FE-2
// SUM-7 ships they become the only valid inputs):
//   - AgentMessageID: primary key of the assistant reply the user confirmed
//     as their draft. When >0, the server loads by (id, user, session,
//     role='assistant', tool_calls IS NULL) instead of "latest assistant";
//     0 keeps the pre-BE-2 legacy behaviour.
//   - SnapshotVersion: the snapshot version the client thinks it is saving.
//     BE-2 only writes v1; other values fail as AGENT_DRAFT_STALE (40901).
//     Must be paired with a non-zero AgentMessageID.
//   - Idempotency-Key is HTTP-header-borne (mirrors bot_summary_create.go);
//     absent header keeps the non-idempotent legacy path.
type createAgentSummaryReq struct {
	SessionID         string           `json:"session_id"`
	OriginChannelID   *string          `json:"origin_channel_id,omitempty"`
	OriginChannelType int              `json:"origin_channel_type,omitempty"`
	Title             string           `json:"title,omitempty"`
	Sources           []sourceReq      `json:"sources,omitempty"`
	Participants      []participantReq `json:"participants,omitempty"`
	// ReferencedTaskIDs 可选:本次 agent chat 引用的已有总结 task_id 数组。
	// 前端在保存时把首轮引用的 task IDs 透传过来,后端记录到 SummaryTask
	// (方便日后做衍生关系追溯),不影响本次生成的 content/citations。
	ReferencedTaskIDs []int64 `json:"referenced_task_ids,omitempty"`
	// RequestID 可选:必须是生成当前最新 assistant 内容的同一 agent chat
	// 轮次的 request_id (SS-03 idempotency key)。当前 AgentMessage 尚未持久化
	// run_id，服务端无法验证跨轮错配；该绑定由客户端负责，SS-08 再持久化
	// 校验。缺省/未知 → 与 legacy 一致的重算路径。
	RequestID string `json:"request_id,omitempty"`
	// AgentMessageID + SnapshotVersion form the trusted draft reference the
	// design (section 6.5) requires. Both are optional in BE-2 for backward
	// compat during the FE-2 (SUM-7) rollout; when either is >0 the other
	// must also be >0 (enforced by service.ValidateAgentSave).
	AgentMessageID  int64 `json:"agent_message_id,omitempty"`
	SnapshotVersion int   `json:"snapshot_version,omitempty"`
}

// CreateAgentSummary handles POST /api/v1/summaries/agent.
//
// SUM-24 change: origin_channel_id and origin_channel_type are now optional.
// If not provided (nil), they are resolved from the session's fetch_channel tool calls.
// If explicitly provided as empty string, the old validation error is returned.
//
// Error codes are chosen to match the SUM-15 v1.0 contract (40000 / 40001 /
// 40004 / 50000) so the front-end can key off the same numeric codes it
// already handles for the traditional create endpoint where possible.
//
// The session_id regex constraint is intentionally the same one enforced by
// AgentChatHandler (see agent_chat.go's sessionIDPattern) — a session_id
// accepted by /agent/chat is also accepted here; both endpoints share one
// canonical validation rule via that shared package-level variable.
func (h *AgentSummaryHandler) CreateAgentSummary(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)

	var req createAgentSummaryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: err.Error()})
		return
	}

	// --- validation (contract-defined error codes) ---
	if req.SessionID == "" || !sessionIDPattern.MatchString(req.SessionID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 缺失或不符合正则 ^[A-Za-z0-9_-]{1,128}$"})
		return
	}
	if agent.SummaryV2Enabled() && req.RequestID != "" && !requestIDPattern.MatchString(req.RequestID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "request_id 非法"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)

	// SUM-24: origin_channel fields are now optional. Distinguish between:
	// - nil (not provided) → resolve from session
	// - non-nil but empty → old validation error
	// - non-nil and non-empty → use provided value
	var finalChannelID string
	var finalChannelType int
	var finalOriginFromDerived bool

	// R7 P2-3 / R9 P2-3 (PR #190): normalize referenced_task_ids at the entry
	// point — dedup first, then REJECT if still over maxReferencedTaskIDs.
	// Chat (agent_chat.go) and ChatStream enforce the identical contract at
	// their binding layer with apiResponse{Code:40000}; previously this path
	// silently truncated to the cap and persisted a partial lineage, breaking
	// contract symmetry. All downstream consumers below (origin borrow,
	// persist, citation borrow) see the clean, in-cap list.
	req.ReferencedTaskIDs = dedupReferencedTaskIDs(req.ReferencedTaskIDs)
	if len(req.ReferencedTaskIDs) > maxReferencedTaskIDs {
		c.JSON(http.StatusBadRequest, apiResponse{
			Code:    40000,
			Message: fmt.Sprintf("too many referenced task IDs: %d distinct (max %d)", len(req.ReferencedTaskIDs), maxReferencedTaskIDs),
		})
		return
	}

	// SUM-BE2 idempotency preflight must run before any session/origin/draft
	// lookup. A successful save deletes the session messages, so replay cannot
	// depend on state that the first request intentionally destroys.
	keyHeader := http.CanonicalHeaderKey("Idempotency-Key")
	keyValues, keyPresent := c.Request.Header[keyHeader]
	idempotencyKey := strings.TrimSpace(c.GetHeader(keyHeader))
	if keyPresent && (len(keyValues) != 1 || idempotencyKey == "") {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "valid Idempotency-Key header is required"})
		return
	}
	var requestHash string
	if idempotencyKey != "" {
		if !validAgentSaveIdempotencyKey(idempotencyKey) {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "valid Idempotency-Key header is required"})
			return
		}
		requestHash = canonicalAgentSaveRequestHash(userID, req)
		existing, mismatched, ok, stale, ferr := findAgentSaveIdempotentTaskWithHash(
			c.Request.Context(), h.db, spaceID, userID, idempotencyKey, requestHash,
		)
		if ferr != nil {
			log.Printf("[handler] CreateAgentSummary idempotency lookup failed space=%s user=%s key=%s: %v", spaceID, userID, idempotencyKey, ferr)
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "idempotency check failed"})
			return
		}
		if ok {
			writeAgentSaveIdempotencyResponse(c, existing, mismatched, stale)
			return
		}
	}

	if req.OriginChannelID == nil {
		// Not provided → resolve from session tool traces
		resolvedID, resolvedType, err := h.resolveOriginChannelFromSession(c.Request.Context(), req.SessionID, userID)
		if err != nil {
			// DB error or other real failure → 500
			log.Printf("[handler] resolveOriginChannelFromSession failed session=%s: %v", req.SessionID, err)
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "resolve origin channel failed"})
			return
		}
		if resolvedID == "" {
			// SUM-24 fallback failed (no fetch_channel in session — typical for
			// pure refine flows where the agent didn't need to re-fetch).
			// CHAT-REFERENCE-BASED-DESIGN-v1 second-order fallback: if the user
			// referenced existing summaries, borrow the FIRST referenced task's
			// origin as the new summary's origin. This keeps the chat/list
			// grouping sensible without asking the user to re-select the channel.
			//
			// SUM-158 blocker 5 (follow-up to blocker 2): this is the third
			// consumption site of ReferencedTaskIDs; buildReferencedSummariesContext
			// and borrowCitationsFromReference already gate through canAccessTaskDB,
			// this fallback was missed. Space-scope alone lets a caller inherit any
			// same-space task's origin_channel_id/type (an in-space authz bypass on
			// metadata) — enforce the same creator/participant rule as the two
			// sibling paths here.
			// R10 (yujiawei, issue comment 5280351017): track WHY origin
			// inheritance was impossible so the 40001 message says so —
			// users were left guessing when a historical referenced task
			// (channels never persisted) failed at the very end of the flow.
			noOriginReason := "也未指定引用总结;请显式传入 origin_channel_id"
			if len(req.ReferencedTaskIDs) > 0 {
				// Pre-populated for the query-miss branch (not found /
				// deleted / non-completed / wrong space all land here).
				noOriginReason = "引用总结也未能提供 origin:引用任务不存在/已删除/未完成,或不属于本空间"
				var refTask model.SummaryTask
				// R9 P2-4 (PR #190, raised in two preceding reviews): gate the
				// borrow on deleted_at IS NULL AND status = completed, mirroring
				// resolveReferencedArtifact. SummaryTask.DeletedAt is *time.Time
				// (not gorm.DeletedAt), so GORM does NOT auto-filter soft-deleted
				// rows — without this the borrow happily inherits origin from a
				// deleted or still-processing task while borrowCitationsFromReference
				// on the same request correctly refuses it (same referenced task,
				// two different answers).
				if err := h.db.WithContext(c.Request.Context()).
					Select("id, creator_id, origin_channel_id, origin_channel_type, origin_from_derived").
					Where("id = ? AND space_id = ? AND deleted_at IS NULL AND status = ?",
						req.ReferencedTaskIDs[0], spaceID, model.StatusCompleted).
					First(&refTask).Error; err == nil {
					if canAccessTaskDB(h.db.WithContext(c.Request.Context()), userID, refTask.ID, refTask.CreatorID) {
						if refTask.OriginChannelID != "" {
							finalChannelID = refTask.OriginChannelID
							finalChannelType = refTask.OriginChannelType
							// R11 Q2: the mask flag propagates — borrowing a
							// masked origin keeps it masked (second-generation
							// refines must not re-expose the channel).
							finalOriginFromDerived = refTask.OriginFromDerived
							log.Printf("[handler] CreateAgentSummary borrowed origin from referenced task_id=%d channel=%s/%d from_derived=%t session=%s",
								refTask.ID, finalChannelID, finalChannelType, finalOriginFromDerived, req.SessionID)
						} else {
							// Tier-4 fallback: pipeline/scheduled summaries never set
							// origin_channel_id. Derive the origin from the referenced
							// task's summary_source rows (the channels it was generated
							// from) so a refine of a non-agent summary can still inherit
							// an origin — the owner's goal of "non-agent summaries
							// referenceable + iterable like agent ones". The helper takes
							// the FIRST usable source row (by id order) and logs when
							// several exist — multi-source tasks inherit their first
							// channel, consistent with the tier-3 first-referenced-task
							// precedent (see deriveOriginFromSummarySources).
							finalChannelID, finalChannelType, finalOriginFromDerived = h.deriveOriginFromSummarySources(c.Request.Context(), refTask.ID)
							if finalChannelID != "" {
								log.Printf("[handler] CreateAgentSummary derived origin from referenced task_id=%d sources channel=%s/%d from_derived=%t session=%s",
									refTask.ID, finalChannelID, finalChannelType, finalOriginFromDerived, req.SessionID)
							} else {
								// R10: the historical-task case — channels
								// lived only in the in-memory channelSet and
								// were never persisted before the backfill
								// landed, so there is nothing to inherit.
								noOriginReason = "引用总结也未能提供 origin:引用任务未记录生成频道(无 origin 且无可用来源行)"
							}
						}
					} else {
						noOriginReason = "引用总结也未能提供 origin:您无权读取引用任务"
						log.Printf("[handler] CreateAgentSummary refused to borrow origin from referenced task_id=%d (user=%s lacks read access) session=%s",
							refTask.ID, userID, req.SessionID)
					}
				}
			}
			if finalChannelID == "" {
				// Truly no origin available anywhere → 400 with specific message
				c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: fmt.Sprintf("origin_channel_id 未传且无法从 session 反查(session 无 fetch_channel 调用),%s", noOriginReason)})
				return
			}
		} else {
			finalChannelID = resolvedID
			// SUM-158 blocker 4: resolveOriginChannelFromSession returns the
			// STORAGE-layer channel_type (1=DM, 2=Group, 5=Thread) recovered
			// from the tool call args. But SummaryTask.OriginChannelType stores
			// the APPLICATION-layer value (1=Group, 2=Thread, 3=DM). Without
			// this translation DM sessions get written as Group, and Thread
			// (5) falls outside the 1..3 validation window entirely.
			appOrigin, ok := storageChannelTypeToAppOrigin(resolvedType)
			if !ok {
				log.Printf("[handler] CreateAgentSummary: resolveOriginChannelFromSession returned unrecognized storage channel_type=%d session=%s (channel_id=%s)",
					resolvedType, req.SessionID, resolvedID)
				c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "无法识别 session 中 fetch_channel 的 channel_type,请重新触发 agent 或显式传 origin_channel_type"})
				return
			}
			finalChannelType = appOrigin
		}
	} else {
		// Provided (even if empty string) → validate as before
		finalChannelID = *req.OriginChannelID
		finalChannelType = req.OriginChannelType

		if finalChannelID == "" {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "origin_channel_id 不能为空"})
			return
		}
		if finalChannelType < model.OriginChannelGroup || finalChannelType > model.OriginChannelDM {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "origin_channel_type 必须是 1(群)/2(thread)/3(DM)"})
			return
		}
	}

	// SUM-BE1: the agent_save-target validator runs a few lines below,
	// AFTER loadAgentMessageForSave + stripAgentPreamble have resolved
	// the server-trusted deliverable — that way ContentLen is measured on
	// the real content the DB will persist, not a request-supplied number
	// the client could lie about. Running the validator here instead would
	// force a placeholder ContentLen, which the review comment on SUM-3
	// specifically flagged as a bypass risk.

	// --- pull the agent's produced deliverable content from agent_message ---
	// SUM-BE2: when the client supplies a positive AgentMessageID we load by
	// primary key AND owner AND session AND role='assistant' AND
	// tool_calls IS NULL — every ownership axis the design (section 6.5.2)
	// requires the server to verify. When AgentMessageID == 0 we fall back
	// to the pre-BE-2 "latest assistant" behaviour so older frontends keep
	// working during the FE-2 rollout window.
	//
	// Every rejection path collapses to errNoAgentOutput → 40004 so an
	// attacker cannot probe whether a specific message id exists (matches
	// the legacy loader's owner-scope 404 discipline, SUM-158
	// blocker 1).
	if h.beforeDraftLoad != nil {
		h.beforeDraftLoad()
	}
	draftMsg, err := loadAgentMessageForSave(h.db.WithContext(c.Request.Context()), req.SessionID, userID, req.AgentMessageID)
	if err != nil {
		if errors.Is(err, errNoAgentOutput) {
			// A concurrent winner may have committed after our preflight and
			// deleted the shared draft. Re-check the durable binding before
			// reporting a missing output so an overlapping retry still replays.
			if idempotencyKey != "" {
				existing, mismatched, ok, stale, ferr := findAgentSaveIdempotentTaskWithHash(
					c.Request.Context(), h.db, spaceID, userID, idempotencyKey, requestHash,
				)
				if ferr != nil {
					log.Printf("[handler] CreateAgentSummary idempotency retry lookup failed space=%s user=%s key=%s: %v", spaceID, userID, idempotencyKey, ferr)
					c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "idempotency check failed"})
					return
				}
				if ok {
					writeAgentSaveIdempotencyResponse(c, existing, mismatched, stale)
					return
				}
			}
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40004, Message: "session 无有效产出,请先在对话中生成总结再保存"})
			return
		}
		log.Printf("[handler] CreateAgentSummary load session %s: %v", req.SessionID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "读取 session 产出失败"})
		return
	}
	content := draftMsg.Content
	resolvedAgentMessageID := draftMsg.ID

	// Strip conversational preamble that agents sometimes leak despite prompt
	// discipline. Defense-in-depth — see agent_content_strip.go for the
	// heuristic (first heading / rule wins, capped at 500 chars) and
	// CHAT-REFERENCE-PREVIEW-AND-RANGE-SAVE-v1 Q1=A+B / Q2=default-on decisions.
	// Owner reported task 51 (2026-07-15) where agent output opened with
	//   「好的。根据引用的老总结内容,我现在将其转化为...」
	// then the actual `## Summary 服务上线项目总结报告`. Stripping the opener
	// keeps the deliverable clean without asking users to hand-edit each time.
	stripped := stripAgentPreamble(content)
	if stripped != content {
		log.Printf("[handler] CreateAgentSummary session %s: stripped %d chars of preamble", req.SessionID, len(content)-len(stripped))
	}
	content = stripped

	// SUM-BE1 (revised per SUM-9): real agent_save gate. Run the shared
	// validator with the server-trusted content (post-strip) so a caller
	// can never bypass the "empty content" check by lying about the
	// payload. Defense-in-depth normally — becomes load-bearing the moment
	// stripAgentPreamble reduces content to empty (an all-preamble reply),
	// which would otherwise silently save an empty deliverable.
	//
	// Note on parameters: message ownership / role / session identity are
	// already enforced by loadAgentMessageForSave's WHERE clause
	// (user_id + session_id + role='assistant'), so this call does not
	// re-declare those. Snapshot-version and message-id enforcement remain
	// BE-2 scope (they require new storage-side reads BE-1 does not add);
	// keeping them as declared-but-ignored parameters here would be the
	// exact shell coverage SUM-9 rejected.
	if bizE := service.ValidateAgentSave(
		userID,
		req.Title,
		req.SessionID,
		content,
		finalChannelID, finalChannelType,
		req.AgentMessageID,  // client-declared id — 0 means legacy fallback
		req.SnapshotVersion, // client-declared expected version — 0 means legacy fallback
	); bizE != nil {
		bizErr(c, bizE)
		return
	}

	// --- title fallback: caller may skip, we generate the same way the
	// traditional endpoint does so the two look identical in list views. ---
	taskNo := service.GenerateTaskNo()
	title := req.Title
	if title == "" {
		title = "Agent总结-" + taskNo[len(taskNo)-8:]
	}

	// --- de-dup participants up front (see task.go CreateSummary for the
	// rationale — a duplicate uid would otherwise turn into a 1062→500). ---
	seenParticipant := map[string]struct{}{userID: {}}
	extraParticipants := make([]participantReq, 0, len(req.Participants))
	for _, p := range req.Participants {
		if p.UserID == "" || p.UserID == userID {
			continue
		}
		if _, dup := seenParticipant[p.UserID]; dup {
			continue
		}
		seenParticipant[p.UserID] = struct{}{}
		extraParticipants = append(extraParticipants, p)
	}

	now := timezone.Now()

	// Time-range fields are non-null in the schema. Agent-created summaries do
	// not carry a real range yet (the range lives inside the deliverable text);
	// use now/now as a neutral placeholder so ordering still works.
	task := model.SummaryTask{
		TaskNo:            taskNo,
		SpaceID:           spaceID,
		CreatorID:         userID,
		Title:             title,
		Topic:             title,
		SummaryMode:       model.ModeByPerson,
		TimeRangeStart:    now,
		TimeRangeEnd:      now,
		Status:            model.StatusCompleted,
		TriggerType:       model.TriggerAgent,
		OriginChannelID:   finalChannelID,
		OriginChannelType: finalChannelType,
		// R11 Q2: persist the provenance flag set by tier-3/tier-4; explicit
		// and session-resolved origins keep the zero value (unmasked).
		OriginFromDerived: finalOriginFromDerived,
		ReferencedTaskIDs: serializeReferencedTaskIDs(req.ReferencedTaskIDs),
		// SUM-BE2 audit trail. resolvedAgentMessageID is the id
		// loadAgentMessageForSave actually returned (so the legacy "latest
		// assistant" fallback also writes the real message id, not 0).
		AgentSessionID:  req.SessionID,
		AgentMessageID:  resolvedAgentMessageID,
		SnapshotVersion: req.SnapshotVersion,
	}

	var createdTaskID int64
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&task).Error; err != nil {
			return fmt.Errorf("create summary_task: %w", err)
		}
		createdTaskID = task.ID

		// Sources: agent-produced summaries carry their own source list from
		// the front-end (which knows the origin channel + any additional
		// referenced channels). Source names are resolved from the IM DB the
		// same way the instant path (CreateSummary) does, so the detail chip
		// shows a human-readable name instead of a raw channel id; the
		// client-supplied name is deliberately NOT trusted (the instant path
		// drops it too — see task.go's CreateSummary). ResolveSourceNameWithType
		// handles a nil imDB with a deterministic "来源-xxxxxxxx" fallback.
		//
		// R10: dedup by (source_type, source_id) — uk_summary_source_task_type_id
		// (migration 20260814-01) would turn duplicate front-end input into a
		// 500; the create endpoint dedups identically.
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

		// Creator participant: pre-accepted (they just clicked "save").
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

		// The creator's PersonalResult IS the deliverable — status=Completed,
		// content pulled from agent_message above. Citations are built from
		// session tool traces below via buildCitationsForSession.
		creatorPR := model.PersonalResult{
			TaskID:           createdTaskID,
			ParticipantRefID: creatorP.ID,
			UserID:           userID,
			Content:          content,
			WorkerStatus:     model.PersonalStatusCompleted,
			GeneratedAt:      &now,
			SubmittedAt:      &now,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		// Build citations from session tool traces (fallback to empty array on error)
		cits, cerr := h.buildCitationsForSessionWithDB(c.Request.Context(), tx, req.SessionID, content, userID, req.RequestID)
		if cerr != nil {
			log.Printf("[handler] buildCitationsForSession failed session=%s: %v (fallback to empty)", req.SessionID, cerr)
			cits = nil
		}
		// Reference-based fallback (CHAT-REFERENCE-BASED-DESIGN-v1):
		// If session has no tool traces (typical refine flow — agent didn't
		// re-fetch, just rewrote from the referenced summary's content) AND
		// user referenced existing summaries, borrow citations from the FIRST
		// referenced task's PR. The content preserves original [n] markers
		// from the referenced summary (per summary_refine.md rule), so we
		// preserve the citation index alignment by borrowing verbatim.
		//
		// Without this, refined content shows "[n]" markers pointing at an
		// empty citations array → frontend renders broken/dangling refs.
		if len(cits) == 0 && len(req.ReferencedTaskIDs) > 0 {
			borrowedCits, unresolvedMarkers := h.borrowCitationsFromReference(
				c.Request.Context(), req.ReferencedTaskIDs[0], spaceID, userID)
			if len(borrowedCits) > 0 {
				cits = borrowedCits
				log.Printf("[handler] CreateAgentSummary borrowed %d citations from referenced task_id=%d session=%s",
					len(cits), req.ReferencedTaskIDs[0], req.SessionID)
			} else if len(unresolvedMarkers) > 0 {
				// R9 P2-2 (PR #190, yujiawei review 4926742282): the borrow
				// returned empty (referenced task's citations redacted or
				// genuinely absent) but content still carries the referenced
				// summary's [n] markers. Save with empty citations + live
				// markers → frontend renders broken/dangling citation links.
				// Strip only marker indices that belong to the referenced
				// artifact. Unrelated bracketed integers remain user content.
				content = stripUnresolvedCitationMarkers(content, unresolvedMarkers)
				creatorPR.Content = content
				log.Printf("[handler] CreateAgentSummary stripped dangling citation markers (borrow returned empty) session=%s ref_task_id=%d",
					req.SessionID, req.ReferencedTaskIDs[0])
			}
		}
		creatorPR.SetCitations(cits)
		// Build v1 snapshot for agent-generated summary
		snapshot := h.buildSnapshotV1(tx, req.SessionID, userID, &task, req.Sources)
		creatorPR.SetSnapshot(snapshot)
		if err := tx.Create(&creatorPR).Error; err != nil {
			return fmt.Errorf("create creator personal_result: %w", err)
		}
		if err := tx.Model(&creatorP).Update("personal_result_id", creatorPR.ID).Error; err != nil {
			return fmt.Errorf("link participant to personal_result: %w", err)
		}

		// Additional participants (if any) — no PersonalResult, they will only
		// see the shared deliverable via the members-list view. Matches the
		// pending-invite semantics of the traditional path's AddMembers.
		for _, p := range extraParticipants {
			pp := model.SummaryParticipant{
				TaskID: createdTaskID,
				UserID: p.UserID,
				UserName: func() string {
					if p.UserName != "" {
						return p.UserName
					}
					return service.ResolveUserName(p.UserID)
				}(),
			}
			if err := tx.Create(&pp).Error; err != nil {
				return fmt.Errorf("create participant %s: %w", p.UserID, err)
			}
		}

		// --- SUM-BE2 idempotency binding ---
		// When the client sent an Idempotency-Key header, persist the
		// (space, user, key) -> task_id + request_hash binding inside the
		// same transaction that created the task. Same-body retries replay
		// via the preflight above; different-body retries hit the preflight
		// 409. A concurrent duplicate hitting Create with the same tuple
		// loses the UNIQUE race; a locked read-back identifies the winner
		// without relying on MySQL's DSN-sensitive RowsAffected value. The
		// outer handler then re-reads the binding to decide replay vs mismatch.
		if idempotencyKey != "" {
			binding := model.SummaryAgentSaveIdempotency{
				SpaceID:        spaceID,
				UserID:         userID,
				IdempotencyKey: idempotencyKey,
				RequestHash:    requestHash,
				TaskID:         task.ID,
				CreatedAt:      now,
			}
			if err := createAgentSaveIdempotencyBinding(tx, &binding); err != nil {
				return err
			}
		}

		// Session lifecycle: chat is a "temporary workshop" — once the
		// deliverable is persisted, DELETE all agent_message rows for this
		// session so the workshop cannot be revisited (see
		// CHAT-REFERENCE-BASED-DESIGN-v1 §core mental model).
		//
		// Best-effort within the transaction: if the DELETE fails we still
		// let the whole transaction commit (the summary was saved fine, we
		// just leave orphan message rows for a cleanup cron). A failure
		// here should NOT block the user from seeing their saved summary.
		// owner-scoped：只删本 uid 的记录，防止 session_id 撞车时误删他人（SUM-158 blocker 1）。
		if err := tx.Where("user_id = ? AND session_id = ?", userID, req.SessionID).Delete(&model.AgentMessage{}).Error; err != nil {
			log.Printf("[handler] CreateAgentSummary: session cleanup DELETE failed session=%s: %v (summary was saved OK, orphan rows will remain)", req.SessionID, err)
			// Intentionally do NOT return err — the summary is safely saved.
		}

		return nil
	})
	if errors.Is(err, errAgentSaveIdempotencyConflict) {
		// A concurrent request won the UNIQUE race. Re-read the binding to
		// decide replay-vs-mismatch, mirroring bot_summary_create.go's flow.
		existing, mismatched, ok, stale, ferr := findAgentSaveIdempotentTaskWithHash(
			c.Request.Context(), h.db, spaceID, userID, idempotencyKey, requestHash,
		)
		if ferr != nil || !ok {
			log.Printf("[handler] CreateAgentSummary idempotency race re-read failed space=%s user=%s key=%s ok=%v: %v", spaceID, userID, idempotencyKey, ok, ferr)
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "idempotency race resolution failed"})
			return
		}
		writeAgentSaveIdempotencyResponse(c, existing, mismatched, stale)
		return
	}
	if err != nil {
		log.Printf("[handler] CreateAgentSummary tx failed space=%s user=%s session=%s: %v", spaceID, userID, req.SessionID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "落库失败"})
		return
	}

	log.Printf("[handler] CreateAgentSummary ok space=%s user=%s task_id=%d session=%s content_len=%d origin_channel=%s/%d",
		spaceID, userID, createdTaskID, req.SessionID, len(content), finalChannelID, finalChannelType)

	// Response shape is intentionally isomorphic to POST /summaries so the
	// front-end can consume both endpoints with the same success handler.
	c.JSON(http.StatusOK, apiResponse{
		Code:    0,
		Message: "ok",
		Data: gin.H{
			"task_id":    createdTaskID,
			"task_no":    task.TaskNo,
			"status":     task.Status,
			"created_at": task.CreatedAt,
		},
	})
}

func writeAgentSaveIdempotencyResponse(c *gin.Context, task model.SummaryTask, mismatched, stale bool) {
	if stale {
		c.JSON(http.StatusConflict, apiResponse{
			Code:    40009,
			Message: "idempotency key is bound to a deleted summary",
			Data: gin.H{
				"task_id":         task.ID,
				"task_no":         task.TaskNo,
				"reason":          "deleted_summary",
				"recovery_action": "start_new_summary",
			},
		})
		return
	}
	if mismatched {
		c.JSON(http.StatusConflict, apiResponse{
			Code:    40009,
			Message: "idempotency key already bound to a different agent save request; open the existing summary to edit it",
			Data: gin.H{
				"task_id":         task.ID,
				"task_no":         task.TaskNo,
				"reason":          "request_mismatch",
				"recovery_action": "open_existing_summary",
			},
		})
		return
	}
	log.Printf("[handler] CreateAgentSummary idempotency replay task_id=%d", task.ID)
	c.JSON(http.StatusOK, apiResponse{
		Code:    0,
		Message: "ok",
		Data: gin.H{
			"task_id":    task.ID,
			"task_no":    task.TaskNo,
			"status":     task.Status,
			"created_at": task.CreatedAt,
			"replayed":   true,
		},
	})
}

// errNoAgentOutput signals that the session exists but has no assistant reply
// worth persisting as a summary yet — mapped by the handler to error code 40004.
var errNoAgentOutput = errors.New("no assistant output on session")

// buildSnapshotV1 constructs the v1 snapshot for an agent-generated summary.
// This is the initial snapshot (parent_snapshot_version=null, user_instruction=null).
// Tool summary is built by counting role='tool' messages in agent_message.
//
// owner-scoped：必须传 userID，避免 tool 统计跨用户聚合别人 session 的行数
// （SUM-158 blocker 1）。
func (h *AgentSummaryHandler) buildSnapshotV1(
	db *gorm.DB,
	sessionID, userID string,
	task *model.SummaryTask,
	sources []sourceReq,
) *model.Snapshot {
	// Build tool_summary: count tool invocations by name
	var toolMessages []model.AgentMessage
	if err := db.Where("user_id = ? AND session_id = ? AND role = ?", userID, sessionID, "tool").
		Find(&toolMessages).Error; err != nil {
		log.Printf("[handler] buildSnapshotV1: failed to query tool messages: %v", err)
		// fallback to empty array on error
	}

	toolCounts := make(map[string]int)
	for _, tm := range toolMessages {
		if tm.Name != "" {
			toolCounts[tm.Name]++
		}
	}

	toolSummary := make([]string, 0, len(toolCounts))
	// Sort tool names for stable output order
	toolNames := make([]string, 0, len(toolCounts))
	for name := range toolCounts {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)

	for _, name := range toolNames {
		toolSummary = append(toolSummary, fmt.Sprintf("%s x %d", name, toolCounts[name]))
	}

	// Build scope: channel_ids from sources, channel_names left empty for now
	// (SUM-36 allows channel_names to be empty array if not available)
	channelIDs := make([]string, 0, len(sources))
	for _, s := range sources {
		if s.SourceID != "" {
			channelIDs = append(channelIDs, s.SourceID)
		}
	}

	// Requirement: use task title as the user requirement
	requirement := task.Title

	snap := &model.Snapshot{
		SnapshotVersion: 1,
		TaskID:          task.ID,
		ContentVersion:  1,
		Requirement:     requirement,
		Scope: model.SnapshotScope{
			ChannelIDs:   channelIDs,
			ChannelNames: []string{}, // empty for now, P0.2 will populate
			TimeRange: model.TimeRangeJSON{
				Start: task.TimeRangeStart.Format("2006-01-02T15:04:05Z07:00"),
				End:   task.TimeRangeEnd.Format("2006-01-02T15:04:05Z07:00"),
			},
		},
		ToolSummary:           toolSummary,
		DataFreshnessNote:     "tool_summary 记录本次生成时的调用轨迹,不代表数据边界,涉及新数据源必须调 fetch_channel 验证",
		ParentSnapshotVersion: nil,
		UserInstruction:       nil,
	}

	return snap
}
