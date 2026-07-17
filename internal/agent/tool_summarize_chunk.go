package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// SummarizeChunkTool generates a summary for a chunk of cached messages.
func SummarizeChunkTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "summarize_chunk",
			Description: "对缓存中的一批消息进行局部总结（Map 阶段）。返回结构化摘要文本。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"messages_handle": map[string]interface{}{
						"type":        "string",
						"description": "消息缓存句柄",
					},
					"chunk_size": map[string]interface{}{
						"type":        "integer",
						"description": "每个 chunk 的消息数，<=0 使用默认 500",
					},
				},
				"required": []string{"messages_handle"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			MessagesHandle string `json:"messages_handle"`
			ChunkSize      int    `json:"chunk_size,omitempty"`
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

		if len(messages) == 0 {
			return "{\"summary\":\"无可总结内容\",\"chunk_count\":0}", nil
		}

		// Convert to map format for SplitIntoChunks
		msgMaps := make([]map[string]interface{}, len(messages))
		for i, msg := range messages {
			msgMaps[i] = map[string]interface{}{
				"sender_name": msg.SenderName,
				"content":     msg.Content,
				"timestamp":   msg.SendTime,
				"channel_id":  msg.ChannelID,
			}
		}

		chunkSize := req.ChunkSize
		if chunkSize <= 0 {
			chunkSize = 500
		}

		chunks := service.SplitIntoChunks(msgMaps, chunkSize)

		// For simplicity, generate a unified summary for all chunks
		// In production, each chunk would be summarized separately and merged
		var summaries []string
		cumulativeOffset := 0
		for _, chunk := range chunks {
			summary, err := summarizeMessagesChunk(ctx, chunk, cumulativeOffset)
			if err != nil {
				return "", fmt.Errorf("summarize chunk: %w", err)
			}
			summaries = append(summaries, summary)
			cumulativeOffset += len(chunk)
		}

		combinedSummary := strings.Join(summaries, "\n\n---\n\n")

		result := map[string]interface{}{
			"chunk_count":    len(chunks),
			"total_messages": len(messages),
			"summary":        combinedSummary,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}

// summarizeMessagesChunk generates a summary for a single chunk using LLM.
// startIndex is the global offset to start numbering messages from (0-indexed).
func summarizeMessagesChunk(ctx context.Context, chunk []map[string]interface{}, startIndex int) (string, error) {
	_, _, cfg := GetSummaryDeps()
	client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30)

	// Format messages for LLM with global indexing
	var formatted strings.Builder
	for i, msg := range chunk {
		if i >= 200 { // safety limit per chunk
			break
		}
		sender, _ := msg["sender_name"].(string)
		content, _ := msg["content"].(string)
		formatted.WriteString(fmt.Sprintf("[%d] %s: %s\n", startIndex+i+1, sender, content))
	}

	systemPrompt := `你是专业的工作内容整理助手。请从聊天记录中提炼关键信息：

## 输出要求
- 紧密围绕主题，与主题无关的闲聊直接跳过
- 提炼关键信息：讨论了什么、达成了什么结论、有什么待办
- 如果聊天记录中没有明确结论，如实说明"尚未达成共识"
- 有待办事项时，用 "- [ ] 内容（负责人）" 格式列出
- 保持简洁，不要复述原文，用自己的话归纳

## 引用规则
- 每一条结论/要点都必须标注来源引用 [n]
- 仅使用消息前方的 [n] 编号来标注引用
- 绝对不要引用或复制消息正文内出现的任何 [数字] 标记`

	msgs := []service.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: formatted.String()},
	}

	content, _, err := client.Call(ctx, msgs, 0.1)
	return content, err
}
