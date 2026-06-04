package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ScheduleHandler handles schedule endpoints.
type ScheduleHandler struct {
	db *gorm.DB
}

// NewScheduleHandler creates a new ScheduleHandler.
func NewScheduleHandler(db *gorm.DB) *ScheduleHandler {
	return &ScheduleHandler{db: db}
}

type createScheduleReq struct {
	Title         string       `json:"title"`
	CronExpr      string       `json:"cron_expr"`
	IntervalDays  int          `json:"interval_days"`
	TimeRangeType int          `json:"time_range_type"`
	Sources       []sourceReq  `json:"sources"`
	Participants  []participantReq `json:"participants"`
}

type updateScheduleReq struct {
	Title         *string      `json:"title"`
	CronExpr      *string      `json:"cron_expr"`
	IntervalDays  *int         `json:"interval_days"`
	TimeRangeType *int         `json:"time_range_type"`
	Sources       []sourceReq  `json:"sources,omitempty"`
	Participants  []participantReq `json:"participants,omitempty"`
}

type toggleScheduleReq struct {
	IsActive bool `json:"is_active"`
}

// CreateSchedule handles POST /api/v1/summary-schedules
func (h *ScheduleHandler) CreateSchedule(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)

	var req createScheduleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: err.Error()})
		return
	}

	if utf8.RuneCountInString(req.Title) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "title 不能超过 1000 字符"})
		return
	}

	now := time.Now().UTC()
	if req.IntervalDays < 0 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: "interval_days 不能为负"})
		return
	}
	if req.IntervalDays == 0 && req.CronExpr == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40012, Message: "cron_expr 与 interval_days 至少提供一个"})
		return
	}
	nextRun, err := service.NextRunWithInterval(req.CronExpr, req.IntervalDays, now)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, apiResponse{Code: 40010, Message: "无效的 cron 表达式: " + req.CronExpr})
		return
	}

	summaryMode := model.ModeByPerson
	if req.TimeRangeType == 0 {
		req.TimeRangeType = 2
	}

	var sourceConfig model.JSON
	if len(req.Sources) > 0 {
		b, _ := json.Marshal(req.Sources)
		sourceConfig = b
	}

	var participantConfig model.JSON
	if len(req.Participants) > 0 {
		b, _ := json.Marshal(req.Participants)
		participantConfig = b
	}

	sched := model.SummarySchedule{
		SpaceID:           spaceID,
		CreatorID:         userID,
		Title:             req.Title,
		SummaryMode:       summaryMode,
		CronExpr:          req.CronExpr,
		IntervalDays:      req.IntervalDays,
		TimeRangeType:     req.TimeRangeType,
		SourceConfig:      sourceConfig,
		ParticipantConfig: participantConfig,
		NextRunAt:         &nextRun,
	}
	if err := h.db.Create(&sched).Error; err != nil {
		log.Printf("[handler] CreateSchedule error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: err.Error()})
		return
	}

	ok(c, gin.H{
		"schedule_id": sched.ID,
		"next_run_at": nextRun.Format(time.RFC3339),
	})
}

// ListSchedules handles GET /api/v1/summary-schedules
func (h *ScheduleHandler) ListSchedules(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)

	var schedules []model.SummarySchedule
	h.db.Where("space_id = ? AND deleted_at IS NULL", spaceID).
		Order("created_at DESC").
		Find(&schedules)

	items := make([]gin.H, 0, len(schedules))
	for _, s := range schedules {
		item := gin.H{
			"schedule_id":        s.ID,
			"title":              s.Title,
			"summary_mode":       s.SummaryMode,
			"cron_expr":          s.CronExpr,
			"interval_days":      s.IntervalDays,
			"time_range_type":    s.TimeRangeType,
			"source_config":      s.SourceConfig,
			"participant_config": s.ParticipantConfig,
			"is_active":          s.IsActive,
			"created_at":         s.CreatedAt.Format(time.RFC3339),
		}
		if s.LastRunAt != nil {
			item["last_run_at"] = s.LastRunAt.Format(time.RFC3339)
		}
		if s.NextRunAt != nil {
			item["next_run_at"] = s.NextRunAt.Format(time.RFC3339)
		}
		items = append(items, item)
	}

	ok(c, items)
}

// GetSchedule handles GET /api/v1/summary-schedules/:id
func (h *ScheduleHandler) GetSchedule(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	schedID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid schedule id"})
		return
	}

	var sched model.SummarySchedule
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", schedID, spaceID).First(&sched).Error; err != nil {
		bizErr(c, service.NewBizError(40008, "定时配置不存在", http.StatusNotFound))
		return
	}

	item := gin.H{
		"schedule_id":        sched.ID,
		"title":              sched.Title,
		"summary_mode":       sched.SummaryMode,
		"cron_expr":          sched.CronExpr,
		"interval_days":      sched.IntervalDays,
		"time_range_type":    sched.TimeRangeType,
		"source_config":      sched.SourceConfig,
		"participant_config": sched.ParticipantConfig,
		"is_active":          sched.IsActive,
		"created_at":         sched.CreatedAt.Format(time.RFC3339),
	}
	if sched.LastRunAt != nil {
		item["last_run_at"] = sched.LastRunAt.Format(time.RFC3339)
	}
	if sched.NextRunAt != nil {
		item["next_run_at"] = sched.NextRunAt.Format(time.RFC3339)
	}

	ok(c, item)
}

