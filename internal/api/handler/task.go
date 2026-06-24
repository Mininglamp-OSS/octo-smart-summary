package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// maxSourceCount is the maximum number of information sources allowed per task.
const maxSourceCount = 30

// TaskHandler handles summary task endpoints.
type TaskHandler struct {
	db               *gorm.DB
	imDB             *gorm.DB
	workerTriggerURL string
}

// NewTaskHandler creates a new TaskHandler.
func NewTaskHandler(db, imDB *gorm.DB, workerTriggerURL string) *TaskHandler {
	return &TaskHandler{db: db, imDB: imDB, workerTriggerURL: workerTriggerURL}
}

// canAccessTask reports whether userID may read the task: creator or explicit
// participant. This is the single source of truth shared by the detail path
// (authorizeTaskAccess) and conceptually the batch path (batchAuthorize) and the
// list query (ListSummaries). Source-group membership alone does NOT grant access.
func (h *TaskHandler) canAccessTask(userID string, taskID int64, creatorID string) bool {
	if creatorID == userID {
		return true
	}
	var cnt int64
	h.db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, userID).
		Count(&cnt)
	return cnt > 0
}

// authorizeTaskAccess loads a task by ID and checks that the current user is
// authorized to access it. Authorization passes if the user is the task creator
// or an explicit participant. Source-group membership alone does NOT grant access.
// Returns the task and true on success; writes a JSON error response and returns
// nil, false on failure.
func (h *TaskHandler) authorizeTaskAccess(c *gin.Context, taskID int64) (*model.SummaryTask, bool) {
	userID := middleware.GetUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, apiResponse{Code: 4010, Message: "authentication required"})
		return nil, false
	}

	// P1 (cross-space isolation): scope the load to the caller's space so a task
	// from another space is reported as 40008 ("任务不存在") exactly like a missing
	// task, never leaked or read across spaces.
	spaceID := middleware.GetSpaceID(c)
	// fail-closed hard gate: an empty X-Space-Id must NEVER reach the query.
	// SummaryTask.SpaceID is `not null default ''`, so historical/anomalous tasks
	// with space_id='' may exist; querying `space_id=''` would MATCH them, leaking
	// a cross-space read (fail-open). Short-circuit to 40008/404 here so empty
	// space access is denied independent of any data invariant. This seals the
	// read path for every endpoint that gates through authorizeTaskAccess
	// (GetSummary/GetResult/Regenerate/DeleteSummary/CancelSummary).
	if spaceID == "" {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return nil, false
	}
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return nil, false
	}

	if h.canAccessTask(userID, task.ID, task.CreatorID) {
		return &task, true
	}

	c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "无权访问此任务"})
	return nil, false
}

func (h *TaskHandler) pickDisplayResult(taskID int64) (model.SummaryResult, bool) {
	result, err := queryDisplayResult(h.db, taskID)
	if err != nil {
		return model.SummaryResult{}, false
	}
	return result, true
}

type apiResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func ok(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: data})
}

func bizErr(c *gin.Context, err *service.BizError) {
	c.JSON(err.HTTPStatus, apiResponse{Code: err.Code, Message: err.Message})
}

type createSummaryReq struct {
	UID                 string           `json:"uid"`
	Title               string           `json:"title"`
	Topic               string           `json:"topic"`
	TimeRange           *timeRange       `json:"time_range"`
	Sources             []sourceReq      `json:"sources"`
	Participants        []participantReq `json:"participants"`
	ConfirmTimeoutHours int              `json:"confirm_timeout_hours"`
	OriginChannelID     string           `json:"origin_channel_id"`
	OriginChannelType   int              `json:"origin_channel_type"`
}

type timeRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type sourceReq struct {
	SourceType int    `json:"source_type"`
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
}

type participantReq struct {
	UserName string `json:"user_name"`
	UserID   string `json:"user_id"`
}

