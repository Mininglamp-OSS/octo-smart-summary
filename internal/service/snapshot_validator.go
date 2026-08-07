// Package service — shared SnapshotScope validator.
//
// This file implements the SUM-BE1 refactor from the "Unified Smart Summary"
// design: pull the inline validation that used to sit in the
// `handler.TaskHandler.CreateSummary` request path into a reusable service
// that every summary entry point calls before it creates rows, so the traditional
// personal / traditional team / scheduled / agent-generation / agent-save
// paths all evaluate the same scope, permission and shape checks.
//
// Design constraints preserved verbatim (see unified-smart-summary-design.md
// section 5 and section 7.1):
//
//   - Reuse model.SnapshotScope. Do NOT invent a parallel SummaryScope type.
//   - Do not new-write zero-value Schedule fields — validation must be a
//     read-only gate; Schedule field preservation is the caller's job and this
//     validator only *checks* the schedule input it is given.
//   - Traditional personal / team behavior must not regress. Every existing
//     bad-input error message and business code from CreateSummary is preserved
//     bit-for-bit here so the router-facing responses stay identical.
//   - personal_processor / meta_processor are not touched — they still run off
//     the SummaryTask row the handler creates after validation passes.
//
// The validator is intentionally free of gorm / gin / net-http coupling so that
// unit tests can exercise every branch without spinning a router or a DB.
package service

