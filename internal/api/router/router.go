package router

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/handler"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/api/ws"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/streaming"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SetupPublic configures the public API router on :8080.
// 合并上游后统一签名：customTemplateLimit(上游模板) + streamHub(上游 SSE) + agent 原始 LLM 配置
// (agent chat/summary handler 用) + 变参 llm(上游 refine/personal 用的 *service.LLMClient，
// 可选，须置于末尾)。
func SetupPublic(db *gorm.DB, imDB *gorm.DB, hub *ws.Hub, authResolver middleware.TokenResolver, workerTriggerURL string, candidateQueryLimit int, featureTeamSchedule bool, customTemplateLimit int, streamHub *streaming.Hub, llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int, llm ...*service.LLMClient) *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// CORS
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,Token,X-Space-Id,Accept,Accept-Language")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// WebSocket (requires auth)
	r.GET("/ws/summaries", middleware.StrictAuthMiddleware(authResolver), middleware.SpaceMiddleware(), hub.HandleWS)
	r.GET("/ws", middleware.StrictAuthMiddleware(authResolver), middleware.SpaceMiddleware(), hub.HandleWS)

	// API routes
	taskH := handler.NewTaskHandler(db, imDB, workerTriggerURL)
	taskH.SetCustomTemplateLimit(customTemplateLimit)
	schedH := handler.NewScheduleHandlerWithFlag(db, featureTeamSchedule)
	personalH := handler.NewPersonalHandler(db, workerTriggerURL, hub)
	var refineLLM *service.LLMClient
	if len(llm) > 0 {
		refineLLM = llm[0]
	}
	editH := handler.NewEditHandler(db, refineLLM)
	personalH.SetLLM(refineLLM)
	streamH := handler.NewStreamHandler(db, streamHub)
	shareH := handler.NewShareHandler(db, imDB)

	v1 := r.Group("/api/v1")
	v1.Use(middleware.StrictAuthMiddleware(authResolver), middleware.StrictSpaceMiddleware())
	{
		v1.POST("/summaries", taskH.CreateSummary)
		// POST /summaries/agent — persist an agent-generated deliverable.
		// Isomorphic response shape with /summaries; born status=Completed +
		// trigger_type=Agent, content pulled from agent_message on the given
		// session_id. See handler/agent_summary.go.
		v1.POST("/summaries/batch-status", taskH.BatchStatus)
		v1.GET("/summaries", taskH.ListSummaries)
		v1.GET("/summaries/:id", taskH.GetSummary)
		v1.POST("/summaries/:id/shares", shareH.Create)
		v1.GET("/summary-shares/:share_id", shareH.Get)
		v1.DELETE("/summary-shares/:share_id", shareH.Revoke)
		v1.GET("/summaries/:id/stream", streamH.StreamSummary)
		v1.POST("/summaries/:id/read", taskH.MarkSummaryRead)
		v1.GET("/summaries/:id/result", taskH.GetResult)
		v1.POST("/summaries/:id/regenerate", taskH.Regenerate)
		v1.POST("/summaries/:id/refine", editH.RefineSummary)
		v1.POST("/summaries/:id/refine/stream", editH.RefineSummaryStream)
		v1.GET("/summaries/:id/versions", editH.ListSummaryVersions)
		v1.GET("/summaries/:id/versions/:result_id", editH.GetSummaryVersion)
		v1.POST("/summaries/:id/versions/:result_id/restore", editH.RestoreSummaryVersion)
		v1.POST("/summaries/:id/personal-refine", personalH.RefinePersonalSummary)
		v1.POST("/summaries/:id/personal-refine/stream", personalH.RefinePersonalSummaryStream)
		v1.POST("/summaries/:id/personal-regenerate", personalH.RegeneratePersonalSummary)
		v1.GET("/summaries/:id/personal-versions", personalH.ListPersonalVersions)
		v1.GET("/summaries/:id/personal-versions/:version_id", personalH.GetPersonalVersion)
		v1.POST("/summaries/:id/personal-versions/:version_id/restore", personalH.RestorePersonalVersion)
		v1.PUT("/summaries/:id/edit", editH.EditSummary)
		// need3/need6: a participant edits their OWN personal report -> triggers team recompute.
		v1.PUT("/summaries/:id/personal-edit", personalH.PersonalEdit)
		// OCT-21: a participant edits their OWN personal report BEFORE submit
		// (draft). Does NOT trigger team recompute, does NOT write edited_at,
		// does NOT revive. Allowed only when worker_status==Completed AND
		// submitted_at IS NULL; once submitted the caller must switch to
		// /personal-edit (which DOES trigger recompute).
		v1.PUT("/summaries/:id/personal-draft", personalH.PersonalDraft)
		// need7: creator adds new members as PENDING/unconfirmed; no PersonalResult,
		// no dispatch -- the new member must Accept to generate their summary.
		v1.POST("/summaries/:id/members", personalH.AddMembers)
		v1.DELETE("/summaries/:id", taskH.DeleteSummary)
		v1.POST("/summaries/:id/cancel", taskH.CancelSummary)
		v1.GET("/summary-infer", taskH.InferScope)
		v1.GET("/summary-member-candidates", handler.NewCandidateHandler(imDB, candidateQueryLimit).SearchCandidates)
		v1.GET("/summary-chat-candidates", handler.NewCandidateHandler(imDB, candidateQueryLimit).SearchChatCandidates)
		v1.GET("/summary-templates", taskH.GetTemplates)
		v1.POST("/summary-templates/my", taskH.CreateCustomTemplate)
		v1.PUT("/summary-templates/my/:id", taskH.UpdateCustomTemplate)
		v1.DELETE("/summary-templates/my/:id", taskH.DeleteCustomTemplate)
		v1.PUT("/summary-templates/:id/my", taskH.UpdateMyTemplate)
		v1.DELETE("/summary-templates/:id/my", taskH.DeleteMyTemplate)

		v1.POST("/summary-schedules", schedH.CreateSchedule)
		v1.GET("/summary-schedules", schedH.ListSchedules)
		v1.GET("/summary-schedules/:id", schedH.GetSchedule)
		v1.PUT("/summary-schedules/:id", schedH.UpdateSchedule)
		v1.DELETE("/summary-schedules/:id", schedH.DeleteSchedule)
		v1.PUT("/summary-schedules/:id/toggle", schedH.ToggleSchedule)
		v1.POST("/summary-schedules/:id/confirm", schedH.ConfirmSchedule)
	}

	// P2 routes: strict auth required
	p2 := r.Group("/api/v1")
	p2.Use(middleware.StrictAuthMiddleware(authResolver), middleware.StrictSpaceMiddleware())
	{
		p2.POST("/summaries/:id/accept", personalH.Accept)
		p2.POST("/summaries/:id/decline", personalH.Decline)
		p2.POST("/summaries/:id/respond", personalH.Respond)
		p2.GET("/summaries/:id/personal", personalH.GetPersonal)
		p2.POST("/summaries/:id/submit", personalH.Submit)
		p2.GET("/summaries/:id/members", personalH.GetMembers)
		// Leave a multi-person collaboration (participant, NOT creator).
		p2.POST("/summaries/:id/leave", personalH.Leave)
		// Creator removes a member from a multi-person collaboration. The target
		// uid is passed as a QUERY param (?uid=...), not a path segment: member ids
		// are opaque strings and a path segment would break gin routing / decoding
		// for any id containing reserved chars (e.g. an encoded '/'). Same prefix as
		// POST .../members (AddMembers); the HTTP method disambiguates.
		p2.DELETE("/summaries/:id/members", personalH.RemoveMember)
	}

	// Agent chat: requires auth. creator_uid is derived from the auth middleware
	// (not from LLM params); the summary profile injects it into tool handlers for
	// channel/message-level permission isolation. db backs multi-turn history.
	agentChatH := handler.NewAgentChatHandler(db, llmApiURL, llmApiKey, llmModel, llmTimeout, llmMaxTokens)
	agentGroup := r.Group("/api/v1/agent")
	agentGroup.Use(middleware.StrictAuthMiddleware(authResolver), middleware.StrictSpaceMiddleware())
	{
		agentGroup.POST("/chat", agentChatH.Chat)
		agentGroup.POST("/chat/stream", agentChatH.ChatStream)
		agentGroup.GET("/chat/history", agentChatH.History)
	}

	// Agent summary: persists agent-generated summaries.
	// (The old /summaries/:id/refine route was removed in favor of the
	// reference-based chat flow — see CHAT-REFERENCE-BASED-DESIGN-v1.)
	agentSummaryH := handler.NewAgentSummaryHandler(db, llmApiURL, llmApiKey, llmModel, llmTimeout, llmMaxTokens)
	v1.POST("/summaries/agent", agentSummaryH.CreateAgentSummary)

	return r
}

// SetupInternal configures the internal API router on :8081.
// Returns the engine and the InternalHandler so the caller can wire the worker trigger channel.
func SetupInternal(hub *ws.Hub, streamHub ...*streaming.Hub) (*gin.Engine, *handler.InternalHandler) {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	intH := handler.NewInternalHandler(hub)
	r.GET("/internal/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.POST("/internal/task-event", intH.TaskEvent)
	r.POST("/internal/worker-trigger", intH.WorkerTrigger)
	if len(streamHub) > 0 && streamHub[0] != nil {
		streamH := handler.NewStreamHandler(nil, streamHub[0])
		r.POST("/internal/summary-stream", streamH.Ingest)
	}

	return r, intH
}
