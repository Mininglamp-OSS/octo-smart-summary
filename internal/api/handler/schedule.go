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
	// errMultiPersonNotSupported (PR#62 yujiawei r3, 方案A; r4 强化): scheduled
	// summary is single-person only this version. This is the API-layer
	// fail-closed guard that complements -- and runs IN ADDITION TO -- the
	// worker-layer "Method A guard" in internal/worker/scheduler.go. Before this
	// guard the API happily accepted a multi-person schedule (200 + next_run_at)
	// while the scheduler silently skipped every cycle, so the user waited
	// forever with no error and no result. We now reject multi-person at the door
	// (HTTP 400) so the front-end can surface a clear message and never persists a
	// schedule the scheduler will never run.
	//
	// r4 (PR#62 三位 reviewer 同时点名 Bug1): the r3 guard only blocked
	// len(req.Participants) > 1. But the worker
	// (scheduled_replace_helpers.go syncScheduledTaskParticipants) ALWAYS prepends
	// task.CreatorID before appending the configured participants. So a request
	// carrying exactly ONE participant that is NOT the task creator slipped through
	// the > 1 guard, then the worker inflated it to 2 rows (creator + that other
	// user) -> processor.go routed 2 participants into the multi-person team
	// branch (unsupported for scheduled this version) -> task stuck Processing,
	// submitted_at never written. The fix raises the API口径 to match the worker's
	// effective set: a scheduled task may ONLY carry participants that are a subset
	// of {task.CreatorID}. Any participant whose UserID != task.CreatorID (the
	// exact set the worker would inflate past single-person) is rejected here.
	errMultiPersonNotSupported = errors.New("scheduled summary not supported for multi-person/team tasks")
)

// teamScheduleNotSupportedMsg is the user-facing (Chinese) message returned for
// the multi-person fail-closed guard. Pairs with code 40015 so the front-end
// can route on the error code.
const teamScheduleNotSupportedMsg = "定时总结暂不支持多人/团队任务"

// loadTaskParticipantCount counts the participants bound to a task using the
// EXACT same measure as the worker-layer Method A guard
// (internal/worker/scheduler.go): COUNT(*) of SummaryParticipant rows where
// task_id = task.ID. Keeping the two口径 identical means a task the API accepts
// as single-person is exactly the set the scheduler is willing to run, and a
// task the API rejects as multi-person is exactly the set the scheduler would
// have skipped. The caller passes the same `tx` it already holds (it has the
// task locked FOR UPDATE), so this count is consistent with that snapshot.
func loadTaskParticipantCount(tx *gorm.DB, taskID int64) (int64, error) {
	var participantCount int64
	if err := tx.Model(&model.SummaryParticipant{}).
		Where("task_id = ?", taskID).
		Count(&participantCount).Error; err != nil {
		return 0, err
	}
	return participantCount, nil
}

// participantsSubsetOfCreator (PR#62 r4 Bug1) returns true iff every entry in
// reqParticipants is the task creator. An empty/nil slice is a subset (vacuously
// true). A participant with an empty UserID is treated as the creator (the
// worker's appendUser skips empty UserIDs, so it never inflates the effective
// set past the creator). Any participant whose non-empty UserID differs from
// creatorID makes the scheduled task effectively multi-person once the worker
// prepends the creator, so it must be rejected.
//
// This is the precise inverse of the worker's syncScheduledTaskParticipants
// effective set: worker = {creator} ∪ {p.UserID for p in config, p.UserID != ""}.
// We accept exactly when that union has cardinality <= 1, i.e. every configured
// participant is the creator (or empty).
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

// storedParticipantConfigSubsetOfCreator (PR#62 r5 Blocker2 / lml2468 Y1-bis)
// applies the SAME participantsSubsetOfCreator口径 to the participant_config
// already stored on a schedule (model.JSON). This closes the third instance of
// the single-person hole: UpdateSchedule only validated when the caller sent
// req.Participants != nil. When req.Participants == nil the bind path reuses the
// schedule's STORED participant_config (loaded into sched.ParticipantConfig),
// which was NEVER validated -- so a historically-dirty schedule whose stored
// config contains a non-creator could still be bound and later inflated to
// multi-person by the worker (scheduled_replace_helpers.go prepends creator then
// appends the stored config). We deserialize the stored config with the exact
// same shape the worker reads (user_id) and reject if it contains anyone other
// than the creator. An empty/nil config is vacuously a subset (PASS), matching
// the worker which then only has {creator}.
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

