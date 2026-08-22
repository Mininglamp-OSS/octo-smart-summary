package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/tokenizer"
)

// finalizeTemperature keeps consolidation near-deterministic: the fragments are
// already written, so this pass is merge-and-clean, not generation. A higher
// temperature would let the model reword settled conclusions.
const finalizeTemperature = 0.3

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
	// A missing bound is an ERROR, not a wider query. The handler always sets a
	// positive bound, so a zero here means the task row is malformed — and
	// silently falling back to "merge the entire unbounded session" would defeat
	// the freeze on exactly the rows whose provenance we cannot trust.
	if task.AgentMessageID <= 0 {
		return "", nil, 0, 0, modelVer, fmt.Errorf("agent-finalize task %d has no freeze bound (agent_message_id)", task.ID)
	}
	q = q.Where("id <= ?", task.AgentMessageID)
	var replies []model.AgentMessage
	if err := q.Order("created_at ASC").Find(&replies).Error; err != nil {
		return "", nil, 0, 0, modelVer, fmt.Errorf("load session assistant replies: %w", err)
	}
	if len(replies) == 0 {
		return "", nil, 0, 0, modelVer, fmt.Errorf("session %s has no usable assistant content to consolidate", sessionID)
	}

	// 2. Bound the prompt. The feature targets exactly the long sessions that
	// break an unbounded concatenation ("一次会话往往聊了七八轮"), and
	// AgentMessage.Content is mediumtext, so without a budget a long enough
	// session overflows the context window and the whole finalize fails.
	//
	// Budgeting drops the OLDEST fragments first: the newest replies are the most
	// refined statement of the session's conclusions, and the prompt's own merge
	// rule ("保留最新/最完整的说法") already treats them as authoritative. Dropping
	// is disclosed to the model so it does not claim coverage it does not have.
	replies, droppedOld := p.budgetFinalizeReplies(task.Title, replies)

	// 3. One lightweight consolidation pass over the already-usable fragments.
	//
	// Call, NOT CallRaw. CallRaw swallows every error and returns ("[]", nil) —
	// a sane degraded value for its intended use (topic narrowing), and a silent
	// data-loss bug here: a gateway 502 or a token-limit overflow would sail past
	// the err check, "[]" is not empty so the empty check passes too, and the
	// worker would mark the task Completed with a two-character body while the
	// retry/failure machinery this path deliberately reuses never fires.
	// Call surfaces the real error AND the token count.
	prompt := buildFinalizeConsolidationPrompt(task.Title, replies, droppedOld)
	out, tokens, err := p.llm.Call(ctx, []service.ChatMessage{{Role: "user", Content: prompt}}, finalizeTemperature)
	if err != nil {
		return "", nil, 0, 0, modelVer, fmt.Errorf("consolidation LLM call: %w", err)
	}
	content := strings.TrimSpace(out)
	if content == "" {
		return "", nil, 0, 0, modelVer, fmt.Errorf("consolidation produced empty content for session %s", sessionID)
	}

	// 4. Citations from the session's frozen evidence pool (same discovery
	// source as the agent save path), so the [n] markers preserved through the
	// merge resolve to real messages.
	//
	// The pool is bounded by the newest merged reply's timestamp: the body was
	// frozen at save time, and the NUMBER SPACE has to be frozen with it. Indices
	// are assigned positionally after a timestamp sort, so evidence written after
	// the user clicked finalize would shift every later index and silently
	// re-point the preserved [n] markers at other messages.
	evidenceBound := replies[len(replies)-1].CreatedAt
	pool := gatherSessionEvidencePool(ctx, p.db, userID, sessionID, evidenceBound)
	nameMap := make(map[string]string, len(pool))
	for _, m := range pool {
		if m.SenderUID != "" && m.SenderName != "" {
			nameMap[m.SenderUID] = m.SenderName
		}
	}
	citations := BuildCitations(content, pool, pool, nameMap)

	return content, citations, len(replies), tokens, modelVer, nil
}

// buildFinalizeConsolidationPrompt assembles the single consolidation prompt.
// The inputs are the agent's ALREADY-usable summary fragments, so the task is
// MERGE + CLEAN (not from-scratch analysis): drop chit-chat / process talk,
// stitch the fragments into one coherent deliverable, and — critically —
// preserve every [n] citation marker verbatim (they index the frozen pool).
func buildFinalizeConsolidationPrompt(title string, replies []model.AgentMessage, droppedOld int) string {
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

	if droppedOld > 0 {
		b.WriteString(fmt.Sprintf(
			"\n\n## 注意:本次会话过长,最早的 %d 个片段因长度限制未纳入\n只根据下面给出的片段撰写。如果正文只覆盖了会话的后半段,如实说明,不要声称覆盖了整场会话。",
			droppedOld))
	}

	b.WriteString("\n\n## 待合并的片段(按时间先后)\n")
	for i, r := range replies {
		b.WriteString(fmt.Sprintf("\n--- 片段 %d ---\n%s\n", i+1, strings.TrimSpace(r.Content)))
	}
	return b.String()
}

