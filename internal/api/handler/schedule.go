package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	Title          string           `json:"title"`
	CronExpr       string           `json:"cron_expr"`
	IntervalDays   int              `json:"interval_days"`
	IntervalMonths int              `json:"interval_months"`
	RunTime        string           `json:"run_time"`
	DayOfWeek      int              `json:"day_of_week"`
	DayOfMonth     int              `json:"day_of_month"`
	TimeRangeType  int              `json:"time_range_type"`
	Sources        []sourceReq      `json:"sources"`
	Participants   []participantReq `json:"participants"`
	Scope          string           `json:"scope,omitempty"`
	TaskID         *int64           `json:"task_id,omitempty"`
}

type updateScheduleReq struct {
	Title          *string          `json:"title"`
	CronExpr       *string          `json:"cron_expr"`
	IntervalDays   *int             `json:"interval_days"`
	IntervalMonths *int             `json:"interval_months"`
	RunTime        *string          `json:"run_time"`
	DayOfWeek      *int             `json:"day_of_week"`
	DayOfMonth     *int             `json:"day_of_month"`
	TimeRangeType  *int             `json:"time_range_type"`
	Sources        []sourceReq      `json:"sources,omitempty"`
	Participants   []participantReq `json:"participants,omitempty"`
	Scope          string           `json:"scope,omitempty"`
	TaskID         *int64           `json:"task_id,omitempty"`
}

type toggleScheduleReq struct {
	IsActive bool `json:"is_active"`
}

var (
	errTaskScopeMissingTaskID = errors.New("scope=task requires task_id")
	errTaskScopeInvalidTask   = errors.New("scope=task task_id invalid")
	errTaskScopeScheduleBound = errors.New("scope=task schedule already bound to another task")
	// Scheduled summary is single-person only this version; reject multi-person at the API.
	errMultiPersonNotSupported = errors.New("scheduled summary not supported for multi-person/team tasks")
	// MySQL 1062 on uk_live_schedule_binding mapped to a clean 409.
	errLiveBindingDuplicate = errors.New("scope=task schedule live-binding unique index conflict (1062)")
	// Pre-read of task.schedule_id went stale under a concurrent rebind; retryable.
	errRebindConcurrentModified = errors.New("scope=task concurrent rebind detected, please retry")
)

// isMySQLDuplicateKey reports whether err is (or wraps) a MySQL 1062 duplicate key.
func isMySQLDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysqldriver.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1062 {
		return true
	}
	return errors.Is(err, gorm.ErrDuplicatedKey)
}

func isMySQLRetryableTxError(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysqldriver.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	return myErr.Number == 1205 || myErr.Number == 1213
}

func isScheduleRetryableConflict(err error) bool {
	return errors.Is(err, errRebindConcurrentModified) || isMySQLRetryableTxError(err)
}

func writeRetryableRebindConflict(c *gin.Context) {
	c.JSON(http.StatusConflict, apiResponse{Code: 40916, Message: "绑定状态被并发修改，请重试"})
}

// 40015 user-facing message for the multi-person guard.
const teamScheduleNotSupportedMsg = "定时总结暂不支持多人/团队任务"

// loadTaskParticipantCount counts participants bound to a task (same measure as the worker guard).
func loadTaskParticipantCount(tx *gorm.DB, taskID int64) (int64, error) {
	var participantCount int64
	if err := tx.Model(&model.SummaryParticipant{}).
		Where("task_id = ?", taskID).
		Count(&participantCount).Error; err != nil {
		return 0, err
	}
	return participantCount, nil
}

// participantsSubsetOfCreator reports whether every configured participant is the creator
// (empty UserID counts as creator). False means the worker would inflate it past single-person.
func participantsSubsetOfCreator(reqParticipants []participantReq, creatorID string) bool {
	for _, p := range reqParticipants {
		if p.UserID == "" {
			continue
		}
		if p.UserID != creatorID {
			return false
		}
	}
	return true
}

// storedParticipantConfigSubsetOfCreator applies participantsSubsetOfCreator to a schedule's
// stored participant_config, so a bind reusing stored config (req.Participants==nil) is also
// rejected when it contains a non-creator. Empty config is a subset (PASS).
func storedParticipantConfigSubsetOfCreator(raw model.JSON, creatorID string) bool {
	if len(raw) == 0 {
		return true
	}
	var stored []participantReq
	if err := json.Unmarshal(raw, &stored); err != nil {
		// Unparseable stored config is treated as unsafe (fail-closed): we cannot
		// prove it is single-person, so refuse to bind. This mirrors the worker,
		// which would also fail to deserialize and skip the cycle.
		return false
	}
	return participantsSubsetOfCreator(stored, creatorID)
}