// lockScheduleForUpdate (PR#62 r5 Blocker1a / J2 lock-order) takes a FOR UPDATE
// row lock on the target summary_schedule so that all concurrent task<->schedule
// binding operations on the SAME schedule serialize. Without it two concurrent
// PUTs binding DIFFERENT tasks to the SAME schedule could each read boundCount==0
// (no DB uniqueness on summary_task.schedule_id) and both bind, breaking the
// one-to-one invariant. Locking the schedule row first makes the second binder
// block until the first commits, then see boundCount>0 and reject.
//
// It is also the anchor for the unified lock ORDER: handlers now lock
// schedule -> task (matching the scheduler in internal/worker/scheduler.go,
// which claims the schedule row then locks the bound task), eliminating the
// task->schedule vs schedule->task cross-direction deadlock window
// (lml2468 J2 non-blocking).
func lockScheduleForUpdate(tx *gorm.DB, scheduleID int64, spaceID string) (model.SummarySchedule, error) {
	var locked model.SummarySchedule
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", scheduleID, spaceID).
		First(&locked).Error
	return locked, err
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

	// 方案A (PR#62 yujiawei r3; r4 Bug1 强化): the multi-person fail-closed guard
	// now needs task.CreatorID to decide whether the configured participants are a
	// subset of {creator}. We therefore defer the participant check into the
	// transaction below, right after loadTaskForTaskScope loads (and FOR-UPDATE
	// locks) the bound task -- see the participantsSubsetOfCreator call there. The
	// old r3 input-level `len(req.Participants) > 1` check is intentionally
	// removed because it let through exactly the "1 non-creator participant" hole
	// the worker would inflate to 2 rows.

	now := timezone.Now()
	// ValidateIntervalForWrite enforces interval-only writes (cron is legacy
	// read+execute-only), bounds (overflow guard) and mutual exclusivity of
	// interval_days / interval_months in one place.
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
	// NextRunInitial: if today's selected run_time is still ahead of now, fire
	// today (需求1); otherwise advance one full interval. Aligns week mode to
	// day_of_week and month mode to day_of_month (需求4).
	nextRun, err := service.NextRunInitial(req.CronExpr, req.IntervalDays, req.IntervalMonths, req.RunTime, req.DayOfWeek, req.DayOfMonth, now)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, apiResponse{Code: 40010, Message: "无效的调度配置: " + err.Error()})
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
		IntervalMonths:    req.IntervalMonths,
		RunTime:           req.RunTime,
		DayOfWeek:         req.DayOfWeek,
		DayOfMonth:        req.DayOfMonth,
		TimeRangeType:     req.TimeRangeType,
		SourceConfig:      sourceConfig,
		ParticipantConfig: participantConfig,
		NextRunAt:         &nextRun,
	}

	if req.Scope != "task" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "定时必须绑定到指定总结(scope=task)"})
		return
	}

	resultScheduleID := int64(0)
	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		if req.TaskID == nil {
			return errTaskScopeMissingTaskID
		}
		task, err := loadTaskForTaskScope(tx, spaceID, userID, *req.TaskID)
		if err != nil {
			return err
		}

		// 方案A r4 Bug1: reject any configured participant that is not the task
		// creator. req.Participants is exactly what gets serialized into
		// participant_config (below) and read back by the worker, which prepends
		// task.CreatorID. So the only single-person-safe config is participants ⊆
		// {task.CreatorID}. Anything else would make the worker inflate the
		// effective set past 1 and route the task into the unsupported team branch.
		if !participantsSubsetOfCreator(req.Participants, task.CreatorID) {
			return errMultiPersonNotSupported
		}

		if task.ScheduleID != nil {
			var existing model.SummarySchedule
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND space_id = ? AND deleted_at IS NULL", *task.ScheduleID, spaceID).
				First(&existing).Error
			switch {
			case err == nil:
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
				// Bug1 (PR#62 Jerry-Xin r3): when the detail page rebuilds a
				// schedule, GetSummary hides the schedule_id for an inactive
				// binding, so the front-end re-enters via this create path and we
				// reuse the existing (possibly inactive) schedule. Re-activate it
				// here (is_active=1) so the scheduler picks it up again; otherwise
				// the request succeeds and returns next_run_at but the job never
				// fires. next_run_at uses NextRunInitial (first-run semantics,
				// same as the fresh-create path) so today's legal first run is not
				// skipped.
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
				if err := tx.Model(&model.SummarySchedule{}).
					Where("id = ?", existing.ID).
					Updates(updates).Error; err != nil {
					return err
				}
				resultScheduleID = existing.ID
				return nil
			case errors.Is(err, gorm.ErrRecordNotFound):
				// The task points to a stale/deleted schedule; create a fresh one
				// and rebind below.
			default:
				return err
			}
		}

		if err := tx.Create(&sched).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.SummaryTask{}).
			Where("id = ? AND space_id = ?", task.ID, spaceID).
			Update("schedule_id", sched.ID).Error; err != nil {
			return err
		}
		resultScheduleID = sched.ID
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
	// 方案A (PR#62 yujiawei r3; r4 Bug1 强化): API-layer fail-closed multi-person
	// guard on update. Only check when the caller actually sends participants
	// (req.Participants != nil); a nil slice means "leave participants untouched"
	// and must not be treated as multi-person. r4: the口径 is no longer "count > 1"
	// but "every configured participant must be the task creator" -- because the
	// worker prepends task.CreatorID, a single non-creator participant still
	// inflates the effective set to 2 and routes into the unsupported team branch.
	// The creator here is the caller (userID): only the creator can own/modify
	// this schedule (verified above via sched.CreatorID == userID) and only the
	// creator can bind a task (loadTaskForTaskScope enforces task.CreatorID ==
	// userID), so task.CreatorID == sched.CreatorID == userID for any legal bind.
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
		if err := service.ValidateIntervalForWrite(effCron, effIntervalDays, effIntervalMonths); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
			return
		}
		if err := service.ValidateScheduleAnchors(effCron, effIntervalDays, effIntervalMonths, effDayOfWeek, effDayOfMonth); err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
			return
		}
		nextRun, err := service.NextRunInitial(effCron, effIntervalDays, effIntervalMonths, effRunTime, effDayOfWeek, effDayOfMonth, timezone.Now())
		if err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
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

	resultScheduleID := sched.ID
	var resultNextRunAt *time.Time

	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		var task model.SummaryTask
		var oldScheduleID *int64
		// PR#62 r5 (Blocker1a + J2 lock-order): lock the TARGET schedule row FOR
		// UPDATE first, BEFORE loading/locking the task. This (1) serializes all
		// concurrent binds against this schedule so the boundCount check below is
		// race-free even without a DB unique constraint, and (2) establishes the
		// schedule->task lock order that matches the scheduler
		// (worker/scheduler.go locks schedule then bound task), removing the
		// previous task->schedule vs schedule->task deadlock window. We re-read the
		// stored config under this lock so the Blocker2 stored-config guard below
		// validates a consistent snapshot.
		lockedSched, err := lockScheduleForUpdate(tx, sched.ID, spaceID)
		if err != nil {
			return err
		}
		if req.Scope == "task" {
			if req.TaskID == nil {
				return errTaskScopeMissingTaskID
			}
			task, err = loadTaskForTaskScope(tx, spaceID, userID, *req.TaskID)
			if err != nil {
				return err
			}
			// PR#62 r5 Blocker2 (lml2468 Y1-bis): fail-closed multi-person guard on
			// the participant set that will ACTUALLY take effect for this bind.
			//   - req.Participants != nil  -> validated at the top of the handler
			//     (errMultiPersonNotSupported / 40015) against req.Participants.
			//   - req.Participants == nil   -> the bind reuses the schedule's STORED
			//     participant_config, which the top-level check skips entirely. We now
			//     validate that stored config here, under the schedule's FOR UPDATE
			//     lock, with the identical口径 (subset of {creator}). creator basis is
			//     userID: loadTaskForTaskScope already enforced task.CreatorID == userID
			//     and sched.CreatorID == userID was enforced above, so a legal bind has
			//     task.CreatorID == sched.CreatorID == userID. Placing the check here
			//     (inside the tx, after loadTaskForTaskScope, only when scope==task)
			//     guarantees a pure non-binding field update is never falsely rejected,
			//     while any bind of a schedule whose stored config contains a
			//     non-creator is refused (40015) before schedule_id is written.
			if req.Participants == nil && !storedParticipantConfigSubsetOfCreator(lockedSched.ParticipantConfig, userID) {
				return errMultiPersonNotSupported
			}
			if task.ScheduleID != nil && *task.ScheduleID != sched.ID {
				oldID := *task.ScheduleID
				oldScheduleID = &oldID
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
		}

		if req.Scope == "task" && (task.ScheduleID == nil || *task.ScheduleID != sched.ID) {
			if err := tx.Model(&model.SummaryTask{}).
				Where("id = ? AND space_id = ?", task.ID, spaceID).
				Update("schedule_id", sched.ID).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&model.SummarySchedule{}).
			Where("id = ?", sched.ID).
			Updates(updates).Error; err != nil {
			return err
		}
		if oldScheduleID != nil {
			now := timezone.Now()
			// Bug2 (PR#62 Jerry-Xin r3): rebinding a task moves it off the old
			// schedule and soft-deletes that schedule. This must respect the
			// same ownership + exclusivity rule as DeleteSummary's cascade
			// (task.go DeleteSummary). Without it a caller could soft-delete a
			// schedule they do not own, or one still bound to another task.
			// The current task has already been rebound to sched.ID above, so
			// the old schedule is unbound from this task already; we only soft
			// delete it when (1) the caller is its creator AND (2) no other
			// live task still binds it. Otherwise we leave the old schedule
			// alone (the task is rebound either way).
			//
			// Bug4 (PR#62 r4 lml2468 J2): read oldSched FOR UPDATE so the row is
			// locked for the whole window below. Previously a plain First() left a
			// race between the otherBound count and the soft-delete: a concurrent
			// rebind could attach another task to oldSched after we counted 0 and
			// before we soft-deleted, deleting a schedule that just became bound
			// again. Locking the row serializes lock+count+soft-delete in one
			// transaction window (same FOR UPDATE pattern used elsewhere here).
			var oldSched model.SummarySchedule
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND deleted_at IS NULL", *oldScheduleID).
				First(&oldSched).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				// old schedule already gone; nothing to soft-delete
			} else {
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
					// Not the creator, or still bound by another live task: do
					// not delete someone else's / still-in-use schedule. The
					// task is already rebound, so just leave the old schedule.
					log.Printf("[handler] UpdateSchedule: old schedule %d not soft-deleted (caller=%s creator=%s otherBound=%d); unbind-only", oldSched.ID, userID, oldSched.CreatorID, otherBound)
				}
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
	// 方案A (PR#62 yujiawei r3): API-layer fail-closed multi-person guard on the
	// bound task. The task is already locked FOR UPDATE above, so we count its
	// SummaryParticipant rows under the same lock snapshot and with the exact
	// same口径 as the worker Method A guard (participantCount > 1). If the task
	// being bound is a team task, refuse to attach a schedule to it -- otherwise
	// the schedule would be created/updated but the scheduler would skip it
	// every cycle, leaving the user with a silently dead timer.
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
	if sched.CreatorID != userID {
		bizErr(c, service.NewBizError(40004, "无权限删除", http.StatusForbidden))
		return
	}

	now := timezone.Now()
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&sched).Update("deleted_at", &now).Error; err != nil {
			return err
		}
		return tx.Model(&model.SummaryTask{}).
			Where("schedule_id = ? AND deleted_at IS NULL", sched.ID).
			Update("schedule_id", nil).Error
	}); err != nil {
		log.Printf("[handler] DeleteSchedule error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: err.Error()})
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

	updates := map[string]interface{}{}
	if req.IsActive {
		// CRITICAL: recompute next_run_at for ALL recurrence types on re-enable.
		// Previously only cron was recomputed, so an interval task (cron_expr
		// empty) kept its stale, already-past next_run_at and fired immediately
		// on the next scan. Route through the same NextRunWithInterval used by
		// create/update/scheduler so the next run is always at least one full
		// interval (or next cron tick) into the future.
		nextRun, err := service.NextRunWithInterval(sched.CronExpr, sched.IntervalDays, sched.IntervalMonths, sched.RunTime, sched.DayOfWeek, sched.DayOfMonth, timezone.Now())
		if err != nil {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40011, Message: err.Error()})
			return
		}
		updates["is_active"] = 1
		updates["next_run_at"] = nextRun
	} else {
		updates["is_active"] = 0
	}

	if err := h.db.Model(&sched).Updates(updates).Error; err != nil {
		log.Printf("[handler] ToggleSchedule error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: err.Error()})
		return
	}

	ok(c, gin.H{
		"schedule_id": sched.ID,
		"is_active":   updates["is_active"],
	})
}
