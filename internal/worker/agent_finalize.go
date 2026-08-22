package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
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
	// Order by id, NOT created_at. agent_message.created_at is DATETIME (1-second
	// precision) and AppendMessages stamps every message of a turn with the same
	// `now`, so two turns that complete inside the same second tie and MySQL may
	// return them in either order. Fragment order decides the merged body, so a
	// tie would make the output non-reproducible across worker retries — which is
	// exactly what the freeze exists to prevent. id is monotonic, and it is
	// already the axis the freeze bound above is expressed in.
	if err := q.Order("id ASC").Find(&replies).Error; err != nil {
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

	// 3. Rebuild the session's frozen evidence pool and REMAP every fragment's
	// [n] markers into it before they are concatenated.
	//
	// The pool is bounded by the newest merged reply's timestamp so evidence
	// written after the user clicked finalize cannot shift indices (the FORWARD
	// drift vector). The remap below closes the CROSS-TURN vector, which is
	// structural to merging and which the bound alone cannot touch — see
	// remapFinalizeCitations.
	evidenceRows := loadSessionEvidenceRows(ctx, p.db, userID, sessionID, replies[len(replies)-1].CreatedAt)
	pool := buildPoolFromEvidenceRows(evidenceRows)
	replies, droppedMarkers := remapFinalizeCitations(replies, evidenceRows, pool)
	if droppedMarkers > 0 {
		log.Printf("[finalize] task %d session %s: dropped %d unresolvable citation marker(s) during cross-turn remap",
			task.ID, sessionID, droppedMarkers)
	}

	// 4. One lightweight consolidation pass over the already-usable fragments.
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

	// 5. Citations resolve against the same frozen pool the markers were remapped
	// into in step 3, so every surviving [n] points at the message its sentence
	// was actually about.
	nameMap := make(map[string]string, len(pool))
	for _, m := range pool {
		if m.SenderUID != "" && m.SenderName != "" {
			nameMap[m.SenderUID] = m.SenderName
		}
	}
	citations := BuildCitations(content, pool, pool, nameMap)

	// msg_count is the number of SOURCE IM messages, everywhere else in the
	// system (personal_processor.go, api/handler/task.go, api/handler/personal.go
	// all surface it raw to clients as msg_count / total_msg_count). Reporting
	// len(replies) here would publish "3" for a finalize over a 500-message
	// session — the same field name meaning two different things depending on
	// which route produced the row. The frozen evidence pool IS this route's
	// source-message set, so it is the honest denominator.
	return content, citations, len(pool), tokens, modelVer, nil
}

