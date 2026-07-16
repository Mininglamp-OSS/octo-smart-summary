package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// FindSharedChannelsTool finds channels shared between creator and participants.
func FindSharedChannelsTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "find_shared_channels",
			Description: "找出创建者与指定参与者共同所在的频道。用于聚焦多人对话场景。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"participant_uids": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "参与者 UID 列表",
					},
				},
				"required": []string{"participant_uids"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			ParticipantUIDs []string `json:"participant_uids"`
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

		imDB, _, _ := GetSummaryDeps()

		creatorChannels, err := pipeline.GetUserChannels(ctx, uid, imDB)
		if err != nil {
			return "", fmt.Errorf("get creator channels: %w", err)
		}

		shared, err := pipeline.IntersectParticipantChannels(ctx, creatorChannels, req.ParticipantUIDs, imDB)
		if err != nil {
			return "", fmt.Errorf("intersect participant channels: %w", err)
		}

		result := map[string]interface{}{
			"total":    len(shared),
			"channels": shared,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}
