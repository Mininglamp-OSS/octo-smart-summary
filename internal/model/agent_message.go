package model

import "time"

// AgentMessage 是一次多轮对话中落库的单条消息（system 不落库，运行时注入）。
// 按 (user_id, session_id, id) 顺序即为对话时序；tool_calls 原样存 JSON，回喂时需完整还原。
//
// 权限模型（SUM-158 blocker 1 修复）：
//   - 每条消息强制携带 user_id（消息属主），由服务端从鉴权中间件注入。
//   - 所有 session_id 查询必须同时校验 user_id；跨用户访问返 404（不泄漏 session 存在）。
//   - 不同用户偶然使用相同 session_id 字面值是允许的——查询按 (user_id, session_id) 过滤，
//     各自看到自己的历史；handler 层 owner check 是安全兜底。
type AgentMessage struct {
	ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	SessionID  string    `gorm:"column:session_id;type:varchar(128);not null" json:"session_id"`
	UserID     string    `gorm:"column:user_id;type:varchar(64);not null;index:idx_user_session_created,priority:1" json:"user_id"`
	Role       string    `gorm:"column:role;type:varchar(16);not null" json:"role"`
	Content    string    `gorm:"column:content;type:mediumtext" json:"content"`
	ToolCalls  *string   `gorm:"column:tool_calls;type:json" json:"tool_calls"`
	ToolCallID string    `gorm:"column:tool_call_id;type:varchar(128)" json:"tool_call_id"`
	Name       string    `gorm:"column:name;type:varchar(128)" json:"name"`
	CreatedAt  time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (AgentMessage) TableName() string { return "agent_message" }
