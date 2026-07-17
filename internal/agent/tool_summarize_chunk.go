package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// getSessionMessagePool retrieves all messages from all tool calls in the session,
// sorts them globally by timestamp, and assigns CitationIndex.
// This ensures the same global ordering that buildCitationsForSession will use.
//
// Handle discovery (post-#158 regression fix, 4-reviewer P1):
// Previously handles were discovered by querying agent_message WHERE
// role='tool'. That table is populated only by AppendMessages, which runs
// AFTER RunWithHistory returns. During the run itself — including when
// summarize_chunk executes inside the agent loop — there are zero tool rows
// for the current turn, so the pool was empty, CitationIndex stayed at the
// Go zero value 0, the LLM emitted `[0]` markers, and worker.BuildCitations
// (idx >= 1 && idx <= maxIdx) discarded every marker at save time. First-
// turn agent summaries produced broken/empty citations on the dominant path.
//
// The fix sources handles from agent_message_evidence instead. Evidence rows
// are written synchronously by PersistEvidence inside fetch_channel /
// peek_channel tool handlers (BEFORE the tool returns), so by the time
// summarize_chunk runs in a later step, the evidence table already contains
// every handle produced this turn. The messages themselves come from the
// in-memory cache when warm, and fall back to the evidence row's JSON
// snapshot when cold — byte-for-byte the same recovery
// buildCitationsForSession performs. Keeping the two symmetric guarantees
// the pre-assigned CitationIndex here matches the post-assignment there
// for both first-turn and long-running/paused sessions.
func getSessionMessagePool(sessionID, uid string) ([]pipeline.Message, error) {
	summaryDB, _, _, _ := GetSummaryDeps()

	// Discover handles from evidence table — populated synchronously by
	// PersistEvidence inside fetch_channel / peek_channel before the tool
	// returns, so it is populated during the run (unlike agent_message which
	// is only written by AppendMessages after RunWithHistory returns).
	var evidenceRows []model.AgentMessageEvidence
	if err := summaryDB.Where("user_id = ? AND session_id = ?", uid, sessionID).
		Order("created_at ASC, handle ASC").
		Find(&evidenceRows).Error; err != nil {
		return nil, fmt.Errorf("query evidence rows: %w", err)
	}

	// Collect all messages from all handles (cache-hot preferred, evidence
	// JSON snapshot as fallback for cold cache / restart)
	var allMessages []pipeline.Message
	seenKey := make(map[string]bool) // de-dup by channel_id+message_seq

	cache := GetMessageCache()
	for _, ev := range evidenceRows {
		if ev.Handle == "" {
			continue
		}

		// Prefer cache (avoids JSON unmarshal on the hot path)
		if cached := cache.Retrieve(ev.Handle, uid); cached != nil {
			for _, msg := range cached {
				key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
				if !seenKey[key] {
					allMessages = append(allMessages, msg)
					seenKey[key] = true
				}
			}
			continue
		}

		// Cache miss: unmarshal the evidence JSON snapshot. Same recovery
		// path as buildCitationsForSession — see agent_summary_citations.go.
		var evidenceMessages []pipeline.Message
		if err := json.Unmarshal([]byte(ev.Evidence), &evidenceMessages); err != nil {
			continue
		}
		for _, msg := range evidenceMessages {
			key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
			if !seenKey[key] {
				allMessages = append(allMessages, msg)
				seenKey[key] = true
			}
		}
	}

	// Sort by timestamp ascending, with (ChannelID, MessageSeq) as deterministic
	// tiebreaker. Must stay byte-identical to the sort in
	// agent_summary_citations.go:120-122 so that the pre-assigned CitationIndex
	// here matches the post-assignment there — see SUM-47 v3 rationale.
	sort.Slice(allMessages, func(i, j int) bool {
		if allMessages[i].Timestamp != allMessages[j].Timestamp {
			return allMessages[i].Timestamp < allMessages[j].Timestamp
		}
		if allMessages[i].ChannelID != allMessages[j].ChannelID {
			return allMessages[i].ChannelID < allMessages[j].ChannelID
		}
		return allMessages[i].MessageSeq < allMessages[j].MessageSeq
	})

	// Assign global CitationIndex (same as agent_summary_citations.go:102-103)
	for i := range allMessages {
		allMessages[i].CitationIndex = i + 1
	}

	return allMessages, nil
}

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

		// Extract sessionID from context
		sessionIDVal := ctx.Value(ContextKeySessionID)
		sessionID, ok := sessionIDVal.(string)
		if !ok || sessionID == "" {
			return "", fmt.Errorf("missing session_id in context")
		}

		messages := messageCache.Retrieve(req.MessagesHandle, uid)
		if messages == nil {
			return "", fmt.Errorf("invalid messages_handle or access denied: %s", req.MessagesHandle)
		}

		if len(messages) == 0 {
			return "{\"summary\":\"无可总结内容\",\"chunk_count\":0}", nil
		}

		// Get the global message pool for the session and pre-assign CitationIndex.
		// This ensures LLM sees the same indexes that buildCitationsForSession will assign.
		globalPool, err := getSessionMessagePool(sessionID, uid)
		if err != nil {
			return "", fmt.Errorf("get session message pool: %w", err)
		}

		// Build a map from (channel_id, message_seq) to CitationIndex
		citationMap := make(map[string]int)
		for _, msg := range globalPool {
			key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
			citationMap[key] = msg.CitationIndex
		}

		// Apply the global CitationIndex to our messages
		for i := range messages {
			key := fmt.Sprintf("%s:%d", messages[i].ChannelID, messages[i].MessageSeq)
			if idx, found := citationMap[key]; found {
				messages[i].CitationIndex = idx
			}
		}

		// Convert to map format for SplitIntoChunks
		msgMaps := make([]map[string]interface{}, len(messages))
		for i, msg := range messages {
			msgMaps[i] = map[string]interface{}{
				"sender_name":    msg.SenderName,
				"content":        msg.Content,
				"timestamp":      msg.SendTime,
				"channel_id":     msg.ChannelID,
				"citation_index": msg.CitationIndex, // Global index
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
		for _, chunk := range chunks {
			summary, err := summarizeMessagesChunk(ctx, chunk)
			if err != nil {
				return "", fmt.Errorf("summarize chunk: %w", err)
			}
			summaries = append(summaries, summary)
		}

		combinedSummary := strings.Join(summaries, "\n\n---\n\n")
		result := map[string]interface{}{
			"summary":     combinedSummary,
			"chunk_count": len(chunks),
		}

		// Marshal result
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(resultJSON), nil
	}

	return schema, handler
}

// summarizeMessagesChunk builds a structured prompt from msgMap chunk and calls LLM.
func summarizeMessagesChunk(ctx context.Context, chunk []map[string]interface{}) (string, error) {
	_, _, _, cfg := GetSummaryDeps()
	client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30)

	// Format messages for LLM with global citation_index
	var formatted strings.Builder
	for i, msg := range chunk {
		if i >= 200 { // safety limit per chunk
			break
		}
		sender, _ := msg["sender_name"].(string)
		content, _ := msg["content"].(string)
		citationIndex, _ := msg["citation_index"].(int)
		formatted.WriteString(fmt.Sprintf("[%d] %s: %s\n", citationIndex, sender, content))
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

	content, _, err := client.Call(ctx, msgs, 0.3)
	if err != nil {
		return "", fmt.Errorf("call LLM: %w", err)
	}

	return strings.TrimSpace(content), nil
}