// CreateSummary handles POST /api/v1/summaries
func (h *TaskHandler) CreateSummary(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)

	var req createSummaryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: err.Error()})
		return
	}

	if req.ConfirmTimeoutHours <= 0 {
		req.ConfirmTimeoutHours = 24
	}

	effectiveUID := req.UID
	if effectiveUID == "" {
		effectiveUID = userID
	}

	// Validate
	if utf8.RuneCountInString(req.Title) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "title 不能超过 1000 字符"})
		return
	}
	if utf8.RuneCountInString(req.Topic) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "topic 不能超过 1000 字符"})
		return
	}
	if req.OriginChannelID != "" && (req.OriginChannelType < model.OriginChannelGroup || req.OriginChannelType > model.OriginChannelDM) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "origin_channel_type must be 1, 2, or 3 when origin_channel_id is set"})
		return
	}
	if req.OriginChannelID == "" && req.OriginChannelType != 0 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "origin_channel_id is required when origin_channel_type is set"})
		return
	}
	if len(req.Sources) == 0 && req.OriginChannelID != "" && req.OriginChannelType >= model.OriginChannelGroup && req.OriginChannelType <= model.OriginChannelDM {
		req.Sources = []sourceReq{{
			SourceType: req.OriginChannelType,
			SourceID:   req.OriginChannelID,
		}}
	}
	if len(req.Sources) == 0 && req.Topic == "" && req.TimeRange == nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "至少提供 sources、topic 或 time_range 之一"})
		return
	}

	summaryMode := model.ModeByPerson

	// Resolve time range
	var timeStart, timeEnd time.Time
	if req.TimeRange != nil {
		timeStart = req.TimeRange.Start
		timeEnd = req.TimeRange.End
	} else {
		timeEnd = timezone.Now()
		timeStart = timeEnd.Add(-31 * 24 * time.Hour)
	}

	if timeEnd.Sub(timeStart) > 31*24*time.Hour {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40002, Message: "时间范围不能超过31天"})
		return
	}

	// Resolve sources: use user-specified sources directly.
	// When no sources are specified, the pipeline Layer 3 (NarrowByTopic)
	// will use LLM to select relevant channels from all user channels.
	var sourceList []sourceReq
	if len(req.Sources) > 0 {
		sourceList = req.Sources
	}

	if len(sourceList) > maxSourceCount {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40003, Message: fmt.Sprintf("信息来源不能超过%d个", maxSourceCount)})
		return
	}

	if len(req.Participants) == 0 {
		req.Participants = []participantReq{{UserID: effectiveUID}}
	}

	taskNo := service.GenerateTaskNo()
	title := req.Title
	if title == "" {
		title = req.Topic
	}
	if title == "" {
		title = "总结-" + taskNo[len(taskNo)-8:]
	}

	initialStatus := model.StatusPending
	dl := timezone.Now().Add(time.Duration(req.ConfirmTimeoutHours) * time.Hour)
	confirmDeadline := &dl

	task := model.SummaryTask{
		TaskNo:            taskNo,
		SpaceID:           spaceID,
		CreatorID:         effectiveUID,
		Title:             title,
		SummaryMode:       summaryMode,
		TimeRangeStart:    timeStart,
		TimeRangeEnd:      timeEnd,
		Status:            initialStatus,
		TriggerType:       model.TriggerManual,
		ConfirmDeadline:   confirmDeadline,
		OriginChannelID:   req.OriginChannelID,
		OriginChannelType: req.OriginChannelType,
	}

	log.Printf("[handler] CreateSummary space=%s user=%s mode=%d", spaceID, effectiveUID, summaryMode)

	var creatorParticipantID int64
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		for _, s := range sourceList {
			src := model.SummarySource{
				TaskID:     task.ID,
				SourceType: s.SourceType,
				SourceID:   s.SourceID,
				SourceName: service.ResolveSourceNameWithType(s.SourceID, s.SourceType, h.imDB),
			}
			if err := tx.Create(&src).Error; err != nil {
				return err
			}
		}
		now := timezone.Now()
		creatorP := model.SummaryParticipant{
			TaskID:      task.ID,
			UserID:      effectiveUID,
			UserName:    service.ResolveUserName(effectiveUID),
			Status:      model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		if err := tx.Create(&creatorP).Error; err != nil {
			return err
		}

		creatorPR := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: creatorP.ID,
			UserID:           effectiveUID,
			WorkerStatus:     model.PersonalStatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := tx.Create(&creatorPR).Error; err != nil {
			return err
		}
		if err := tx.Model(&creatorP).Update("personal_result_id", creatorPR.ID).Error; err != nil {
			return err
		}
		creatorParticipantID = creatorP.ID

		seenParticipant := map[string]struct{}{effectiveUID: {}}
		for _, p := range req.Participants {
			if p.UserID == effectiveUID {
				continue
			}
			// De-duplicate repeated participant ids up front: the
			// (task_id,user_id) unique index would otherwise turn a duplicate
			// payload into a 1062 -> 500 instead of a clean insert.
			if _, dup := seenParticipant[p.UserID]; dup {
				continue
			}
			seenParticipant[p.UserID] = struct{}{}
			pp := model.SummaryParticipant{
				TaskID: task.ID,
				UserID: p.UserID,
				UserName: func() string {
					if p.UserName != "" {
						return p.UserName
					}
					return service.ResolveUserName(p.UserID)
				}(),
			}
			if err := tx.Create(&pp).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("[handler] CreateSummary tx error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: err.Error()})
		return
	}

	// Trigger personal worker for creator (async, after tx committed)
	if creatorParticipantID > 0 {
		go h.triggerWorker(model.WorkerTriggerRequest{
			Type:             "personal_summary",
			TaskID:           task.ID,
			ParticipantRefID: creatorParticipantID,
		})
	}

	result := gin.H{
		"task_id":    task.ID,
		"task_no":    task.TaskNo,
		"status":     task.Status,
		"created_at": task.CreatedAt.Format(time.RFC3339),
	}
	if len(req.Sources) == 0 {
		result["inferred"] = true
	}
	ok(c, result)
}

// ListSummaries handles GET /api/v1/summaries
func (h *TaskHandler) ListSummaries(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	// fail-closed hard gate: GET requests are NOT caught by StrictSpaceMiddleware,
	// and SummaryTask.SpaceID is `not null default ''`, so rows with space_id='' may
	// exist; querying `space_id=''` would MATCH them, leaking cross-space tasks.
	// Reject an empty X-Space-Id before any query.
	if spaceID == "" {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	userID := middleware.GetUserID(c)

	// Build the shared filter fragment (applied identically to the folded COUNT and
	// the folded page query). All business filters live INSIDE the window subquery
	// so "latest per schedule" is computed over the already-filtered set.
	whereSQL := "space_id = ? AND deleted_at IS NULL AND (creator_id = ? OR id IN (SELECT task_id FROM summary_participant WHERE user_id = ?))"
	args := []interface{}{spaceID, userID, userID}

	if s := c.Query("status"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			whereSQL += " AND status = ?"
			args = append(args, v)
		}
	}
	if s := c.Query("trigger_type"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			whereSQL += " AND trigger_type = ?"
			args = append(args, v)
		}
	}
	if s := c.Query("keyword"); s != "" {
		whereSQL += " AND title LIKE ?"
		args = append(args, "%"+s+"%")
	}
	if s := c.Query("origin_channel_id"); s != "" {
		whereSQL += " AND origin_channel_id = ?"
		args = append(args, s)
	}
	if s := c.Query("created_after"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			whereSQL += " AND created_at >= ?"
			args = append(args, t)
		}
	}
	if s := c.Query("created_before"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			whereSQL += " AND created_at <= ?"
			args = append(args, t)
		}
	}

	sortBy := c.DefaultQuery("sort_by", "created_at")
	sortOrder := c.DefaultQuery("sort_order", "desc")
	if sortBy != "created_at" && sortBy != "updated_at" {
		sortBy = "created_at"
	}
	if strings.ToLower(sortOrder) != "asc" {
		sortOrder = "desc"
	}
	orderClause := sortBy + " " + sortOrder

	// Folding (1->N): scheduled tasks fold by schedule_id (only the latest run per
	// schedule shows); manual tasks (schedule_id IS NULL) each form their own group.
	// The grouping key MUST namespace the two id spaces apart: summary_schedule.id and
	// summary_task.id are independent auto-increment sequences with overlapping value
	// ranges, so a bare COALESCE(schedule_id, id) collides a manual task id=N with a
	// scheduled group schedule_id=N and wrongly folds them together (dropping rows).
	// Prefix the key by type ('t' for manual task id, 's' for schedule id) to keep the
	// namespaces fully separate. "Latest per group" uses id DESC (monotonic -> stable,
	// unlike created_at which can tie at the same second). Pagination + total are
	// computed AFTER folding (rn = 1).
	innerSQL := "SELECT *, ROW_NUMBER() OVER (PARTITION BY (CASE WHEN schedule_id IS NULL THEN CONCAT('t', id) ELSE CONCAT('s', schedule_id) END) ORDER BY id DESC) AS rn FROM summary_task WHERE " + whereSQL

	var total int64
	countSQL := "SELECT COUNT(*) FROM (" + innerSQL + ") sub WHERE sub.rn = 1"
	h.db.Raw(countSQL, args...).Scan(&total)

	pageSQL := "SELECT * FROM (" + innerSQL + ") sub WHERE sub.rn = 1 ORDER BY " + orderClause + " LIMIT ? OFFSET ?"
	pageArgs := append(append([]interface{}{}, args...), pageSize, (page-1)*pageSize)

	var tasks []model.SummaryTask
	h.db.Raw(pageSQL, pageArgs...).Scan(&tasks)

	// FIX2: batch-load participants for ALL listed tasks in ONE query (avoid N+1),
	// grouped by task_id. The frontend SummaryCard needs task.participants to decide
	// whether the current user is a participant (to show the "退出" button) and to
	// surface the "接受/拒绝" buttons for pending invitees; without it isParticipant
	// is always false. Shape mirrors GetSummary's partList.
	partsByTask := make(map[int64][]gin.H)
	if len(tasks) > 0 {
		taskIDs := make([]int64, 0, len(tasks))
		for _, t := range tasks {
			taskIDs = append(taskIDs, t.ID)
		}
		var allParts []model.SummaryParticipant
		h.db.Where("task_id IN ?", taskIDs).Find(&allParts)
		for _, p := range allParts {
			item := gin.H{
				"user_id":   p.UserID,
				"user_name": p.UserName,
				"status":    p.Status,
			}
			if p.ConfirmedAt != nil {
				item["confirmed_at"] = p.ConfirmedAt.Format(time.RFC3339)
			}
			partsByTask[p.TaskID] = append(partsByTask[p.TaskID], item)
		}
	}

	items := make([]gin.H, 0, len(tasks))
	for _, t := range tasks {
		var sources []model.SummarySource
		h.db.Where("task_id = ?", t.ID).Find(&sources)

		srcList := make([]gin.H, 0, len(sources))
		for _, s := range sources {
			srcList = append(srcList, gin.H{
				"source_type": s.SourceType,
				"source_id":   s.SourceID,
				"source_name": s.SourceName,
			})
		}

		latestResult, hasResult := h.pickDisplayResult(t.ID)

		totalMsgCount := 0
		var completedAt *string
		var resultEditedAt *string
		resultIsEdited := false
		if hasResult {
			totalMsgCount = latestResult.TotalMsgCount
			s := latestResult.GeneratedAt.Format(time.RFC3339)
			completedAt = &s
			if latestResult.EditedAt != nil {
				editedAt := latestResult.EditedAt.Format(time.RFC3339)
				resultEditedAt = &editedAt
				resultIsEdited = true
			}
		}

		creatorName := ""
		var creatorParticipant model.SummaryParticipant
		if err := h.db.Where("task_id = ? AND user_id = ?", t.ID, t.CreatorID).First(&creatorParticipant).Error; err == nil {
			creatorName = creatorParticipant.UserName
		}
		if creatorName == "" {
			creatorName = service.ResolveUserName(t.CreatorID)
		}

		// Expose schedule_id on list items so the frontend can detect a scheduled
		// task by its bound schedule (the authoritative signal) instead of relying
		// on trigger_type. A manual task that later gets a scheduled-update added
		// keeps trigger_type=MANUAL until the scheduler actually runs, so
		// trigger_type alone misses "scheduled but not yet executed" tasks.
		var scheduleIDOut interface{}
		if t.ScheduleID != nil {
			scheduleIDOut = *t.ScheduleID
		}

		parts := partsByTask[t.ID]
		if parts == nil {
			parts = []gin.H{}
		}

		items = append(items, gin.H{
			"task_id":             t.ID,
			"task_no":             t.TaskNo,
			"title":               t.Title,
			"summary_mode":        t.SummaryMode,
			"status":              t.Status,
			"trigger_type":        t.TriggerType,
			"schedule_id":         scheduleIDOut,
			"creator_id":          t.CreatorID,
			"participants":        parts,
			"time_range_start":    t.TimeRangeStart.Format(time.RFC3339),
			"time_range_end":      t.TimeRangeEnd.Format(time.RFC3339),
			"sources":             srcList,
			"total_msg_count":     totalMsgCount,
			"creator_name":        creatorName,
			"origin_channel_id":   t.OriginChannelID,
			"origin_channel_type": t.OriginChannelType,
			"created_at":          t.CreatedAt.Format(time.RFC3339),
			"completed_at":        completedAt,
			"result_is_edited":    resultIsEdited,
			"result_edited_at":    resultEditedAt,
		})
	}

	ok(c, gin.H{"total": total, "items": items})
}

