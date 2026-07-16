package model

import "time"

// AgentMessage 是一次多轮对话中落库的单条消息（system 不落库，运行时注入）。
// 按 (session_id, id) 顺序即为对话时序；tool_calls 原样存 JSON，回喂时需完整还原。
type AgentMessage struct {
	ID         int64     `gorm:"primaryKey;autoIncrement;index:idx_session_created,priority:2" json:"id"`
	SessionID  string    `gorm:"column:session_id;type:varchar(128);not null;index:idx_session_created,priority:1" json:"session_id"`
	Role       string    `gorm:"column:role;type:varchar(16);not null" json:"role"`
	Content    string    `gorm:"column:content;type:mediumtext" json:"content"`
	ToolCalls  *string   `gorm:"column:tool_calls;type:json" json:"tool_calls"`
	ToolCallID string    `gorm:"column:tool_call_id;type:varchar(128)" json:"tool_call_id"`
	Name       string    `gorm:"column:name;type:varchar(128)" json:"name"`
	CreatedAt  time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (AgentMessage) TableName() string { return "agent_message" }
