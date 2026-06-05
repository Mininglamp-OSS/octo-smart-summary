package handler

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

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

// authorizeTaskAccess loads a task by ID and checks that the current user is
// authorized to access it. Authorization passes if the user is the task creator,
// a participant, or a member of at least one source group.
// Returns the task and true on success; writes a JSON error response and returns
// nil, false on failure.
func (h *TaskHandler) authorizeTaskAccess(c *gin.Context, taskID int64) (*model.SummaryTask, bool) {
	userID := middleware.GetUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, apiResponse{Code: 4010, Message: "authentication required"})
		return nil, false
	}

	var task model.SummaryTask
	if err := h.db.Where("id = ? AND deleted_at IS NULL", taskID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return nil, false
	}

	// 1. Creator check
	if task.CreatorID == userID {
		return &task, true
	}

	// 2. Participant check
	var participantCount int64
	h.db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, userID).
		Count(&participantCount)
	if participantCount > 0 {
		return &task, true
	}

	// 3. Source group membership check (via imDB)
	var sourceIDs []string
	h.db.Model(&model.SummarySource{}).
		Where("task_id = ? AND source_type = ?", taskID, model.SourceGroup).
		Pluck("source_id", &sourceIDs)
	if len(sourceIDs) > 0 {
		var memberCount int64
		h.imDB.Table("group_member").
			Where("group_no IN ? AND uid = ? AND is_deleted = 0", sourceIDs, userID).
			Count(&memberCount)
		if memberCount > 0 {
			return &task, true
		}
	}

	c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "无权访问此任务"})
	return nil, false
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
	UID                 string       `json:"uid"`
	Title               string       `json:"title"`
	Topic               string       `json:"topic"`
	TimeRange           *timeRange   `json:"time_range"`
	Sources             []sourceReq  `json:"sources"`
	Participants        []participantReq `json:"participants"`
	ConfirmTimeoutHours int          `json:"confirm_timeout_hours"`
}

type timeRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type sourceReq struct {
	SourceType int    `json:"source_type"`
	SourceID   string `json:"source_id"`
}

