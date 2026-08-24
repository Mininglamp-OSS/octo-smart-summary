package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// MergeSummariesTool merges multiple chunk summaries into a final structured summary.
func MergeSummariesTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "merge_summaries",
			Description: "将本次请求中 summarize_chunk 返回的全部 summary_handle 合并为最终结构化摘要（Reduce 阶段）。只传 handle，不要复制摘要正文。",
			Parameters: map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"summary_handles": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"minItems":    1,
						"uniqueItems": true,
						"description": "本次请求内 summarize_chunk 返回的全部 summary_handle，按工具结果顺序传入",
					},
				},
				"required": []string{"summary_handles"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			SummaryHandles []string `json:"summary_handles"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		store, err := summaryHandleStoreFromContext(ctx)
		if err != nil {
			return "", err
		}
		resolved, err := store.ResolveAllBefore(req.SummaryHandles, summaryToolStepFromContext(ctx))
		if err != nil {
			return "", err
		}

		// The planner only sees short handles. The full Map text is joined here,
		// inside the backend, and sent directly to the Reduce LLM.
		summaries := make([]string, 0, len(resolved.Entries))
		chunkCount := 0
		for _, entry := range resolved.Entries {
			summaries = append(summaries, entry.Text)
			chunkCount += entry.ChunkCount
		}
		combined := strings.Join(summaries, "\n\n--- Chunk Boundary ---\n\n")

		// SS-06: load the run's SummarySpec-derived guidance so the Reduce
		// respects the user's language / detail level / sections / exclusions
		// instead of forcing a generic short-item JSON. Empty when V2 off.
		specGuidance := loadRunSpecGuidance(ctx)

		// Use LLM to merge and structure
		merged, truncated, err := mergeSummariesLLM(ctx, combined, specGuidance)
		if err != nil {
			return "", fmt.Errorf("merge summaries: %w", err)
		}
		// This check is load-bearing precisely BECAUSE the client no longer
		// appends the disclosure: `merged` here is the raw model body. When the
		// client concatenated TruncationNotice before returning, a whitespace-only
		// truncated completion arrived as " \n\t" + notice, this guard could never
		// fire, MarkReduced below cleared the completeness gate, and the user got
		// a deliverable whose entire body was the notice.
		if strings.TrimSpace(merged) == "" {
			return "", fmt.Errorf("merge summaries: empty Reduce result")
		}

		// A length-truncated Reduce is degraded, not failed. The planner must be
		// able to see the degradation structurally (not only as prose buried in
		// merged_summary) so it repeats the disclosure in the final answer rather
		// than presenting a partial merge as a complete one. The notice is
		// appended here, after the emptiness check above has passed on the raw
		// body, and only in merged_summary: `truncation_notice` stays the
		// structural signal so a planner concatenating both fields does not emit
		// the same sentence twice.
		result := map[string]interface{}{
			"merged_summary": merged,
			"chunk_count":    chunkCount,
		}
		if truncated {
			result["merged_summary"] = merged + service.TruncationNotice
			result["truncated"] = true
			result["truncation_notice"] = strings.TrimSpace(service.TruncationNotice)
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		// Reached on the truncated-but-usable path too. Skipping it would leave
		// NeedsReduce() true forever: the runner gate would reject every final
		// answer and nudge an IDENTICAL merge_summaries retry (same handles, same
		// combined text, no shrinking parameter), which truncates again until
		// MaxSteps and ends the request with zero output.
		store.MarkReduced(resolved.Generation)
		return string(data), nil
	}

	return schema, handler
}

var mergeSummariesLLM = mergeSummariesWithLLM

// mergeSummariesWithLLM uses LLM to merge and structure multiple summaries.
//
// specGuidance (SS-06) is appended when non-empty so the Reduce honors the
// user's language / detail level / required sections / exclusions; the priority
// note inside the guidance lets detail_level=detailed override the default
// "每条不超过 50 字" compression. Empty (V2 off) → the exact legacy prompt.
//
// Truncation policy: this is the TERMINAL deliverable of the agent Map/Reduce
// pipeline, so it DISCLOSES rather than rejects (CallDisclosingTruncation).
// Map (summarize_chunk) keeps CallStrict: a truncated chunk drops messages that
// no later stage can recover, which is the SS-01 no-silent-loss invariant. Here
// the input is already-summarized text and the output goes straight to the
// user, so a hard failure trades a degraded answer for no answer at all.
//
// The returned content is the RAW model body: CallDisclosingTruncation does not
// append TruncationNotice, so the handler can still tell an empty truncated
// Reduce from a usable one before it decides to disclose.
func mergeSummariesWithLLM(ctx context.Context, combined string, specGuidance string) (string, bool, error) {
	_, _, _, cfg := GetSummaryDeps()
	client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30, cfg.LLMFallbackModels)

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

	// Tag the path so this tool's LLM traffic is attributable in metrics
	// instead of landing in path="unknown" (the generic service client cannot
	// know which caller drove it).
	content, truncated, _, err := client.CallDisclosingTruncation(
		llmfallback.WithPath(ctx, llmfallback.PathAgentTool), msgs, 0.1)
	return content, truncated, err
}