// validateEffectiveParticipantsSubsetOfCreator is the single post-load check that
// the participant set actually taking effect (req if sent, else stored config)
// is a subset of {creatorID}. creatorID must be the loaded task.CreatorID.
func validateEffectiveParticipantsSubsetOfCreator(reqParticipants []participantReq, storedConfig model.JSON, creatorID string) error {
	if reqParticipants != nil {
		if !participantsSubsetOfCreator(reqParticipants, creatorID) {
			return errMultiPersonNotSupported
		}
		return nil
	}
	if !storedParticipantConfigSubsetOfCreator(storedConfig, creatorID) {
		return errMultiPersonNotSupported
	}
	return nil
}

// peekTaskScheduleID reads task.schedule_id without locking, so the caller can lock
// the schedule rows before the task (keeps tx order schedule->task). Re-validated after the task lock.
func peekTaskScheduleID(tx *gorm.DB, spaceID, userID string, taskID int64) (*int64, error) {
	var row struct {
		ScheduleID *int64
	}
	err := tx.Model(&model.SummaryTask{}).
		Select("schedule_id").
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	return row.ScheduleID, nil
}

// int64PtrEqual reports whether two *int64 hold equal values (both nil => equal).
func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// anchorDOMForMonthlyCreate stores the original intended monthly day-of-month.
// An explicit DOM (1..31) is self-describing; DOM=0 means "anchor to this
// create/change date", so only the create/change baseline may seed it.
func anchorDOMForMonthlyCreate(dayOfMonth int, changeBase time.Time) int {
	if dayOfMonth >= 1 && dayOfMonth <= 31 {
		return dayOfMonth
	}
	return service.ResolveAnchorDOM(dayOfMonth, changeBase)
}

// anchorDOMForMonthlyUpdate decides whether an UPDATE should write anchor_dom.
// Unrelated edits (for example only changing run_time) must keep the stored
// anchor untouched; only entering month mode or explicitly changing
// day_of_month mutates anchor_dom. When the caller explicitly switches to
// day_of_month=0, we preserve an existing anchor if one is already trusted;
// otherwise we fall back to the create/change baseline because that is the
// only reliable source of the user's implicit monthly anchor.
func anchorDOMForMonthlyUpdate(existing model.SummarySchedule, effIntervalMonths int, effDayOfMonth int, reqDayOfMonth *int, changeBase time.Time) (int, bool) {
	if effIntervalMonths <= 0 {
		return existing.AnchorDOM, false
	}
	if existing.IntervalMonths <= 0 {
		return anchorDOMForMonthlyCreate(effDayOfMonth, changeBase), true
	}
	if reqDayOfMonth == nil || *reqDayOfMonth == existing.DayOfMonth {
		return existing.AnchorDOM, false
	}
	if effDayOfMonth >= 1 && effDayOfMonth <= 31 {
		return effDayOfMonth, true
	}
	if existing.AnchorDOM >= 1 && existing.AnchorDOM <= 31 {
		return existing.AnchorDOM, true
	}
	return anchorDOMForMonthlyCreate(effDayOfMonth, changeBase), true
}

func effectiveScheduleDayOfMonth(intervalMonths int, dayOfMonth int, anchorDOM int) int {
	if intervalMonths <= 0 {
		return dayOfMonth
	}
	return service.EffectiveMonthlyDOM(dayOfMonth, anchorDOM)
}

// lockScheduleForUpdate FOR UPDATE-locks the target schedule row so concurrent binds on the
// same schedule serialize. Locking schedule before task keeps handlers in the scheduler's
// schedule->task order, avoiding the cross-direction deadlock.
func lockScheduleForUpdate(tx *gorm.DB, scheduleID int64, spaceID string) (model.SummarySchedule, error) {
	var locked model.SummarySchedule
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", scheduleID, spaceID).
		First(&locked).Error
	return locked, err
}

func lockOptionalScheduleForUpdate(tx *gorm.DB, scheduleID int64) (*model.SummarySchedule, error) {
	var locked model.SummarySchedule
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", scheduleID).
		First(&locked).Error
	switch {
	case err == nil:
		return &locked, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil
	default:
		return nil, err
	}
}

func orderedScheduleLockIDs(targetID int64, oldScheduleID *int64) (int64, *int64) {
	if oldScheduleID == nil || *oldScheduleID == targetID {
		return targetID, nil
	}
	if targetID < *oldScheduleID {
		return targetID, oldScheduleID
	}
	return *oldScheduleID, &targetID
}