// UpdateSchedule handles PUT /api/v1/summary-schedules/:id
func (h *ScheduleHandler) UpdateSchedule(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)
	schedID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid schedule id"})
		return
	}

	var sched model.SummarySchedule
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", schedID, spaceID).First(&sched).Error; err != nil {
		bizErr(c, service.NewBizError(40008, "定时配置不存在", http.StatusNotFound))
		return
	}
	if sched.CreatorID != userID {
		bizErr(c, service.NewBizError(40004, "无权限修改", http.StatusForbidden))
		return
	}

	var req updateScheduleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: err.Error()})
		return
	}

	if req.Title != nil && utf8.RuneCountInString(*req.Title) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "title 不能超过 1000 字符"})
		return
	}

	updates := make(map[string]interface{})
	if req.Title != nil {
		updates["title"] = *req.Title
	}

	// Determine effective cron/interval after this update to recompute next_run_at
	// whenever either scheduling field changes.
	effCron := sched.CronExpr
	effInterval := sched.IntervalDays
	schedChanged := false
	if req.CronExpr != nil {
		effCron = *req.CronExpr
		updates["cron_expr"] = *req.CronExpr
		schedChanged = true
	}
	if req.IntervalDays != nil {
		if *req.IntervalDays < 0 {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: "interval_days 不能为负"})
			return
		}
		effInterval = *req.IntervalDays
		updates["interval_days"] = *req.IntervalDays
		schedChanged = true
	}
	if schedChanged {
		if effInterval == 0 && effCron == "" {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40012, Message: "cron_expr 与 interval_days 至少提供一个"})
			return
		}
		nextRun, err := service.NextRunWithInterval(effCron, effInterval, time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, apiResponse{Code: 40010, Message: "无效的 cron 表达式: " + effCron})
			return
		}
		updates["next_run_at"] = nextRun
	}
	if req.TimeRangeType != nil {
		updates["time_range_type"] = *req.TimeRangeType
	}
	if req.Sources != nil {
		b, _ := json.Marshal(req.Sources)
		updates["source_config"] = model.JSON(b)
	}
	if req.Participants != nil {
		b, _ := json.Marshal(req.Participants)
		updates["participant_config"] = model.JSON(b)
	}

	h.db.Model(&sched).Updates(updates)

	var nextRunAt *string
	if sched.NextRunAt != nil {
		s := sched.NextRunAt.Format(time.RFC3339)
		nextRunAt = &s
	}

	ok(c, gin.H{
		"schedule_id": sched.ID,
		"next_run_at": nextRunAt,
	})
}

// DeleteSchedule handles DELETE /api/v1/summary-schedules/:id
func (h *ScheduleHandler) DeleteSchedule(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)
	schedID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid schedule id"})
		return
	}

	var sched model.SummarySchedule
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", schedID, spaceID).First(&sched).Error; err != nil {
		bizErr(c, service.NewBizError(40008, "定时配置不存在", http.StatusNotFound))
		return
	}
	if sched.CreatorID != userID {
		bizErr(c, service.NewBizError(40004, "无权限删除", http.StatusForbidden))
		return
	}

	now := time.Now().UTC()
	h.db.Model(&sched).Update("deleted_at", &now)

	ok(c, nil)
}

// ToggleSchedule handles PUT /api/v1/summary-schedules/:id/toggle
func (h *ScheduleHandler) ToggleSchedule(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	userID := middleware.GetUserID(c)
	schedID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid schedule id"})
		return
	}

	var sched model.SummarySchedule
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", schedID, spaceID).First(&sched).Error; err != nil {
		bizErr(c, service.NewBizError(40008, "定时配置不存在", http.StatusNotFound))
		return
	}
	if sched.CreatorID != userID {
		bizErr(c, service.NewBizError(40004, "无权限操作", http.StatusForbidden))
		return
	}

	var req toggleScheduleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if req.IsActive {
		updates["is_active"] = 1
		if sched.CronExpr != "" {
			if nextRun, err := service.NextRun(sched.CronExpr, time.Now().UTC()); err == nil {
				updates["next_run_at"] = nextRun
			}
		}
	} else {
		updates["is_active"] = 0
	}

	h.db.Model(&sched).Updates(updates)

	ok(c, gin.H{
		"schedule_id": sched.ID,
		"is_active":   updates["is_active"],
	})
}
