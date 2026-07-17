package handler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// maxHistoryRows 是 LoadHistory 在 DB 层的粗筛上限：只取每 session 最近这么多条，
// 避免长会话全表扫描。精确滑窗仍由 agent.TruncateHistory 按 user 轮边界在上层做，
// 这里只做数量兜底（取 200 与 window*8 的较大者，window*8 覆盖单轮可能多条 tool 消息）。
const maxHistoryRows = 200

// agentHistoryStore 抽象多轮记忆的读写，便于 handler 单测注入 mock（无需真 DB）。
//
// 权限模型（SUM-158 blocker 1 修复）：所有查询必须携带 userID，服务端从鉴权
// 中间件（middleware.GetUserID）注入。跨用户命中返回空历史（LoadHistory）或
// 不落库（AppendMessages 侧不该出现——handler 层保证 userID 一致后才调）。
type agentHistoryStore interface {
	LoadHistory(ctx context.Context, sessionID, userID string) ([]agent.Message, error)
	AppendMessages(ctx context.Context, sessionID, userID string, msgs []agent.Message) error
}

// agentMessageRepo 是 agentHistoryStore 的 gorm 实现，落在 agent_message 表。
type agentMessageRepo struct {
	db *gorm.DB
}

func newAgentMessageRepo(db *gorm.DB) *agentMessageRepo {
	return &agentMessageRepo{db: db}
}

// LoadHistory 取该 (user_id, session_id) 最近 maxHistoryRows 条（DB 层 id DESC + Limit
// 粗筛），在 Go 里反转回 id 升序（即对话时序）后返回，维持 LoadHistory 升序返回的既有契约。
//
// 权限：强制 user_id 过滤——即使跨用户猜到相同 session_id 字面值，也只会看到自己那部分
// 记录（各自表现为一个不存在的会话，返回空历史）。
func (r *agentMessageRepo) LoadHistory(ctx context.Context, sessionID, userID string) ([]agent.Message, error) {
	var rows []model.AgentMessage
	// id DESC + Limit 命中 idx_user_session_created(user_id, session_id, id) 只扫最近后缀，
	// 避免全表拉取。
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("id DESC").
		Limit(maxHistoryRows).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rowsDescToMessagesAsc(rows)
}

// rowsDescToMessagesAsc 把 DB 层按 id 降序取回的行反转成升序（对话时序）并映射为 agent.Message。
// 独立成函数以便无 DB 单测锁定“反转升序 + 映射”契约。
func rowsDescToMessagesAsc(rows []model.AgentMessage) ([]agent.Message, error) {
	// DB 取回的是 id 降序（最近在前），反转成升序，保持返回升序契约。
	for l, h := 0, len(rows)-1; l < h; l, h = l+1, h-1 {
		rows[l], rows[h] = rows[h], rows[l]
	}
	msgs := make([]agent.Message, 0, len(rows))
	for i := range rows {
		m := agent.Message{
			Role:       rows[i].Role,
			Content:    rows[i].Content,
			ToolCallID: rows[i].ToolCallID,
			Name:       rows[i].Name,
		}
		// tool_calls 仅 assistant 轮非空；反序列化失败则整条历史不可信，直接报错。
		if rows[i].ToolCalls != nil && *rows[i].ToolCalls != "" {
			if err := json.Unmarshal([]byte(*rows[i].ToolCalls), &m.ToolCalls); err != nil {
				return nil, err
			}
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// AppendMessages 批量落本回合新增消息；tool_calls 序列化成 JSON 存列。
//
// 权限：user_id 由 handler 从鉴权中间件注入，与 LoadHistory 走同一属主，确保写入
// 时属主一致，杜绝跨用户污染。
func (r *agentMessageRepo) AppendMessages(ctx context.Context, sessionID, userID string, msgs []agent.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	now := time.Now()
	rows := make([]model.AgentMessage, 0, len(msgs))
	for i := range msgs {
		row := model.AgentMessage{
			SessionID:  sessionID,
			UserID:     userID,
			Role:       msgs[i].Role,
			Content:    msgs[i].Content,
			ToolCallID: msgs[i].ToolCallID,
			Name:       msgs[i].Name,
			CreatedAt:  now,
		}
		if len(msgs[i].ToolCalls) > 0 {
			b, err := json.Marshal(msgs[i].ToolCalls)
			if err != nil {
				return err
			}
			s := string(b)
			row.ToolCalls = &s
		}
		rows = append(rows, row)
	}
	return r.db.WithContext(ctx).Create(&rows).Error
}
