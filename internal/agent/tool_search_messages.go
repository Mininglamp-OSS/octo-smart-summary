package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// SearchMessagesTool searches messages by keywords within cached messages.
func SearchMessagesTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "search_messages",
			Description: "在已缓存的消息中按关键词搜索，返回匹配的消息列表。用于快速定位相关内容。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"messages_handle": map[string]interface{}{
						"type":        "string",
						"description": "消息缓存句柄，由 fetch_channel 或 peek_channel 返回",
					},
					"keywords": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "搜索关键词列表",
					},
					"topic": map[string]interface{}{
						"type":        "string",
						"description": "可选的主题描述，用于语义相关性过滤",
					},
				},
				"required": []string{"messages_handle", "keywords"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			MessagesHandle string   `json:"messages_handle"`
			Keywords       []string `json:"keywords"`
			Topic          string   `json:"topic,omitempty"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Extract uid from context
		uidVal := ctx.Value(ContextKeyUID)
		uid, ok := uidVal.(string)
		if !ok || uid == "" {
			return "", fmt.Errorf("missing user identity in context")
		}

		messages := messageCache.Retrieve(req.MessagesHandle, uid)
		if messages == nil {
			return "", fmt.Errorf("invalid messages_handle or access denied: %s", req.MessagesHandle)
		}

		// Filter by keyword match in content
		var matched []pipeline.Message
		for _, msg := range messages {
			if matchesKeywords(msg.Content, req.Keywords) {
				matched = append(matched, msg)
			}
		}

		// If topic is provided, further filter by relevance
		if req.Topic != "" && len(matched) > 0 {
			matched = pipeline.FilterMessagesByRelevance(matched, req.Topic, nil, nil)
		}

		// Store filtered results back to cache with new handle
		newHandle := messageCache.Store(matched, uid)

		result := map[string]interface{}{
			"total":           len(matched),
			"matched_count":   len(matched),
			"messages_handle": newHandle,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}

// matchesKeywords checks if any keyword appears in the text (case-insensitive).
func matchesKeywords(text string, keywords []string) bool {
	lowerText := strings.ToLower(text)
	for _, kw := range keywords {
		if strings.Contains(lowerText, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
