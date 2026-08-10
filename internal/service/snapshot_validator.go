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

// AgentSaveExpectedSnapshotVersion is the only snapshot_version an Agent draft
// save is currently allowed to declare. BE-2 lands the first-generation save
// path; the future Refine pipeline (BE-3+) will lift this cap once it starts
// producing snapshot_version=2..N, at which point ValidateAgentSave switches
// from "must equal 1" to "must equal PersonalResult.snapshot_json version".
// Declaring the constant here (instead of leaving it a magic number in the
// handler) keeps the invariant reviewable in one place.
const AgentSaveExpectedSnapshotVersion = 1

// ValidateAgentSave is called by handler.CreateAgentSummary AFTER the
// server-trusted assistant content has been loaded and preamble-stripped.
//
// SUM-BE1 (reviewed under SUM-9) intentionally dropped AgentMessageID /
// SnapshotVersion from this signature because BE-1 could not persist them —
// carrying them as unused parameters would have been the exact "shell
// coverage" the review rejected. SUM-BE2 lands the storage side
// (summary_task.agent_message_id / agent_session_id / snapshot_version, see
// migration 20260810-01), so the parameters are now real inputs the caller
// must fill from server-trusted state and this function enforces them.
//
// Message ownership / role / session identity is still the handler's DB job
// (loadAgentMessageForSave filters WHERE id = ? AND user_id = ? AND
// session_id = ? AND role = 'assistant' AND tool_calls IS NULL). This
// function checks the SHAPE contract:
//
//   - actor / session_id / content: unchanged from BE-1.
//   - agentMessageID: must be positive when supplied. Zero is allowed only
//     when expectedSnapshotVersion is also zero, i.e. the caller is still
//     on the pre-BE-2 "latest assistant" fallback path (FE-2 will supply
//     both fields; older FE builds supply neither). Mixing the two —
//     message id without version or vice versa — is a client bug and 400.
//   - expectedSnapshotVersion: when the client claims a version, it must
//     equal AgentSaveExpectedSnapshotVersion (BE-2 only writes v1); any
//     other value is a stale-draft race and maps to AGENT_DRAFT_STALE.
func ValidateAgentSave(
	actor, title, sessionID, content string,
	originChannelID string, originChannelType int,
	agentMessageID int64, expectedSnapshotVersion int,
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
	// Message-id / snapshot-version pairing: either both set (new FE-2 path)
	// or both zero (legacy pre-BE-2 path). Half-supplied is a client bug.
	if (agentMessageID > 0) != (expectedSnapshotVersion > 0) {
		return NewBizError(40001, "agent_message_id 与 snapshot_version 必须同时提供或同时省略", http.StatusBadRequest)
	}
	if agentMessageID < 0 {
		return NewBizError(40001, "agent_message_id 必须为正整数", http.StatusBadRequest)
	}
	if expectedSnapshotVersion > 0 && expectedSnapshotVersion != AgentSaveExpectedSnapshotVersion {
		// AGENT_DRAFT_STALE — client is looking at a snapshot the server never
		// wrote. Once Refine lands v2+, the caller-supplied version must
		// match the persisted PersonalResult snapshot version and this hard
		// equality relaxes into a lookup.
		return NewBizError(40901, "agent_draft_stale: snapshot 版本已过期,请刷新草稿", http.StatusConflict)
	}
	return nil
}
