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
// 1. 从 agent_message 提取本 session 所有 role='tool' 的 Content
// 2. 解析 JSON,提取 messages_handle (工具返回里的缓存句柄)
// 3. 尝试从 agent.messageCache 恢复 messages (30分钟 TTL)
// 5. 合并去重 → 得到 allMessages 池
// 6. 为每条 message 分配 CitationIndex(1-indexed, 全局唯一, 时间升序)
// 7. 收集 nameMap: sender_uid -> sender_name
// 8. 调 worker.BuildCitations(content, allMessages, allMessages, nameMap)
// 9. 返回结果; 出错走 log + 返回空数组不阻塞落库(citations 是锦上添花不是必要)
func (h *AgentSummaryHandler) buildCitationsForSession(
	ctx context.Context,
	sessionID string,
	content string,
	uid string,
) ([]model.Citation, error) {
	// 1. 从 agent_message 拿本 session 所有 role='tool' 的返回值
	var toolMessages []model.AgentMessage
	err := h.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND role = ?", uid, sessionID, "tool").
		Order("id ASC").
		Find(&toolMessages).Error
	if err != nil {
		log.Printf("[citations] query tool messages failed session=%s: %v", sessionID, err)
		return nil, err
	}

	if len(toolMessages) == 0 {
		// No tool calls = no messages to cite
		return []model.Citation{}, nil
	}

	// 2. 提取所有 messages,尝试从 cache 或直接从 content
	var allMessages []pipeline.Message
	seenKey := make(map[string]bool) // de-dup by channel_id+message_seq

	cache := agent.GetMessageCache()

	for _, tm := range toolMessages {
		if tm.Content == "" {
			continue
		}

		// Parse tool return JSON
		var toolReturn map[string]interface{}
		if err := json.Unmarshal([]byte(tm.Content), &toolReturn); err != nil {
			log.Printf("[citations] parse tool return failed session=%s tool=%s: %v", sessionID, tm.Name, err)
			continue
		}

		// Try to get messages from cache via handle
		if handleRaw, ok := toolReturn["messages_handle"]; ok {
			if handle, ok := handleRaw.(string); ok && handle != "" {
				cached := cache.Retrieve(handle, uid)
				if cached != nil {
					for _, msg := range cached {
						key := fmt.Sprintf("%s:%d", msg.ChannelID, msg.MessageSeq)
						if !seenKey[key] {
							allMessages = append(allMessages, msg)
							seenKey[key] = true
						}
					}
					log.Printf("[citations] retrieved %d messages from cache handle=%s", len(cached), handle)
				} else {
					log.Printf("[citations] cache miss or expired for handle=%s session=%s", handle, sessionID)
				}
			}
		}

	}

	if len(allMessages) == 0 {
		// Tools were called but cache expired or no messages extracted
		log.Printf("[citations] no messages recovered session=%s (cache likely expired)", sessionID)
		return []model.Citation{}, nil
	}

	// 3. Sort by time ascending
	sort.Slice(allMessages, func(i, j int) bool {
		return allMessages[i].Timestamp < allMessages[j].Timestamp
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

