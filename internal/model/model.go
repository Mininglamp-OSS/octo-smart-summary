package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// JSON is a custom type for JSON columns in MySQL.
type JSON json.RawMessage

func (j JSON) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return []byte(j), nil
}

func (j *JSON) Scan(src interface{}) error {
	if src == nil {
		*j = nil
		return nil
	}
	switch v := src.(type) {
	case []byte:
		cp := make([]byte, len(v))
		copy(cp, v)
		*j = cp
		return nil
	case string:
		*j = []byte(v)
		return nil
	default:
		return errors.New("unsupported type for JSON")
	}
}

func (j JSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return []byte(j), nil
}

func (j *JSON) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*j = nil
		return nil
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	*j = cp
	return nil
}

// Task status constants.
const (
	StatusPending        = 0
	StatusWaitingConfirm = 1
	StatusProcessing     = 2
	StatusCompleted      = 3
	StatusFailed         = 4
	StatusCancelled      = 5
)

// Trigger type constants.
const (
	TriggerManual    = 1
	TriggerScheduled = 2
)

// Summary mode constants.
const (
	ModeByPerson = 2
)

// Source type constants.
const (
	SourceGroup  = 1
	SourceThread = 2
	SourceDirect = 3
)

// Origin channel type constants.
const (
	OriginChannelGlobal = 0
	OriginChannelGroup  = 1
	OriginChannelThread = 2
	OriginChannelDM     = 3
)

// Channel type constants (aligned with the IM server protocol).
const (
	ChannelTypeDM    = 1
	ChannelTypeGroup = 2
)

// SummaryTask represents a summary generation task.
type SummaryTask struct {
	ID                 int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskNo             string     `gorm:"column:task_no;type:varchar(32);uniqueIndex:uk_task_no;not null" json:"task_no"`
	SpaceID            string     `gorm:"column:space_id;type:varchar(64);not null;default:''" json:"space_id"`
	CreatorID          string     `gorm:"column:creator_id;type:varchar(64);not null" json:"creator_id"`
	Title              string     `gorm:"column:title;type:varchar(1000);not null;default:''" json:"title"`
	SummaryMode        int        `gorm:"column:summary_mode;type:tinyint;not null" json:"summary_mode"`
	TimeRangeStart     time.Time  `gorm:"column:time_range_start;not null" json:"time_range_start"`
	TimeRangeEnd       time.Time  `gorm:"column:time_range_end;not null" json:"time_range_end"`
	Status             int        `gorm:"column:status;type:tinyint;not null;default:0" json:"status"`
	TriggerType        int        `gorm:"column:trigger_type;type:tinyint;not null;default:1" json:"trigger_type"`
	RetryCount         int        `gorm:"column:retry_count;type:tinyint;not null;default:0" json:"retry_count"`
	ErrorMessage       *string    `gorm:"column:error_message;type:varchar(500)" json:"error_message"`
	ScheduleID         *int64     `gorm:"column:schedule_id" json:"schedule_id"`
	OriginChannelID    string     `gorm:"column:origin_channel_id;type:varchar(64);not null;default:'';index:idx_origin_channel" json:"origin_channel_id"`
	OriginChannelType  int        `gorm:"column:origin_channel_type;type:tinyint;not null;default:0" json:"origin_channel_type"`
	ProcessingDeadline *time.Time `gorm:"column:processing_deadline" json:"processing_deadline"`
	ConfirmDeadline    *time.Time `gorm:"column:confirm_deadline" json:"confirm_deadline"`
	// NotifiedAt is the crash-safe idempotency anchor for the summary-notify
	// tip. A single-writer CAS (notified_at IS NULL → now) guarantees
	// exactly-once emit per task even across worker restarts. NULL = not yet
	// notified.
	NotifiedAt *time.Time `gorm:"column:notified_at" json:"notified_at,omitempty"`
	CreatedAt  time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt  *time.Time `gorm:"column:deleted_at;index" json:"deleted_at,omitempty"`
}

func (SummaryTask) TableName() string { return "summary_task" }

