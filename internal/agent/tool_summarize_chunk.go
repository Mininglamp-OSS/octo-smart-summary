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

// SS-01 止血常量（见 docs/AGENT-SUMMARY-...zh.md 缺点二）。
//
// 历史 bug：chunk_size 默认 500，但 formatChunkForLLM 每片只格式化前 200 条，
// 满片时静默丢弃 300/500（60%），且工具返回值只报 chunk_count，Planner 误判
// 已覆盖全部消息。修法：把默认值与硬上限统一收敛到 200，使
// SplitIntoChunks 产出的每片恒 <= maxChunkSize，格式化环节不再丢消息；同时
// 返回结构化覆盖计数让上层可识别截断/超长输入。
//
// 注意执行顺序纪律：本阶段只压上限 + 报计数，不引入 Token-aware 分片，也不删
// 200 硬限制——删限制属于 SS-06（Token 分片就绪之后），提前删会把“确定性丢消息”
// 变成“上下文溢出/请求失败”。
const (
	// defaultChunkSize 是未显式指定或非法 chunk_size 时的每片消息数。
	defaultChunkSize = 200
	// maxChunkSize 是每片消息数的硬上限，与 formatChunkForLLM 的格式化上限一致，
	// 消除“分片 500 / 格式化 200”双上限造成的静默丢失。
	maxChunkSize = 200
	// oversizedMessageRunes 是单条消息被标记为“超长”的字符阈值。超长消息只
	// 计数上报（oversized_message_count），不做静默截断——真正的按预算单独成片
	// 属于后续 Token 分片阶段（SS-06）。
	oversizedMessageRunes = 4000
)

// chunkCoverage 汇总 summarize_chunk 实际喂给模型的消息覆盖情况，随工具结果
// 返回，让 Runner/Planner 能判断是否发生丢弃或截断，而不是只看到 chunk_count。
type chunkCoverage struct {
	InputCount            int  `json:"input_count"`
	ProcessedCount        int  `json:"processed_count"`
	DroppedCount          int  `json:"dropped_count"`
	OversizedMessageCount int  `json:"oversized_message_count"`
	Truncated             bool `json:"truncated"`
	ChunkSize             int  `json:"chunk_size"`
}

// clampChunkSize 把请求的 chunk_size 收敛到 [1, maxChunkSize]；<=0 取默认值，
// 超过上限截到 maxChunkSize。返回值保证 SplitIntoChunks 产出的每片 <= maxChunkSize。
func clampChunkSize(requested int) int {
	if requested <= 0 {
		return defaultChunkSize
	}
	if requested > maxChunkSize {
		return maxChunkSize
	}
	return requested
}

