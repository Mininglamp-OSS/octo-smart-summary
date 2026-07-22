package handler

import "sync"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// agentChatProfile 是未显式指定 profile 时的默认场景（提示词 + 工具名单在 internal/agent/profile.go 配）。
const agentChatProfile = "chat"

// maxMessageLen 是单条用户 message 的最大字符数（rune），超长直接 400，避免超长入参打爆上游。
const maxMessageLen = 8192

// sessionIDPattern 约束前端生成的 session_id：仅字母数字下划线连字符、1..128 长。
// 既防注入/异常键，也与 DB varchar(128) 对齐。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AgentChatHandler 提供非流式一问一答对话入口，复用 internal/agent 底座。
//
// 融合两条能力：
//   - 动态 profile + uid 注入：每请求按 req.Profile 构建 runner；summary 场景把
//     鉴权得到的 uid 注入工具 handler，做频道/消息级权限隔离（工具能力线）。
//   - 多轮记忆：按 session_id 从 store 读历史、滑窗截断后喂给 RunWithHistory，
//     成功回复后落库（多轮对话线）。
//
// LLM 配置（url/key/model/...）在构造期注入并留存，供每请求动态建 runner；
// 敏感值（key）全程从环境变量经 config 传入，不出现在代码中。
type AgentChatHandler struct {
	llmApiURL    string
	llmApiKey    string
	llmModel     string
	llmTimeout   int
	llmMaxTokens int

	db     *gorm.DB          // 用于 fetch 引用总结的产物 + 快照(见 CHAT-REFERENCE-BASED-DESIGN-v1)
	store  agentHistoryStore // 多轮记忆读写（生产为 gorm 实现，测试可注入 mock）
	window int               // 滑窗保留的最近轮数

	// test-only fields: when set, bypass dynamic runner construction
	testRunner *agent.Runner
	testSystem string
}

// newAgentChatHandlerWithRunner 用一个已构造好的 Runner + 系统提示词 + 记忆存储造 handler。
// 供测试注入带假 LLM 的 Runner + mock store（走 testRunner 分支，跳过动态构建）。
func newAgentChatHandlerWithRunner(r *agent.Runner, system string, store agentHistoryStore, window int) *AgentChatHandler {
	return &AgentChatHandler{
		testRunner: r,
		testSystem: system,
		store:      store,
		window:     window,
	}
}

// NewAgentChatHandler 生产构造：留存 LLM 配置供每请求动态建 runner，
// 并接入多轮记忆存储（db）与滑窗。提示词/工具/策略在 profile.go 与 prompts/*.md 配置。
func NewAgentChatHandler(db *gorm.DB, llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) *AgentChatHandler {
	return &AgentChatHandler{
		llmApiURL:    llmApiURL,
		llmApiKey:    llmApiKey,
		llmModel:     llmModel,
		llmTimeout:   llmTimeout,
		llmMaxTokens: llmMaxTokens,
		db:           db,
		store:        newAgentMessageRepo(db),
		window:       agent.HistoryWindow(),
	}
}

// buildRunnerForProfile constructs a runner for the given profile name.
// If uid is non-empty and profile is "summary", it will be injected into tool handlers.
func (h *AgentChatHandler) buildRunnerForProfile(profileName, uid, sessionID string) (*agent.Runner, string, error) {
	profile, err := agent.GetProfile(profileName)
	if err != nil {
		return nil, "", fmt.Errorf("load profile %q: %w", profileName, err)
	}
	system, err := agent.LoadPrompt(profile.PromptFile)
	if err != nil {
		return nil, "", fmt.Errorf("load prompt %q: %w", profile.PromptFile, err)
	}

	var reg *agent.Registry
	if (profileName == "summary" || profileName == "summary_refine") && uid != "" {
		reg, err = h.buildSummaryRegistryWithUID(uid, sessionID)
	} else {
		reg, err = agent.BuildRegistry(profile.Tools)
	}
	if err != nil {
		return nil, "", fmt.Errorf("build registry: %w", err)
	}

	client := agent.NewClient(h.llmApiURL, h.llmApiKey, h.llmModel, h.llmTimeout, h.llmMaxTokens)
	pool := agent.NewPool(4)
	runner := agent.NewRunner(client, reg, pool, profile.Policy)
	return runner, system, nil
}

