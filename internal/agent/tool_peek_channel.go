package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// PeekChannelTool samples a small number of messages from a channel.
func PeekChannelTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "peek_channel",
			Description: "从指定频道采样少量消息（默认 10 条），快速浏览内容以决定是否需要深读。结果存入内部缓存，返回采样片段和 handle。",
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
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "采样消息数，默认 10",
					},
					"time_start": map[string]interface{}{
						"type":        "string",
						"description": "起始时间 RFC3339，留空则用最近 7 天",
					},
					"time_end": map[string]interface{}{
						"type":        "string",
						"description": "结束时间 RFC3339，留空则用当前时间",
					},
				},
				"required": []string{"channel_id"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			ChannelID   string `json:"channel_id"`
			ChannelType int    `json:"channel_type,omitempty"`
			Limit       int    `json:"limit,omitempty"`
			TimeStart   string `json:"time_start,omitempty"`
			TimeEnd     string `json:"time_end,omitempty"`
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

		// Same enforcement as fetch_channel — see rationale there.
		if req.ChannelType == 0 {
			log.Printf("[peek_channel] rejecting call: agent did not supply channel_type. channel=%s", req.ChannelID)
			return "", fmt.Errorf("channel_type is required (1=DM, 2=Group, 5=Thread); check reference material's candidate channels for the correct value")
		}
		if req.Limit <= 0 {
			req.Limit = 10
		}

		now := time.Now()
		timeStart := now.AddDate(0, 0, -7).Unix()
		timeEnd := now.Unix()

		if req.TimeStart != "" {
			if t, err := time.Parse(time.RFC3339, req.TimeStart); err == nil {
				timeStart = t.Unix()
			}
		}
		if req.TimeEnd != "" {
			if t, err := time.Parse(time.RFC3339, req.TimeEnd); err == nil {
				timeEnd = t.Unix()
			}
		}

		imDB, _, cfg := GetSummaryDeps()

		// Security: validate channel accessibility for system-injected uid
		accessibleChannels, err := pipeline.GetUserChannels(ctx, uid, imDB)
		if err != nil {
			return "", fmt.Errorf("get user channels: %w", err)
		}

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

		messages, err := pipeline.FetchMessagesFromChannel(ctx, req.ChannelID, req.ChannelType, timeStart, timeEnd, imDB, cfg.MsgTableCount, uid, req.Limit)
		if err != nil {
			return "", fmt.Errorf("fetch messages: %w", err)
		}

		handle := messageCache.Store(messages, uid)

		const sampleSize = 5
		var sampled []map[string]interface{}
		limit := sampleSize
		if len(messages) < limit {
			limit = len(messages)
		}
		for i := 0; i < limit; i++ {
			msg := messages[i]
			sampled = append(sampled, map[string]interface{}{
				"sender_name": msg.SenderName,
				"content":     truncateStr(msg.Content, 150),
				"send_time":   msg.SendTime,
			})
		}

		result := map[string]interface{}{
			"total":           len(messages),
			"sample_size":     len(sampled),
			"messages":        sampled,
			"messages_handle": handle,
			"truncated":       len(messages) > sampleSize,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}
