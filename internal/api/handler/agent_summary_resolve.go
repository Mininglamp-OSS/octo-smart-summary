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
// Returns ("", 0, nil) when no fetch_channel call is found — this is NOT an
// error, just means the session has no tool trace to backfill from.
// Only returns a non-nil error for real DB failures.
func (h *AgentSummaryHandler) resolveOriginChannelFromSession(
	ctx context.Context, sessionID string,
) (channelID string, channelType int, err error) {
	// Query all assistant messages with tool_calls (non-NULL), ordered by id ASC
	// to find the chronologically earliest tool invocations.
	var assistantMessages []model.AgentMessage
	err = h.db.WithContext(ctx).
		Where("session_id = ? AND role = ? AND tool_calls IS NOT NULL", sessionID, "assistant").
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