// gatherSessionEvidencePool rebuilds the session's citation pool from
// agent_message_evidence, mirroring the discovery + de-dup + ordering of
// getSessionMessagePool / buildCitationsForSession, so the CitationIndex
// assigned here matches the [n] markers the agent emitted during the
// conversation.
//
// createdBefore BOUNDS the pool to evidence that existed at save time. Without
// it the numbering was not frozen even though the body was: a user who clicks
// finalize and keeps chatting makes the next turn persist new evidence, and
// because indices are assigned positionally after a timestamp sort, one new row
// sorting early shifts EVERY subsequent index. The preserved [n] markers would
// then resolve to different messages — confidently wrong attribution, which is
// worse than dropping the citations outright.
//
// Best-effort: on any DB or decode error it returns what it has (citations
// degrade, the body does not).
//
// Note this filters on ev.Evidence == "" where the two canonical builders filter
// on ev.Handle == "". Both are no-ops on real rows (PersistEvidence writes both
// together); this one is the filter that matters here because the cache tier the
// others consult is not available in the worker process, so the JSON snapshot is
// the only source.
func gatherSessionEvidencePool(ctx context.Context, db *gorm.DB, userID, sessionID string, createdBefore time.Time) []pipeline.Message {
	q := db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID)
	if !createdBefore.IsZero() {
		q = q.Where("created_at <= ?", createdBefore)
	}
	var rows []model.AgentMessageEvidence
	if err := q.Order("created_at ASC, handle ASC").Find(&rows).Error; err != nil {
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

// executeFinalizeTask is the task-level entry point for a Session-Finalize v0
// task, standing in for executePipeline.
//
// executePipeline exists to DISCOVER channels and FETCH raw messages. A finalize
// task has neither: its input is the agent replies already persisted for the
// session, and the handler already validated they exist. Running the fetch
// pipeline for it would perform channel discovery against imDB, participant
// intersection, a full intent-recognition tool call, and a message fetch over a
// zero-width time range — paying exactly the cost this feature exists to avoid,
// and it could fail the finalize for reasons unrelated to finalizing.
//
// So this does only the one thing executePipeline does that finalize also needs:
// make sure the creator participant + a Pending personal_result exist, then hand
// off. processTask's own dispatch logic takes it from there and
// processPersonalSummaryWithOptions routes to executeAgentFinalize.
func (p *Processor) executeFinalizeTask(task model.SummaryTask) error {
	if task.AgentSessionID == "" {
		return fmt.Errorf("agent-finalize task %d has no agent_session_id", task.ID)
	}
	// The handler creates participant + personal_result in the same tx as the
	// task, so this is normally a no-op; it is kept for the same defensive reason
	// processTask bootstraps them, since a task with no participant row can never
	// be dispatched and would sit in Processing forever.
	if _, err := p.bootstrapCreatorParticipant(task); err != nil {
		return fmt.Errorf("bootstrap creator artifacts for finalize task %d: %w", task.ID, err)
	}
	return nil
}

// budgetFinalizeReplies caps the consolidation prompt at the Map-phase token
// budget, returning the fragments that fit plus how many older ones were cut.
//
// It reuses ResolveMapMaxTokens rather than introducing a finalize-specific knob:
// both are "how much text may go into one LLM call for this model", and a second
// independent budget would drift from the first. systemPromptOverhead mirrors the
// allowance the Map-Reduce path reserves for its own instructions.
//
// Oldest-first eviction: the newest replies are the session's most refined
// conclusions, and the merge prompt already prefers the latest statement.
func (p *Processor) budgetFinalizeReplies(title string, replies []model.AgentMessage) ([]model.AgentMessage, int) {
	budget := p.cfg.ResolveMapMaxTokens()
	if budget <= 0 {
		return replies, 0
	}
	const systemPromptOverhead = 3000
	budget -= systemPromptOverhead
	if budget <= 0 {
		return replies, 0
	}

	tok := tokenizer.New(p.cfg.LLMModel, tokenizer.Config{
		CharsPerTokenCJK:   p.cfg.ResolveCharsPerTokenCJK(),
		CharsPerTokenASCII: p.cfg.CharsPerTokenASCII,
		KimiAPIKey:         p.cfg.KimiAPIKey,
		HTTPTimeout:        p.cfg.TokenizerHTTPTimeout,
	})
	budget -= tok.Count(title)

	// Walk newest→oldest, keeping while the budget holds.
	keepFrom := len(replies)
	for i := len(replies) - 1; i >= 0; i-- {
		cost := tok.Count(replies[i].Content)
		if budget-cost < 0 && keepFrom < len(replies) {
			break
		}
		budget -= cost
		keepFrom = i
		if budget <= 0 {
			break
		}
	}
	// Always keep at least the newest reply: an empty prompt is strictly worse
	// than an over-budget one, and the LLM error path handles the overflow.
	if keepFrom >= len(replies) {
		keepFrom = len(replies) - 1
	}
	if keepFrom <= 0 {
		return replies, 0
	}
	log.Printf("[finalize] session prompt over budget: dropping %d oldest of %d fragments", keepFrom, len(replies))
	return replies[keepFrom:], keepFrom
}