// GetSummary handles GET /api/v1/summaries/:id
func (h *TaskHandler) GetSummary(c *gin.Context) {
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}

	taskPtr, authorized := h.authorizeTaskAccess(c, taskID)
	if !authorized {
		return
	}
	task := *taskPtr

	var sources []model.SummarySource
	h.db.Where("task_id = ?", taskID).Find(&sources)

	srcList := make([]gin.H, 0, len(sources))
	for _, s := range sources {
		srcList = append(srcList, gin.H{
			"source_type": s.SourceType,
			"source_id":   s.SourceID,
			"source_name": s.SourceName,
		})
	}

	var participants []model.SummaryParticipant
	h.db.Where("task_id = ?", taskID).Find(&participants)

	partList := make([]gin.H, 0, len(participants))
	for _, p := range participants {
		item := gin.H{
			"user_id":   p.UserID,
			"user_name": p.UserName,
			"status":    p.Status,
		}
		if p.ConfirmedAt != nil {
			item["confirmed_at"] = p.ConfirmedAt.Format(time.RFC3339)
		}
		partList = append(partList, item)
	}

	latestResult, hasResult := h.pickDisplayResult(taskID)

	var resultOut interface{}
	if hasResult {
		// B2 (privacy): only a TRUE multi-person BY_PERSON team summary has plain
		// citations that contain OTHER members' raw chat messages, so those must not
		// be shipped to any participant. A single-person task's plain citations only
		// reference the caller's own messages -> keep them so [n] source links work.
		// Multi-person judged by participantCount>1 (same rule as edit.go:107 etc).
		// participants is already loaded above (task.go:579); do NOT re-query.
		plainCitations := latestResult.GetCitations()
		if len(participants) > 1 {
			plainCitations = []model.Citation{}
		}
		resultOut = gin.H{
			"content":          latestResult.Content,
			"citations":        plainCitations,
			"team_citations":   latestResult.GetTeamCitations(),
			"total_msg_count":  latestResult.TotalMsgCount,
			"total_token_used": latestResult.TotalTokenUsed,
			"model_version":    latestResult.ModelVersion,
			"version":          latestResult.Version,
			"generated_at":     latestResult.GeneratedAt.Format(time.RFC3339),
		}
	}

	resp := gin.H{
		"task_id":             task.ID,
		"task_no":             task.TaskNo,
		"title":               task.Title,
		"summary_mode":        task.SummaryMode,
		"status":              task.Status,
		"creator_id":          task.CreatorID,
		"trigger_type":        task.TriggerType,
		"time_range_start":    task.TimeRangeStart.Format(time.RFC3339),
		"time_range_end":      task.TimeRangeEnd.Format(time.RFC3339),
		"sources":             srcList,
		"participants":        partList,
		"result":              resultOut,
		"error_message":       task.ErrorMessage,
		"origin_channel_id":   task.OriginChannelID,
		"origin_channel_type": task.OriginChannelType,
		"created_at":          task.CreatedAt.Format(time.RFC3339),
		"updated_at":          task.UpdatedAt.Format(time.RFC3339),
	}

	// Plan C (P0 protocol fix): expose the task's associated schedule_id so the
	// detail page can correctly distinguish "edit existing schedule" vs "create
	// new schedule". Previously this field was missing, so detail.schedule_id was
	// always empty on the frontend and the update branch never fired.
	if task.ScheduleID != nil {
		var sched model.SummarySchedule
		if err := h.db.Where("id = ? AND deleted_at IS NULL", *task.ScheduleID).First(&sched).Error; err == nil {
			resp["schedule_id"] = *task.ScheduleID
			resp["schedule_is_active"] = sched.IsActive
		} else {
			resp["schedule_id"] = nil
			resp["schedule_is_active"] = nil
		}
	} else {
		resp["schedule_id"] = nil
		resp["schedule_is_active"] = nil
	}

	if hasResult {
		resp["result_id"] = latestResult.ID
		if latestResult.EditedAt != nil {
			resp["result_edited_at"] = latestResult.EditedAt.Format(time.RFC3339)
			resp["result_is_edited"] = true
		} else {
			resp["result_edited_at"] = nil
			resp["result_is_edited"] = false
		}
	} else {
		resp["result_id"] = nil
		resp["result_edited_at"] = nil
		resp["result_is_edited"] = false
	}

	// Add personal_result and members info
	userID := middleware.GetUserID(c)

	// B1 (permission split):
	//   can_edit: legacy field, kept for backward-compat with the current frontend.
	//     Same old semantics (creator + completed + single-participant). The
	//     frontend is migrating to can_edit_team; do NOT change this value.
	//   can_edit_team (need4): team SummaryResult edit button. Creator only, with
	//     the <=1 participant gate REMOVED -- a multi-person creator can now edit the
	//     team draft (edit.go relaxed the multi-person rejection).
	//   can_edit_personal (need3): caller may edit their OWN personal report. True
	//     iff the caller is a participant of this task.
	//   can_view_schedule (need2): schedule read-only info is visible to ANY
	//     participant (creator included). Distinct from can_schedule which gates the
	//     schedule *settings* button.
	//   can_add_member (need7): "add member" entry, creator only.
	//   can_schedule (kept): schedule settings button, creator only.
	isParticipant := false
	for _, p := range participants {
		if p.UserID == userID {
			isParticipant = true
			break
		}
	}
	isCreator := task.CreatorID == userID
	canEdit := isCreator && task.Status == model.StatusCompleted && len(participants) <= 1
	canSchedule := isCreator
	resp["permissions"] = gin.H{
		"can_edit":          canEdit,
		"can_schedule":      canSchedule,
		"can_edit_team":     isCreator && task.Status == model.StatusCompleted,
		"can_edit_personal": isParticipant,
		"can_view_schedule": isParticipant,
		"can_add_member":    isCreator,
		"can_remove_member": isCreator,
	}

	var pr model.PersonalResult
	personalOut := gin.H{
		"worker_status": 0,
		"content":       "",
		"submitted_at":  nil,
	}
	if userID != "" {
		if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err == nil {
			personalOut["worker_status"] = pr.WorkerStatus
			personalOut["content"] = pr.Content
			if pr.SubmittedAt != nil {
				personalOut["submitted_at"] = pr.SubmittedAt.Format(time.RFC3339)
			}
		}
	}
	resp["personal_result"] = personalOut

	members := make([]gin.H, 0, len(participants))
	prMap := make(map[int64]*model.PersonalResult)
	var prs []model.PersonalResult
	h.db.Where("task_id = ?", taskID).Find(&prs)
	for i := range prs {
		prMap[prs[i].ParticipantRefID] = &prs[i]
	}
	for _, p := range participants {
		member := gin.H{
			"user_id":      p.UserID,
			"user_name":    p.UserName,
			"status":       model.ParticipantStatusLabel(p.Status),
			"submitted_at": nil,
			"content":      "",
		}
		if pr, exists := prMap[p.ID]; exists {
			if pr.SubmittedAt != nil {
				member["submitted_at"] = pr.SubmittedAt.Format(time.RFC3339)
				member["content"] = pr.Content
			}
		}
		members = append(members, member)
	}
	resp["members"] = members

	if resultOut != nil {
		if resultMap, ok := resultOut.(gin.H); ok {
			var submittedCount int64
			h.db.Model(&model.PersonalResult{}).Where("task_id = ? AND submitted_at IS NOT NULL", taskID).Count(&submittedCount)
			resultMap["submitted_count"] = submittedCount
			resp["result"] = resultMap
		}
	}

	ok(c, resp)
}