// loadBoundTaskForScheduleUpdate validates the schedule->task live binding on the
// non-rebind update path. We reject orphaned/inconsistent schedules instead of
// mutating detached config: once the live task is gone, the caller must refresh
// and repair/recreate the schedule rather than editing a stale row in place.
func loadBoundTaskForScheduleUpdate(tx *gorm.DB, lockedSched model.SummarySchedule, userID string) (model.SummaryTask, error) {
	var tasks []model.SummaryTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("schedule_id = ? AND deleted_at IS NULL", lockedSched.ID).
		Order("id ASC").
		Find(&tasks).Error; err != nil {
		return model.SummaryTask{}, err
	}
	if len(tasks) == 0 {
		return model.SummaryTask{}, service.NewBizError(40008, "定时配置已失去绑定，请刷新后重试", http.StatusNotFound)
	}
	if len(tasks) > 1 {
		return model.SummaryTask{}, service.NewBizError(40008, "定时配置绑定关系异常，请刷新后重试", http.StatusConflict)
	}
	task := tasks[0]
	if task.SpaceID != lockedSched.SpaceID || task.ScheduleID == nil || *task.ScheduleID != lockedSched.ID {
		return model.SummaryTask{}, service.NewBizError(40008, "定时配置绑定关系异常，请刷新后重试", http.StatusConflict)
	}
	if task.CreatorID != userID {
		return model.SummaryTask{}, service.NewBizError(40004, "无权限修改", http.StatusForbidden)
	}
	return task, nil
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

	// Multi-person guard needs task.CreatorID, so the participant check runs in the
	// transaction after loadTaskForTaskScope locks the task.

	now := timezone.Now()
	// Interval-only writes: bounds + mutual exclusivity of interval_days/interval_months.
	if err := service.ValidateIntervalForWrite(req.CronExpr, req.IntervalDays, req.IntervalMonths); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
		return
	}
	// Strict run_time validation: reject malformed HH:MM rather than silently
	// falling back to the trigger instant.
	if err := service.ValidateRunTime(req.RunTime); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40012, Message: err.Error()})
		return
	}
	if err := service.ValidateDayOfWeek(req.DayOfWeek); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40013, Message: err.Error()})
		return
	}
	if err := service.ValidateDayOfMonth(req.DayOfMonth); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40014, Message: err.Error()})
		return
	}
	if err := service.ValidateScheduleAnchors(req.CronExpr, req.IntervalDays, req.IntervalMonths, req.DayOfWeek, req.DayOfMonth); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
		return
	}
	if req.TimeRangeType == 0 {
		req.TimeRangeType = 2
	}
	if err := service.ValidateTimeRangeType(req.TimeRangeType); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
		return
	}
	summaryMode := model.ModeByPerson

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
		IntervalMonths:    req.IntervalMonths,
		RunTime:           req.RunTime,
		DayOfWeek:         req.DayOfWeek,
		DayOfMonth:        req.DayOfMonth,
		TimeRangeType:     req.TimeRangeType,
		SourceConfig:      sourceConfig,
		ParticipantConfig: participantConfig,
	}

	if req.Scope != "task" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "定时必须绑定到指定总结(scope=task)"})
		return
	}

	resultScheduleID := int64(0)
	var resultNextRunAt time.Time
	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		if req.TaskID == nil {
			return errTaskScopeMissingTaskID
		}

		// Lock schedules before the task (schedule->task), so pre-read the task's
		// schedule_id without a lock, then lock that existing schedule first.
		peekedExisting, err := peekTaskScheduleID(tx, spaceID, userID, *req.TaskID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errTaskScopeInvalidTask
			}
			return err
		}

		var existing model.SummarySchedule
		haveExisting := false
		if peekedExisting != nil {
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND space_id = ? AND deleted_at IS NULL", *peekedExisting, spaceID).
				First(&existing).Error
			switch {
			case err == nil:
				haveExisting = true
			case errors.Is(err, gorm.ErrRecordNotFound):
				// stale/deleted schedule; treat as none.
			default:
				return err
			}
		}

		task, err := loadTaskForTaskScope(tx, spaceID, userID, *req.TaskID)
		if err != nil {
			return err
		}

		// TOCTOU: bail out retryable if the binding changed after the pre-read.
		if !int64PtrEqual(task.ScheduleID, peekedExisting) {
			return errRebindConcurrentModified
		}

		// Single-person guard: configured participants must be a subset of {creator}.
		if !participantsSubsetOfCreator(req.Participants, task.CreatorID) {
			return errMultiPersonNotSupported
		}

		if haveExisting {
			finalAnchorDOM := existing.AnchorDOM
			if anchorDOM, writeAnchorDOM := anchorDOMForMonthlyUpdate(existing, sched.IntervalMonths, sched.DayOfMonth, &req.DayOfMonth, now); writeAnchorDOM {
				finalAnchorDOM = anchorDOM
			}
			nextRun, err := service.NextRunInitial(
				sched.CronExpr,
				sched.IntervalDays,
				sched.IntervalMonths,
				sched.RunTime,
				sched.DayOfWeek,
				effectiveScheduleDayOfMonth(sched.IntervalMonths, sched.DayOfMonth, finalAnchorDOM),
				now,
			)
			if err != nil {
				return service.NewBizError(40010, "无效的调度配置: "+err.Error(), http.StatusUnprocessableEntity)
			}
			var boundCount int64
			if err := tx.Model(&model.SummaryTask{}).
				Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("schedule_id = ? AND deleted_at IS NULL AND id <> ?", existing.ID, task.ID).
				Count(&boundCount).Error; err != nil {
				return err
			}
			if boundCount > 0 {
				return errTaskScopeScheduleBound
			}
			if existing.CreatorID != userID {
				return service.NewBizError(40004, "无权限修改", http.StatusForbidden)
			}
			// Reuse the (possibly inactive) schedule and re-activate it so the
			// scheduler picks it up; first-run semantics via nextRun.
			updates := map[string]interface{}{
				"title":              sched.Title,
				"cron_expr":          sched.CronExpr,
				"interval_days":      sched.IntervalDays,
				"interval_months":    sched.IntervalMonths,
				"run_time":           sched.RunTime,
				"day_of_week":        sched.DayOfWeek,
				"day_of_month":       sched.DayOfMonth,
				"time_range_type":    sched.TimeRangeType,
				"source_config":      sched.SourceConfig,
				"participant_config": sched.ParticipantConfig,
				"next_run_at":        nextRun,
				"is_active":          1,
			}
			if sched.IntervalMonths > 0 {
				updates["anchor_dom"] = finalAnchorDOM
			}
			if err := tx.Model(&model.SummarySchedule{}).
				Where("id = ?", existing.ID).
				Updates(updates).Error; err != nil {
				return err
			}
			resultScheduleID = existing.ID
			resultNextRunAt = nextRun
			return nil
		}

		finalAnchorDOM := 0
		if sched.IntervalMonths > 0 {
			finalAnchorDOM = anchorDOMForMonthlyCreate(req.DayOfMonth, now)
			sched.AnchorDOM = finalAnchorDOM
		}
		nextRun, err := service.NextRunInitial(
			sched.CronExpr,
			sched.IntervalDays,
			sched.IntervalMonths,
			sched.RunTime,
			sched.DayOfWeek,
			effectiveScheduleDayOfMonth(sched.IntervalMonths, sched.DayOfMonth, finalAnchorDOM),
			now,
		)
		if err != nil {
			return service.NewBizError(40010, "无效的调度配置: "+err.Error(), http.StatusUnprocessableEntity)
		}
		sched.NextRunAt = &nextRun
		if err := tx.Create(&sched).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.SummaryTask{}).
			Where("id = ? AND space_id = ?", task.ID, spaceID).
			Update("schedule_id", sched.ID).Error; err != nil {
			if isMySQLDuplicateKey(err) {
				return errLiveBindingDuplicate
			}
			return err
		}
		resultScheduleID = sched.ID
		resultNextRunAt = nextRun
		return nil
	})
	if txErr != nil {
		switch {
		case errors.Is(txErr, errTaskScopeMissingTaskID):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "scope=task 时必须传 task_id"})
			return
		case errors.Is(txErr, errTaskScopeInvalidTask):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "task_id 无效或不属于当前空间"})
			return
		case errors.Is(txErr, errTaskScopeScheduleBound):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "该定时已绑定其它总结，不能重复绑定"})
			return
		case errors.Is(txErr, errLiveBindingDuplicate):
			c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "该定时已绑定其它总结，不能重复绑定"})
			return
		case isScheduleRetryableConflict(txErr):
			writeRetryableRebindConflict(c)
			return
		case errors.Is(txErr, errMultiPersonNotSupported):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40015, Message: teamScheduleNotSupportedMsg})
			return
		}
		if biz, ok := txErr.(*service.BizError); ok {
			bizErr(c, biz)
			return
		}
		log.Printf("[handler] CreateSchedule error: %v", txErr)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: txErr.Error()})
		return
	}

	ok(c, gin.H{
		"schedule_id": resultScheduleID,
		"next_run_at": resultNextRunAt.Format(time.RFC3339),
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
			"interval_months":    s.IntervalMonths,
			"run_time":           s.RunTime,
			"day_of_week":        s.DayOfWeek,
			"day_of_month":       s.DayOfMonth,
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
		"interval_months":    sched.IntervalMonths,
		"run_time":           sched.RunTime,
		"day_of_week":        sched.DayOfWeek,
		"day_of_month":       sched.DayOfMonth,
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
	// Fail-closed multi-person guard on update; only when participants are sent
	// (nil = leave untouched). Stored-config bind path is checked later in the tx.
	if req.Participants != nil && !participantsSubsetOfCreator(req.Participants, userID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40015, Message: teamScheduleNotSupportedMsg})
		return
	}
	if req.Scope != "" && req.Scope != "task" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "定时必须绑定到指定总结(scope=task)"})
		return
	}

	updates := make(map[string]interface{})
	if req.Title != nil {
		updates["title"] = *req.Title
	}

	// Determine effective cron/interval after this update to recompute next_run_at
	// whenever any scheduling field changes. Validation + mutual exclusivity go
	// through service.ValidateInterval so create/update/toggle stay consistent.
	effCron := sched.CronExpr
	effIntervalDays := sched.IntervalDays
	effIntervalMonths := sched.IntervalMonths
	effRunTime := sched.RunTime
	effDayOfWeek := sched.DayOfWeek
	effDayOfMonth := sched.DayOfMonth
	schedChanged := false
	if req.CronExpr != nil {
		effCron = *req.CronExpr
		updates["cron_expr"] = *req.CronExpr
		schedChanged = true
	}
	if req.IntervalDays != nil {
		effIntervalDays = *req.IntervalDays
		updates["interval_days"] = *req.IntervalDays
		schedChanged = true
	}
	if req.IntervalMonths != nil {
		effIntervalMonths = *req.IntervalMonths
		updates["interval_months"] = *req.IntervalMonths
		schedChanged = true
	}
	if req.RunTime != nil {
		effRunTime = *req.RunTime
		// Strict run_time validation on update too.
		if err := service.ValidateRunTime(*req.RunTime); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40012, Message: err.Error()})
			return
		}
		updates["run_time"] = *req.RunTime
		schedChanged = true
	}
	if req.DayOfWeek != nil {
		effDayOfWeek = *req.DayOfWeek
		if err := service.ValidateDayOfWeek(*req.DayOfWeek); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40013, Message: err.Error()})
			return
		}
		updates["day_of_week"] = *req.DayOfWeek
		schedChanged = true
	}
	if req.DayOfMonth != nil {
		effDayOfMonth = *req.DayOfMonth
		if err := service.ValidateDayOfMonth(*req.DayOfMonth); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40014, Message: err.Error()})
			return
		}
		updates["day_of_month"] = *req.DayOfMonth
		schedChanged = true
	}
	if schedChanged {
		// Interval-only write contract: reject any attempt to set/keep a cron
		// expression through update. Legacy cron tasks remain executable but can
		// no longer be created or modified into cron mode. If the caller sent a
		// non-empty cron_expr, reject; otherwise force a single interval source.
		if req.CronExpr != nil && *req.CronExpr != "" {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: "不再支持修改为自定义 cron 模式, 请选择间隔(天/周/月)"})
			return
		}
		// When an interval is set, always drop any stored/legacy cron so the
		// recompute is unambiguous and the task migrates off cron.
		effCron = ""
		updates["cron_expr"] = ""
		if req.DayOfWeek == nil && effIntervalMonths > 0 && effDayOfWeek != 0 {
			effDayOfWeek = 0
			updates["day_of_week"] = 0
		}
		// Switching from week mode (interval_days a multiple of 7) to a non-week
		// day interval leaves a stale day_of_week that ValidateScheduleAnchors
		// rejects ("仅周模式支持 day_of_week"). Clear it when the caller did not set
		// it explicitly, mirroring the month-switch case above.
		if req.DayOfWeek == nil && effIntervalDays > 0 && effIntervalDays%7 != 0 && effDayOfWeek != 0 {
			effDayOfWeek = 0
			updates["day_of_week"] = 0
		}
		if req.DayOfMonth == nil && effIntervalDays > 0 && effDayOfMonth != 0 {
			effDayOfMonth = 0
			updates["day_of_month"] = 0
		}
		if err := service.ValidateIntervalForWrite(effCron, effIntervalDays, effIntervalMonths); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
			return
		}
		if err := service.ValidateScheduleAnchors(effCron, effIntervalDays, effIntervalMonths, effDayOfWeek, effDayOfMonth); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
			return
		}
		recomputeNow := timezone.Now()
		finalAnchorDOM := sched.AnchorDOM
		anchorDOM, writeAnchorDOM := anchorDOMForMonthlyUpdate(sched, effIntervalMonths, effDayOfMonth, req.DayOfMonth, recomputeNow)
		if writeAnchorDOM {
			finalAnchorDOM = anchorDOM
		}
		nextRun, err := service.NextRunInitial(
			effCron,
			effIntervalDays,
			effIntervalMonths,
			effRunTime,
			effDayOfWeek,
			effectiveScheduleDayOfMonth(effIntervalMonths, effDayOfMonth, finalAnchorDOM),
			recomputeNow,
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
			return
		}
		updates["next_run_at"] = nextRun
		if writeAnchorDOM {
			updates["anchor_dom"] = finalAnchorDOM
		}
	}
	if req.TimeRangeType != nil {
		if err := service.ValidateTimeRangeType(*req.TimeRangeType); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
			return
		}
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

	resultScheduleID := sched.ID
	var resultNextRunAt *time.Time

	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		var task model.SummaryTask
		var oldScheduleID *int64
		// Reused below for the soft-delete; locked here, before the task, to keep the
		// whole tx schedule->task (matching the scheduler).
		var lockedOldSched *model.SummarySchedule
		var lockedSched model.SummarySchedule
		if req.Scope == "task" {
			if req.TaskID == nil {
				return errTaskScopeMissingTaskID
			}

			// Non-locking pre-read of the task's schedule_id so we can lock the old
			// schedule BEFORE the task. Candidate; re-validated after the task lock.
			peekedOldID, err := peekTaskScheduleID(tx, spaceID, userID, *req.TaskID)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errTaskScopeInvalidTask
				}
				return err
			}
			if peekedOldID != nil && *peekedOldID != sched.ID {
				cand := *peekedOldID
				oldScheduleID = &cand
			}
		}

		firstScheduleID, secondScheduleID := orderedScheduleLockIDs(sched.ID, oldScheduleID)
		lockScheduleByID := func(scheduleID int64) error {
			if scheduleID == sched.ID {
				locked, err := lockScheduleForUpdate(tx, sched.ID, spaceID)
				if err != nil {
					return err
				}
				lockedSched = locked
				return nil
			}
			locked, err := lockOptionalScheduleForUpdate(tx, scheduleID)
			if err != nil {
				return err
			}
			lockedOldSched = locked
			return nil
		}
		if err := lockScheduleByID(firstScheduleID); err != nil {
			return err
		}
		if secondScheduleID != nil {
			if err := lockScheduleByID(*secondScheduleID); err != nil {
				return err
			}
		}

		if req.Scope == "task" {
			task, err = loadTaskForTaskScope(tx, spaceID, userID, *req.TaskID)
			if err != nil {
				return err
			}

			// TOCTOU: if the binding changed between the pre-read and the task lock,
			// the schedules we locked no longer match; bail out retryable rather than
			// locking a new schedule after the task lock.
			var lockedOldID *int64
			if task.ScheduleID != nil && *task.ScheduleID != sched.ID {
				oid := *task.ScheduleID
				lockedOldID = &oid
			}
			if !int64PtrEqual(lockedOldID, oldScheduleID) {
				return errRebindConcurrentModified
			}

			// Single post-load single-person guard against the loaded task's creator.
			if err := validateEffectiveParticipantsSubsetOfCreator(req.Participants, lockedSched.ParticipantConfig, task.CreatorID); err != nil {
				return err
			}

			var boundCount int64
			if err := tx.Model(&model.SummaryTask{}).
				Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("schedule_id = ? AND deleted_at IS NULL AND id <> ?", sched.ID, task.ID).
				Count(&boundCount).Error; err != nil {
				return err
			}
			if boundCount > 0 {
				return errTaskScopeScheduleBound
			}
		} else {
			if _, err := loadBoundTaskForScheduleUpdate(tx, lockedSched, userID); err != nil {
				return err
			}
		}

		if req.Scope == "task" && (task.ScheduleID == nil || *task.ScheduleID != sched.ID) {
			if err := tx.Model(&model.SummaryTask{}).
				Where("id = ? AND space_id = ?", task.ID, spaceID).
				Update("schedule_id", sched.ID).Error; err != nil {
				if isMySQLDuplicateKey(err) {
					return errLiveBindingDuplicate
				}
				return err
			}
		}
		// TOCTOU fix: the effective recurrence values and the recomputed
		// next_run_at / anchor_dom above were derived from `sched`, read WITHOUT a
		// lock at the top of the handler. A concurrent UpdateSchedule on the same
		// row could have changed interval_days / interval_months / run_time /
		// day_of_week / day_of_month in between, so recompute from the FOR UPDATE
		// locked snapshot for every field the caller did not explicitly send, then
		// rewrite next_run_at / anchor_dom before persisting.
		if schedChanged {
			lEffCron := ""
			lEffIntervalDays := effIntervalDays
			lEffIntervalMonths := effIntervalMonths
			lEffRunTime := effRunTime
			lEffDayOfWeek := effDayOfWeek
			lEffDayOfMonth := effDayOfMonth
			if req.IntervalDays == nil {
				lEffIntervalDays = lockedSched.IntervalDays
			}
			if req.IntervalMonths == nil {
				lEffIntervalMonths = lockedSched.IntervalMonths
			}
			if req.RunTime == nil {
				lEffRunTime = lockedSched.RunTime
			}
			if req.DayOfWeek == nil {
				lEffDayOfWeek = lockedSched.DayOfWeek
			}
			if req.DayOfMonth == nil {
				lEffDayOfMonth = lockedSched.DayOfMonth
			}
			// Re-apply the interval-only normalization (drop cron, clear stale
			// anchors) against the locked base so the same invariants hold.
			if req.DayOfWeek == nil && lEffIntervalMonths > 0 && lEffDayOfWeek != 0 {
				lEffDayOfWeek = 0
			}
			if req.DayOfWeek == nil && lEffIntervalDays > 0 && lEffIntervalDays%7 != 0 && lEffDayOfWeek != 0 {
				lEffDayOfWeek = 0
			}
			if req.DayOfMonth == nil && lEffIntervalDays > 0 && lEffDayOfMonth != 0 {
				lEffDayOfMonth = 0
			}
			if err := service.ValidateIntervalForWrite(lEffCron, lEffIntervalDays, lEffIntervalMonths); err != nil {
				return service.NewBizError(40011, err.Error(), http.StatusBadRequest)
			}
			if err := service.ValidateScheduleAnchors(lEffCron, lEffIntervalDays, lEffIntervalMonths, lEffDayOfWeek, lEffDayOfMonth); err != nil {
				return service.NewBizError(40011, err.Error(), http.StatusBadRequest)
			}
			recomputeNow := timezone.Now()
			lFinalAnchorDOM := lockedSched.AnchorDOM
			lAnchorDOM, lWriteAnchorDOM := anchorDOMForMonthlyUpdate(lockedSched, lEffIntervalMonths, lEffDayOfMonth, req.DayOfMonth, recomputeNow)
			if lWriteAnchorDOM {
				lFinalAnchorDOM = lAnchorDOM
			}
			lNextRun, err := service.NextRunInitial(
				lEffCron,
				lEffIntervalDays,
				lEffIntervalMonths,
				lEffRunTime,
				lEffDayOfWeek,
				effectiveScheduleDayOfMonth(lEffIntervalMonths, lEffDayOfMonth, lFinalAnchorDOM),
				recomputeNow,
			)
			if err != nil {
				return service.NewBizError(40011, err.Error(), http.StatusBadRequest)
			}
			updates["day_of_week"] = lEffDayOfWeek
			updates["day_of_month"] = lEffDayOfMonth
			updates["next_run_at"] = lNextRun
			if lWriteAnchorDOM {
				updates["anchor_dom"] = lFinalAnchorDOM
			}
		}
		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", sched.ID).
			Updates(updates).Error; err != nil {
			return err
		}
		if lockedOldSched != nil {
			now := timezone.Now()
			// Soft-delete the old schedule only when the caller owns it and no other
			// live task still binds it. Reuses the lock taken above.
			oldSched := *lockedOldSched
			var otherBound int64
			if err := tx.Model(&model.SummaryTask{}).
				Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("schedule_id = ? AND deleted_at IS NULL", oldSched.ID).
				Count(&otherBound).Error; err != nil {
				return err
			}
			if oldSched.CreatorID == userID && otherBound == 0 {
				if err := tx.Model(&model.SummarySchedule{}).
					Where("id = ? AND deleted_at IS NULL", oldSched.ID).
					Update("deleted_at", &now).Error; err != nil {
					return err
				}
			} else {
				log.Printf("[handler] UpdateSchedule: old schedule %d not soft-deleted (caller=%s creator=%s otherBound=%d); unbind-only", oldSched.ID, userID, oldSched.CreatorID, otherBound)
			}
		}
		if nr, ok := updates["next_run_at"].(time.Time); ok {
			resultNextRunAt = &nr
		} else {
			resultNextRunAt = sched.NextRunAt
		}
		return nil
	})
	if txErr != nil {
		switch {
		case errors.Is(txErr, errTaskScopeMissingTaskID):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "scope=task 时必须传 task_id"})
			return
		case errors.Is(txErr, errTaskScopeInvalidTask):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "task_id 无效或不属于当前空间"})
			return
		case errors.Is(txErr, errTaskScopeScheduleBound):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "该定时已绑定其它总结，不能重复绑定"})
			return
		case errors.Is(txErr, errLiveBindingDuplicate):
			c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "该定时已绑定其它总结，不能重复绑定"})
			return
		case isScheduleRetryableConflict(txErr):
			writeRetryableRebindConflict(c)
			return
		case errors.Is(txErr, errMultiPersonNotSupported):
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40015, Message: teamScheduleNotSupportedMsg})
			return
		}
		if biz, ok := txErr.(*service.BizError); ok {
			bizErr(c, biz)
			return
		}
		log.Printf("[handler] UpdateSchedule error: %v", txErr)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: txErr.Error()})
		return
	}

	var nextRunAt *string
	if resultNextRunAt != nil {
		s := resultNextRunAt.Format(time.RFC3339)
		nextRunAt = &s
	}

	ok(c, gin.H{
		"schedule_id": resultScheduleID,
		"next_run_at": nextRunAt,
	})
}

