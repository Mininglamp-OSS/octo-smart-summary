// Package service — shared SnapshotScope validator (SUM-BE1, revised per SUM-9).
//
// Design constraints (unified-smart-summary-design.md section 5 / 7):
//
//   - Use model.SnapshotScope as the shared validator input. Do NOT introduce
//     any parallel Scope-mirror types or converter layers — reviewers on SUM-9
//     called that out as decoration-only.
//   - Every declared target must have a real production call site AND real
//     enforcement. Targets that BE-1 cannot legitimately wire (team_workflow,
//     agent_generation) are intentionally NOT exposed here; the design lists
//     them but the traditional-team and agent-generation endpoints belong to
//     BE-2 / FE-2 and will land the target APIs alongside their handlers.
//   - "actor" is not just a log breadcrumb: an unauthenticated caller must be
//     rejected up-front (401), otherwise the validator would be a soft gate
//     downstream code cannot rely on.
//   - No mutable process-level globals for limits / max-days: values are
//     passed in explicitly so parallel tests and future concurrent config
//     changes cannot race.
//   - Error codes and messages match handler.CreateSummary's pre-refactor
//     inline checks bit-for-bit so router-facing responses stay identical
//     and existing regression tests (task_limit_test.go, etc.) keep passing.
//
// This file exports three focused functions — one per BE-1 production entry
// point — instead of a single string-target switch. That makes the reviewed
// "does the caller actually enforce this?" check trivial: the compiler
// answers it (each function must be called somewhere or dead-code lint fires).
package service