// GetResult handles GET /api/v1/summaries/:id/result
func (h *TaskHandler) GetResult(c *gin.Context) {
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}

	_, authorized := h.authorizeTaskAccess(c, taskID)
	if !authorized {
		return
	}

	var result model.SummaryResult
	var found bool
	if result, found = h.pickDisplayResult(taskID); !found {
		bizErr(c, service.NewBizError(40008, "暂无结果", http.StatusNotFound))
		return
	}

	// B2 (privacy): only a TRUE multi-person team result leaks OTHER members' raw
	// chat citations; a single-person task's plain citations reference only the
	// caller's own messages -> keep them so [n] source links work. Multi-person is
	// participantCount>1 (same rule as edit.go:107 etc). No participant list is in
	// scope here, so query the count once via loadTaskParticipantCount.
	plainCitations := result.GetCitations()
	pc, err := loadTaskParticipantCount(h.db, taskID)
	if err != nil {
		// Conservative fail-closed (privacy over feature): on count error, strip.
		log.Printf("[task] GetResult participant count error task=%d: %v", taskID, err)
		plainCitations = []model.Citation{}
	} else if pc > 1 {
		plainCitations = []model.Citation{}
	}

	ok(c, gin.H{
		"content":          result.Content,
		"citations":        plainCitations,
		"team_citations":   result.GetTeamCitations(),
		"total_msg_count":  result.TotalMsgCount,
		"total_token_used": result.TotalTokenUsed,
		"model_version":    result.ModelVersion,
		"version":          result.Version,
		"generated_at":     result.GeneratedAt.Format(time.RFC3339),
		"result_is_edited": result.EditedAt != nil,
		"result_edited_at": func() interface{} {
			if result.EditedAt == nil {
				return nil
			}
			return result.EditedAt.Format(time.RFC3339)
		}(),
	})
}