// remapFinalizeCitations rewrites each fragment's [n] markers from the numbering
// THAT TURN saw into the numbering of the final merged pool.
//
// Why this is necessary: [n] markers are not assigned once per session. They are
// assigned once per TURN, positionally, over the evidence pool as it existed at
// that moment (internal/agent/tool_summarize_chunk.go: sort, then
// CitationIndex = i+1). So the ordinal a marker means is a function of which
// evidence rows existed during that turn.
//
// Turn 1 fetches #alpha (today 10:00-11:00) → 8 messages, indices 1-8, and the
// reply cites [3] = alpha's 3rd message. Turn 3 fetches #beta (last week) → 12
// messages with EARLIER timestamps, so turn 3's pool sorts beta into 1-12 and
// alpha shifts to 13-20. Finalize merges both replies against ONE pool built at
// the newest reply — turn 3's numbering — and turn 1's preserved [3] now names a
// beta message. BuildCitations only clamps OUT-OF-RANGE indices; 3 is in range,
// so nothing is dropped and nothing is logged. The deliverable ships confidently
// wrong attribution, which this file's own comments (and every reviewer) call
// worse than a missing citation.
//
// The remap resolves each marker through MESSAGE IDENTITY (channel_id:message_seq)
// rather than through its ordinal: turn pool index → identity → final pool index.
//
// FAIL CLOSED. A marker that cannot be resolved is DROPPED, never left pointing
// at whatever now occupies its slot. Two ways that happens:
//   - the index is out of range in its own turn's pool (the fragment cited
//     something the re-derived pool does not contain);
//   - the identity it resolves to is absent from the final pool (evidence that
//     existed at that turn is not in the frozen merged set).
//
// KNOWN WEAKNESS — this RE-DERIVES each turn's numbering instead of reading an
// authoritative record of it. PersistEvidence upserts with
// ON DUPLICATE KEY UPDATE evidence = VALUES(evidence) and deliberately does NOT
// refresh created_at, so a row rewritten after the fact re-derives differently
// than the turn actually saw. The window is narrow — it needs a handle counter
// to be reused, and handles are msg_<uid>_<counter> with a PROCESS-LOCAL counter
// (internal/agent/cache.go), so it takes a process restart during a live session
// — but it is real. Reading the frozen per-run manifests
// (internal/agent/citation_manifest.go) would remove the re-derivation entirely;
// it is not available in v0 because agent_message has no run_id column and
// agent_citation_manifest is keyed by run_id, so a reply cannot be linked to its
// own turn's manifest without a schema change.
func remapFinalizeCitations(replies []model.AgentMessage, rows []model.AgentMessageEvidence, finalPool []pipeline.Message) ([]model.AgentMessage, int) {
	if len(replies) == 0 {
		return replies, 0
	}

	// identity → index in the final merged pool.
	finalIdx := make(map[string]int, len(finalPool))
	for _, m := range finalPool {
		finalIdx[messageIdentity(m)] = m.CitationIndex
	}

	out := make([]model.AgentMessage, len(replies))
	copy(out, replies)

	dropped := 0
	for i := range out {
		// Re-derive the pool THIS turn saw: the same rows, filtered to those that
		// already existed when the reply was written, run through the identical
		// de-dup / sort / index rules so the numbering is byte-identical to what
		// tool_summarize_chunk assigned. rows is already ordered
		// (created_at ASC, handle ASC), and the filter keeps a prefix-by-time, so
		// first-seen-wins de-dup resolves the same way a separate query would.
		turnPool := buildPoolFromEvidenceRows(filterEvidenceRowsCreatedBefore(rows, out[i].CreatedAt))
		turnIdentity := make(map[int]string, len(turnPool))
		for _, m := range turnPool {
			turnIdentity[m.CitationIndex] = messageIdentity(m)
		}

		fragment := out[i].Content
		out[i].Content = citationRe.ReplaceAllStringFunc(fragment, func(match string) string {
			sub := citationRe.FindStringSubmatch(match)
			n, err := strconv.Atoi(sub[1])
			if err != nil {
				return match
			}
			identity, ok := turnIdentity[n]
			if !ok {
				dropped++
				log.Printf("[finalize] dropping citation %s in fragment %d (msg id=%d): index out of range in its own turn's pool (size=%d)",
					match, i+1, out[i].ID, len(turnPool))
				return ""
			}
			target, ok := finalIdx[identity]
			if !ok {
				dropped++
				log.Printf("[finalize] dropping citation %s in fragment %d (msg id=%d): message %s is absent from the frozen merged pool",
					match, i+1, out[i].ID, identity)
				return ""
			}
			return fmt.Sprintf("[%d]", target)
		})
	}
	return out, dropped
}

// messageIdentity is the stable, index-independent name of a source message.
// It is the same key gatherSessionEvidencePool de-dups on, so the two cannot
// disagree about what "the same message" means.
func messageIdentity(m pipeline.Message) string {
	return fmt.Sprintf("%s:%d", m.ChannelID, m.MessageSeq)
}

