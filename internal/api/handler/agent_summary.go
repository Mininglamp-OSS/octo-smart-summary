package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"unicode/utf8"

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
	llmApiURL    string
	llmApiKey    string
	llmModel     string
	llmTimeout   int
	llmMaxTokens int
	store        agentHistoryStore
	// runnerFactory is an optional test-only hook for injecting a fake agent
	// runner without going through the real LLM. When nil (production path),
	// newRunner falls back to buildRunner with handler's LLM config.
	// Returns refineRunner (an interface, so tests can plug in a fake struct)
	// rather than *agent.Runner (a concrete type whose dependencies are
	// unexported).
	// Not exposed via NewAgentSummaryHandler — tests assign this field
	// directly using same-package access.
	runnerFactory func(profile, uid string) (refineRunner, string, error)
}

// refineRunner is the minimal subset of *agent.Runner used by RefineAgentSummary.
// Declared as an interface (not a concrete type) so tests can inject a fake
// without depending on unexported types in the agent package.
type refineRunner interface {
	RunWithHistory(ctx context.Context, system string, history []agent.Message, userInput string) (string, []agent.Message, error)
}

func NewAgentSummaryHandler(db *gorm.DB, llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) *AgentSummaryHandler {
	return &AgentSummaryHandler{
		db:           db,
		llmApiURL:    llmApiURL,
		llmApiKey:    llmApiKey,
		llmModel:     llmModel,
		llmTimeout:   llmTimeout,
		llmMaxTokens: llmMaxTokens,
		store:        newAgentMessageRepo(db),
	}
}

// createAgentSummaryReq mirrors the SUM-24 v1.0 contract where origin_channel
// fields are now optional. OriginChannelID is a pointer to distinguish between
// "not provided" (nil) and "explicitly provided as empty string" (non-nil pointing to "").
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

	// SUM-24: origin_channel fields are now optional. Distinguish between:
	// - nil (not provided) → resolve from session
	// - non-nil but empty → old validation error
	// - non-nil and non-empty → use provided value
	var finalChannelID string
	var finalChannelType int

	if req.OriginChannelID == nil {
		// Not provided → resolve from session tool traces
		resolvedID, resolvedType, err := h.resolveOriginChannelFromSession(c.Request.Context(), req.SessionID, userID)
		if err != nil {
			// DB error or other real failure → 500
			log.Printf("[handler] resolveOriginChannelFromSession failed session=%s: %v", req.SessionID, err)
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "resolve origin channel failed: " + err.Error()})
			return
		}
		if resolvedID == "" {
			// SUM-24 fallback failed (no fetch_channel in session — typical for
			// pure refine flows where the agent didn't need to re-fetch).
			// CHAT-REFERENCE-BASED-DESIGN-v1 second-order fallback: if the user
			// referenced existing summaries, borrow the FIRST referenced task's
			// origin as the new summary's origin. This keeps the chat/list
			// grouping sensible without asking the user to re-select the channel.
			if len(req.ReferencedTaskIDs) > 0 {
				var refTask model.SummaryTask
				if err := h.db.WithContext(c.Request.Context()).
					Select("id, origin_channel_id, origin_channel_type").
					Where("id = ? AND space_id = ? AND origin_channel_id != ''", req.ReferencedTaskIDs[0], spaceID).
					First(&refTask).Error; err == nil {
					finalChannelID = refTask.OriginChannelID
					finalChannelType = refTask.OriginChannelType
					log.Printf("[handler] CreateAgentSummary borrowed origin from referenced task_id=%d channel=%s/%d session=%s",
						refTask.ID, finalChannelID, finalChannelType, req.SessionID)
				}
			}
			if finalChannelID == "" {
				// Truly no origin available anywhere → 400 with specific message
				c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "origin_channel_id 未传且无法从 session 反查(session 无 fetch_channel 调用),也无引用总结可继承 origin"})
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

	if utf8.RuneCountInString(req.Title) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "title 不能超过 1000 字符"})
		return
	}

	// --- pull the agent's produced deliverable content from agent_message ---
	// Contract: use the latest role=assistant message on this session as the
	// deliverable. Empty content ⇒ 40004 (must block, no empty summary allowed).
	// We only look at messages with tool_calls IS NULL to skip the intermediate
	// "call this tool" assistant messages; the final answer never has tool_calls.
	content, err := loadLatestAssistantContent(h.db, req.SessionID, userID)
	if err != nil {
		if errors.Is(err, errNoAgentOutput) {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40004, Message: "session 无有效产出,请先在对话中生成总结再保存"})
			return
		}
		log.Printf("[handler] CreateAgentSummary load session %s: %v", req.SessionID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "读取 session 产出失败"})
		return
	}

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
		SummaryMode:       model.ModeByPerson,
		TimeRangeStart:    now,
		TimeRangeEnd:      now,
		Status:            model.StatusCompleted,
		TriggerType:       model.TriggerAgent,
		OriginChannelID:   finalChannelID,
		OriginChannelType: finalChannelType,
		ReferencedTaskIDs: serializeReferencedTaskIDs(req.ReferencedTaskIDs),
	}

	var createdTaskID int64
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&task).Error; err != nil {
			return fmt.Errorf("create summary_task: %w", err)
		}
		createdTaskID = task.ID

		// Sources: agent-produced summaries carry their own source list from
		// the front-end (which knows the origin channel + any additional
		// referenced channels). We do not currently resolve source names via
		// IM DB — the deliverable already contains channel names in prose —
		// so we store source_id only; a future PR can plumb imDB in and call
		// ResolveSourceNameWithType if the UI wants the resolved display name.
		for _, s := range req.Sources {
			if s.SourceID == "" {
				continue
			}
			src := model.SummarySource{
				TaskID:     createdTaskID,
				SourceType: s.SourceType,
				SourceID:   s.SourceID,
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
		cits, cerr := h.buildCitationsForSession(c.Request.Context(), req.SessionID, content, userID)
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
			borrowedCits := h.borrowCitationsFromReference(
				c.Request.Context(), req.ReferencedTaskIDs[0], spaceID, userID)
			if len(borrowedCits) > 0 {
				cits = borrowedCits
				log.Printf("[handler] CreateAgentSummary borrowed %d citations from referenced task_id=%d session=%s",
					len(cits), req.ReferencedTaskIDs[0], req.SessionID)
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
	if err != nil {
		log.Printf("[handler] CreateAgentSummary tx failed space=%s user=%s session=%s: %v", spaceID, userID, req.SessionID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "落库失败: " + err.Error()})
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

// errNoAgentOutput signals that the session exists but has no assistant reply
// worth persisting as a summary yet — mapped by the handler to error code 40004.
var errNoAgentOutput = errors.New("no assistant output on session")

// loadLatestAssistantContent returns the latest non-empty assistant message
// text on the given session (skipping intermediate "call this tool" messages,
// which are recognisable because they carry a tool_calls payload).
//
// owner-scoped：必须传 userID，跨用户匹配返回 errNoAgentOutput（与真实空会话
// 在响应上不可区分，不泄漏 session 存在）(SUM-158 blocker 1)。
func loadLatestAssistantContent(db *gorm.DB, sessionID, userID string) (string, error) {
	var msg model.AgentMessage
	err := db.Where("user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''", userID, sessionID, "assistant").
		Order("id DESC").
		Limit(1).
		Take(&msg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", errNoAgentOutput
		}
		return "", err
	}
	return msg.Content, nil
}

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
