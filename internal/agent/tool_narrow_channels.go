package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// NarrowChannelsByTopicTool narrows channels by topic using LLM.
func NarrowChannelsByTopicTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "narrow_channels_by_topic",
			Description: "根据总结主题，从候选频道中筛选出相关的频道。使用 LLM 判断频道名称与主题的相关性。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"topic": map[string]interface{}{
						"type":        "string",
						"description": "总结主题，例如'项目进度'、'产品讨论'",
					},
					"channel_ids": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "候选频道 ID 列表",
					},
				},
				"required": []string{"topic", "channel_ids"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Topic      string   `json:"topic"`
			ChannelIDs []string `json:"channel_ids"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		summaryDB, _, _, cfg := GetSummaryDeps()

		var candidates []pipeline.ChannelInfo
		for _, id := range req.ChannelIDs {
			candidates = append(candidates, pipeline.ChannelInfo{ChannelID: id})
		}

		llmFn := func(ctx context.Context, prompt string) (string, error) {
			client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30, cfg.LLMFallbackModels)
			msgs := []service.ChatMessage{{Role: "user", Content: prompt}}
			content, _, err := client.Call(
				llmfallback.WithPath(ctx, llmfallback.PathAgentTool), msgs, 0.3)
			return content, err
		}

		narrowed, didNarrow := pipeline.NarrowByTopicReport(ctx, req.Topic, candidates, llmFn)

		// Record the narrowed set as the run's discovered scope (open-scope only):
		// this is a deliberate topic-relevant subset the run chose to focus on, so
		// it is a sound baseline for the finish gate's under-fetch check — unlike
		// list_channels' raw visible surface, which is not scope.
		//
		// ONLY when the filter actually narrowed. NarrowByTopic is the identity on
		// four paths (empty topic / no candidates / nil llmFn, an LLM error,
		// unparseable model output, zero matches), and recording unconditionally
		// meant a transient LLM blip during narrowing persisted the ENTIRE candidate
		// list — in the documented flow, the full list_channels output — as the run's
		// committed scope. Since the union is monotonic that is unrecoverable for the
		// rest of the run: every later clean fetch still reports the untouched
		// candidates as never-fetched gaps. A pass-through is the absence of a scope
		// decision, not a decision to cover everything.
		if uid, ok := ctx.Value(ContextKeyUID).(string); ok && didNarrow {
			recordDiscoveredChannels(ctx, summaryDB, uid, channelIDsOf(narrowed))
		}

		result := map[string]interface{}{
			"original_count": len(candidates),
			"narrowed_count": len(narrowed),
			"channels":       narrowed,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}
