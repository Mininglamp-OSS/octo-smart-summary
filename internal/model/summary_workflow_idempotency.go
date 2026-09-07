package model

import "time"

// SummaryWorkflowIdempotency binds a user-owned workflow creation request to
// exactly one summary task. It is shared by the traditional HTTP endpoint and
// the future Agent workflow tools so retries cannot create duplicate tasks or
// dispatch duplicate workers.
type SummaryWorkflowIdempotency struct {
	ID             int64     `gorm:"primaryKey;autoIncrement"`
	SpaceID        string    `gorm:"column:space_id;type:varchar(64);not null;uniqueIndex:uk_summary_workflow_idempotency,priority:1"`
	UserID         string    `gorm:"column:user_id;type:varchar(64);not null;uniqueIndex:uk_summary_workflow_idempotency,priority:2"`
	IdempotencyKey string    `gorm:"column:idempotency_key;type:varchar(128);not null;uniqueIndex:uk_summary_workflow_idempotency,priority:3"`
	RequestHash    string    `gorm:"column:request_hash;type:char(64);not null"`
	TaskID         int64     `gorm:"column:task_id;not null;index:idx_summary_workflow_idempotency_task"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
}

func (SummaryWorkflowIdempotency) TableName() string {
	return "summary_workflow_idempotency"
}