// buildSummaryRegistryWithUID builds a summary registry with uid injected into tool handlers.
func (h *AgentChatHandler) buildSummaryRegistryWithUID(uid, sessionID string) (*agent.Registry, error) {
	reg := agent.NewRegistry()

	// Non-summary tools (no uid injection needed)
	for _, name := range []string{"get_current_time", "extract_time_range"} {
		factory, ok := agent.GetToolFactory(name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		schema, handler := factory()
		reg.Register(schema, handler)
	}

	// Summary tools: wrap handlers to inject uid via context
	summaryTools := []string{
		"list_channels", "narrow_channels_by_topic", "find_shared_channels",
		"peek_channel", "fetch_channel", "search_messages",
		"filter_relevant", "summarize_chunk", "merge_summaries",
	}
	for _, name := range summaryTools {
		factory, ok := agent.GetToolFactory(name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		schema, origHandler := factory()

		// Wrap handler to inject uid and sessionID into context
		wrappedHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
			ctx = context.WithValue(ctx, agent.ContextKeyUID, uid)
			ctx = context.WithValue(ctx, agent.ContextKeySessionID, sessionID)
			return origHandler(ctx, args)
		}
		reg.Register(schema, wrappedHandler)
	}

	return reg, nil
}

// agentChatRequest 是聊天入参。session_id 由前端生成并必传，后端据此串联多轮历史。
// profile 可选，指定使用的场景名（默认 "chat"）；总结场景传 "summary" 以挂载真实工具。
// referenced_task_ids 可选：每轮都会重新 fetch 引用总结,拼进 system prompt。
// 前端可全程带此字段(引用锁定后每轮都用相同 id),后端每轮拉最新版本;
// 若空数组或字段缺,当轮 chat 无引用材料(等同普通 chat)。
// 见 CHAT-REFERENCE-BASED-DESIGN-v1。
type agentChatRequest struct {
	Message           string  `json:"message"`
	SessionID         string  `json:"session_id"`
	Profile           string  `json:"profile,omitempty"`
	ReferencedTaskIDs []int64 `json:"referenced_task_ids,omitempty"`
}

// Chat 处理 POST /api/v1/agent/chat：非流式一问一答，携带多轮历史。
//
// 流程：校验 → 取鉴权 uid → 按 profile 动态建 runner（summary 注入 uid 工具）
//
//	→ 读 session 历史并滑窗截断 → RunWithHistory 多轮驱动 → 成功后落库。
//
// 并发约束：单 session 依赖前端单飞（同一 session_id 勿并发发送）。LoadHistory→LLM→
// AppendMessages 全程无锁，若同 session 并发进入会读到相同历史各自续写，产生分叉历史；
// 锁 / 版本号方案留后续，本轮不实现。
func (h *AgentChatHandler) Chat(c *gin.Context) {
	var req agentChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	if req.Message == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "message 不能为空"})
		return
	}
	if len([]rune(req.Message)) > maxMessageLen {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "message 过长"})
		return
	}
	// session_id 前端必传；缺失则无法串联历史，直接 400。
	if req.SessionID == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 不能为空"})
		return
	}
	if !sessionIDPattern.MatchString(req.SessionID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 非法"})
		return
	}

	profileName := req.Profile
	if profileName == "" {
		profileName = agentChatProfile // default profile
	}

	// Inject session_id into context for tool handlers (evidence persistence, Stage 3 Blocker C).
	ctx := context.WithValue(c.Request.Context(), agent.ContextKeySessionID, req.SessionID)

	// Extract uid from middleware (authenticated identity).
	// 鉴权中间件已保证到此处 uid 非空；此处再做一次显式守卫，与 ChatStream()/History()
	// 对称，避免将来路由若误配为非严格鉴权时在本入口静默降级（PR #158 Octo-Q P2）。
	uid := middleware.GetUserID(c)
	if uid == "" {
		c.JSON(http.StatusUnauthorized, apiResponse{Code: 40100, Message: "missing auth context"})
		return
	}

	// 按 profile 组装 runner（summary 场景注入 uid 工具）；测试可注入现成 runner。
	var runner *agent.Runner
	var system string
	var err error
	if h.testRunner != nil {
		runner = h.testRunner
		system = h.testSystem
	} else {
		runner, system, err = h.buildRunnerForProfile(profileName, uid, req.SessionID)
		if err != nil {
			log.Printf("[agent] build runner for profile %q: %v", profileName, err)
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "failed to initialize agent", Detail: safeErrorDetail(err)})
			return
		}
	}

	// 读多轮历史并滑窗截断。owner-scoped：只加载当前 uid 归属的记录，
	// 跨用户猜到相同 session_id 也只会得到空历史（SUM-158 blocker 1）。
	history, err := h.store.LoadHistory(ctx, req.SessionID, uid)
	if err != nil {
		log.Printf("[agent] load history error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat failed", Detail: safeErrorDetail(err)})
		return
	}

	// 每轮 chat 都重新拼引用进 system(每次拉最新版本、多轮迭代持续可见)。
	// system 每轮独立传给 LLM 不会自动继承 —— 首轮限定的老实现会导致
	// 第 2+ 轮 agent 看不到引用材料(见 CHAT-REFERENCE-BASED-DESIGN-v1
	// 多轮上下文修复)。token 增量按引用大小约 5-15K/轮,可接受。
	if len(req.ReferencedTaskIDs) > 0 {
		spaceID := middleware.GetSpaceID(c)
		refContext, loaded, refErr := buildReferencedSummariesContext(
			ctx, h.db, spaceID, uid, req.ReferencedTaskIDs)
		if refErr != nil {
			log.Printf("[agent] chat build reference context error: %v", refErr)
			// 引用加载失败不阻断本次对话,agent 走无引用路径
		} else if refContext != "" {
			system = system + refContext
			log.Printf("[agent] chat session=%s loaded %d referenced tasks: %v", req.SessionID, len(loaded), loaded)
		}
	}

	history = agent.TruncateHistory(history, h.window)

	reply, newMsgs, err := runner.RunWithHistory(ctx, system, history, req.Message)
	if err != nil {
		// 真实错误只记服务端日志，避免向调用方泄漏上游 LLM 地址/网络/内部细节。
		// Detail 走白名单：仅 context deadline / max steps / empty response 等
		// 明确不含内部地址/IP/token 的 error 会被透传给客户端，其它一律为
		// "internal error"（safeErrorDetail 保证），前端可据此区分超时 vs 未知错。
		log.Printf("[agent] chat runner error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat failed", Detail: safeErrorDetail(err)})
		return
	}

	// 成功回复后才落库；落库失败不阻断本次回复（宁可丢本回合历史，也不只落 user 造脏历史）。
	// user_id 与 LoadHistory 保持一致，杜绝跨用户污染（SUM-158 blocker 1）。
	if err := h.store.AppendMessages(ctx, req.SessionID, uid, newMsgs); err != nil {
		log.Printf("[agent] append messages error: %v", err)
	}

	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"reply":      reply,
		"session_id": req.SessionID,
		"profile":    profileName,
	}})
}

