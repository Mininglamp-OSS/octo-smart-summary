package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// ListChannelsTool lists all channels visible to a user.
func ListChannelsTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "list_channels",
			Description: "列出指定用户可见的所有频道（群组 + 私聊）。用于探索阶段了解可用频道范围。",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		// Extract uid from context (injected by handler middleware)
		uidVal := ctx.Value(ContextKeyUID)
		uid, ok := uidVal.(string)
		if !ok || uid == "" {
			return "", fmt.Errorf("missing user identity in context")
		}

		imDB, _, _ := GetSummaryDeps()

		channels, err := pipeline.GetUserChannels(ctx, uid, imDB)
		if err != nil {
			return "", fmt.Errorf("get user channels: %w", err)
		}

		result := map[string]interface{}{
			"total":    len(channels),
			"channels": channels,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}
