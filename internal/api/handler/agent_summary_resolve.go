package handler

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// toolCall mirrors the OpenAI-compatible tool call structure stored in
// AgentMessage.ToolCalls JSON. We only decode the fields needed to find
// fetch_channel invocations.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // stringified JSON, needs nested unmarshal
	} `json:"function"`
}

// fetchChannelArgs matches the argument structure of the fetch_channel tool.
type fetchChannelArgs struct {
	ChannelID   string `json:"channel_id"`
	ChannelType int    `json:"channel_type"` // optional, defaults to 1 if 0
}

// resolveOriginChannelFromSession searches the given session's assistant
// messages for the first fetch_channel tool call and returns its channel_id
// and channel_type.
//
// owner-scoped：必须传 userID，避免从他人 session 反推 channel 信息
// （SUM-158 blocker 1）。
//
// Returns ("", 0, nil) when no fetch_channel call is found — this is NOT an
// error, just means the session has no tool trace to backfill from.
// Only returns a non-nil error for real DB failures.
func (h *AgentSummaryHandler) resolveOriginChannelFromSession(
	ctx context.Context, sessionID, userID string,
) (channelID string, channelType int, err error) {
	// Query all assistant messages with tool_calls (non-NULL), ordered by id ASC
	// to find the chronologically earliest tool invocations.
	var assistantMessages []model.AgentMessage
	err = h.db.WithContext(ctx).
		Where("space_id = ? AND user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NOT NULL", legacyAgentMessageSpaceID, userID, sessionID, "assistant").
		Order("id ASC").
		Find(&assistantMessages).Error
	if err != nil {
		log.Printf("[resolve] query assistant messages failed session=%s: %v", sessionID, err)
		return "", 0, err
	}

	// Iterate through messages in chronological order, looking for the first
	// fetch_channel call.
	for _, msg := range assistantMessages {
		if msg.ToolCalls == nil || *msg.ToolCalls == "" {
			continue
		}

		// Parse the tool_calls JSON array
		var calls []toolCall
		if err := json.Unmarshal([]byte(*msg.ToolCalls), &calls); err != nil {
			log.Printf("[resolve] parse tool_calls failed session=%s msg_id=%d: %v", sessionID, msg.ID, err)
			continue // skip malformed, try next message
		}

		// Look for the first fetch_channel call
		for _, tc := range calls {
			if tc.Function.Name != "fetch_channel" {
				continue
			}

			// Parse the nested arguments JSON string
			var args fetchChannelArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				log.Printf("[resolve] parse fetch_channel arguments failed session=%s call_id=%s: %v",
					sessionID, tc.ID, err)
				continue
			}

			// Found a fetch_channel call with valid arguments
			channelID = args.ChannelID
			channelType = args.ChannelType

			// Apply the same default as the fetch_channel tool itself:
			// channel_type == 0 defaults to 1 (group)
			if channelType == 0 {
				channelType = 1
			}

			log.Printf("[resolve] resolved origin from session=%s: channel_id=%s channel_type=%d",
				sessionID, channelID, channelType)
			return channelID, channelType, nil
		}
	}

	// No fetch_channel call found in any assistant message
	log.Printf("[resolve] no fetch_channel call found in session=%s", sessionID)
	return "", 0, nil
}

// deriveOriginFromSummarySources derives an origin channel from the channels
// a referenced task was generated from (its summary_source rows).
//
// Pipeline/scheduled/bot summaries never populate summary_task.origin_channel_id,
// so the tier-3 borrow (refTask.OriginChannelID) comes up empty for them and a
// pure refine of such a task used to dead-end in 40001. This tier-4 fallback
// recovers the origin from the task's generation sources so non-agent
// summaries can be referenced + refined + iterated like agent ones.
//
// Deterministic: the FIRST usable source row (by summary_source.id, creation
// order) wins; multi-source tasks inherit their first channel — consistent
// with the tier-3 precedent of borrowing the FIRST referenced task's origin.
// source_type and origin_channel_type are two separate enums that happen to
// be numerically aligned today (SourceGroup/Thread/Direct = 1/2/3 vs
// OriginChannelGroup/Thread/DM = 1/2/3 — see model.go:103-115), and
// source_id is already a canonical channel ID, so no conversion is needed.
// The range check below uses the Source* constants because the value under
// test is a SummarySource.SourceType.
//
// Returns ("", 0) when the task has no usable source rows — the caller then
// falls through to the 40001 error as before. Authorization is enforced by
// the caller (canAccessTaskDB on the referenced task) — this helper must only
// be reached for tasks the user can already read.
func (h *AgentSummaryHandler) deriveOriginFromSummarySources(ctx context.Context, taskID int64) (string, int, bool) {
	var sources []model.SummarySource
	if err := h.db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("id ASC").
		Find(&sources).Error; err != nil {
		log.Printf("[resolve] query summary_source failed task_id=%d: %v", taskID, err)
		return "", 0, false
	}
	for _, s := range sources {
		if s.SourceID == "" {
			continue
		}
		// R7 P2-4: bound by the Source* enum (the value under test is a
		// SummarySource.SourceType), not the OriginChannel* enum — the two
		// are separate enums that only happen to share values today.
		if s.SourceType < model.SourceGroup || s.SourceType > model.SourceDirect {
			continue
		}
		if len(sources) > 1 {
			log.Printf("[resolve] task_id=%d has %d summary_source rows; inheriting first usable channel=%s/%d",
				taskID, len(sources), s.SourceID, s.SourceType)
		}
		// R11 Q2: report whether the winning row is DERIVED — a derived
		// origin must be masked on the wire (owner decision 2026-08-14,
		// option 1). tier-4 keeps reading derived rows (dropping them would
		// dead-end auto-select refines into 40001 — the reason this feature
		// exists); only the echo is masked.
		return s.SourceID, s.SourceType, s.Derived
	}
	return "", 0, false
}
