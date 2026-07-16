package agent

import (
	"context"
	"encoding/json"
	"fmt"

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

		_, _, cfg := GetSummaryDeps()

		var candidates []pipeline.ChannelInfo
		for _, id := range req.ChannelIDs {
			candidates = append(candidates, pipeline.ChannelInfo{ChannelID: id})
		}

		llmFn := func(ctx context.Context, prompt string) (string, error) {
			client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30)
			msgs := []service.ChatMessage{{Role: "user", Content: prompt}}
			content, _, err := client.Call(ctx, msgs, 0.3)
			return content, err
		}

		narrowed := pipeline.NarrowByTopic(ctx, req.Topic, candidates, llmFn)

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
