package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

type setSummaryScopeArgs struct {
	SourceMode string `json:"source_mode"`
	Channels   []struct {
		ChannelID   string `json:"channel_id"`
		ChannelType int    `json:"channel_type"`
	} `json:"channels,omitempty"`
	TimeRange *struct {
		Start string `json:"start"`
		End   string `json:"end"`
		Label string `json:"label,omitempty"`
	} `json:"time_range,omitempty"`
}

// SetSummaryScopeTool lets the workspace Agent declare a structured scope
// change instead of relying on substring heuristics. Trusted discovery still
// authorizes channel ids, and the application layer validates the final scope.
func SetSummaryScopeTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "set_summary_scope",
			Description: "仅当用户明确要求改变聊天来源或时间范围时调用。先完成频道发现，再声明最终来源；普通改写、疑问、否定或历史描述不得调用。",
			Parameters: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"source_mode": map[string]any{
						"type": "string",
						"enum": []string{WorkspaceSourceKeep, WorkspaceSourceReplace, WorkspaceSourceExtend},
					},
					"channels": map[string]any{
						"type":     "array",
						"maxItems": MaxWorkspaceSelectedChannels,
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"channel_id":   map[string]any{"type": "string"},
								"channel_type": map[string]any{"type": "integer", "enum": []int{1, 2, 5}},
							},
							"required": []string{"channel_id", "channel_type"},
						},
					},
					"time_range": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]any{
							"start": map[string]any{"type": "string", "description": "RFC3339 起始时间"},
							"end":   map[string]any{"type": "string", "description": "RFC3339 结束时间"},
							"label": map[string]any{"type": "string", "maxLength": MaxWorkspaceTimeRangeLabel},
						},
						"required": []string{"start", "end"},
					},
				},
				"required": []string{"source_mode"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		if hasEvidence, _ := summaryCitationEvidenceWindow(ctx); hasEvidence {
			return "", errors.New("summary scope must be set before fetching messages")
		}
		var parsed setSummaryScopeArgs
		decoder := json.NewDecoder(strings.NewReader(string(args)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&parsed); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		sourceMode := strings.TrimSpace(parsed.SourceMode)
		if sourceMode != WorkspaceSourceKeep && sourceMode != WorkspaceSourceReplace && sourceMode != WorkspaceSourceExtend {
			return "", fmt.Errorf("invalid workspace source mode %q", sourceMode)
		}
		change := WorkspaceScopeChange{SourceMode: sourceMode}
		if len(parsed.Channels) > MaxWorkspaceSelectedChannels {
			return "", fmt.Errorf("workspace scope cannot exceed %d channels", MaxWorkspaceSelectedChannels)
		}
		for _, channel := range parsed.Channels {
			change.Channels = append(change.Channels, ChannelScope{
				ChannelID: strings.TrimSpace(channel.ChannelID), ChannelType: channel.ChannelType,
			})
		}
		if parsed.TimeRange != nil {
			start, startErr := time.Parse(time.RFC3339, strings.TrimSpace(parsed.TimeRange.Start))
			end, endErr := time.Parse(time.RFC3339, strings.TrimSpace(parsed.TimeRange.End))
			if startErr != nil || endErr != nil || !end.After(start) {
				return "", errors.New("time_range must contain a valid increasing RFC3339 range")
			}
			if end.Sub(start) > time.Duration(pipeline.MaxTimeRangeDays)*24*time.Hour {
				return "", fmt.Errorf("time_range cannot exceed %d days", pipeline.MaxTimeRangeDays)
			}
			if utf8.RuneCountInString(strings.TrimSpace(parsed.TimeRange.Label)) > MaxWorkspaceTimeRangeLabel {
				return "", fmt.Errorf("time_range label cannot exceed %d characters", MaxWorkspaceTimeRangeLabel)
			}
			change.TimeRange = &WorkspaceTimeRange{
				Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339), Label: strings.TrimSpace(parsed.TimeRange.Label),
			}
		}
		if change.SourceMode == WorkspaceSourceKeep && change.TimeRange == nil {
			return "", errors.New("scope change must modify sources or time_range")
		}
		if err := DeclareWorkspaceScopeChange(ctx, change); err != nil {
			return "", err
		}
		declared, ok := DeclaredWorkspaceScopeChange(ctx)
		if !ok {
			return "", errors.New("workspace scope declaration was not retained")
		}
		uid, _ := ctx.Value(ContextKeyUID).(string)
		recordDeclaredChannels(ctx, uid, channelScopeIDsOf(declared.Channels))
		return "已记录并约束本轮总结范围。", nil
	}
	return schema, handler
}