// historyBubble 是只读历史接口返回的单条可展示气泡：只含 role + content，
// 不带 tool_calls/tool_call_id/name 等中间态字段（前端只展示最终对话气泡）。
type historyBubble struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// History 处理 GET /api/v1/agent/chat/history：按 session_id 只读回该会话的历史消息。
//
// 契约：
//   - session_id 必传，校验规则与 Chat 入口一致（sessionIDPattern），非法/缺失 400（code 40000）。
//   - 复用 h.store.LoadHistory 拿到按 id 升序的全量消息，在 handler 层过滤：只保留
//     role∈{user,assistant} 且 content 非空的消息，剥掉 tool_calls 等中间步骤。
//   - session 无历史时返回空数组 messages:[]，不是错误。
func (h *AgentChatHandler) History(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 不能为空"})
		return
	}
	if !sessionIDPattern.MatchString(sessionID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 非法"})
		return
	}

	// 拿鉴权 uid：owner-scoped 加载，跨用户猜到相同 session_id 只会返回空历史，
	// 与真实空会话在响应上不可区分（不泄漏 session 存在）(SUM-158 blocker 1)。
	uid := middleware.GetUserID(c)
	if uid == "" {
		c.JSON(http.StatusUnauthorized, apiResponse{Code: 40100, Message: "missing auth context"})
		return
	}

	ctx := c.Request.Context()
	history, err := h.store.LoadHistory(ctx, sessionID, uid)
	if err != nil {
		log.Printf("[agent] load history error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat history failed", Detail: safeErrorDetail(err)})
		return
	}

	// 只保留可展示气泡：role 为 user/assistant 且 content 非空；过滤 tool 消息与
	// assistant 消息里的 tool_calls 中间步骤（只留 role+content）。
	bubbles := make([]historyBubble, 0, len(history))
	for i := range history {
		m := history[i]
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if m.Content == "" {
			continue
		}
		bubbles = append(bubbles, historyBubble{Role: m.Role, Content: m.Content})
	}

	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"session_id": sessionID,
		"messages":   bubbles,
	}})
	c.Writer.Flush()
}

// sseSink provides thread-safe SSE event writing with a per-request mutex.
// Each ChatStream request creates one sseSink to serialize concurrent OnEvent callbacks.
type sseSink struct {
	mu sync.Mutex
	w  gin.ResponseWriter
}

// write emits an SSE event with the given name and JSON payload, gracefully handling write failures.
func (s *sseSink) write(event string, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		log.Printf("[agent] write SSE %s failed (client disconnect?): %v", event, err)
		return
	}
	s.w.Flush()
}