// ComputeCoverage reports how the CURRENT chunking path would cover inputCount
// messages summarized at the given requested chunk_size: how many are fed to the
// model (processed), how many are lost (dropped), and how many chunks are formed.
//
// It routes through the exact production seams — clampChunkSize + maxChunkSize —
// so the SS-02 eval harness asserts the no-silent-loss invariant against real
// logic. If anyone regresses defaultChunkSize back to 500 (or widens the format
// cap out of step with the split size), dropped goes non-zero and eval fails.
func ComputeCoverage(inputCount, requestedChunkSize int) (processed, dropped, chunks int) {
	if inputCount <= 0 {
		return 0, 0, 0
	}
	size := clampChunkSize(requestedChunkSize)
	for i := 0; i < inputCount; i += size {
		end := i + size
		if end > inputCount {
			end = inputCount
		}
		n := end - i
		if n > maxChunkSize { // mirrors formatChunkForLLM's hard cap
			n = maxChunkSize
		}
		processed += n
		chunks++
	}
	dropped = inputCount - processed
	return processed, dropped, chunks
}

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
//
// Two-phase pool invariant (#161 P2, yujiawei):
// The pre-assigned CitationIndex here (mid-run) and the rebuilt index in
// buildCitationsForSession (save-time) are only guaranteed to match if the
// evidence row set does not change between the two phases. In practice this
// is upheld by the profile ordering `fetch/search/filter → summarize_chunk →
// merge_summaries → answer`: any handle-producing tool runs BEFORE
// summarize_chunk in the same or an earlier step, so no evidence row is
// added after summarize_chunk's pool snapshot. If a future profile ever
// interleaves a data-fetching tool after summarize_chunk in the same turn,
// the newly-persisted evidence would appear only at save time, shifting
// CitationIndex and breaking the [n]-marker alignment. Enforce this
// invariant at profile design time — the runner does not check it.
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
						"description": "每个 chunk 的消息数；<=0 或 >200 时取 200（硬上限）。返回值含 input_count/processed_count/dropped_count/oversized_message_count/truncated。",
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

		chunkSize := clampChunkSize(req.ChunkSize)

		chunks := service.SplitIntoChunks(msgMaps, chunkSize)

		// Summarize each chunk and aggregate honest coverage counts. With
		// chunkSize clamped to maxChunkSize, formatChunkForLLM never drops a
		// message, so dropped_count is 0 on the normal path; the counters stay
		// truthful if a future change ever reintroduces a per-chunk cap.
		cov := chunkCoverage{InputCount: len(messages), ChunkSize: chunkSize}
		var summaries []string
		for _, chunk := range chunks {
			summary, processed, oversized, err := summarizeMessagesChunk(ctx, chunk)
			if err != nil {
				return "", fmt.Errorf("summarize chunk: %w", err)
			}
			summaries = append(summaries, summary)
			cov.ProcessedCount += processed
			cov.OversizedMessageCount += oversized
		}
		cov.DroppedCount = cov.InputCount - cov.ProcessedCount
		cov.Truncated = cov.DroppedCount > 0

		combinedSummary := strings.Join(summaries, "\n\n---\n\n")
		result := map[string]interface{}{
			"summary":                 combinedSummary,
			"chunk_count":             len(chunks),
			"input_count":             cov.InputCount,
			"processed_count":         cov.ProcessedCount,
			"dropped_count":           cov.DroppedCount,
			"oversized_message_count": cov.OversizedMessageCount,
			"truncated":               cov.Truncated,
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

// formatChunkForLLM renders a chunk into the "[n] sender: content" lines fed to
// the LLM. It is a pure function (no LLM, no I/O) so coverage can be asserted
// deterministically in tests / the eval harness without a live model.
//
// It returns how many messages it actually formatted (processed) and how many
// exceeded oversizedMessageRunes (oversized). Oversized messages are counted but
// NOT truncated — their full content is still emitted. The maxChunkSize guard is
// a redundant safety net: with clampChunkSize upstream, chunks are always
// <= maxChunkSize, so it never drops here; if it ever did, processed would be
// less than len(chunk) and the caller would surface a non-zero dropped_count
// instead of losing messages silently.
func formatChunkForLLM(chunk []map[string]interface{}) (formatted string, processed, oversized int) {
	var b strings.Builder
	for i, msg := range chunk {
		if i >= maxChunkSize {
			break
		}
		sender, _ := msg["sender_name"].(string)
		content, _ := msg["content"].(string)
		citationIndex, _ := msg["citation_index"].(int)
		if len([]rune(content)) > oversizedMessageRunes {
			oversized++
		}
		b.WriteString(fmt.Sprintf("[%d] %s: %s\n", citationIndex, sender, content))
		processed++
	}
	return b.String(), processed, oversized
}

// summarizeMessagesChunk builds a structured prompt from msgMap chunk and calls LLM.
// Returns the summary plus how many messages were processed / were oversized, so
// the caller can aggregate honest coverage across chunks.
func summarizeMessagesChunk(ctx context.Context, chunk []map[string]interface{}) (summary string, processed, oversized int, err error) {
	_, _, _, cfg := GetSummaryDeps()
	client := service.NewLLMClient(cfg.LLMApiURL, cfg.LLMApiKey, cfg.LLMModel, cfg.LLMTimeout, cfg.LLMMaxToken, cfg.LLMEnableThinking, 30)

	// Format messages for LLM with global citation_index (pure, testable).
	formatted, processed, oversized := formatChunkForLLM(chunk)

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
		{Role: "user", Content: formatted},
	}

	content, _, err := client.Call(ctx, msgs, 0.3)
	if err != nil {
		return "", processed, oversized, fmt.Errorf("call LLM: %w", err)
	}

	return strings.TrimSpace(content), processed, oversized, nil
}
