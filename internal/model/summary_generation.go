package model

import "time"

// SummaryGenerationRun is the cross-engine admission and commit record.
// A run input is frozen before any LLM/retrieval call. Engine-specific sessions
// remain separate; their preview versions are not formal summary versions.
type SummaryGenerationRun struct {
	ID                  string     `gorm:"column:id;type:varchar(36);primaryKey" json:"generation_id"`
	SpaceID             string     `gorm:"column:space_id;type:varchar(64);not null;index:idx_generation_task" json:"space_id"`
	TaskID              int64      `gorm:"column:task_id;not null;index:idx_generation_task" json:"task_id"`
	ContentID           string     `gorm:"column:content_id;type:varchar(512);not null" json:"content_id"`
	ActorID             string     `gorm:"column:actor_id;type:varchar(64);not null" json:"-"`
	OperationType       string     `gorm:"column:operation_type;type:varchar(32);not null" json:"operation_type"`
	Executor            string     `gorm:"column:executor;type:varchar(16);not null" json:"executor"`
	Scope               string     `gorm:"column:scope;type:varchar(16);not null" json:"generation_scope"`
	ParentGenerationID  *string    `gorm:"column:parent_generation_id;type:varchar(36);index" json:"parent_generation_id,omitempty"`
	IdempotencyHash     string     `gorm:"column:idempotency_hash;type:char(64);not null;uniqueIndex:uk_generation_idempotency" json:"-"`
	RequestHash         string     `gorm:"column:request_hash;type:char(64);not null" json:"-"`
	ActiveSlot          *string    `gorm:"column:active_slot;type:char(64);uniqueIndex:uk_generation_active_slot" json:"-"`
	ScheduleID          *int64     `gorm:"column:schedule_id;uniqueIndex:uk_generation_schedule_slot" json:"schedule_id,omitempty"`
	ScheduledFor        *time.Time `gorm:"column:scheduled_for;type:datetime(6);uniqueIndex:uk_generation_schedule_slot" json:"scheduled_for,omitempty"`
	EffectiveAt         time.Time  `gorm:"column:effective_at;type:datetime(6);not null" json:"effective_at"`
	BaseVersionID       string     `gorm:"column:base_version_id;type:varchar(768);not null" json:"base_version_id"`
	BaseContentRevision int64      `gorm:"column:base_content_revision;not null" json:"base_content_revision"`
	ConfigRevision      int64      `gorm:"column:config_revision;not null" json:"config_revision"`
	InputJSON           JSON       `gorm:"column:input_json;type:json;not null" json:"-"`
	Status              string     `gorm:"column:status;type:varchar(24);not null;index:idx_generation_recovery" json:"status"`
	Stage               string     `gorm:"column:stage;type:varchar(64);not null;default:''" json:"stage"`
	LeaseUntil          *time.Time `gorm:"column:lease_until;type:datetime(6);index:idx_generation_recovery" json:"lease_until,omitempty"`
	ExecutionToken      int64      `gorm:"column:execution_token;not null;default:0" json:"-"`
	CancelRequested     bool       `gorm:"column:cancel_requested;not null;default:false" json:"cancel_requested"`
	OutputVersionID     string     `gorm:"column:output_version_id;type:varchar(768);not null;default:''" json:"output_version_id,omitempty"`
	Applied             bool       `gorm:"column:applied;not null;default:false" json:"applied"`
	ConflictReason      string     `gorm:"column:conflict_reason;type:varchar(64);not null;default:''" json:"conflict_reason,omitempty"`
	ErrorCode           string     `gorm:"column:error_code;type:varchar(64);not null;default:''" json:"error_code,omitempty"`
	CreatedAt           time.Time  `gorm:"column:created_at;type:datetime(6);not null" json:"created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at;type:datetime(6);not null" json:"updated_at"`
}

func (SummaryGenerationRun) TableName() string { return "summary_generation_run" }

// SummaryContentAudit retains operation metadata, not overwritten content.
// An edit/restore is deliberately destructive, as required by product policy.
type SummaryContentAudit struct {
	ID                    int64     `gorm:"primaryKey;autoIncrement"`
	SpaceID               string    `gorm:"column:space_id;type:varchar(64);not null"`
	TaskID                int64     `gorm:"column:task_id;not null;index:idx_content_audit_task"`
	ContentID             string    `gorm:"column:content_id;type:varchar(512);not null"`
	VersionID             string    `gorm:"column:version_id;type:varchar(768);not null"`
	ContentRevision       int64     `gorm:"column:content_revision;not null"`
	OperationType         string    `gorm:"column:operation_type;type:varchar(32);not null"`
	ActorID               string    `gorm:"column:actor_id;type:varchar(64);not null"`
	SourceVersionID       string    `gorm:"column:source_version_id;type:varchar(768);not null;default:''"`
	SourceContentRevision int64     `gorm:"column:source_content_revision;not null;default:0"`
	CreatedAt             time.Time `gorm:"column:created_at;type:datetime(6);not null"`
}

func (SummaryContentAudit) TableName() string { return "summary_content_audit" }
