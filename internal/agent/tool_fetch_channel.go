package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// FetchChannelTool fetches full messages from a channel.
func FetchChannelTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "fetch_channel",
			Description: "从指定频道抓取全量消息（受 max_per_channel 限制）。结果全量存入内部缓存，只返回统计信息和 handle。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"channel_id": map[string]interface{}{
						"type":        "string",
						"description": "频道 ID",
					},
					"channel_type": map[string]interface{}{
						"type":        "integer",
						"description": "频道类型(WuKongIM 存储层协议)：1=DM(私聊), 2=Group(群), 5=Thread(子区)。**必须显式传递**,禁止省略。",
					},
					"time_start": map[string]interface{}{
						"type":        "string",
						"description": "起始时间 RFC3339",
					},
					"time_end": map[string]interface{}{
						"type":        "string",
						"description": "结束时间 RFC3339",
					},
					"max_messages": map[string]interface{}{
						"type":        "integer",
						"description": "每频道最大消息数，<=0 使用配置默认值",
					},
				},
				"required": []string{"channel_id", "channel_type", "time_start", "time_end"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			ChannelID   string `json:"channel_id"`
			ChannelType int    `json:"channel_type,omitempty"`
			TimeStart   string `json:"time_start"`
			TimeEnd     string `json:"time_end"`
			MaxMessages int    `json:"max_messages,omitempty"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Extract uid from context (injected by handler middleware)
		uidVal := ctx.Value(ContextKeyUID)
		uid, ok := uidVal.(string)
		if !ok || uid == "" {
			return "", fmt.Errorf("missing user identity in context")
		}

		// channel_type must be explicitly supplied — silently defaulting to 1
		// (Group) caused SQL mismatch when the real channel was Thread (type=5)
		// or DM (type=1), returning 0 rows and misleading agent into "no
		// messages" answers. See CHAT-REFERENCE-BASED-DESIGN-v1 diagnostic.
		if req.ChannelType == 0 {
			log.Printf("[fetch_channel] rejecting call: agent did not supply channel_type. channel=%s", req.ChannelID)
			return "", fmt.Errorf("channel_type is required (1=DM, 2=Group, 5=Thread); check reference material's candidate channels for the correct value")
		}

		timeStart, err := time.Parse(time.RFC3339, req.TimeStart)
		if err != nil {
			return "", fmt.Errorf("parse time_start: %w", err)
		}
		timeEnd, err := time.Parse(time.RFC3339, req.TimeEnd)
		if err != nil {
			return "", fmt.Errorf("parse time_end: %w", err)
		}

		imDB, _, cfg := GetSummaryDeps()

		// Security: validate channel accessibility for system-injected uid
		accessibleChannels, err := pipeline.GetUserChannels(ctx, uid, imDB)
		if err != nil {
			return "", fmt.Errorf("get user channels: %w", err)
		}

		// Build set of accessible channel IDs
		allowedSet := make(map[string]bool)
		for _, ch := range accessibleChannels {
			allowedSet[ch.ChannelID] = true
		}

		if !allowedSet[req.ChannelID] {
			errResult := map[string]interface{}{
				"error":      "channel not accessible",
				"channel_id": req.ChannelID,
			}
			errData, _ := json.Marshal(errResult)
			return string(errData), fmt.Errorf("channel %s not accessible by user %s", req.ChannelID, uid)
		}

		maxPerChannel := req.MaxMessages
		if maxPerChannel <= 0 {
			maxPerChannel = cfg.MaxMessagesPerChannel
		}

		messages, err := pipeline.FetchMessagesFromChannel(ctx, req.ChannelID, req.ChannelType, timeStart.Unix(), timeEnd.Unix(), imDB, cfg.MsgTableCount, uid, maxPerChannel)
		if err != nil {
			return "", fmt.Errorf("fetch messages: %w", err)
		}

		// Enrich messages with SenderName, SourceName, ChannelType before caching.
		// This fixes citation metadata loss (SUM-46 Blocker A).
		// Rationale: pipeline.FetchMessagesFromChannel only fills 5 fields (SenderUID,
		// ChannelID, Timestamp, SendTime, Content). Citations need SenderName/SourceName/
		// ChannelType. We enrich here (tool layer) rather than in pipeline because:
		// (1) tool layer already has accessibleChannels with ChannelName/ChannelType
		// (2) keeps pipeline focused on message fetching, not metadata resolution
		// (3) no circular dependency risk
		enrichMessagesWithMetadata(ctx, messages, req.ChannelID, accessibleChannels, imDB)

		handle := messageCache.Store(messages, uid)

		result := map[string]interface{}{
			"total":           len(messages),
			"messages_handle": handle,
			"channel_id":      req.ChannelID,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}