// SummarySource represents a data source for a task.
type SummarySource struct {
	ID            int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID        int64     `gorm:"column:task_id;not null;index:idx_task_id" json:"task_id"`
	SourceType    int       `gorm:"column:source_type;type:tinyint;not null" json:"source_type"`
	SourceID      string    `gorm:"column:source_id;type:varchar(64);not null" json:"source_id"`
	SourceName    string    `gorm:"column:source_name;type:varchar(200);not null;default:''" json:"source_name"`
	ParticipantID *int64    `gorm:"column:participant_id;index:idx_participant_id" json:"participant_id"`
	CreatedAt     time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (SummarySource) TableName() string { return "summary_source" }

// SummaryParticipant represents a participant in a by-person task.
type SummaryParticipant struct {
	ID               int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID           int64      `gorm:"column:task_id;not null" json:"task_id"`
	UserID           string     `gorm:"column:user_id;type:varchar(64);not null" json:"user_id"`
	UserName         string     `gorm:"column:user_name;type:varchar(100);not null;default:''" json:"user_name"`
	Status           int        `gorm:"column:status;type:tinyint;not null;default:0" json:"status"`
	ConfirmedAt      *time.Time `gorm:"column:confirmed_at" json:"confirmed_at"`
	PersonalResultID *int64     `gorm:"column:personal_result_id" json:"personal_result_id"`
	WorkerStartedAt  *time.Time `gorm:"column:worker_started_at" json:"worker_started_at"`
	CreatedAt        time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SummaryParticipant) TableName() string { return "summary_participant" }

// SummaryChunk represents a Map-phase intermediate result.
type SummaryChunk struct {
	ID              int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID          int64      `gorm:"column:task_id;not null" json:"task_id"`
	ChunkIndex      int        `gorm:"column:chunk_index;not null" json:"chunk_index"`
	ParticipantID   *int64     `gorm:"column:participant_id" json:"participant_id"`
	SummarySourceID *int64     `gorm:"column:summary_source_id" json:"summary_source_id"`
	MsgCount        int        `gorm:"column:msg_count;not null;default:0" json:"msg_count"`
	MsgStartTime    *time.Time `gorm:"column:msg_start_time" json:"msg_start_time"`
	MsgEndTime      *time.Time `gorm:"column:msg_end_time" json:"msg_end_time"`
	ChunkSummary    string     `gorm:"column:chunk_summary;type:mediumtext;not null" json:"chunk_summary"`
	TokenUsed       int        `gorm:"column:token_used;not null;default:0" json:"token_used"`
	Status          int        `gorm:"column:status;type:tinyint;not null;default:0" json:"status"`
	CreatedAt       time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SummaryChunk) TableName() string { return "summary_chunk" }

// Citation represents a reference from a summary back to the original message.
type Citation struct {
	Index         int          `json:"index"`
	Sender        string       `json:"sender"`
	Content       string       `json:"content"`
	SentAt        string       `json:"sent_at"`
	Source        string       `json:"source"`
	ChannelID     string       `json:"channel_id"`
	ChannelType   int          `json:"channel_type"`
	MessageSeq    int64        `json:"message_seq"`
	ContextBefore []ContextMsg `json:"context_before,omitempty"`
	ContextAfter  []ContextMsg `json:"context_after,omitempty"`
}

// ContextMsg represents a surrounding message used as context for a citation.
type ContextMsg struct {
	Sender     string `json:"sender"`
	Content    string `json:"content"`
	SentAt     string `json:"sent_at"`
	MessageSeq int64  `json:"message_seq"`
}

// TeamCitation represents a [Pn] reference in a team summary pointing to a participant.
type TeamCitation struct {
	Index    int    `json:"index"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

// SummaryResult represents the final summary output.
type SummaryResult struct {
	ID                int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID            int64      `gorm:"column:task_id;not null" json:"task_id"`
	Content           string     `gorm:"column:content;type:mediumtext;not null" json:"content"`
	CitationsJSON     string     `gorm:"column:citations_json;type:mediumtext" json:"-"`
	TeamCitationsJSON string     `gorm:"column:team_citations_json;type:mediumtext" json:"-"`
	TotalMsgCount     int        `gorm:"column:total_msg_count;not null;default:0" json:"total_msg_count"`
	TotalTokenUsed    int        `gorm:"column:total_token_used;not null;default:0" json:"total_token_used"`
	ModelVersion      string     `gorm:"column:model_version;type:varchar(50);not null;default:''" json:"model_version"`
	Version           int        `gorm:"column:version;not null;default:1" json:"version"`
	EditedAt          *time.Time `gorm:"column:edited_at" json:"edited_at"`
	GeneratedAt       time.Time  `gorm:"column:generated_at;not null" json:"generated_at"`
	CreatedAt         time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

// GetCitations deserializes CitationsJSON into a slice of Citation.
func (r *SummaryResult) GetCitations() []Citation {
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
func (r *SummaryResult) SetCitations(citations []Citation) {
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

// GetTeamCitations deserializes TeamCitationsJSON into a slice of TeamCitation.
func (r *SummaryResult) GetTeamCitations() []TeamCitation {
	if r.TeamCitationsJSON == "" {
		return []TeamCitation{}
	}
	var citations []TeamCitation
	if err := json.Unmarshal([]byte(r.TeamCitationsJSON), &citations); err != nil {
		return []TeamCitation{}
	}
	return citations
}

// SetTeamCitations serializes a slice of TeamCitation into TeamCitationsJSON.
func (r *SummaryResult) SetTeamCitations(citations []TeamCitation) {
	if len(citations) == 0 {
		r.TeamCitationsJSON = "[]"
		return
	}
	data, err := json.Marshal(citations)
	if err != nil {
		r.TeamCitationsJSON = "[]"
		return
	}
	r.TeamCitationsJSON = string(data)
}

func (SummaryResult) TableName() string { return "summary_result" }

// SummarySchedule represents a recurring schedule configuration.
type SummarySchedule struct {
	ID             int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	SpaceID        string `gorm:"column:space_id;type:varchar(64);not null;default:''" json:"space_id"`
	CreatorID      string `gorm:"column:creator_id;type:varchar(64);not null" json:"creator_id"`
	Title          string `gorm:"column:title;type:varchar(1000);not null;default:''" json:"title"`
	SummaryMode    int    `gorm:"column:summary_mode;type:tinyint;not null" json:"summary_mode"`
	CronExpr       string `gorm:"column:cron_expr;type:varchar(50);not null" json:"cron_expr"`
	IntervalDays   int    `gorm:"column:interval_days;type:int;not null;default:0" json:"interval_days"`
	IntervalMonths int    `gorm:"column:interval_months;type:int;not null;default:0" json:"interval_months"`
	RunTime        string `gorm:"column:run_time;type:varchar(5);not null;default:''" json:"run_time"`
	// DayOfWeek aligns WEEK mode (interval_days multiple of 7) to a specific
	// weekday: 1=Mon..7=Sun, 0=unconstrained. Ignored for non-week modes.
	DayOfWeek int `gorm:"column:day_of_week;type:tinyint;not null;default:0" json:"day_of_week"`
	// DayOfMonth aligns MONTH mode (interval_months>0) to a specific day:
	// 1..31 (clamped to month end), 0=unconstrained. Ignored for non-month modes.
	DayOfMonth        int        `gorm:"column:day_of_month;type:tinyint;not null;default:0" json:"day_of_month"`
	AnchorDOM         int        `gorm:"column:anchor_dom;type:tinyint;not null;default:0" json:"-"`
	TimeRangeType     int        `gorm:"column:time_range_type;type:tinyint;not null" json:"time_range_type"`
	SourceConfig      JSON       `gorm:"column:source_config;type:json" json:"source_config"`
	ParticipantConfig JSON       `gorm:"column:participant_config;type:json" json:"participant_config"`
	IsActive          int        `gorm:"column:is_active;type:tinyint;not null;default:1" json:"is_active"`
	LastRunAt         *time.Time `gorm:"column:last_run_at" json:"last_run_at"`
	NextRunAt         *time.Time `gorm:"column:next_run_at" json:"next_run_at"`
	CreatedAt         time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt         *time.Time `gorm:"column:deleted_at;index" json:"deleted_at,omitempty"`
}

func (SummarySchedule) TableName() string { return "summary_schedule" }

// SummaryEvent is used for Worker → API status callback fallback.
type SummaryEvent struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID    int64     `gorm:"column:task_id;not null" json:"task_id"`
	Status    int       `gorm:"column:status;type:tinyint;not null" json:"status"`
	Progress  int       `gorm:"column:progress;type:tinyint;not null;default:0" json:"progress"`
	Message   string    `gorm:"column:message;type:varchar(200);not null;default:''" json:"message"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (SummaryEvent) TableName() string { return "summary_event" }

// TaskEvent is the payload for Worker → API HTTP callback.
type TaskEvent struct {
	TaskID       int64  `json:"task_id"`
	Status       int    `json:"status"`
	Progress     int    `json:"progress"`
	Message      string `json:"message"`
	EventType    string `json:"event_type,omitempty"`
	TargetUserID string `json:"target_user_id,omitempty"`
}