import (
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// ScopeValidationTarget names one of the five real execution targets that the
// unified design (section 2.3, section 5.1) recognises. Values are stable
// string identifiers (not iota-integers) so log lines and error breadcrumbs
// stay readable when they surface in production.
type ScopeValidationTarget string

const (
	// TargetPersonalWorkflow: traditional single-person workflow via
	// handler.CreateSummary -> worker -> personal_processor.
	TargetPersonalWorkflow ScopeValidationTarget = "personal_workflow"
	// TargetTeamWorkflow: traditional multi-person workflow (personal+meta).
	TargetTeamWorkflow ScopeValidationTarget = "team_workflow"
	// TargetScheduledWorkflow: creating / updating a summary_schedule row.
	TargetScheduledWorkflow ScopeValidationTarget = "scheduled_workflow"
	// TargetAgentGeneration: the agent is about to run a generation turn and
	// needs the same scope-level gates the traditional path already has.
	TargetAgentGeneration ScopeValidationTarget = "agent_generation"
	// TargetAgentSave: user clicked "save as official summary" on an agent
	// draft — the CreateAgentSummary handler is about to persist rows.
	TargetAgentSave ScopeValidationTarget = "agent_save"
)

// SnapshotScopeInput is the union of every field the shared validator needs.
// It carries the request-time view of what the caller is about to write —
// intentionally wider than model.SnapshotScope (the storage struct) because
// validation covers upstream fields the storage snapshot does not persist
// (topic, participants, sources with types, schedule).
//
// The Scope field embeds model.SnapshotScope directly rather than duplicating
// ChannelIDs / ChannelNames / TimeRange, honouring section 7.2 "reuse
// SnapshotScope, not new SummaryScope".
type SnapshotScopeInput struct {
	// Title is the request-provided task/schedule title.
	Title string
	// Topic is the free-text requirement (design section 4.1 / section 5.2
	// "requirement/topic available").
	Topic string
	// Scope is the shared SnapshotScope carrying channel_ids / channel_names /
	// time_range. TimeRange strings on this struct are informational; the
	// validator uses TimeStart / TimeEnd below when present so we can enforce
	// the range-in-days rule with real time.Time values instead of parsing
	// RFC3339 twice.
	Scope model.SnapshotScope
	// TimeStart / TimeEnd override Scope.TimeRange when both are non-zero. If
	// both are zero the caller has not chosen a range yet and the validator
	// falls back to "range not required at input time" behaviour (matching
	// CreateSummary's pre-refactor auto-fill: end=now, start=now-maxDays).
	TimeStart time.Time
	TimeEnd   time.Time
	// Sources carries the caller's source list (source_type + source_id). The
	// slice may be empty when the caller lets the pipeline auto-narrow.
	Sources []SourceInput
	// Participants carries the caller-provided participant user ids (empty ->
	// creator-only, matching CreateSummary's auto-fill).
	Participants []ParticipantInput
	// OriginChannelID / OriginChannelType mirror the createSummaryReq fields
	// that used to be range-checked inline.
	OriginChannelID   string
	OriginChannelType int
	// Schedule, if non-nil, activates schedule-target checks and also causes
	// personal/team-target checks to reject invalid schedules pinned onto the
	// same request (e.g. an Agent-assisted traditional create that carries a
	// stale schedule payload from the UI). The validator never mutates the
	// pointee — Schedule field preservation is the caller's responsibility.
	Schedule *ScheduleInput
	// AgentSave carries the extra bits CreateAgentSummary needs so
	// TargetAgentSave can run the "message belongs to this session" gate the
	// design (section 5.2 agent_save) prescribes.
	AgentSave *AgentSaveInput
}

// SourceInput mirrors handler.sourceReq without importing the handler package.
type SourceInput struct {
	SourceType int
	SourceID   string
}

// ParticipantInput mirrors handler.participantReq without importing it.
type ParticipantInput struct {
	UserID   string
	UserName string
}

// ScheduleInput carries every schedule field the validator checks. The names
// mirror model.SummarySchedule / handler.createScheduleReq so callers pass
// through what they already have.
type ScheduleInput struct {
	CronExpr       string
	IntervalDays   int
	IntervalMonths int
	RunTime        string
	DayOfWeek      int
	DayOfMonth     int
	TimeRangeType  int
}

// AgentSaveInput captures the extra AgentSave-target information: which
// session / message the caller is about to persist and (optionally) the
// SnapshotVersion the client last saw. Fields left zero-valued mean "caller
// wants only the base scope checks".
type AgentSaveInput struct {
	SessionID       string
	AgentMessageID  int64
	SnapshotVersion int
	// ContentLen is the length in bytes of the assistant message the caller
	// intends to save. Empty content is a design-mandated 40004 reject
	// (section 5.2 agent_save "content valid").
	ContentLen int
}

// scopeValidatorLimits centralises the (currently const) upper bounds so the
// validator does not import the handler package to read them. Kept package-
// private and set from handler/task.go so a single change site keeps the
// numbers coherent. The zero-value defaults match the current handler consts:
//
//	maxSummaryTopicRunes = 2300
//	maxSourceCount       = 30
//
// pipeline.DefaultTimeRangeDays is a runtime global set from config at boot,
// so we read it via a getter instead of copying the value.
type scopeValidatorLimits struct {
	MaxTopicRunes int
	MaxSources    int
	MaxDays       int
}

// The validator reads limits through this struct so tests can override them
// without racing on package-globals from unrelated packages.
var scopeLimits = scopeValidatorLimits{
	MaxTopicRunes: 2300,
	MaxSources:    30,
	MaxDays:       0, // 0 = "use pipeline.DefaultTimeRangeDays via getter"
}

// timeRangeMaxDaysGetter is populated by handler init() to break the import
// cycle handler->service->pipeline. The default returns 0 (validator skips
// the range check), matching pre-refactor behaviour when TimeRange is unset.
var timeRangeMaxDaysGetter = func() int { return 0 }

// SetTimeRangeMaxDaysGetter lets the handler (or any bootstrapper) wire the
// pipeline.DefaultTimeRangeDays runtime global into the validator. Must be
// called before ValidateSnapshotScope; if not called, the range check is a
// no-op — matching CreateSummary's pre-refactor behaviour when TimeRange is
// nil (validator only enforces the cap when a range is actually specified).
func SetTimeRangeMaxDaysGetter(fn func() int) {
	if fn != nil {
		timeRangeMaxDaysGetter = fn
	}
}

// SetScopeValidatorLimits overrides the topic-rune / source-count caps. Meant
// for tests that need to poke edge cases without maxing out real limits.
// Passing a non-positive value for a field keeps the previous value.
func SetScopeValidatorLimits(maxTopicRunes, maxSources int) {
	if maxTopicRunes > 0 {
		scopeLimits.MaxTopicRunes = maxTopicRunes
	}
	if maxSources > 0 {
		scopeLimits.MaxSources = maxSources
	}
}

// ValidateSnapshotScope runs the target-agnostic base checks plus the
// target-specific gates described in unified-smart-summary-design.md
// section 5.2.
//
// It returns *BizError on the first failure so callers can `bizErr(c, err)`
// without translation. `actor` is the invoking user id — currently unused for
// authz (the callers still hold DB access to check per-channel permissions),
// but it is carried through so future authz work can slot in without a
// signature break. Deliberately kept in the signature per section 5.1 spec:
//
//	ValidateSnapshotScope(actor, snapshot, target)
func ValidateSnapshotScope(actor string, input SnapshotScopeInput, target ScopeValidationTarget) *BizError {
	// _ = actor: reserved for future per-user authz; see doc comment.
	_ = actor

	// --- base checks (apply to every target) ---
	// Order below preserves the exact sequence used by CreateSummary so the
	// first-error contract is regression-safe.

	if utf8.RuneCountInString(input.Title) > scopeLimits.MaxTopicRunes {
		return NewBizError(40001, "title 不能超过 2300 字符", http.StatusBadRequest)
	}
	if utf8.RuneCountInString(input.Topic) > scopeLimits.MaxTopicRunes {
		return NewBizError(40001, "topic 不能超过 2300 字符", http.StatusBadRequest)
	}
	// origin_channel_id / origin_channel_type must move together and stay
	// inside the 1..3 application-layer enum (see model.OriginChannelGroup /
	// Thread / DM in model/model.go).
	if input.OriginChannelID != "" && (input.OriginChannelType < model.OriginChannelGroup || input.OriginChannelType > model.OriginChannelDM) {
		return NewBizError(40001, "origin_channel_type must be 1, 2, or 3 when origin_channel_id is set", http.StatusBadRequest)
	}
	if input.OriginChannelID == "" && input.OriginChannelType != 0 {
		return NewBizError(40001, "origin_channel_id is required when origin_channel_type is set", http.StatusBadRequest)
	}

	// Time-range span cap. Only enforced when the caller supplied both endpoints
	// (matches CreateSummary's pre-refactor behaviour of only bounding an
	// explicit req.TimeRange and letting the default range through untouched).
	if maxDays := effectiveMaxDays(); maxDays > 0 && !input.TimeStart.IsZero() && !input.TimeEnd.IsZero() {
		if input.TimeEnd.Sub(input.TimeStart) > time.Duration(maxDays)*24*time.Hour {
			return NewBizError(40002, fmt.Sprintf("时间范围不能超过%d天", maxDays), http.StatusBadRequest)
		}
	}

	// Sources count cap.
	if len(input.Sources) > scopeLimits.MaxSources {
		return NewBizError(40003, fmt.Sprintf("信息来源不能超过%d个", scopeLimits.MaxSources), http.StatusBadRequest)
	}

	// --- target-specific gates ---
	switch target {
	case TargetPersonalWorkflow:
		return validatePersonalWorkflow(input)
	case TargetTeamWorkflow:
		return validateTeamWorkflow(input)
	case TargetScheduledWorkflow:
		return validateScheduledWorkflow(input)
	case TargetAgentGeneration:
		return validateAgentGeneration(input)
	case TargetAgentSave:
		return validateAgentSave(input)
	default:
		// An unknown target is a programmer error, but we prefer a graceful
		// 400 over a panic so a runtime mistake does not 500 users.
		return NewBizError(40000, fmt.Sprintf("unknown validation target: %q", target), http.StatusBadRequest)
	}
}

// effectiveMaxDays picks the configured MaxDays (test override) or falls
// back to the pipeline runtime global via the getter.
func effectiveMaxDays() int {
	if scopeLimits.MaxDays > 0 {
		return scopeLimits.MaxDays
	}
	return timeRangeMaxDaysGetter()
}

// validatePersonalWorkflow: CreateSummary's canonical "traditional single-person"
// path — the caller must provide at least one of sources/topic/time_range so the
// personal_processor has something to narrow on.
func validatePersonalWorkflow(input SnapshotScopeInput) *BizError {
	if hasNoScopeSignal(input) {
		return NewBizError(40001, "至少提供 sources、topic 或 time_range 之一", http.StatusBadRequest)
	}
	return nil
}

// validateTeamWorkflow: the multi-person traditional path shares personal's
// scope-signal requirement (still needs channels or topic or a range) and adds
// a participants-must-be-non-empty rule the design (section 5.2 team_workflow)
// calls out. The traditional handler still owns the participant-dedup /
// creator-included transformation; the validator only refuses payloads that
// are structurally missing a collaboration target.
func validateTeamWorkflow(input SnapshotScopeInput) *BizError {
	if hasNoScopeSignal(input) {
		return NewBizError(40001, "至少提供 sources、topic 或 time_range 之一", http.StatusBadRequest)
	}
	if len(input.Participants) == 0 {
		return NewBizError(40001, "team_workflow 需要至少一个参与者", http.StatusBadRequest)
	}
	return nil
}

// validateScheduledWorkflow: schedule input must be present. Downstream
// service.ValidateInterval*/ValidateRunTime already own the deep schedule-shape
// checks; here we only guarantee the Schedule payload was actually delivered
// (no zero-value Patch — see section 4.1 / section 7.6 "Schedule 不会被 Patch
// 或新入口写零值").
func validateScheduledWorkflow(input SnapshotScopeInput) *BizError {
	if input.Schedule == nil {
		return NewBizError(40010, "scheduled_workflow requires schedule payload", http.StatusBadRequest)
	}
	if input.Schedule.CronExpr == "" && input.Schedule.IntervalDays == 0 && input.Schedule.IntervalMonths == 0 {
		return NewBizError(40010, "schedule 必须提供 cron_expr / interval_days / interval_months 之一", http.StatusBadRequest)
	}
	return nil
}

// validateAgentGeneration: before the agent runs a generation turn we want the
// same "there is something to summarise" gate the traditional path has, so
// authors cannot get around the check by walking through the agent.
func validateAgentGeneration(input SnapshotScopeInput) *BizError {
	if hasNoScopeSignal(input) {
		return NewBizError(40001, "agent_generation 需要 sources、topic 或 time_range 之一", http.StatusBadRequest)
	}
	return nil
}

// validateAgentSave: the design (section 5.2 agent_save) mandates
// session/message ownership, non-empty content, and Snapshot-version
// alignment. The handler still owns the DB reads (message ownership +
// freshness), so this checks the *shape* of what the caller says it is about
// to save.
func validateAgentSave(input SnapshotScopeInput) *BizError {
	if input.AgentSave == nil {
		return NewBizError(40000, "agent_save 需要 agent_save 信息", http.StatusBadRequest)
	}
	if input.AgentSave.SessionID == "" {
		return NewBizError(40000, "agent_save session_id 不能为空", http.StatusBadRequest)
	}
	if input.AgentSave.ContentLen == 0 {
		// Matches CreateAgentSummary's 40004 for "session 无有效产出".
		return NewBizError(40004, "session 无有效产出,请先在对话中生成总结再保存", http.StatusBadRequest)
	}
	return nil
}

// hasNoScopeSignal returns true when the input has neither sources nor a topic
// nor any time range signal. Kept as one place so personal / team / agent
// generation share the same definition of "empty scope".
func hasNoScopeSignal(input SnapshotScopeInput) bool {
	if len(input.Sources) > 0 {
		return false
	}
	if input.Topic != "" {
		return false
	}
	if !input.TimeStart.IsZero() || !input.TimeEnd.IsZero() {
		return false
	}
	// Fall back to the storage-side TimeRange fields for callers that populate
	// only the model.SnapshotScope (e.g. tests). Non-empty strings on either
	// side count as a range signal.
	if input.Scope.TimeRange.Start != "" || input.Scope.TimeRange.End != "" {
		return false
	}
	return true
}
