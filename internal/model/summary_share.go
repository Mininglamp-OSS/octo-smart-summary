package model

import "time"

const (
	ShareGrantActive  = 1
	ShareGrantRevoked = 2
)

// SummaryShareSnapshot is an immutable, citation-free copy of the summary that
// was visible when the user shared it. One snapshot may be granted to multiple
// target conversations without duplicating the full body.
type SummaryShareSnapshot struct {
	ID               int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID           int64     `gorm:"column:task_id;not null;index:idx_summary_share_task" json:"task_id"`
	TaskNo           string    `gorm:"column:task_no;type:varchar(32);not null" json:"task_no"`
	SpaceID          string    `gorm:"column:space_id;type:varchar(64);not null;uniqueIndex:uk_summary_share_idempotency" json:"space_id"`
	CreatorID        string    `gorm:"column:creator_id;type:varchar(64);not null;uniqueIndex:uk_summary_share_idempotency" json:"-"`
	IdempotencyKey   string    `gorm:"column:idempotency_key;type:varchar(64);not null;uniqueIndex:uk_summary_share_idempotency" json:"-"`
	RequestHash      string    `gorm:"column:request_hash;type:char(64);not null" json:"-"`
	Title            string    `gorm:"column:title;type:varchar(2300);not null" json:"title"`
	SourceName       string    `gorm:"column:source_name;type:varchar(500);not null" json:"source_name"`
	SourceCount      int       `gorm:"column:source_count;not null" json:"source_count"`
	ParticipantCount int       `gorm:"column:participant_count;not null" json:"participant_count"`
	MessageCount     int       `gorm:"column:message_count;not null" json:"message_count"`
	TimeRangeStart   time.Time `gorm:"column:time_range_start;not null" json:"time_range_start"`
	TimeRangeEnd     time.Time `gorm:"column:time_range_end;not null" json:"time_range_end"`
	SummaryMode      int       `gorm:"column:summary_mode;not null" json:"summary_mode"`
	ResultVersion    int       `gorm:"column:result_version;not null" json:"result_version"`
	Preview          string    `gorm:"column:preview;type:text;not null" json:"preview"`
	Content          string    `gorm:"column:content;type:mediumtext;not null" json:"content"`
	CreatedAt        time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SummaryShareSnapshot) TableName() string { return "summary_share_snapshot" }

type SummaryShareGrant struct {
	ID          int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	SnapshotID  int64      `gorm:"column:snapshot_id;not null;uniqueIndex:uk_summary_share_target" json:"snapshot_id"`
	ShareID     string     `gorm:"column:share_id;type:varchar(64);not null;uniqueIndex:uk_summary_share_id" json:"share_id"`
	ChannelID   string     `gorm:"column:channel_id;type:varchar(128);not null;uniqueIndex:uk_summary_share_target" json:"channel_id"`
	ChannelType int        `gorm:"column:channel_type;not null;uniqueIndex:uk_summary_share_target" json:"channel_type"`
	Status      int        `gorm:"column:status;not null;default:1" json:"status"`
	RevokedAt   *time.Time `gorm:"column:revoked_at" json:"revoked_at,omitempty"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SummaryShareGrant) TableName() string { return "summary_share_grant" }