// filterEvidenceRowsCreatedBefore keeps the rows that already existed when a
// given reply was written, preserving input order.
func filterEvidenceRowsCreatedBefore(rows []model.AgentMessageEvidence, bound time.Time) []model.AgentMessageEvidence {
	if bound.IsZero() {
		return rows
	}
	out := make([]model.AgentMessageEvidence, 0, len(rows))
	for _, r := range rows {
		if !r.CreatedAt.After(bound) {
			out = append(out, r)
		}
	}
	return out
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
// NOTE: this bound freezes the pool against evidence written AFTER the click. It
// does NOT by itself make a merged multi-turn body's markers correct — each turn
// numbered against its own smaller pool, so the merge needs an explicit remap
// (remapFinalizeCitations) on top of this bound. Do not read the bound as "the
// number space is frozen"; it is not, and saying so is what the previous round's
// comment here got wrong.
//
// Best-effort: on any DB or decode error it returns what it has (citations
// degrade, the body does not).
//
// Note this filters on ev.Evidence == "" where the two canonical builders filter
// on ev.Handle == "". Both are no-ops on real rows (PersistEvidence writes both
// together); this one is the filter that matters here because the cache tier the
// others consult is not available in the worker process, so the JSON snapshot is
// the only source.
//
// Split into loadSessionEvidenceRows + buildPoolFromEvidenceRows because the
// cross-turn remap needs to rebuild N per-turn pools from ONE query rather than
// issuing N queries; this composition is the original single-shot form, kept for
// callers that only want the final pool.
//
// P2-8 follow-up note: this split makes the still-pending extraction of ONE
// shared pool builder CHEAPER, not harder. buildPoolFromEvidenceRows is now a
// pure (rows -> indexed pool) function with no DB, context or session coupling,
// which is exactly the shape the two canonical builders would have to converge
// on; what remains to reconcile is only the query/source tier (they consult a
// cache tier this process does not have) and the ev.Handle == "" vs
// ev.Evidence == "" filter.
func gatherSessionEvidencePool(ctx context.Context, db *gorm.DB, userID, sessionID string, createdBefore time.Time) []pipeline.Message {
	return buildPoolFromEvidenceRows(loadSessionEvidenceRows(ctx, db, userID, sessionID, createdBefore))
}

// loadSessionEvidenceRows fetches the session's evidence rows up to createdBefore,
// in the canonical (created_at ASC, handle ASC) order the de-dup depends on.
//
// On created_at vs updated_at (P2-4): the bound stays on created_at, and both
// this query and remapFinalizeCitations' per-turn filter use the SAME field, so
// they cannot disagree about which rows a turn saw. The reviewer is right that
// created_at immutability is not enforced by the writer — PersistEvidence upserts
// with ON DUPLICATE KEY UPDATE evidence = VALUES(evidence) and deliberately does
// not refresh created_at — so a row rewritten after the freeze passes this
// filter with new content. Switching to updated_at would express "unchanged
// since the freeze" more literally, but it would be WRONG for the remap: a row
// legitimately created during turn 1 and touched during turn 5 would drop out of
// turn 1's re-derived pool entirely, shifting turn 1's numbering away from what
// that turn actually saw and turning correct markers into dropped ones.
// created_at is the field that answers "did this row exist when that turn ran", which
// is the question the remap asks. The rewritten-content risk is real but narrow
// (it needs a process-local handle counter to be reused, i.e. a restart during a
// live session) and is the same residual documented on remapFinalizeCitations;
// the durable fix for both is reading the frozen per-run manifests, which needs
// a schema change (agent_message has no run_id).
func loadSessionEvidenceRows(ctx context.Context, db *gorm.DB, userID, sessionID string, createdBefore time.Time) []model.AgentMessageEvidence {
	q := db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID)
	if !createdBefore.IsZero() {
		q = q.Where("created_at <= ?", createdBefore)
	}
	var rows []model.AgentMessageEvidence
	if err := q.Order("created_at ASC, handle ASC").Find(&rows).Error; err != nil {
		return nil
	}
	return rows
}

// buildPoolFromEvidenceRows decodes, de-dups, sorts and positionally indexes the
// evidence rows into a citation pool. This is the numbering rule
// tool_summarize_chunk.go applies per turn; reproducing it byte-for-byte is what
// makes the per-turn re-derivation in remapFinalizeCitations meaningful.
func buildPoolFromEvidenceRows(rows []model.AgentMessageEvidence) []pipeline.Message {

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
			key := messageIdentity(m)
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
// nothing. The handler creates the creator participant + a Pending
// personal_result in the SAME transaction as the task, and processTask
// bootstraps them defensively one line after this returns when
// participantCount == 0. Calling bootstrapCreatorParticipant here as well was a
// GUARANTEED unique-key conflict on uk_summary_participant_task_user on every
// finalize run — and bootstrapCreatorParticipant decides insert-vs-conflict via
// result.RowsAffected == 0, the exact DSN-sensitive pattern this PR replaced in
// the handler: under clientFoundRows=true the conflict reports one affected row,
// the reload is skipped, participant.ID stays 0, and the personal_result insert
// writes an orphan with participant_ref_id = 0. Not calling it sidesteps the DSN
// question entirely and loses no coverage.
//
// So this validates the task shape and hands off; processTask's own dispatch
// logic takes it from there and processPersonalSummaryWithOptions routes to
// executeAgentFinalize.
func (p *Processor) executeFinalizeTask(task model.SummaryTask) error {
	if task.AgentSessionID == "" {
		return fmt.Errorf("agent-finalize task %d has no agent_session_id", task.ID)
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

	// Walk newest→oldest, keeping while the budget holds. keepFrom is assigned on
	// the FIRST iteration unconditionally (the break guard requires
	// keepFrom < len(replies), which is false at i == len-1), and the caller
	// guarantees replies is non-empty, so no "nothing was kept" branch is
	// reachable afterwards.
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
	if keepFrom <= 0 {
		return replies, 0
	}
	log.Printf("[finalize] session prompt over budget: dropping %d oldest of %d fragments", keepFrom, len(replies))
	return replies[keepFrom:], keepFrom
}