// regenerateReq is the optional request body for Regenerate. When Topic is
// provided it replaces the task title; an empty/absent body keeps it unchanged.
type regenerateReq struct {
	Topic string `json:"topic"`
}

// Regenerate handles POST /api/v1/summaries/:id/regenerate
func (h *TaskHandler) Regenerate(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}

	taskPtr, authorized := h.authorizeTaskAccess(c, taskID)
	if !authorized {
		return
	}
	task := *taskPtr

	if task.CreatorID != userID {
		bizErr(c, service.NewBizError(40004, "仅创建者可重新生成", http.StatusForbidden))
		return
	}
	if task.Status != model.StatusCompleted && task.Status != model.StatusFailed && task.Status != model.StatusCancelled {
		bizErr(c, service.NewBizError(40005, "任务状态不允许此操作", http.StatusConflict))
		return
	}

	// Optionally accept a new topic to update the task title. The body is
	// optional: an empty body or a body without a topic field keeps the
	// existing title unchanged (backward compatible).
	var req regenerateReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	topic := strings.TrimSpace(req.Topic)
	if utf8.RuneCountInString(topic) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "topic 不能超过 1000 字符"})
		return
	}
	newTitle := task.Title
	if topic != "" && topic != task.Title {
		newTitle = topic
	}

	nextVer, _ := service.GetNextVersion(h.db, taskID)

	err = h.db.Transaction(func(tx *gorm.DB) error {
		// Atomic status transition: only proceed if the task is still in a
		// terminal state. This prevents concurrent regenerate requests from
		// both passing the pre-check and duplicating work. Also resets
		// processing_deadline so the scheduler's stale-task sweep does not
		// immediately re-trip the freshly re-queued task.
		res := tx.Model(&model.SummaryTask{}).
			Where("id = ? AND status IN ?", taskID, []int{model.StatusCompleted, model.StatusFailed, model.StatusCancelled}).
			Updates(map[string]interface{}{
				"status":              model.StatusPending,
				"retry_count":         0,
				"error_message":       nil,
				"processing_deadline": nil,
				"title":               newTitle,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return service.NewBizError(40005, "任务已在处理中，请稍后再试", http.StatusConflict)
		}

		if err := tx.Where("task_id = ?", taskID).Delete(&model.SummaryResult{}).Error; err != nil {
			return err
		}
		if err := tx.Where("task_id = ?", taskID).Delete(&model.SummaryChunk{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Updates(map[string]interface{}{
			"status":             model.ParticipantPending,
			"worker_started_at":  nil,
			"confirmed_at":       nil,
			"personal_result_id": nil,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.PersonalResult{}).Where("task_id = ?", taskID).Updates(map[string]interface{}{
			"worker_status":  model.PersonalStatusPending,
			"content":        "",
			"citations_json": "",
			"error_message":  nil,
			"submitted_at":   nil,
			"generated_at":   nil,
			"edited_at":      nil,
		}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		var be *service.BizError
		if errors.As(err, &be) {
			bizErr(c, be)
			return
		}
		log.Printf("[handler] Regenerate tx error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: err.Error()})
		return
	}

	var creatorParticipant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, task.CreatorID).First(&creatorParticipant).Error; err == nil {
		go h.triggerWorker(model.WorkerTriggerRequest{
			Type:             "personal_summary",
			TaskID:           taskID,
			ParticipantRefID: creatorParticipant.ID,
		})
	}

	ok(c, gin.H{
		"task_id":     task.ID,
		"status":      model.StatusPending,
		"new_version": nextVer,
		"title":       newTitle,
	})
}

// InferScope handles GET /api/v1/summary-infer
func (h *TaskHandler) InferScope(c *gin.Context) {
	topic := c.Query("topic")
	if topic == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "topic is required"})
		return
	}
	result := service.InferScope(topic)
	ok(c, result)
}

// GetTemplates handles GET /api/v1/summary-templates
func (h *TaskHandler) GetTemplates(c *gin.Context) {
	type placeholder struct {
		Key      string `json:"key"`
		Label    string `json:"label"`
		Position []int  `json:"position,omitempty"`
	}
	type tmpl struct {
		ID           string        `json:"id"`
		Label        string        `json:"label"`
		Icon         string        `json:"icon"`
		Description  string        `json:"description"`
		Type         string        `json:"type"`
		Pattern      string        `json:"pattern"`
		Placeholders []placeholder `json:"placeholders,omitempty"`
	}

	templates := []tmpl{
		{
			ID:           "project_progress",
			Label:        "汇总项目进展",
			Icon:         "FileText",
			Description:  "与团队成员一起总结进展",
			Type:         "parameterized",
			Pattern:      "总结 {project_name} 的项目进展",
			Placeholders: []placeholder{{Key: "project_name", Label: "输入项目名称", Position: []int{3, 9}}},
		},
		{
			ID:           "task_tracking",
			Label:        "跟踪任务进度",
			Icon:         "ListChecks",
			Description:  "邀请同事汇总任务完成情况",
			Type:         "parameterized",
			Pattern:      "总结 {task_name} 的完成情况",
			Placeholders: []placeholder{{Key: "task_name", Label: "输入任务名称", Position: []int{3, 9}}},
		},
		{
			ID:          "weekly_report",
			Label:       "总结团队周报",
			Icon:        "Calendar",
			Description: "总结团队成员每周的工作",
			Type:        "fixed",
			Pattern:     "总结每周的工作周报",
		},
		{
			ID:          "chat_content",
			Label:       "总结聊天内容",
			Icon:        "MessageSquare",
			Description: "总结指定聊天中的事情进展",
			Type:        "fixed",
			Pattern:     "总结本群中的关键内容",
		},
	}

	ok(c, gin.H{"templates": templates})
}

func (h *TaskHandler) triggerWorker(req model.WorkerTriggerRequest) {
	if h.workerTriggerURL == "" {
		return
	}
	body, err := json.Marshal(req)
	if err != nil {
		log.Printf("[task] marshal trigger: %v", err)
		return
	}
	resp, err := triggerClient.Post(h.workerTriggerURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[task] trigger worker POST failed: %v", err)
		return
	}
	resp.Body.Close()
}

// DeleteSummary handles DELETE /api/v1/summaries/:id
func (h *TaskHandler) DeleteSummary(c *gin.Context) {
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}

	task, authorized := h.authorizeTaskAccess(c, taskID)
	if !authorized {
		return
	}

	// A4: tighten non-creator delete. authorizeTaskAccess lets both the creator and
	// participants through (read/cancel access). But a mere participant must NOT be
	// able to soft-delete the WHOLE multi-person task -- they can only LEAVE it
	// (POST /summaries/:id/leave). Only the creator may delete the task. For a
	// single-person task creator==caller, so this is a strict no-op there. The
	// scheduled-group branch below still applies its own schedule-creator rule for the creator.
	if middleware.GetUserID(c) != task.CreatorID {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40006, Message: "非创建者请使用退出多人协作"})
		return
	}

	now := timezone.Now()
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		peekedScheduleID, err := peekTaskScheduleID(tx, task.SpaceID, middleware.GetUserID(c), task.ID)
		if err != nil {
			return err
		}

		var lockedSched *model.SummarySchedule
		if peekedScheduleID != nil {
			var sched model.SummarySchedule
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND deleted_at IS NULL", *peekedScheduleID).
				First(&sched).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
			} else {
				lockedSched = &sched
			}
		}

		var liveTask model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND deleted_at IS NULL", task.ID).
			First(&liveTask).Error; err != nil {
			return err
		}

		if !int64PtrEqual(liveTask.ScheduleID, peekedScheduleID) {
			return errRebindConcurrentModified
		}

		if liveTask.ScheduleID != nil {
			// 1->N group delete (Boss product decision): deleting a SCHEDULED summary
			// row from the list = soft-delete the WHOLE group (all tasks under that
			// schedule) + stop the schedule. Only the schedule creator may do this
			// (same ownership rule as DeleteSchedule); a mere participant must not take
			// down another user's whole schedule, so a non-creator only deletes their
			// own single task (unbind + pause the schedule).
			userID := middleware.GetUserID(c)
			if lockedSched == nil {
				// schedule already gone; fall through to single-task soft-delete below.
			} else if lockedSched.CreatorID == userID {
				// Stop the schedule.
				if err := tx.Model(&model.SummarySchedule{}).
					Where("id = ? AND deleted_at IS NULL", lockedSched.ID).
					Update("deleted_at", &now).Error; err != nil {
					return err
				}
				// Soft-delete EVERY live task in the group in one batch UPDATE (never
				// loop per-row; a long-lived schedule may own thousands of tasks).
				// schedule_id is preserved (no unbind) so deleted history stays
				// attributable; subtables are left intact (no soft-delete column).
				if err := tx.Model(&model.SummaryTask{}).
					Where("schedule_id = ? AND deleted_at IS NULL", lockedSched.ID).
					Updates(map[string]interface{}{
						"status":     -1,
						"deleted_at": now,
					}).Error; err != nil {
					return err
				}
				// The whole group (including liveTask) is now soft-deleted; done.
				return nil
			} else {
				// Not the schedule creator (a mere participant): forbid the delete
				// entirely (Option A). Previously this branch unbound the task and
				// paused (is_active=0) someone else's schedule, which let a participant
				// silently disable the creator's scheduled summary -- a privilege
				// escalation. Return 403 so the tx rolls back and nothing is touched.
				log.Printf("[task] DeleteSummary: task %d caller %s is not schedule %d creator; denying (403)", liveTask.ID, userID, lockedSched.ID)
				return service.NewBizError(40006, "仅创建者可删除该定时总结", http.StatusForbidden)
			}
		}

		return tx.Model(&liveTask).Updates(map[string]interface{}{
			"status":     -1,
			"deleted_at": now,
		}).Error
	}); err != nil {
		var be *service.BizError
		if errors.As(err, &be) {
			bizErr(c, be)
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
			return
		}
		if isScheduleRetryableConflict(err) {
			writeRetryableRebindConflict(c)
			return
		}
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok"})
}

