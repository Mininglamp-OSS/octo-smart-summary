package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// MergeSummariesTool merges multiple chunk summaries into a final structured summary.
func MergeSummariesTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "merge_summaries",
			Description: "将多个局部总结合并为最终的结构化摘要（Reduce 阶段）。提取关键点、决策、待办事项。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"summaries": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "局部总结文本列表，由 summarize_chunk 返回",
					},
				},
				"required": []string{"summaries"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Summaries []string `json:"summaries"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if len(req.Summaries) == 0 {
			return "{\"highlights\":[\"无内容可合并\"]}", nil
		}

		// Combine all summaries
		combined := strings.Join(req.Summaries, "\n\n--- Chunk Boundary ---\n\n")

		// SS-06: load the run's SummarySpec-derived guidance so the Reduce
		// respects the user's language / detail level / sections / exclusions
		// instead of forcing a generic short-item JSON. Empty when V2 off.
		specGuidance := loadRunSpecGuidance(ctx)

		// Use LLM to merge and structure
		merged, err := mergeSummariesWithLLM(ctx, combined, specGuidance)
		if err != nil {
			return "", fmt.Errorf("merge summaries: %w", err)
		}

		result := map[string]interface{}{
			"merged_summary": merged,
			"chunk_count":    len(req.Summaries),
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}

// mergeSummariesWithLLM uses LLM to merge and structure multiple summaries.
//
// specGuidance (SS-06) is appended when non-empty so the Reduce honors the
// user's language / detail level / required sections / exclusions; the priority
// note inside the guidance lets detail_level=detailed override the default
// "每条不超过 50 字" compression. Empty (V2 off) → the exact legacy prompt.
func mergeSummariesWithLLM(ctx context.Context, combined string, specGuidance string) (string, error) {
	_, _, _, cfg := GetSummaryDeps()
	client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30)

	systemPrompt := `你是专业的工作内容整理助手。请将多个局部总结合并为一个结构化摘要：

## 输出格式（JSON）
{
  "highlights": ["关键要点1", "关键要点2"],
  "decisions": ["达成的结论1", "达成的结论2"],
  "open_questions": ["待解决的问题1"],
  "candidate_actions": [
    {"action": "待办事项", "assignee": "负责人"}
  ]
}

## 要求
- 去重合并相似内容
- 保持简洁，每条不超过 50 字
- 如果没有某类内容，返回空数组
- 不要编造不存在的信息` + specGuidance

	msgs := []service.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: combined},
	}

	content, _, err := client.Call(ctx, msgs, 0.1)
	return content, err
}
