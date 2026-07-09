package model

import "time"

// SummaryUserTemplate stores a per-user topic template. Rows with IsCustom=false
// override a built-in template's pattern; rows with IsCustom=true are user-created
// templates and carry their own label/description.
type SummaryUserTemplate struct {
	ID          int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	SpaceID     string     `gorm:"column:space_id;type:varchar(64);not null;default:'';uniqueIndex:uk_summary_user_template" json:"space_id"`
	UserID      string     `gorm:"column:user_id;type:varchar(64);not null;uniqueIndex:uk_summary_user_template" json:"user_id"`
	TemplateID  string     `gorm:"column:template_id;type:varchar(64);not null;uniqueIndex:uk_summary_user_template" json:"template_id"`
	Label       string     `gorm:"column:label;type:varchar(100);not null;default:''" json:"label"`
	Description string     `gorm:"column:description;type:varchar(200);not null;default:''" json:"description"`
	IsCustom    bool       `gorm:"column:is_custom;type:tinyint;not null;default:0" json:"is_custom"`
	Pattern     string     `gorm:"column:pattern;type:text;not null" json:"pattern"`
	SortOrder   int        `gorm:"column:sort_order;type:int;not null;default:0" json:"sort_order"`
	DeletedAt   *time.Time `gorm:"column:deleted_at;index" json:"deleted_at,omitempty"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SummaryUserTemplate) TableName() string { return "summary_user_template" }