import (
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// Base caps applied by every entry point. These are const rather than package
// variables because callers should never be able to override them at runtime —
// the SUM-9 review flagged the previous exported setter as a race risk.
const (
	// MaxSummaryTitleRunes bounds every user-supplied title / topic string.
	// Matches handler.maxSummaryTopicRunes (which stays const there).
	MaxSummaryTitleRunes = 2300
	// MaxSummarySourceCount bounds len(sources). Matches handler.maxSourceCount.
	MaxSummarySourceCount = 30
)

// requireActor rejects callers with no identity. Every entry point runs this
// first so downstream code can rely on a non-empty actor.
func requireActor(actor string) *BizError {
	if actor == "" {
		return NewBizError(40100, "actor is required", http.StatusUnauthorized)
	}
	return nil
}

// validateTitleAndTopic runs the two rune-cap checks shared by every entry
// point. Message strings match the pre-refactor inline checks bit-for-bit.
func validateTitleAndTopic(title, topic string) *BizError {
	if utf8.RuneCountInString(title) > MaxSummaryTitleRunes {
		return NewBizError(40001, "title 不能超过 2300 字符", http.StatusBadRequest)
	}
	if utf8.RuneCountInString(topic) > MaxSummaryTitleRunes {
		return NewBizError(40001, "topic 不能超过 2300 字符", http.StatusBadRequest)
	}
	return nil
}

// validateOriginChannel checks the (id, type) coupling used by traditional and
// agent-save paths. Both fields either move together, or both are unset.
// application-layer type range is 1..3 (see model.OriginChannelGroup / DM).
func validateOriginChannel(originChannelID string, originChannelType int) *BizError {
	if originChannelID != "" && (originChannelType < model.OriginChannelGroup || originChannelType > model.OriginChannelDM) {
		return NewBizError(40001, "origin_channel_type must be 1, 2, or 3 when origin_channel_id is set", http.StatusBadRequest)
	}
	if originChannelID == "" && originChannelType != 0 {
		return NewBizError(40001, "origin_channel_id is required when origin_channel_type is set", http.StatusBadRequest)
	}
	return nil
}

// scopeHasSignal returns true when the shared SnapshotScope carries at least
// one of channel ids or a time range — i.e. the caller narrowed the summary
// in *some* way. Empty ChannelIDs AND empty TimeRange with no topic hint at
// the caller-site is the classic "at least one of sources/topic/time_range"
// failure the traditional path has always rejected.
func scopeHasSignal(scope model.SnapshotScope) bool {
	if len(scope.ChannelIDs) > 0 {
		return true
	}
	if scope.TimeRange.Start != "" || scope.TimeRange.End != "" {
		return true
	}
	return false
}

// ValidatePersonalWorkflow is called by handler.CreateSummary before it
// creates any row. Every argument is either data the handler already has in
// hand (no converter needed) or the shared model.SnapshotScope built from
// req.Sources + req.TimeRange in one place.
//
// Parameters
//
//   - actor:  the invoking user id; empty ⇒ 401.
//   - title:  createSummaryReq.Title
//   - topic:  createSummaryReq.Topic
//   - scope:  model.SnapshotScope actually built from the request; this is
//     what the validator gates on, NOT a parallel DTO.
//   - sourceCount:      len(req.Sources) — the count cap is target-specific.
//   - originChannelID / originChannelType: raw request fields.
//   - explicitTimeStart / explicitTimeEnd: the caller-supplied range (zero
//     values ⇒ caller let the server compute a default range and
//     wants the span cap skipped, matching pre-refactor behaviour).
//   - maxDays: pipeline.DefaultTimeRangeDays passed explicitly so the
//     validator has no runtime dependency on a mutable global.
func ValidatePersonalWorkflow(
	actor, title, topic string,
	scope model.SnapshotScope,
	sourceCount int,
	originChannelID string, originChannelType int,
	explicitTimeStart, explicitTimeEnd time.Time,
	maxDays int,
) *BizError {
	if err := requireActor(actor); err != nil {
		return err
	}
	if err := validateTitleAndTopic(title, topic); err != nil {
		return err
	}
	if err := validateOriginChannel(originChannelID, originChannelType); err != nil {
		return err
	}
	if maxDays > 0 && !explicitTimeStart.IsZero() && !explicitTimeEnd.IsZero() {
		if explicitTimeEnd.Sub(explicitTimeStart) > time.Duration(maxDays)*24*time.Hour {
			return NewBizError(40002, fmt.Sprintf("时间范围不能超过%d天", maxDays), http.StatusBadRequest)
		}
	}
	if sourceCount > MaxSummarySourceCount {
		return NewBizError(40003, fmt.Sprintf("信息来源不能超过%d个", MaxSummarySourceCount), http.StatusBadRequest)
	}
	// Scope-signal gate — either the shared scope carries a signal, or the
	// caller provided a topic. This is the "at least one of sources/topic/
	// time_range" contract the traditional path has always enforced.
	if !scopeHasSignal(scope) && topic == "" {
		return NewBizError(40001, "至少提供 sources、topic 或 time_range 之一", http.StatusBadRequest)
	}
	return nil
}

// ValidateScheduledWorkflow is called by handler.CreateSchedule. The deep
// recurrence / anchor / run-time / day-of-week / day-of-month / time-range-
// type checks stay in the existing service.ValidateInterval* / ValidateRunTime
// / ValidateScheduleAnchors / ValidateTimeRangeType helpers — this function
// runs the shared base cap and the "recurrence must be non-empty" invariant
// (design section 4.1: Schedule payload must be actually delivered, not
// zero-filled by an accidental patch).
//
// scope carries channel ids (from req.Sources) and time range (from
// req.TimeRange or the schedule's time_range_type). Handlers build it once
// and pass it in — same model.SnapshotScope as the other paths.
func ValidateScheduledWorkflow(
	actor, title string,
	scope model.SnapshotScope,
	cronExpr string, intervalDays, intervalMonths int,
) *BizError {
	if err := requireActor(actor); err != nil {
		return err
	}
	if err := validateTitleAndTopic(title, ""); err != nil {
		return err
	}
	if cronExpr == "" && intervalDays == 0 && intervalMonths == 0 {
		return NewBizError(40010, "schedule 必须提供 cron_expr / interval_days / interval_months 之一", http.StatusBadRequest)
	}
	// Non-blocking: an entirely empty scope on a schedule is legal in this
	// repo (Layer 3 auto-narrow picks channels at run time), so we do NOT
	// insist on a channel/time signal here — the design allows it for
	// scheduled runs whose scope is intentionally template-shaped.
	_ = scope
	return nil
}

// ValidateAgentSave is called by handler.CreateAgentSummary AFTER the
// server-trusted assistant content has been loaded and preamble-stripped.
// The handler owns the message ownership / role / session identity DB reads
// (loadLatestAssistantContent already filters WHERE user_id = ? AND
// session_id = ? AND role = 'assistant'), so this validator focuses on the
// shape / length checks and leaves the DB-side identity contract with the
// handler where it can actually be enforced.
//
// The design lists Snapshot-version and message-id enforcement under the
// agent_save target; those DB-driven checks are BE-2 scope (they require new
// storage-side reads and version columns that BE-1 does not add), so they
// are NOT declared as parameters here — leaving them as unused fields on a
// DTO would be exactly the "shell coverage" SUM-9 rejected.
func ValidateAgentSave(
	actor, title, sessionID, content string,
	originChannelID string, originChannelType int,
) *BizError {
	if err := requireActor(actor); err != nil {
		return err
	}
	if err := validateTitleAndTopic(title, ""); err != nil {
		return err
	}
	if err := validateOriginChannel(originChannelID, originChannelType); err != nil {
		return err
	}
	if sessionID == "" {
		return NewBizError(40000, "agent_save session_id 不能为空", http.StatusBadRequest)
	}
	if content == "" {
		return NewBizError(40004, "session 无有效产出,请先在对话中生成总结再保存", http.StatusBadRequest)
	}
	return nil
}