type participantReq struct {
	UserName string `json:"user_name"`
	UserID string `json:"user_id"`
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

	if len(sourceList) > 10 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40003, Message: "信息来源不能超过10个"})
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
		TaskNo:          taskNo,
		SpaceID:         spaceID,
		CreatorID:       effectiveUID,
		Title:           title,
		SummaryMode:     summaryMode,
		TimeRangeStart:  timeStart,
		TimeRangeEnd:    timeEnd,
		Status:          initialStatus,
		TriggerType:     model.TriggerManual,
		ConfirmDeadline: confirmDeadline,
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

		for _, p := range req.Participants {
			if p.UserID == effectiveUID {
				continue
			}
			pp := model.SummaryParticipant{
				TaskID:   task.ID,
				UserID:   p.UserID,
				UserName: func() string { if p.UserName != "" { return p.UserName }; return service.ResolveUserName(p.UserID) }(),
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

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	userID := middleware.GetUserID(c)

	query := h.db.Model(&model.SummaryTask{}).
		Where("space_id = ? AND deleted_at IS NULL AND (creator_id = ? OR id IN (SELECT task_id FROM summary_participant WHERE user_id = ?))",
			spaceID, userID, userID)

	if s := c.Query("status"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			query = query.Where("status = ?", v)
		}
	}
	if s := c.Query("trigger_type"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			query = query.Where("trigger_type = ?", v)
		}
	}
	if s := c.Query("keyword"); s != "" {
		query = query.Where("title LIKE ?", "%"+s+"%")
	}
	if s := c.Query("created_after"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			query = query.Where("created_at >= ?", t)
		}
	}
	if s := c.Query("created_before"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			query = query.Where("created_at <= ?", t)
		}
	}

	var total int64
	query.Count(&total)

	sortBy := c.DefaultQuery("sort_by", "created_at")
	sortOrder := c.DefaultQuery("sort_order", "desc")
	orderClause := sortBy + " " + sortOrder
	if sortBy != "created_at" && sortBy != "updated_at" {
		orderClause = "created_at desc"
	}

	var tasks []model.SummaryTask
	query.Order(orderClause).Offset((page - 1) * pageSize).Limit(pageSize).Find(&tasks)

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

		var latestResult model.SummaryResult
		h.db.Where("task_id = ?", t.ID).Order("version DESC").Limit(1).Find(&latestResult)

		totalMsgCount := 0
		var completedAt *string
		if latestResult.ID > 0 {
			totalMsgCount = latestResult.TotalMsgCount
			s := latestResult.GeneratedAt.Format(time.RFC3339)
			completedAt = &s
		}

		creatorName := ""
		var creatorParticipant model.SummaryParticipant
		if err := h.db.Where("task_id = ? AND user_id = ?", t.ID, t.CreatorID).First(&creatorParticipant).Error; err == nil {
			creatorName = creatorParticipant.UserName
		}
		if creatorName == "" {
			creatorName = service.ResolveUserName(t.CreatorID)
		}

		items = append(items, gin.H{
			"task_id":          t.ID,
			"task_no":          t.TaskNo,
			"title":            t.Title,
			"summary_mode":     t.SummaryMode,
			"status":           t.Status,
			"trigger_type":     t.TriggerType,
			"time_range_start": t.TimeRangeStart.Format(time.RFC3339),
			"time_range_end":   t.TimeRangeEnd.Format(time.RFC3339),
			"sources":          srcList,
			"total_msg_count":  totalMsgCount,
			"creator_name":     creatorName,
			"created_at":       t.CreatedAt.Format(time.RFC3339),
			"completed_at":     completedAt,
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

	var latestResult model.SummaryResult
	h.db.Where("task_id = ?", taskID).Order("version DESC").Limit(1).Find(&latestResult)

	var resultOut interface{}
	if latestResult.ID > 0 {
		resultOut = gin.H{
			"content":          latestResult.Content,
			"citations":        latestResult.GetCitations(),
			"total_msg_count":  latestResult.TotalMsgCount,
			"total_token_used": latestResult.TotalTokenUsed,
			"model_version":    latestResult.ModelVersion,
			"version":          latestResult.Version,
			"generated_at":     latestResult.GeneratedAt.Format(time.RFC3339),
		}
	}

	resp := gin.H{
		"task_id":          task.ID,
		"task_no":          task.TaskNo,
		"title":            task.Title,
		"summary_mode":     task.SummaryMode,
		"status":           task.Status,
		"trigger_type":     task.TriggerType,
		"time_range_start": task.TimeRangeStart.Format(time.RFC3339),
		"time_range_end":   task.TimeRangeEnd.Format(time.RFC3339),
		"sources":          srcList,
		"participants":     partList,
		"result":           resultOut,
		"error_message":    task.ErrorMessage,
		"created_at":       task.CreatedAt.Format(time.RFC3339),
		"updated_at":       task.UpdatedAt.Format(time.RFC3339),
	}

	// Plan C (P0 protocol fix): expose the task's associated schedule_id so the
	// detail page can correctly distinguish "edit existing schedule" vs "create
	// new schedule". Previously this field was missing, so detail.schedule_id was
	// always empty on the frontend and the update branch never fired.
	if task.ScheduleID != nil {
		resp["schedule_id"] = *task.ScheduleID
	} else {
		resp["schedule_id"] = nil
	}

	if latestResult.ID > 0 {
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

	canEdit := task.CreatorID == userID && task.Status == model.StatusCompleted && len(participants) <= 1
	resp["permissions"] = gin.H{"can_edit": canEdit}

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

	if _, authorized := h.authorizeTaskAccess(c, taskID); !authorized {
		return
	}

	var result model.SummaryResult
	if err := h.db.Where("task_id = ?", taskID).Order("version DESC").Limit(1).First(&result).Error; err != nil {
		bizErr(c, service.NewBizError(40008, "暂无结果", http.StatusNotFound))
		return
	}

	ok(c, gin.H{
		"content":          result.Content,
		"citations":        result.GetCitations(),
		"total_msg_count":  result.TotalMsgCount,
		"total_token_used": result.TotalTokenUsed,
		"model_version":    result.ModelVersion,
		"version":          result.Version,
		"generated_at":     result.GeneratedAt.Format(time.RFC3339),
	})
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

	nextVer, _ := service.GetNextVersion(h.db, taskID)

	err = h.db.Transaction(func(tx *gorm.DB) error {
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
		if err := tx.Model(&task).Updates(map[string]interface{}{
			"status":              model.StatusPending,
			"retry_count":         0,
			"error_message":       nil,
			"processing_deadline": nil,
		}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
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

	// Soft delete: set status = -1 AND deleted_at = NOW()
	if err := h.db.Model(task).Updates(map[string]interface{}{
		"status":     -1,
		"deleted_at": timezone.Now(),
	}).Error; err != nil {
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
// Known limitation: source_type=2 (thread) is not checked here, consistent with
// the existing authorizeTaskAccess implementation.
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

	stillRemaining := make([]int64, 0)
	for _, id := range remainingIDs {
		if _, ok := authorized[id]; !ok {
			stillRemaining = append(stillRemaining, id)
		}
	}
	if len(stillRemaining) == 0 {
		return mapKeys(authorized)
	}

	var sources []model.SummarySource
	h.db.Where("task_id IN ? AND source_type = ?", stillRemaining, model.SourceGroup).
		Find(&sources)

	if len(sources) > 0 {
		groupToTasks := make(map[string][]int64)
		for _, s := range sources {
			groupToTasks[s.SourceID] = append(groupToTasks[s.SourceID], s.TaskID)
		}
		groupNos := make([]string, 0, len(groupToTasks))
		for gno := range groupToTasks {
			groupNos = append(groupNos, gno)
		}

		var memberGroups []string
		h.imDB.Table("group_member").
			Where("group_no IN ? AND uid = ? AND is_deleted = 0", groupNos, userID).
			Pluck("group_no", &memberGroups)

		for _, gno := range memberGroups {
			for _, taskID := range groupToTasks[gno] {
				authorized[taskID] = struct{}{}
			}
		}
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
