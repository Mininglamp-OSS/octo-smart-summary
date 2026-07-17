package model

import "time"

// AgentMessageEvidence stores complete pipeline.Message snapshots for citation recovery.
// Fixes SUM-158 Blocker C: buildCitationsForSession failing after 30-minute cache expiry.
//
// Write timing: every fetch_channel / peek_channel tool success (via PersistEvidence helper).
// Read timing: buildCitationsForSession fallback when cache.Retrieve returns nil.
//
// PK (user_id, session_id, handle) ensures one evidence row per cache handle per session;
// upsert semantics allow idempotent tool re-execution without duplicate rows.
type AgentMessageEvidence struct {
	UserID    string    `gorm:"column:user_id;type:varchar(64);not null;primaryKey;priority:1" json:"user_id"`
	SessionID string    `gorm:"column:session_id;type:varchar(128);not null;primaryKey;priority:2" json:"session_id"`
	Handle    string    `gorm:"column:handle;type:varchar(128);not null;primaryKey;priority:3" json:"handle"`
	Evidence  string    `gorm:"column:evidence;type:mediumtext;not null" json:"evidence"` // JSON []pipeline.Message
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (AgentMessageEvidence) TableName() string { return "agent_message_evidence" }
