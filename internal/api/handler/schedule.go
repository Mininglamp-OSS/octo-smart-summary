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
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ScheduleHandler handles schedule endpoints.
type ScheduleHandler struct {
	db   *gorm.DB
	imDB *gorm.DB // IM database; used for user-access validation on write. nil = access check bypassed (test paths).
	// featureTeamSchedule, when true, bypasses the multi-person rejection guards
	// (FEATURE_TEAM_SCHEDULE). Default false keeps the existing 40015 behavior.
	featureTeamSchedule bool
}

// NewScheduleHandler creates a new ScheduleHandler.
// Retained for backwards compatibility with tests that don't need IM-DB access checks.
func NewScheduleHandler(db *gorm.DB) *ScheduleHandler {
	return &ScheduleHandler{db: db}
}

// NewScheduleHandlerWithFlag creates a ScheduleHandler with the team-schedule
// feature flag explicitly set. When featureTeamSchedule is true the multi-person
// rejection guards are bypassed so multi-participant schedules can be created.
// Retained for backwards compatibility with tests that don't need IM-DB access checks.
func NewScheduleHandlerWithFlag(db *gorm.DB, featureTeamSchedule bool) *ScheduleHandler {
	return &ScheduleHandler{db: db, featureTeamSchedule: featureTeamSchedule}
}

// NewScheduleHandlerWithIMDB is the production constructor; wires the IM DB so
// Create/Update can run source-access validation. imDB==nil is tolerated and
// disables the access check (see pipeline.ValidateUserAccessibleSources).
func NewScheduleHandlerWithIMDB(db, imDB *gorm.DB, featureTeamSchedule bool) *ScheduleHandler {
	return &ScheduleHandler{db: db, imDB: imDB, featureTeamSchedule: featureTeamSchedule}
}

// sourceReqsToPipelineRefs converts handler-private sourceReq entries into the
// pipeline access-check shape. Kept inline so sourceReq stays package-private.
func sourceReqsToPipelineRefs(in []sourceReq) []pipeline.SourceRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]pipeline.SourceRef, 0, len(in))
	for _, s := range in {
		out = append(out, pipeline.SourceRef{
			SourceType: s.SourceType,
			SourceID:   s.SourceID,
			SourceName: s.SourceName,
		})
	}
	return out
}

