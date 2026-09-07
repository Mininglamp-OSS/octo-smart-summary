package model

import "time"

// AgentSummarySession stores the folded, authoritative state of one summary
// workspace. The ownership tuple is space-scoped so identical user/session
// identifiers in different spaces never share drafts or workflow state.
type AgentSummarySession struct {
	ID             int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	SpaceID        string `gorm:"column:space_id;type:varchar(64);not null;uniqueIndex:uk_agent_summary_session,priority:1" json:"space_id"`
	UserID         string `gorm:"column:user_id;type:varchar(64);not null;uniqueIndex:uk_agent_summary_session,priority:2;index:idx_agent_summary_session_agent_identity,priority:1" json:"user_id"`
	SessionID      string `gorm:"column:session_id;type:varchar(128);not null;uniqueIndex:uk_agent_summary_session,priority:3" json:"session_id"`
	AgentSessionID string `gorm:"column:agent_session_id;type:varchar(80);not null;default:'';index:idx_agent_summary_session_agent_identity,priority:2" json:"-"`

	ContractVersion string `gorm:"column:contract_version;type:varchar(16);not null;default:'1'" json:"contract_version"`
	State           string `gorm:"column:state;type:varchar(32);not null;default:'idle'" json:"state"`
	StateVersion    int64  `gorm:"column:state_version;not null;default:1" json:"state_version"`
	ScopeVersion    int    `gorm:"column:scope_version;not null;default:1" json:"scope_version"`
	ScopeJSON       string `gorm:"column:scope_json;type:json" json:"scope_json"`
	ScopeHash       string `gorm:"column:scope_hash;type:char(64);not null;default:''" json:"-"`
	ActiveTurnID    int64  `gorm:"column:active_turn_id;not null;default:0" json:"active_turn_id"`

	ArtifactVersion          int   `gorm:"column:artifact_version;not null;default:0" json:"artifact_version"`
	LatestPreviewMessageID   int64 `gorm:"column:latest_preview_message_id;not null;default:0" json:"latest_preview_message_id"`
	LatestPreviewSavedTaskID int64 `gorm:"column:latest_preview_saved_task_id;not null;default:0" json:"latest_preview_saved_task_id"`

	PendingProposalVersion      int     `gorm:"column:pending_proposal_version;not null;default:0" json:"pending_proposal_version"`
	PendingProposalStatus       string  `gorm:"column:pending_proposal_status;type:varchar(16);not null;default:''" json:"pending_proposal_status"`
	PendingProposalToken        string  `gorm:"column:pending_proposal_token;type:varchar(128);not null;default:''" json:"-"`
	PendingProposalJSON         *string `gorm:"column:pending_proposal_json;type:json" json:"pending_proposal_json,omitempty"`
	PendingProposalMessageID    int64   `gorm:"column:pending_proposal_message_id;not null;default:0" json:"pending_proposal_message_id"`
	PendingProposalScopeVersion int     `gorm:"column:pending_proposal_scope_version;not null;default:0" json:"pending_proposal_scope_version"`
	PendingProposalTaskID       int64   `gorm:"column:pending_proposal_task_id;not null;default:0" json:"pending_proposal_task_id"`

	WorkflowTaskID            int64  `gorm:"column:workflow_task_id;not null;default:0;index:idx_agent_summary_session_workflow" json:"workflow_task_id"`
	WorkflowScope             string `gorm:"column:workflow_scope;type:varchar(16);not null;default:''" json:"workflow_scope"`
	WorkflowScopeVersion      int    `gorm:"column:workflow_scope_version;not null;default:0" json:"workflow_scope_version"`
	WorkflowStartedMessageID  int64  `gorm:"column:workflow_started_message_id;not null;default:0" json:"workflow_started_message_id"`
	WorkflowTerminalMessageID int64  `gorm:"column:workflow_terminal_message_id;not null;default:0" json:"workflow_terminal_message_id"`

	ExpiresAt *time.Time `gorm:"column:expires_at;index:idx_agent_summary_session_expires" json:"expires_at,omitempty"`
	CreatedAt time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (AgentSummarySession) TableName() string { return "agent_summary_session" }

// AgentSummaryTurn is the durable idempotency and lease record for one client
// request. Request IDs use binary collation in the SQL migration.
type AgentSummaryTurn struct {
	ID             int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	SpaceID        string     `gorm:"column:space_id;type:varchar(64);not null;uniqueIndex:uk_agent_summary_turn_request,priority:1;index:idx_agent_summary_turn_owner_session,priority:1" json:"space_id"`
	UserID         string     `gorm:"column:user_id;type:varchar(64);not null;uniqueIndex:uk_agent_summary_turn_request,priority:2;index:idx_agent_summary_turn_owner_session,priority:2" json:"user_id"`
	SessionID      string     `gorm:"column:session_id;type:varchar(128);not null;uniqueIndex:uk_agent_summary_turn_request,priority:3;index:idx_agent_summary_turn_owner_session,priority:3" json:"session_id"`
	RequestID      string     `gorm:"column:request_id;type:varchar(128);not null;uniqueIndex:uk_agent_summary_turn_request,priority:4" json:"request_id"`
	RequestHash    string     `gorm:"column:request_hash;type:char(64);not null" json:"-"`
	ScopeVersion   int        `gorm:"column:scope_version;not null" json:"scope_version"`
	Status         string     `gorm:"column:status;type:varchar(16);not null;index:idx_agent_summary_turn_owner_session,priority:4" json:"status"`
	Attempt        int        `gorm:"column:attempt;not null;default:1" json:"attempt"`
	LeaseExpiresAt *time.Time `gorm:"column:lease_expires_at" json:"lease_expires_at,omitempty"`
	RunID          string     `gorm:"column:run_id;type:varchar(64);not null;default:''" json:"run_id,omitempty"`

	ResponseMessageID int64   `gorm:"column:response_message_id;not null;default:0" json:"response_message_id"`
	ResultType        string  `gorm:"column:result_type;type:varchar(32);not null;default:''" json:"result_type"`
	ResponseJSON      *string `gorm:"column:response_json;type:json" json:"response_json,omitempty"`
	ErrorCode         string  `gorm:"column:error_code;type:varchar(64);not null;default:''" json:"error_code,omitempty"`

	CreatedAt   time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
	CompletedAt *time.Time `gorm:"column:completed_at" json:"completed_at,omitempty"`
}

func (AgentSummaryTurn) TableName() string { return "agent_summary_turn" }
