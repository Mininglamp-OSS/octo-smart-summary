package model

import (
	"encoding/json"
	"time"
)

// Personal result worker status constants.
const (
	PersonalStatusPending    = 0
	PersonalStatusProcessing = 1
	PersonalStatusCompleted  = 2
	PersonalStatusFailed     = 3
)

const (
	WorkflowStageUnderstandQuestion  = "understand_question"
	WorkflowStageFindRelevantChats   = "find_relevant_chats"
	WorkflowStageFilterUsefulContent = "filter_useful_content"
	WorkflowStageAnalyzeChatContent  = "analyze_chat_content"
	WorkflowStageGenerateSummary     = "generate_summary"
)

// Participant status constants for by-person mode.
const (
	ParticipantPending    = 0
	ParticipantAccepted   = 1
	ParticipantDeclined   = 2
	ParticipantProcessing = 3
	ParticipantCompleted  = 4
	ParticipantSubmitted  = 5
)

// PersonalResult represents a per-participant summary result.
type PersonalResult struct {
	ID               int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID           int64      `gorm:"column:task_id;not null" json:"task_id"`
	ParticipantRefID int64      `gorm:"column:participant_ref_id;not null" json:"participant_ref_id"`
	UserID           string     `gorm:"column:user_id;type:varchar(64);not null" json:"user_id"`
	Content          string     `gorm:"column:content;type:mediumtext;not null" json:"content"`
	CitationsJSON    string     `gorm:"column:citations_json;type:mediumtext" json:"-"`
	SnapshotJSON     *string    `gorm:"column:snapshot_json;type:mediumtext" json:"-"`
	MsgCount         int        `gorm:"column:msg_count;not null;default:0" json:"msg_count"`
	TotalTokenUsed   int        `gorm:"column:total_token_used;not null;default:0" json:"total_token_used"`
	ModelVersion     string     `gorm:"column:model_version;type:varchar(50);not null;default:''" json:"model_version"`
	CurrentVersionID *int64     `gorm:"column:current_version_id" json:"current_version_id"`
	ContentRevision  int64      `gorm:"column:content_revision;not null;default:0" json:"content_revision"`
	WorkerStatus     int        `gorm:"column:worker_status;type:tinyint;not null;default:0" json:"worker_status"`
	WorkflowStage    string     `gorm:"column:workflow_stage;type:varchar(32);not null;default:''" json:"workflow_stage"`
	RetryCount       int        `gorm:"column:retry_count;type:tinyint;not null;default:0" json:"retry_count"`
	ErrorMessage     *string    `gorm:"column:error_message;type:varchar(500)" json:"error_message"`
	EditedAt         *time.Time `gorm:"column:edited_at" json:"edited_at"`
	SubmittedAt      *time.Time `gorm:"column:submitted_at" json:"submitted_at"`
	SubmitSource     int        `gorm:"column:submit_source;type:tinyint;not null;default:0" json:"submit_source"`
	GeneratedAt      *time.Time `gorm:"column:generated_at" json:"generated_at"`
	CreatedAt        time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (PersonalResult) TableName() string { return "summary_personal_result" }

// PersonalResultVersion stores lightweight history for the caller-owned
// PersonalResult. summary_personal_result remains the current materialized row
// used by read paths; this table is only version history.
type PersonalResultVersion struct {
	ID                     int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID                 int64      `gorm:"column:task_id;not null;uniqueIndex:uk_personal_result_version_task_user_version" json:"task_id"`
	ParticipantRefID       int64      `gorm:"column:participant_ref_id;not null" json:"participant_ref_id"`
	UserID                 string     `gorm:"column:user_id;type:varchar(64);not null;uniqueIndex:uk_personal_result_version_task_user_version;uniqueIndex:uk_personal_generation_user" json:"user_id"`
	Content                string     `gorm:"column:content;type:mediumtext;not null" json:"content"`
	CitationsJSON          string     `gorm:"column:citations_json;type:mediumtext" json:"-"`
	MsgCount               int        `gorm:"column:msg_count;not null;default:0" json:"msg_count"`
	TotalTokenUsed         int        `gorm:"column:total_token_used;not null;default:0" json:"total_token_used"`
	ModelVersion           string     `gorm:"column:model_version;type:varchar(50);not null;default:''" json:"model_version"`
	Version                int        `gorm:"column:version;not null;default:1;uniqueIndex:uk_personal_result_version_task_user_version" json:"version"`
	OperationType          string     `gorm:"column:operation_type;type:varchar(32);not null;default:'generate'" json:"operation_type"`
	OperationNote          string     `gorm:"column:operation_note;type:text" json:"operation_note"`
	ParentVersionID        *int64     `gorm:"column:parent_version_id" json:"parent_version_id,omitempty"`
	CreatedBy              string     `gorm:"column:created_by;type:varchar(64);not null;default:''" json:"created_by"`
	GeneratedAt            time.Time  `gorm:"column:generated_at;not null" json:"generated_at"`
	CreatedAt              time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
	ContentRevision        int64      `gorm:"column:content_revision;not null;default:0" json:"content_revision"`
	BaseContentRevision    int64      `gorm:"column:base_content_revision;not null;default:0" json:"base_content_revision"`
	GenerationSpecSnapshot JSON       `gorm:"column:generation_spec_snapshot;type:json" json:"-"`
	GenerationID           *string    `gorm:"column:generation_id;type:varchar(36);uniqueIndex:uk_personal_generation_user" json:"generation_id,omitempty"`
	EditedAt               *time.Time `gorm:"column:edited_at" json:"edited_at,omitempty"`
	EditedBy               string     `gorm:"column:edited_by;type:varchar(64);not null;default:''" json:"edited_by,omitempty"`
	RestoredFromVersionID  *int64     `gorm:"column:restored_from_version_id" json:"-"`
	RestoredAt             *time.Time `gorm:"column:restored_at" json:"restored_at,omitempty"`
}

func (PersonalResultVersion) TableName() string { return "summary_personal_result_version" }

func (r *PersonalResultVersion) GetCitations() []Citation {
	if r.CitationsJSON == "" {
		return []Citation{}
	}
	var citations []Citation
	if err := json.Unmarshal([]byte(r.CitationsJSON), &citations); err != nil {
		return []Citation{}
	}
	return citations
}

func (r *PersonalResultVersion) SetCitations(citations []Citation) {
	if len(citations) == 0 {
		r.CitationsJSON = "[]"
		return
	}
	data, err := json.Marshal(citations)
	if err != nil {
		r.CitationsJSON = "[]"
		return
	}
	r.CitationsJSON = string(data)
}

// GetCitations deserializes CitationsJSON into a slice of Citation.
func (r *PersonalResult) GetCitations() []Citation {
	if r.CitationsJSON == "" {
		return []Citation{}
	}
	var citations []Citation
	if err := json.Unmarshal([]byte(r.CitationsJSON), &citations); err != nil {
		return []Citation{}
	}
	return citations
}

// SetCitations serializes a slice of Citation into CitationsJSON.
func (r *PersonalResult) SetCitations(citations []Citation) {
	if len(citations) == 0 {
		r.CitationsJSON = "[]"
		return
	}
	data, err := json.Marshal(citations)
	if err != nil {
		r.CitationsJSON = "[]"
		return
	}
	r.CitationsJSON = string(data)
}

// WorkerTriggerRequest is the payload for POST /internal/worker-trigger.
type WorkerTriggerRequest struct {
	Type             string `json:"type"` // "personal_summary" or "meta_summary"
	TaskID           int64  `json:"task_id"`
	ParticipantRefID int64  `json:"participant_ref_id,omitempty"`
}

// ParticipantStatusLabel maps participant status int to a display string.
func ParticipantStatusLabel(status int) string {
	switch status {
	case ParticipantPending:
		return "pending"
	case ParticipantAccepted:
		return "accepted"
	case ParticipantDeclined:
		return "declined"
	case ParticipantProcessing:
		return "processing"
	case ParticipantCompleted:
		return "completed"
	case ParticipantSubmitted:
		return "submitted"
	default:
		return "unknown"
	}
}

// GetSnapshot deserializes SnapshotJSON into a Snapshot.
func (r *PersonalResult) GetSnapshot() *Snapshot {
	if r.SnapshotJSON == nil || *r.SnapshotJSON == "" {
		return nil
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(*r.SnapshotJSON), &snap); err != nil {
		return nil
	}
	return &snap
}

// SetSnapshot serializes a Snapshot into SnapshotJSON.
func (r *PersonalResult) SetSnapshot(snap *Snapshot) {
	if snap == nil {
		r.SnapshotJSON = nil
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		r.SnapshotJSON = nil
		return
	}
	str := string(data)
	r.SnapshotJSON = &str
}
