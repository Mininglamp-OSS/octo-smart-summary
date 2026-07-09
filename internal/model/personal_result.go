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
	MsgCount         int        `gorm:"column:msg_count;not null;default:0" json:"msg_count"`
	TotalTokenUsed   int        `gorm:"column:total_token_used;not null;default:0" json:"total_token_used"`
	ModelVersion     string     `gorm:"column:model_version;type:varchar(50);not null;default:''" json:"model_version"`
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