// ChatStream handles POST /api/v1/agent/chat/stream: SSE-based streaming chat with progress events.
//
// Workflow: identical to Chat, but emits SSE events (progress/done/error) instead of JSON response.
// - progress: emitted for each tool_end (phase, label, step, elapsed_ms, detail)
// - done: final reply + session_id
// - error: on any failure
//
// Context timeout: 300s (longer than Chat's 120s for map-reduce workloads).
// Database persistence: same as Chat (AppendMessages only on success, no progress events stored).
func (h *AgentChatHandler) ChatStream(c *gin.Context) {
	var req agentChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	if req.Message == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "message 不能为空"})
		return
	}
	if len([]rune(req.Message)) > maxMessageLen {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "message 过长"})
		return
	}
	if req.SessionID == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 不能为空"})
		return
	}
	if !sessionIDPattern.MatchString(req.SessionID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 非法"})
		return
	}

	profileName := req.Profile
	if profileName == "" {
		profileName = agentChatProfile
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // Disable nginx buffering

	// Flush headers immediately to trigger frontend open callback
	c.Writer.Flush()

	// 300s context timeout for long-running map-reduce tasks
	// Inject session_id into base context for tool handlers (evidence persistence, Stage 3 Blocker C).
	baseCtx := context.WithValue(c.Request.Context(), agent.ContextKeySessionID, req.SessionID)
	ctx, cancel := context.WithTimeout(baseCtx, 300*time.Second)
	defer cancel()

	uid := middleware.GetUserID(c)
	if uid == "" {
		// Defense-in-depth (post-#158 Octo-Q P2, 4-reviewer): Chat() and
		// History() explicitly reject empty uid; ChatStream() previously
		// relied only on StrictAuthMiddleware. Symmetric guard closes the
		// asymmetry so a future route misconfiguration cannot silently
		// degrade only this endpoint's authz. Errors are emitted via the
		// SSE sink because response headers are already sent by this point.
		sink := &sseSink{w: c.Writer}
		h.writeSSEErrorViaSink(sink, 40100, "missing auth context")
		return
	}

	// Build runner with OnEvent callback for SSE progress
	var runner *agent.Runner
	var system string
	var err error
	if h.testRunner != nil {
		runner = h.testRunner
		system = h.testSystem
	} else {
		runner, system, err = h.buildRunnerForProfile(profileName, uid, req.SessionID)
		if err != nil {
			log.Printf("[agent] build runner for profile %q: %v", profileName, err)
			sink := &sseSink{w: c.Writer}
			h.writeSSEErrorViaSinkWithDetail(sink, 50000, "failed to initialize agent", safeErrorDetail(err))
			return
		}
	}

	// Create per-request SSE sink for thread-safe concurrent writes
	sink := &sseSink{w: c.Writer}

	// Inject OnEvent callback to emit SSE progress events.
	// SECURITY: we emit ONLY the abstract phase (+ a safe integer count) — never the
	// raw tool name, label, or free-text detail — so the progress stream does not
	// leak which concrete tools drive summarization.
	runner.OnEvent = func(e agent.Event) {
		if e.Type == "tool_end" {
			phase, _ := agent.GetToolLabel(e.Tool)
			h.writeSSEProgressViaSink(sink, phase, e.Step, e.OfSteps, e.Count, e.ElapsedMs)
		} else if e.Type == "step_end" && !e.StepHasTools {
			// Final answer step (no tool calls) → abstract "reply" phase.
			h.writeSSEProgressViaSink(sink, "reply", e.Step, e.OfSteps, 0, e.ElapsedMs)
		}
	}

	// Load and truncate history (same as Chat). owner-scoped by uid（SUM-158 blocker 1）。
	history, err := h.store.LoadHistory(ctx, req.SessionID, uid)
	if err != nil {
		log.Printf("[agent] load history error: %v", err)
		h.writeSSEErrorViaSinkWithDetail(sink, 50000, "agent chat failed", safeErrorDetail(err))
		return
	}

	// 每轮 chat 都重新拼引用进 system —— 与 Chat 逻辑严格一致
	// (见 CHAT-REFERENCE-BASED-DESIGN-v1 多轮上下文修复)。
	if len(req.ReferencedTaskIDs) > 0 {
		spaceID := middleware.GetSpaceID(c)
		refContext, loaded, refErr := buildReferencedSummariesContext(
			ctx, h.db, spaceID, uid, req.ReferencedTaskIDs)
		if refErr != nil {
			log.Printf("[agent] chat/stream build reference context error: %v", refErr)
		} else if refContext != "" {
			system = system + refContext
			log.Printf("[agent] chat/stream session=%s loaded %d referenced tasks: %v", req.SessionID, len(loaded), loaded)
		}
	}

	history = agent.TruncateHistory(history, h.window)

	// Run agent with history
	reply, newMsgs, err := runner.RunWithHistory(ctx, system, history, req.Message)
	if err != nil {
		log.Printf("[agent] chat runner error: %v", err)
		h.writeSSEErrorViaSinkWithDetail(sink, 50000, "agent chat failed", safeErrorDetail(err))
		return
	}

	// Persist messages on success (same as Chat). owner-scoped by uid（SUM-158 blocker 1）。
	if err := h.store.AppendMessages(ctx, req.SessionID, uid, newMsgs); err != nil {
		log.Printf("[agent] append messages error: %v", err)
	}

	// Emit done event with final reply
	h.writeSSEDoneViaSink(sink, reply, req.SessionID)
}

