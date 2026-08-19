package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// executeAgentFinalize is the Session-Finalize v0 generation core. Instead of
// re-fetching raw channel messages and re-running Map-Reduce
// (executePersonalPipeline — the slow path the agent already did during the
// conversation), it CONSOLIDATES the assistant replies the agent has already
// produced this session into one clean deliverable, in a single LLM pass.
//
// The return shape is identical to executePersonalPipeline
// (content, citations, msgCount, tokens, modelVersion, err) so
// processPersonalSummaryWithOptions persists it through the exact same
// Processing→Completed path — we reuse the worker's落库 shell and swap only the
// generation core.
func (p *Processor) executeAgentFinalize(ctx context.Context, task model.SummaryTask, userID string) (string, []model.Citation, int, int, string, error) {
	modelVer := p.llm.ModelVersion()
	sessionID := task.AgentSessionID
	if sessionID == "" {
		return "", nil, 0, 0, modelVer, fmt.Errorf("agent-finalize task %d has no agent_session_id", task.ID)
	}

	// 1. Gather the whole session's usable assistant replies — the already-formed
	// summary fragments — in chronological order, BOUNDED by the freeze point the
	// handler captured (task.AgentMessageID = max assistant id at save time). This
	// is the §3.4 revision freeze: replies produced after the user clicked save
	// (id > bound) are excluded, so the deliverable is stable and idempotent.
	// Tool-call wrappers and empty placeholders are excluded (mirrors
	// loadAgentMessageForSave's trusted filter), so process noise never merges.
	q := p.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''",
			userID, sessionID, "assistant")
	if task.AgentMessageID > 0 {
		q = q.Where("id <= ?", task.AgentMessageID)
	}
	var replies []model.AgentMessage
	if err := q.Order("created_at ASC").Find(&replies).Error; err != nil {
		return "", nil, 0, 0, modelVer, fmt.Errorf("load session assistant replies: %w", err)
	}
	if len(replies) == 0 {
		return "", nil, 0, 0, modelVer, fmt.Errorf("session %s has no usable assistant content to consolidate", sessionID)
	}

	// 2. One lightweight consolidation pass over the already-usable fragments.
	prompt := buildFinalizeConsolidationPrompt(task.Title, replies)
	out, err := p.llm.CallRaw(ctx, prompt)
	if err != nil {
		return "", nil, 0, 0, modelVer, fmt.Errorf("consolidation LLM call: %w", err)
	}
	content := strings.TrimSpace(out)
	if content == "" {
		return "", nil, 0, 0, modelVer, fmt.Errorf("consolidation produced empty content for session %s", sessionID)
	}

	// 3. Citations from the session's frozen evidence pool (same discovery
	// source as the agent save path), so the [n] markers preserved through the
	// merge resolve to real messages.
	pool := gatherSessionEvidencePool(ctx, p.db, userID, sessionID)
	nameMap := make(map[string]string, len(pool))
	for _, m := range pool {
		if m.SenderUID != "" && m.SenderName != "" {
			nameMap[m.SenderUID] = m.SenderName
		}
	}
	citations := BuildCitations(content, pool, pool, nameMap)

	// tokens=0: CallRaw does not surface token usage; finalize is a small single
	// call, so per-run token accounting is deferred (record-only if needed later).
	return content, citations, len(replies), 0, modelVer, nil
}

// buildFinalizeConsolidationPrompt assembles the single consolidation prompt.
// The inputs are the agent's ALREADY-usable summary fragments, so the task is
// MERGE + CLEAN (not from-scratch analysis): drop chit-chat / process talk,
// stitch the fragments into one coherent deliverable, and — critically —
// preserve every [n] citation marker verbatim (they index the frozen pool).
func buildFinalizeConsolidationPrompt(title string, replies []model.AgentMessage) string {
	var b strings.Builder
	b.WriteString(`你是专业的总结定稿助手。下面是同一次会话里 AI 助手先后产出的、已经基本可用的总结片段。请把它们【合并成一篇连贯、可独立阅读的正式总结】。

## 要求
- 合并去重:多个片段讲同一件事时合并,保留最新/最完整的说法,删掉重复。
- 去过程性内容:寒暄、元对话(如"我来帮你总结")、工具调用说明、失败重试等一律删除。
- 保留实质:目标、结论、决策、事实、风险、待办(待办用 "- [ ] 内容(负责人)")。
- 【严格保留引用】:片段中出现的 [n] 引用编号必须原样保留,不得改号、重编或删除;不要新增未出现过的引用编号。
- 不要编造:片段里没有的事实不要补充;若片段之间冲突且未解决,如实并列说明,不要替用户选边。
- 直接输出正文,不要加"以下是总结"之类的开场白或结尾语。`)

	if strings.TrimSpace(title) != "" {
		b.WriteString("\n\n## 用户确认的标题(围绕它组织正文,但不要改写标题本身)\n")
		b.WriteString(strings.TrimSpace(title))
	}

	b.WriteString("\n\n## 待合并的片段(按时间先后)\n")
	for i, r := range replies {
		b.WriteString(fmt.Sprintf("\n--- 片段 %d ---\n%s\n", i+1, strings.TrimSpace(r.Content)))
	}
	return b.String()
}

// gatherSessionEvidencePool rebuilds the session's global citation pool from
// agent_message_evidence — byte-for-byte the ordering getSessionMessagePool /
// buildCitationsForSession use — so the CitationIndex assigned here matches the
// [n] markers the agent emitted during the conversation. Best-effort: on any DB
// or decode error it returns what it has (citations degrade, the body does not).
func gatherSessionEvidencePool(ctx context.Context, db *gorm.DB, userID, sessionID string) []pipeline.Message {
	var rows []model.AgentMessageEvidence
	if err := db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("created_at ASC, handle ASC").
		Find(&rows).Error; err != nil {
		return nil
	}

	var pool []pipeline.Message
	seen := make(map[string]bool)
	for _, ev := range rows {
		if ev.Evidence == "" {
			continue
		}
		var msgs []pipeline.Message
		if err := json.Unmarshal([]byte(ev.Evidence), &msgs); err != nil {
			continue
		}
		for _, m := range msgs {
			key := fmt.Sprintf("%s:%d", m.ChannelID, m.MessageSeq)
			if !seen[key] {
				pool = append(pool, m)
				seen[key] = true
			}
		}
	}

	sort.Slice(pool, func(i, j int) bool {
		if pool[i].Timestamp != pool[j].Timestamp {
			return pool[i].Timestamp < pool[j].Timestamp
		}
		if pool[i].ChannelID != pool[j].ChannelID {
			return pool[i].ChannelID < pool[j].ChannelID
		}
		return pool[i].MessageSeq < pool[j].MessageSeq
	})
	for i := range pool {
		pool[i].CitationIndex = i + 1
	}
	return pool
}