func loadTaskForTaskScope(tx *gorm.DB, spaceID, userID string, taskID int64) (model.SummaryTask, error) {
	var task model.SummaryTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).
		First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SummaryTask{}, errTaskScopeInvalidTask
		}
		return model.SummaryTask{}, err
	}
	if task.CreatorID != userID {
		return model.SummaryTask{}, service.NewBizError(40004, "仅创建者可绑定定时", http.StatusForbidden)
	}
	// Refuse binding a schedule to a multi-person task (same measure as the worker guard);
	// otherwise the scheduler would skip it every cycle, leaving a silently dead timer.
	participantCount, err := loadTaskParticipantCount(tx, task.ID)
	if err != nil {
		return model.SummaryTask{}, err
	}
	if participantCount > 1 {
		return model.SummaryTask{}, errMultiPersonNotSupported
	}
	return task, nil
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

	now := timezone.Now()
	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		lockedSched, err := lockScheduleForUpdate(tx, schedID, spaceID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errRebindConcurrentModified
			}
			return err
		}
		if lockedSched.CreatorID != userID {
			return service.NewBizError(40004, "无权限删除", http.StatusForbidden)
		}

		var boundTasks []model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("schedule_id = ? AND deleted_at IS NULL", lockedSched.ID).
			Order("id ASC").
			Find(&boundTasks).Error; err != nil {
			return err
		}

		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Update("deleted_at", &now).Error; err != nil {
			return err
		}

		if len(boundTasks) == 0 {
			return nil
		}

		taskIDs := make([]int64, 0, len(boundTasks))
		for _, task := range boundTasks {
			taskIDs = append(taskIDs, task.ID)
		}
		return tx.Model(&model.SummaryTask{}).
			Where("id IN ?", taskIDs).
			Update("schedule_id", nil).Error
	})
	if txErr != nil {
		var biz *service.BizError
		switch {
		case isScheduleRetryableConflict(txErr):
			writeRetryableRebindConflict(c)
		case errors.As(txErr, &biz):
			bizErr(c, biz)
		default:
			log.Printf("[handler] DeleteSchedule error: %v", txErr)
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: txErr.Error()})
		}
		return
	}

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

	resultIsActive := 0
	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		lockedSched, err := lockScheduleForUpdate(tx, sched.ID, spaceID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return service.NewBizError(40008, "定时配置不存在", http.StatusNotFound)
			}
			return err
		}
		if lockedSched.CreatorID != userID {
			return service.NewBizError(40004, "无权限操作", http.StatusForbidden)
		}

		updates := map[string]interface{}{}
		if req.IsActive {
			updates["is_active"] = 1
			if lockedSched.IsActive != 1 {
				task, err := loadBoundTaskForScheduleUpdate(tx, lockedSched, userID)
				if err != nil {
					return err
				}
				if err := validateEffectiveParticipantsSubsetOfCreator(nil, lockedSched.ParticipantConfig, task.CreatorID); err != nil {
					return err
				}
				nextRun, err := service.NextRunInitial(
					lockedSched.CronExpr,
					lockedSched.IntervalDays,
					lockedSched.IntervalMonths,
					lockedSched.RunTime,
					lockedSched.DayOfWeek,
					effectiveScheduleDayOfMonth(lockedSched.IntervalMonths, lockedSched.DayOfMonth, lockedSched.AnchorDOM),
					timezone.Now(),
				)
				if err != nil {
					return service.NewBizError(40011, err.Error(), http.StatusBadRequest)
				}
				updates["next_run_at"] = nextRun
			}
		} else {
			updates["is_active"] = 0
		}

		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Updates(updates).Error; err != nil {
			return err
		}
		resultIsActive = updates["is_active"].(int)
		return nil
	})
	if txErr != nil {
		if errors.Is(txErr, errMultiPersonNotSupported) {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40015, Message: teamScheduleNotSupportedMsg})
			return
		}
		if biz, ok := txErr.(*service.BizError); ok {
			bizErr(c, biz)
			return
		}
		log.Printf("[handler] ToggleSchedule error: %v", txErr)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: txErr.Error()})
		return
	}

	ok(c, gin.H{
		"schedule_id": sched.ID,
		"is_active":   resultIsActive,
	})
}