// writeSSEProgressViaSink writes a progress SSE event via the provided sink.
// Contract (stable with frontend): only abstract, non-leaking fields are emitted —
//
//	phase (safe enum: understand|retrieve|filter|distill|compose|reply),
//	step / ofSteps / elapsed_ms, and an optional integer count (omitted when 0).
//
// It intentionally does NOT emit the raw tool name, an internal label, or free-text detail.
func (h *AgentChatHandler) writeSSEProgressViaSink(sink *sseSink, phase string, step, ofSteps, count int, elapsedMs int64) {
	data := map[string]interface{}{
		"phase":      phase,
		"step":       step,
		"ofSteps":    ofSteps,
		"elapsed_ms": elapsedMs,
	}
	if count > 0 {
		data["count"] = count
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("[agent] marshal progress event: %v", err)
		return
	}

	sink.write("progress", jsonData)
}

// writeSSEDoneViaSink writes a done SSE event via the provided sink.
func (h *AgentChatHandler) writeSSEDoneViaSink(sink *sseSink, reply, sessionID string) {
	data := map[string]interface{}{
		"reply":      reply,
		"session_id": sessionID,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("[agent] marshal done event: %v", err)
		return
	}

	sink.write("done", jsonData)
}

// writeSSEErrorViaSink writes an error SSE event via the provided sink.
func (h *AgentChatHandler) writeSSEErrorViaSink(sink *sseSink, code int, message string) {
	h.writeSSEErrorViaSinkWithDetail(sink, code, message, "")
}

// writeSSEErrorViaSinkWithDetail is the detail-aware variant. Emits the
// standard SSE error frame plus a safe-to-expose detail string when
// non-empty. See safeErrorDetail below for the whitelist policy.
func (h *AgentChatHandler) writeSSEErrorViaSinkWithDetail(sink *sseSink, code int, message string, detail string) {
	data := map[string]interface{}{
		"code":    code,
		"message": message,
	}
	if detail != "" {
		data["detail"] = detail
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("[agent] marshal error event: %v", err)
		return
	}

	sink.write("error", jsonData)
}

// safeErrorDetail returns a compact, safe-to-expose string derived from err
// for inclusion in JSON/SSE error responses. Whitelist-only: we return the
// raw err.Error() only for a small set of well-known agent runner failure
// modes whose text is known not to embed URLs, IPs, tokens, or stack
// fragments. Everything else collapses to "internal error", which lets the
// client distinguish "your request timed out" (actionable) from "something
// broke server-side" (open an issue / poll the ops channel) without
// leaking backend geometry.
//
// Grow this whitelist deliberately: each new pattern must be traceable to
// a specific errors.New / fmt.Errorf site in the runner or handler code
// path whose format string contains no operator-supplied data.
func safeErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// runner.go stepCtx timeout (single LLM planning call). Tune via
		// AGENT_STEP_TIMEOUT env; see profile.go / CONFIGURATION.md.
		return "context deadline exceeded"
	case errors.Is(err, context.Canceled):
		// upstream cancellation (client disconnect or outer ctx timeout).
		return "context canceled"
	case strings.Contains(err.Error(), "max steps exceeded"):
		// runner.go MaxSteps loop guard. Model failed to converge.
		return "max steps exceeded"
	case strings.Contains(err.Error(), "LLM returned empty response with no tool_calls"):
		// runner.go final-step empty content guard (SUM-158 blocker follow-up).
		return "LLM returned empty response with no tool_calls at final step"
	case strings.Contains(err.Error(), "unknown agent profile"):
		// profile.go GetProfile lookup miss.
		return "unknown agent profile"
	}
	return "internal error"
}