// CancelSummary handles POST /api/v1/summaries/:id/cancel
func (h *TaskHandler) CancelSummary(c *gin.Context) {
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}

	task, authorized := h.authorizeTaskAccess(c, taskID)
	if !authorized {
		return
	}

	result := h.db.Model(&model.SummaryTask{}).
		Where("id = ? AND status IN (?, ?, ?)", task.ID,
			model.StatusPending, model.StatusWaitingConfirm, model.StatusProcessing).
		Updates(map[string]interface{}{
			"status":        model.StatusCancelled,
			"error_message": "用户取消",
		})
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: result.Error.Error()})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "任务已结束，无法取消"})
		return
	}

	// TODO(#11): send WS callback (TaskEvent with StatusCancelled) to notify frontend in real-time
	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok"})
}

type batchStatusReq struct {
	TaskIDs []int64 `json:"task_ids" binding:"required"`
}

type batchStatusItem struct {
	ID        int64  `json:"id"`
	Status    int    `json:"status"`
	Progress  int    `json:"progress"`
	UpdatedAt string `json:"updated_at"`
}

// BatchStatus handles POST /api/v1/summaries/batch-status
func (h *TaskHandler) BatchStatus(c *gin.Context) {
	userID := middleware.GetUserID(c)
	spaceID := middleware.GetSpaceID(c)

	var req batchStatusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}

	if len(req.TaskIDs) == 0 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40050, Message: "task_ids must not be empty"})
		return
	}
	if len(req.TaskIDs) > 50 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40051, Message: "task_ids exceeds maximum of 50"})
		return
	}

	seen := make(map[int64]struct{}, len(req.TaskIDs))
	uniqueIDs := make([]int64, 0, len(req.TaskIDs))
	for _, id := range req.TaskIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			uniqueIDs = append(uniqueIDs, id)
		}
	}
	if len(uniqueIDs) == 0 {
		ok(c, gin.H{"tasks": []batchStatusItem{}})
		return
	}

	var tasks []model.SummaryTask
	if err := h.db.Where("id IN ? AND space_id = ? AND deleted_at IS NULL", uniqueIDs, spaceID).
		Find(&tasks).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	if len(tasks) == 0 {
		ok(c, gin.H{"tasks": []batchStatusItem{}})
		return
	}

	taskIDs := make([]int64, 0, len(tasks))
	taskMap := make(map[int64]*model.SummaryTask, len(tasks))
	for i := range tasks {
		taskIDs = append(taskIDs, tasks[i].ID)
		taskMap[tasks[i].ID] = &tasks[i]
	}

	authorizedIDs := h.batchAuthorize(userID, taskIDs, taskMap)

	progressMap := h.fetchLatestProgress(authorizedIDs)

	items := make([]batchStatusItem, 0, len(authorizedIDs))
	for _, id := range authorizedIDs {
		t := taskMap[id]
		progress := 0
		if p, exists := progressMap[id]; exists {
			progress = min(max(p, 0), 100)
		}
		items = append(items, batchStatusItem{
			ID:        t.ID,
			Status:    t.Status,
			Progress:  progress,
			UpdatedAt: t.UpdatedAt.Format(time.RFC3339),
		})
	}

	ok(c, gin.H{"tasks": items})
}

