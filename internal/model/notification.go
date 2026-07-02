package model

import "time"

// Notification kind constants (summary_notification.notify_kind).
// Leader 拍板选 A：failed/completed 各发一次（UNIQUE(task_id, notify_kind)）。
const (
	NotifyKindCompleted = "completed"
	NotifyKindFailed    = "failed"
)

// Notification status constants (summary_notification.status).
const (
	NotifyStatusPending = "pending" // 抢占式插入后的初始态
	NotifyStatusSent    = "sent"    // 投递成功（HTTP 2xx）
	NotifyStatusFailed  = "failed"  // 重试耗尽，终态失败
)

// SummaryNotification is the dedup / idempotency state machine row for a single
// terminal-state notification. It lives in the summary DB (never the IM DB).
//
// The UNIQUE(task_id, notify_kind, recipient_uid) key is the per-recipient
// preemptive-insert lock: the first writer INSERTs status='pending' for a given
// (task, kind, uid) and wins; a duplicate-key error means that recipient is
// already owned so the current run skips it. by-person tasks fan out to one row
// per recipient, so each person is deduped/retried independently. A row is NEVER
// DELETEd — failed deliveries stay as status='failed' with last_error for audit.
type SummaryNotification struct {
	ID           int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID       int64      `gorm:"column:task_id;not null;uniqueIndex:uk_task_kind_uid,priority:1" json:"task_id"`
	NotifyKind   string     `gorm:"column:notify_kind;type:varchar(16);not null;uniqueIndex:uk_task_kind_uid,priority:2" json:"notify_kind"`
	RecipientUID string     `gorm:"column:recipient_uid;type:varchar(64);not null;default:'';uniqueIndex:uk_task_kind_uid,priority:3" json:"recipient_uid"`
	Status       string     `gorm:"column:status;type:varchar(16);not null;default:'pending'" json:"status"`
	AttemptCount int        `gorm:"column:attempt_count;not null;default:0" json:"attempt_count"`
	LastError    *string    `gorm:"column:last_error;type:varchar(500)" json:"last_error"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
	SentAt       *time.Time `gorm:"column:sent_at" json:"sent_at"`
}

func (SummaryNotification) TableName() string { return "summary_notification" }
