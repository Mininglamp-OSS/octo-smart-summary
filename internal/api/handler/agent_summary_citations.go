package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/worker"
)

// buildCitationsForSession 反查 session_id 的所有工具轨迹,组 messages 池,
// 调 worker.BuildCitations 得到结构化 Citation 数组。
// 若 content 里没有任何 [n] 标记,返回 []Citation{} (等价于 SetCitations(nil))。
//
// 实现策略:
// 1. 从 agent_message_evidence 提取本 (user_id, session_id) 的所有 handle
// 2. 每个 handle 优先走 agent.messageCache 恢复 messages (30分钟 TTL),
//    cache miss 时 fallback 到 evidence.Evidence 的 JSON snapshot
// 3. 合并去重 → 得到 allMessages 池
// 4. 为每条 message 分配 CitationIndex(1-indexed, 全局唯一, 时间升序)
// 5. 收集 nameMap: sender_uid -> sender_name
// 6. 调 worker.BuildCitations(content, allMessages, allMessages, nameMap)
// 7. 返回结果; 出错走 log + 返回空数组不阻塞落库(citations 是锦上添花不是必要)
//
// Discovery-source symmetry (#161 P1-A · yujiawei):
// Must discover handles from agent_message_evidence — byte-identical to
// getSessionMessagePool (internal/agent/tool_summarize_chunk.go). Previously
// this function discovered from agent_message WHERE role='tool' while
// getSessionMessagePool discovered from agent_message_evidence, so an
// orphan-evidence scenario (chat step fails before AppendMessages persists
// tool rows, but PersistEvidence already wrote its evidence row) produced
// a pool asymmetry: mid-run pool saw orphan rows, save-time pool did not,
// CitationIndex 1..N drifted between the two, [n] markers no longer lined
// up with saved Citation rows. Aligning both sites on evidence discovery
// closes that reachable failure path.
func (h *AgentSummaryHandler) buildCitationsForSession(
	ctx context.Context,
	sessionID string,
	content string,
	uid string,
) ([]model.Citation, error) {
	// 1. Discover handles from agent_message_evidence — must stay symmetric
	// with getSessionMessagePool in tool_summarize_chunk.go. Rows are written
	// synchronously by PersistEvidence inside every data-fetching tool
	// (fetch_channel, peek_channel, search_messages, filter_relevant) before
	// the tool returns, so this discovery source is populated for every
	// handle the LLM could cite — regardless of whether the subsequent
	// AppendMessages persisted the corresponding agent_message tool row.
	var evidenceRows []model.AgentMessageEvidence
	err := h.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", uid, sessionID).
		Order("created_at ASC, handle ASC").
		Find(&evidenceRows).Error
	if err != nil {
		log.Printf("[citations] query evidence rows failed session=%s: %v", sessionID, err)
		return nil, err
	}

	if len(evidenceRows) == 0 {
		// No tool calls = no messages to cite
		return []model.Citation{}, nil
	}

	// 2. Resolve each handle to its messages: cache preferred (hot path),
	// evidence JSON snapshot as fallback (cold cache / restart).
	var allMessages []pipeline.Message
	seenKey := make(map[string]bool) // de-dup by channel_id+message_seq

	cache := agent.GetMessageCache()

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
			log.Printf("[citations] retrieved %d messages from cache handle=%s", len(cached), ev.Handle)
			continue
		}

		// Cache miss: fallback to evidence JSON snapshot. Log both success
		// and unmarshal failure for parity with observability elsewhere.
		log.Printf("[citations] cache miss for handle=%s session=%s, falling back to evidence JSON", ev.Handle, sessionID)
		var evidenceMessages []pipeline.Message
		if err := json.Unmarshal([]byte(ev.Evidence), &evidenceMessages); err != nil {
			log.Printf("[citations] evidence unmarshal failed handle=%s: %v", ev.Handle, err)
			continue
		}
		for _, msg := range evidenceMessages {
			key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
			if !seenKey[key] {
				allMessages = append(allMessages, msg)
				seenKey[key] = true
			}
		}
		log.Printf("[citations] retrieved %d messages from evidence table handle=%s", len(evidenceMessages), ev.Handle)
	}

	if len(allMessages) == 0 {
		// Tools were called but cache expired or no messages extracted
		log.Printf("[citations] no messages recovered session=%s (cache likely expired)", sessionID)
		return []model.Citation{}, nil
	}

	// 3. Sort by timestamp ascending, with (ChannelID, MessageSeq) as deterministic
	// tiebreaker. Must stay byte-identical to the sort in
	// internal/agent/tool_summarize_chunk.go:60-70 so that the pre-assigned
	// CitationIndex from the tool layer matches the post-assignment here —
	// see SUM-47 v3 rationale.
	sort.Slice(allMessages, func(i, j int) bool {
		if allMessages[i].Timestamp != allMessages[j].Timestamp {
			return allMessages[i].Timestamp < allMessages[j].Timestamp
		}
		if allMessages[i].ChannelID != allMessages[j].ChannelID {
			return allMessages[i].ChannelID < allMessages[j].ChannelID
		}
		return allMessages[i].MessageSeq < allMessages[j].MessageSeq
	})

	// 4. Assign CitationIndex (1-indexed, global sequential)
	for i := range allMessages {
		allMessages[i].CitationIndex = i + 1
	}

	// 5. Build nameMap
	nameMap := make(map[string]string)
	for _, msg := range allMessages {
		if msg.SenderUID != "" && msg.SenderName != "" {
			nameMap[msg.SenderUID] = msg.SenderName
		}
	}

	// 6. Call worker.BuildCitations
	citations := worker.BuildCitations(content, allMessages, allMessages, nameMap)

	log.Printf("[citations] built %d citations from %d messages session=%s", len(citations), len(allMessages), sessionID)
	return citations, nil
}