// batchAuthorize returns the subset of taskIDs that userID is allowed to access.
// Access is granted to task creators and explicit participants only.
// Source-group membership does NOT grant access. This must stay semantically
// equal to canAccessTask / authorizeTaskAccess; the batch Pluck form is only a
// performance optimization to avoid N per-task queries.
func (h *TaskHandler) batchAuthorize(userID string, taskIDs []int64, taskMap map[int64]*model.SummaryTask) []int64 {
	authorized := make(map[int64]struct{})

	remainingIDs := make([]int64, 0, len(taskIDs))
	for _, id := range taskIDs {
		if taskMap[id].CreatorID == userID {
			authorized[id] = struct{}{}
		} else {
			remainingIDs = append(remainingIDs, id)
		}
	}
	if len(remainingIDs) == 0 {
		return taskIDs
	}

	var participantTaskIDs []int64
	h.db.Model(&model.SummaryParticipant{}).
		Where("task_id IN ? AND user_id = ?", remainingIDs, userID).
		Distinct().Pluck("task_id", &participantTaskIDs)
	for _, id := range participantTaskIDs {
		authorized[id] = struct{}{}
	}

	return mapKeys(authorized)
}

func mapKeys(m map[int64]struct{}) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// fetchLatestProgress returns the most recent progress value for each task ID
// from the summary_event table.
func (h *TaskHandler) fetchLatestProgress(taskIDs []int64) map[int64]int {
	if len(taskIDs) == 0 {
		return nil
	}
	type row struct {
		TaskID   int64 `gorm:"column:task_id"`
		Progress int   `gorm:"column:progress"`
	}
	var rows []row
	h.db.Raw(`
		SELECT e.task_id, e.progress
		FROM summary_event e
		INNER JOIN (
			SELECT task_id, MAX(id) AS max_id
			FROM summary_event
			WHERE task_id IN ?
			GROUP BY task_id
		) latest ON e.id = latest.max_id
	`, taskIDs).Scan(&rows)

	m := make(map[int64]int, len(rows))
	for _, r := range rows {
		m[r.TaskID] = r.Progress
	}
	return m
}