// respondSourceAccessDenied writes the canonical 40017 response for a
// non-empty missing-sources list: HTTP 403, code 40017, generic message, and
// data.missing_sources with per-entry (source_type, source_id, source_name)
// so the frontend can pinpoint offenders without trusting client-supplied text.
func respondSourceAccessDenied(c *gin.Context, missing []pipeline.SourceRef) {
	items := make([]map[string]interface{}, 0, len(missing))
	for _, m := range missing {
		items = append(items, map[string]interface{}{
			"source_type": m.SourceType,
			"source_id":   m.SourceID,
			"source_name": m.SourceName,
		})
	}
	c.JSON(http.StatusForbidden, apiResponse{
		Code:    40017,
		Message: "存在无权访问或不存在的来源",
		Data:    map[string]interface{}{"missing_sources": items},
	})
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
	// ConfirmPolicy: 0=AUTO (no confirm), 1=CONFIRM (V5 one-time schedule-level
	// confirm). Pointer so "not sent" is distinguishable from an explicit 0; the
	// handler defaults multi-person schedules to CONFIRM. confirm_lead_minutes is
	// intentionally NOT accepted (deprecated under V5 — one-time confirm has no lead).
	ConfirmPolicy *int   `json:"confirm_policy"`
	Scope         string `json:"scope,omitempty"`
	TaskID        *int64 `json:"task_id,omitempty"`
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
	ConfirmPolicy  *int             `json:"confirm_policy"`
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

// resolveCreateConfirmPolicy resolves the V5 confirm_policy for CreateSchedule.
// An explicit request value wins (clamped to known constants). Otherwise a
// multi-person schedule (any configured participant other than the creator)
// defaults to CONFIRM (1) and a single-person schedule defaults to AUTO (0).
func resolveCreateConfirmPolicy(reqPolicy *int, participants []participantReq, creatorID string) int {
	if reqPolicy != nil {
		if *reqPolicy == model.SchedConfirmAuto {
			return model.SchedConfirmAuto
		}
		return model.SchedConfirmRequire
	}
	if participantsSubsetOfCreator(participants, creatorID) {
		return model.SchedConfirmAuto
	}
	return model.SchedConfirmRequire
}

// buildInitialConfirmConfig builds the V5 object-form participant_config for a
// CONFIRM schedule at create time: every member (creator included, Q2) starts
// confirmed=false and the gate is not passed. The creator is always present so
// it also gets a confirm toggle and is never auto-accepted.
func buildInitialConfirmConfig(participants []participantReq, creatorID string) (model.JSON, error) {
	cfg := model.ScheduleParticipantConfig{Participants: []model.ScheduleParticipantEntry{}}
	seen := map[string]struct{}{}
	add := func(uid, name string) {
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		cfg.Participants = append(cfg.Participants, model.ScheduleParticipantEntry{
			UserID:    uid,
			UserName:  name,
			Confirmed: false,
		})
	}
	add(creatorID, "")
	for _, p := range participants {
		add(p.UserID, p.UserName)
	}
	cfg.RecomputeGate(creatorID)
	return cfg.Marshal()
}

// loadTaskParticipantReqs reads the task's in-roster participants (creator +
// collaborators) from summary_participant and returns them as []participantReq.
// Declined members are excluded so a member who opted out of the source task does
// not get re-added to the schedule roster. The creator is always part of the
// schedule roster regardless (callers seed it), so we keep every non-declined
// row here as the collaborator source of truth.
func loadTaskParticipantReqs(tx *gorm.DB, taskID int64) ([]participantReq, error) {
	var parts []model.SummaryParticipant
	if err := tx.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND status <> ?", taskID, model.ParticipantDeclined).
		Order("id ASC").
		Find(&parts).Error; err != nil {
		return nil, err
	}
	out := make([]participantReq, 0, len(parts))
	for _, p := range parts {
		if p.UserID == "" {
			continue
		}
		out = append(out, participantReq{UserID: p.UserID, UserName: p.UserName})
	}
	return out, nil
}

// effectiveConfirmParticipants merges the request-supplied participants with the
// task's real participant roster. Two callers with DIFFERENT membership-authority
// semantics share it, controlled by allowReqOnlyAdditions:
//
//   - CreateSchedule / "manual -> scheduled" (allowReqOnlyAdditions=false): the
//     task roster is the SOLE membership authority. A conversion that did NOT
//     forward participants (frontend gap) still captures every collaborator from
//     the task; req participants ONLY contribute user_name overrides for ids the
//     task roster already contains. A request can NEVER add a user who is not a
//     real task participant -- otherwise a creator-only task could be inflated
//     into a bogus multi-person CONFIRM schedule by a crafted/stale request body.
//
//   - UpdateSchedule member-change (Q3) (allowReqOnlyAdditions=true): the request
//     supplies the edited member list (e.g. adds u3), so req ids not yet in the
//     task roster are legitimately ADDED; the task roster is still unioned in and
//     RETAINED, so an explicit-but-partial req cannot silently drop existing task
//     collaborators. (Note: this means the current implementation does NOT support
//     removing a task collaborator from the schedule roster via a shrunken req --
//     task members are always kept. Add an explicit removal semantics + test if
//     that is ever required.)
//
// The union is keyed by user_id; the creator is added by the confirm-config builders, not here.
func effectiveConfirmParticipants(reqParticipants, taskParticipants []participantReq, allowReqOnlyAdditions bool) []participantReq {
	nameByID := map[string]string{}
	for _, p := range reqParticipants {
		if p.UserID != "" && p.UserName != "" {
			nameByID[p.UserID] = p.UserName
		}
	}
	seen := map[string]struct{}{}
	out := make([]participantReq, 0, len(taskParticipants)+len(reqParticipants))
	add := func(p participantReq) {
		if p.UserID == "" {
			return
		}
		if _, ok := seen[p.UserID]; ok {
			return
		}
		seen[p.UserID] = struct{}{}
		if name, ok := nameByID[p.UserID]; ok && name != "" {
			p.UserName = name
		}
		out = append(out, p)
	}
	// Task roster first (authoritative membership).
	for _, p := range taskParticipants {
		add(p)
	}
	// req-only ids are added ONLY for the member-change path (Q3); for the
	// create/manual->scheduled path they are deliberately ignored.
	if allowReqOnlyAdditions {
		for _, p := range reqParticipants {
			add(p)
		}
	}
	return out
}

// mergeConfirmRoster implements V5/Q3 "member change only re-confirms new members":
// it rebuilds the confirm roster from the new participant list (req), PRESERVING the
// confirm state of members still present in `stored`, defaulting members not in
// `stored` (newly added) to confirmed=false, and always keeping the creator. The
// caller recomputes the gate. Members removed from the roster naturally drop their confirm state.
func mergeConfirmRoster(stored model.ScheduleParticipantConfig, participants []participantReq, creatorID string) model.ScheduleParticipantConfig {
	out := model.ScheduleParticipantConfig{Participants: []model.ScheduleParticipantEntry{}}
	seen := map[string]struct{}{}
	add := func(uid, name string) {
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		entry := model.ScheduleParticipantEntry{UserID: uid, UserName: name, Confirmed: false}
		if prev := stored.FindParticipant(uid); prev != nil {
			// Existing member: keep its confirm state (Q3).
			entry.Confirmed = prev.Confirmed
			entry.ConfirmedAt = prev.ConfirmedAt
			if entry.UserName == "" {
				entry.UserName = prev.UserName
			}
		}
		out.Participants = append(out.Participants, entry)
	}
	// Creator is always part of the roster (Q2).
	add(creatorID, "")
	for _, p := range participants {
		add(p.UserID, p.UserName)
	}
	return out
}

// storedParticipantConfigSubsetOfCreator applies participantsSubsetOfCreator to a schedule's
// stored participant_config, so a bind reusing stored config (req.Participants==nil) is also
// rejected when it contains a non-creator. Empty config is a subset (PASS).
func storedParticipantConfigSubsetOfCreator(raw model.JSON, creatorID string) bool {
	if len(raw) == 0 {
		return true
	}
	// V5 §3.1: participant_config is the object form
	// {"participants":[...],"confirm_gate_passed":...}. Use the single normalizer
	// so the V5 object form parses correctly; the old bare-array Unmarshal failed
	// on the object form and fell into the fail-closed path, wrongly rejecting
	// creator-only V5 schedules when FEATURE_TEAM_SCHEDULE is off.
	cfg := model.ParseScheduleParticipantConfig(raw)
	for _, uid := range cfg.EffectiveUserIDs(creatorID) {
		if uid != creatorID {
			return false
		}
	}
	return true
}

// validateEffectiveParticipantsSubsetOfCreator is the single post-load check that
// the participant set actually taking effect (req if sent, else stored config)
// is a subset of {creatorID}. creatorID must be the loaded task.CreatorID.
func validateEffectiveParticipantsSubsetOfCreator(featureTeamSchedule bool, reqParticipants []participantReq, storedConfig model.JSON, creatorID string) error {
	if featureTeamSchedule {
		// Team schedules enabled: multi-person is allowed, skip the subset guard.
		return nil
	}
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

// loadBoundTaskForScheduleUpdate validates the schedule->task binding on the
// non-rebind update/toggle path. Under the 1->N model a schedule owns many tasks
// (full run history), so we no longer require "exactly one"; we load the LATEST
// live bound task and validate ownership/consistency against it. The latest task
// is the representative used for the single-person guard and creator check.
func loadBoundTaskForScheduleUpdate(tx *gorm.DB, lockedSched model.SummarySchedule, userID string) (model.SummaryTask, error) {
	var task model.SummaryTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("schedule_id = ? AND deleted_at IS NULL", lockedSched.ID).
		Order("id DESC").
		First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SummaryTask{}, service.NewBizError(40008, "定时配置已失去绑定，请刷新后重试", http.StatusNotFound)
		}
		return model.SummaryTask{}, err
	}
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

	// Source-access check (same contract as UpdateSchedule): reject sources the
	// user cannot see before persisting. imDB==nil bypasses (test path).
	if len(req.Sources) > 0 {
		if missing, err := pipeline.ValidateUserAccessibleSources(c.Request.Context(), userID, h.imDB, sourceReqsToPipelineRefs(req.Sources)); err != nil {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "source access check failed: " + err.Error()})
			return
		} else if len(missing) > 0 {
			respondSourceAccessDenied(c, missing)
			return
		}
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

	// V5 confirm_policy resolution. Multi-person schedules default to CONFIRM
	// (one-time, schedule-level confirm); single-person (subset-of-creator) defaults
	// to AUTO. An explicit confirm_policy in the request always wins.
	confirmPolicy := resolveCreateConfirmPolicy(req.ConfirmPolicy, req.Participants, userID)
	// For a CONFIRM schedule, persist participant_config in the V5 object form with an
	// embedded confirm state (all members confirmed=false, creator included per Q2,
	// gate not passed). AUTO keeps the legacy bare-array shape (normalized on read).
	if confirmPolicy != model.SchedConfirmAuto {
		if normalized, err := buildInitialConfirmConfig(req.Participants, userID); err == nil {
			participantConfig = normalized
		}
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
		ConfirmPolicy:     confirmPolicy,
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

		task, err := loadTaskForTaskScope(tx, spaceID, userID, *req.TaskID, h.featureTeamSchedule)
		if err != nil {
			return err
		}

		// TOCTOU: bail out retryable if the binding changed after the pre-read.
		if !int64PtrEqual(task.ScheduleID, peekedExisting) {
			return errRebindConcurrentModified
		}

		// Single-person guard: configured participants must be a subset of {creator}.
		// Bypassed when team schedules are enabled.
		if !h.featureTeamSchedule && !participantsSubsetOfCreator(req.Participants, task.CreatorID) {
			return errMultiPersonNotSupported
		}

		// Root-cause fix: a "manual -> scheduled" conversion (frontend detail-page
		// entry) historically called createSchedule WITHOUT forwarding participants,
		// so participant_config above (computed outside the tx from the possibly-empty
		// req.Participants) degenerated to creator-only and the scheduled run lost every
		// collaborator. Now that the task is loaded+locked, rebuild the CONFIRM roster
		// from the task's REAL participant list (union with any req participants) so the
		// schedule keeps all collaborators. A genuinely single-person task (creator-only)
		// stays single-person and is not inflated.
		taskParts, err := loadTaskParticipantReqs(tx, task.ID)
		if err != nil {
			return err
		}
		// Create / manual->scheduled: task roster is the SOLE membership authority
		// (allowReqOnlyAdditions=false) so a crafted/stale req cannot inflate a
		// creator-only task into a bogus multi-person CONFIRM schedule.
		effParts := effectiveConfirmParticipants(req.Participants, taskParts, false)
		// Re-resolve confirm_policy against the effective roster: a multi-person task
		// with an empty req must still default to CONFIRM (unless explicitly AUTO).
		confirmPolicy = resolveCreateConfirmPolicy(req.ConfirmPolicy, effParts, task.CreatorID)
		sched.ConfirmPolicy = confirmPolicy
		if confirmPolicy != model.SchedConfirmAuto {
			if normalized, err := buildInitialConfirmConfig(effParts, task.CreatorID); err == nil {
				sched.ParticipantConfig = normalized
			}
		} else if len(effParts) > 0 {
			// AUTO with a non-empty effective roster: persist the bare-array shape so a
			// degraded (empty req) AUTO bind still records collaborators.
			if b, err := json.Marshal(effParts); err == nil {
				sched.ParticipantConfig = model.JSON(b)
			}
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
			// 1->N: a schedule legitimately owns many tasks (run history), so we no
			// longer reject reusing a schedule that already has other bound tasks.
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
				"confirm_policy":     sched.ConfirmPolicy,
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
	// fail-closed hard gate: GET requests are NOT caught by StrictSpaceMiddleware,
	// and SpaceID is `not null default ''`, so rows with space_id='' may exist;
	// querying `space_id=''` would MATCH them, leaking cross-space schedules.
	// Reject an empty X-Space-Id before any query.
	if spaceID == "" {
		bizErr(c, service.NewBizError(40008, "定时配置不存在", http.StatusNotFound))
		return
	}

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
			"confirm_policy":     s.ConfirmPolicy,
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
	// fail-closed hard gate: GET requests are NOT caught by StrictSpaceMiddleware,
	// and SpaceID is `not null default ''`, so rows with space_id='' may exist;
	// querying `space_id=''` would MATCH them, leaking a cross-space schedule.
	// Reject an empty X-Space-Id before any query.
	if spaceID == "" {
		bizErr(c, service.NewBizError(40008, "定时配置不存在", http.StatusNotFound))
		return
	}
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
		"confirm_policy":     sched.ConfirmPolicy,
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

// ConfirmSchedule handles POST /api/v1/summary-schedules/:id/confirm
//
// V5 §4 (one-time, schedule-level confirm): the caller confirms participation in
// THIS schedule (not a per-round task). It read-modify-writes the schedule's
// participant_config under a FOR UPDATE row lock (竞态-2 defense, serializes with
// UpdateSchedule), setting the caller's confirmed=true / confirmed_at=now. When
// every roster member (creator included, Q2) is confirmed, confirm_gate_passed is
// set true. Idempotent: confirming again is a no-op success. AUTO schedules need
// no confirmation (returns success without changing state).
func (h *ScheduleHandler) ConfirmSchedule(c *gin.Context) {
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

	var gatePassed bool
	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		lockedSched, err := lockScheduleForUpdate(tx, schedID, spaceID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return service.NewBizError(40008, "定时配置不存在", http.StatusNotFound)
			}
			return err
		}

		cfg := model.ParseScheduleParticipantConfig(lockedSched.ParticipantConfig)
		cfg.EnsureCreatorEntry(lockedSched.CreatorID)

		// The caller must be part of the roster (creator or a configured
		// participant) REGARDLESS of confirm policy. This membership check runs
		// before the AUTO fast-path so a non-member can never get a 200 from
		// confirm (previously AUTO returned success before any membership check,
		// letting outsiders probe/confirm AUTO schedules).
		entry := cfg.FindParticipant(userID)
		if entry == nil {
			return service.NewBizError(40003, "你不在该定时的参与名单中", http.StatusForbidden)
		}

		// AUTO schedules have no confirm step: a roster member calling confirm is a
		// no-op success (gate is implicitly passed), but we only reach here after proving membership above.
		if lockedSched.ConfirmPolicy == model.SchedConfirmAuto {
			gatePassed = true
			return nil
		}

		if !entry.Confirmed {
			now := timezone.Now()
			entry.Confirmed = true
			entry.ConfirmedAt = &now
		}
		cfg.RecomputeGate(lockedSched.CreatorID)
		gatePassed = cfg.ConfirmGatePassed

		marshaled, err := cfg.Marshal()
		if err != nil {
			return err
		}
		return tx.Model(&model.SummarySchedule{}).
			Where("id = ?", lockedSched.ID).
			Update("participant_config", marshaled).Error
	})
	if txErr != nil {
		if biz, ok := txErr.(*service.BizError); ok {
			bizErr(c, biz)
			return
		}
		log.Printf("[handler] ConfirmSchedule error: %v", txErr)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: txErr.Error()})
		return
	}

	ok(c, gin.H{
		"schedule_id":         schedID,
		"confirmed":           true,
		"confirm_gate_passed": gatePassed,
	})
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
	// Bypassed when team schedules are enabled.
	if !h.featureTeamSchedule && req.Participants != nil && !participantsSubsetOfCreator(req.Participants, userID) {
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
		// Source-access check: reject sources the user cannot see (unbound / no
		// membership / deleted / archived non-thread). imDB==nil bypasses (test path).
		if missing, err := pipeline.ValidateUserAccessibleSources(c.Request.Context(), userID, h.imDB, sourceReqsToPipelineRefs(req.Sources)); err != nil {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "source access check failed: " + err.Error()})
			return
		} else if len(missing) > 0 {
			respondSourceAccessDenied(c, missing)
			return
		}
		b, _ := json.Marshal(req.Sources)
		updates["source_config"] = model.JSON(b)
	}
	// participant_config / confirm_policy reset+merge is done INSIDE the tx where the
	// FOR UPDATE-locked stored config is available (so we can preserve existing
	// confirm state under Q3). See the confirm-state block below.
	if req.ConfirmPolicy != nil {
		if *req.ConfirmPolicy == model.SchedConfirmAuto {
			updates["confirm_policy"] = model.SchedConfirmAuto
		} else {
			updates["confirm_policy"] = model.SchedConfirmRequire
		}
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
			task, err = loadTaskForTaskScope(tx, spaceID, userID, *req.TaskID, h.featureTeamSchedule)
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
			if err := validateEffectiveParticipantsSubsetOfCreator(h.featureTeamSchedule, req.Participants, lockedSched.ParticipantConfig, task.CreatorID); err != nil {
				return err
			}

			// 1->N: a schedule may own many tasks (history); no "already bound" rejection.
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

		// V5 confirm-state reset/merge (§4.2 / §4.3 / Q3). Computed from the FOR UPDATE
		// locked stored config so concurrent confirm-API writes are serialized.
		//
		// Effective confirm_policy after this update (req wins, else stored).
		effConfirmPolicy := lockedSched.ConfirmPolicy
		if req.ConfirmPolicy != nil {
			if *req.ConfirmPolicy == model.SchedConfirmAuto {
				effConfirmPolicy = model.SchedConfirmAuto
			} else {
				effConfirmPolicy = model.SchedConfirmRequire
			}
		}

		// Detect a manual->scheduled / re-activation transition. UpdateSchedule
		// re-activates an inactive schedule (CreateSchedule reuse sets is_active=1; the
		// reuse path goes through CreateSchedule, but an UpdateSchedule that flips an
		// inactive schedule active is treated as "turning it (back) into a live timer").
		// Per §4.2 this triggers a FULL re-confirm (every member, creator AND others,
		// confirmed=false). We approximate "became scheduled/active" as: stored is_active
		// != 1 and this update keeps/sets it usable. is_active is not directly settable
		// via UpdateSchedule (ToggleSchedule owns that), so the only full-reset trigger
		// here is the AUTO->CONFIRM policy switch (manual/auto schedule converted to a confirm-required one).
		policyBecameConfirm := effConfirmPolicy != model.SchedConfirmAuto &&
			lockedSched.ConfirmPolicy == model.SchedConfirmAuto

		// Root-cause fix (parity with CreateSchedule): on the task-scope path, a
		// "manual -> scheduled / enable timer" conversion may arrive WITHOUT
		// req.Participants (frontend gap). Backfill the effective roster from the
		// task's REAL participants so the schedule keeps every collaborator instead of
		// degenerating to the stored (possibly creator-only) config. Only meaningful
		// for the task-scope path where `task` is loaded; for scope="" we leave the stored roster untouched.
		var taskParts []participantReq
		if req.Scope == "task" {
			var lerr error
			taskParts, lerr = loadTaskParticipantReqs(tx, task.ID)
			if lerr != nil {
				return lerr
			}
		}
		// effRosterParticipants is the roster source used below: an explicit req wins
		// (union with the task roster so an explicit-but-partial req still keeps task
		// collaborators); otherwise fall back to the task roster alone. nil means
		// "no roster info available" (non-task-scope, no req) -> keep stored.
		var effRosterParticipants []participantReq
		if req.Participants != nil {
			// Member-change (Q3): the request IS the authoritative new roster, so
			// req-only ids (e.g. a newly added u3) are legitimately added
			// (allowReqOnlyAdditions=true), unioned with the task roster.
			effRosterParticipants = effectiveConfirmParticipants(req.Participants, taskParts, true)
		} else if len(taskParts) > 0 {
			// No explicit req roster: pure task-roster backfill (manual->scheduled
			// gap). task is authoritative; nothing req-only to add.
			effRosterParticipants = effectiveConfirmParticipants(nil, taskParts, false)
		}

		if effConfirmPolicy == model.SchedConfirmAuto {
			// AUTO: persist participants as the legacy bare-array shape. Prefer the
			// effective roster (req union task) so a degraded (empty req) bind still
			// records collaborators; fall back to stored when nothing new is known.
			if effRosterParticipants != nil {
				b, _ := json.Marshal(effRosterParticipants)
				updates["participant_config"] = model.JSON(b)
			}
		} else {
			// CONFIRM: maintain the V5 object-form participant_config with embedded confirm state.
			creatorID := lockedSched.CreatorID
			stored := model.ParseScheduleParticipantConfig(lockedSched.ParticipantConfig)

			var newCfg model.ScheduleParticipantConfig
			if effRosterParticipants != nil {
				// Member change (Q3): rebuild the roster from the EFFECTIVE participants
				// (req union task roster), preserving the confirm state of members still
				// present, defaulting NEW members to confirmed=false, then recompute the
				// gate. The creator is always kept.
				newCfg = mergeConfirmRoster(stored, effRosterParticipants, creatorID)
			} else {
				// No member change: start from the stored confirm roster (normalized).
				newCfg = stored
				newCfg.EnsureCreatorEntry(creatorID)
			}

			if policyBecameConfirm {
				// §4.2 manual/auto -> confirm conversion: FULL re-confirm. Reset EVERY
				// member (creator included) to confirmed=false and clear confirmed_at.
				for i := range newCfg.Participants {
					newCfg.Participants[i].Confirmed = false
					newCfg.Participants[i].ConfirmedAt = nil
				}
			}
			newCfg.RecomputeGate(creatorID)
			if marshaled, err := newCfg.Marshal(); err == nil {
				updates["participant_config"] = marshaled
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

func loadTaskForTaskScope(tx *gorm.DB, spaceID, userID string, taskID int64, featureTeamSchedule bool) (model.SummaryTask, error) {
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
	// When team schedules are enabled this guard is bypassed.
	if !featureTeamSchedule {
		participantCount, err := loadTaskParticipantCount(tx, task.ID)
		if err != nil {
			return model.SummaryTask{}, err
		}
		if participantCount > 1 {
			return model.SummaryTask{}, errMultiPersonNotSupported
		}
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
		// 1->N: soft-delete the WHOLE group of bound tasks (do NOT unbind). One batch
		// UPDATE, same tx as the schedule soft-delete -- a long-lived schedule can own
		// thousands of tasks, so never loop per-row. schedule_id is preserved on every
		// row so the deleted history stays attributable to its schedule. Subtables
		// (result/chunk/participant/personal_result) are left intact: they have no
		// soft-delete column and hard-deleting them would lose history.
		return tx.Model(&model.SummaryTask{}).
			Where("id IN ?", taskIDs).
			Updates(map[string]interface{}{
				"status":     -1,
				"deleted_at": now,
			}).Error
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
				if err := validateEffectiveParticipantsSubsetOfCreator(h.featureTeamSchedule, nil, lockedSched.ParticipantConfig, task.CreatorID); err != nil {
					return err
				}
				// V5 §4.2: re-activating a CONFIRM schedule (is_active 0->1) triggers a FULL
				// re-confirm — every member (creator included, Q2) must confirm again for
				// this activation. Reset confirm state inside the same row lock so it does
				// not race the confirm API. AUTO schedules carry no confirm state.
				if lockedSched.ConfirmPolicy != model.SchedConfirmAuto {
					cfg := model.ParseScheduleParticipantConfig(lockedSched.ParticipantConfig)
					cfg.EnsureCreatorEntry(lockedSched.CreatorID)
					for i := range cfg.Participants {
						cfg.Participants[i].Confirmed = false
						cfg.Participants[i].ConfirmedAt = nil
					}
					cfg.RecomputeGate(lockedSched.CreatorID)
					if marshaled, err := cfg.Marshal(); err == nil {
						updates["participant_config"] = marshaled
					}
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
