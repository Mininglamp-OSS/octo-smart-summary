package model

import "time"

// Run status (task-flow state). Distinct from FinishStatus, which is the
// generation-quality verdict produced by the SS-07 Finish Gate.
const (
	RunStatusCreated  = "created"
	RunStatusRunning  = "running"
	RunStatusFinished = "finished"
	RunStatusFailed   = "failed"
)

// Finish status (generation-quality verdict). Written by SS-07; the column
// exists from SS-03 so no ALTER is needed later (checklist A4).
const (
	FinishStatusNone     = ""
	FinishStatusComplete = "COMPLETE"
	FinishStatusPartial  = "PARTIAL"
	FinishStatusFailed   = "FAILED"
)

// Scope policy governs whether the agent may expand beyond the resolved scope.
const (
	ScopePolicyClosed = "closed"
	ScopePolicyOpen   = "open"
)

// AgentSummaryRun is the persistent, resumable state for one agent-summary run
// (SS-03). It replaces the previous "request-scoped, thrown away when the
// context dies" model: identity + progress now live in the DB so runs can be
// deduplicated, serialized, and resumed.
//
// Idempotency: UNIQUE(user_id, session_id, request_id) makes CreateOrGetRun a
// DB-level guard — a retried request (e.g. SSE stream failure downgrading to the
// non-streaming endpoint) reuses the same run instead of re-fetching and
// re-summarizing. Concurrency: Version is an optimistic lock; run updates go
// through a compare-and-swap so two concurrent requests cannot fork run state.
type AgentSummaryRun struct {
	RunID     string `gorm:"column:run_id;type:varchar(64);not null;primaryKey" json:"run_id"`
	UserID    string `gorm:"column:user_id;type:varchar(64);not null;uniqueIndex:uk_run_request,priority:1;index:idx_run_user_session,priority:1" json:"user_id"`
	SessionID string `gorm:"column:session_id;type:varchar(128);not null;uniqueIndex:uk_run_request,priority:2;index:idx_run_user_session,priority:2" json:"session_id"`
	RequestID string `gorm:"column:request_id;type:varchar(128);not null;uniqueIndex:uk_run_request,priority:3" json:"request_id"`

	SpecID      string `gorm:"column:spec_id;type:varchar(64);not null;default:''" json:"spec_id"`
	SpecVersion int    `gorm:"column:spec_version;not null;default:0" json:"spec_version"`

	ScopePolicy  string `gorm:"column:scope_policy;type:varchar(16);not null;default:'closed'" json:"scope_policy"`
	Status       string `gorm:"column:status;type:varchar(32);not null;default:'created'" json:"status"`
	FinishStatus string `gorm:"column:finish_status;type:varchar(16);not null;default:''" json:"finish_status"`

	// Version is the optimistic-lock counter for serialized run updates.
	Version int `gorm:"column:version;not null;default:0" json:"version"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (AgentSummaryRun) TableName() string { return "agent_summary_run" }

// AgentSummarySpec is the immutable-per-version summary specification (SS-03).
// A new user goal / closed-scope change creates a new version rather than
// mutating an existing row — UNIQUE(run_id, version) enforces that.
//
// Shape discipline (checklist A1): only the frequently-queried scalars are
// promoted to typed columns. Everything with an evolving shape
// (output_sections, exclusions, channels, time_range, citation_policy) lives in
// SpecJSON so adding a sub-field never requires an ALTER. FieldSources records
// per-field provenance (user / ui / default / inferred) so the Finish Gate can
// tell an explicit request from a model guess (checklist A2). SpecHash is the
// sha256 of the canonical spec, used to decide whether a Spec changed / can be
// reused (checklist A2).
type AgentSummarySpec struct {
	SpecID  string `gorm:"column:spec_id;type:varchar(64);not null;primaryKey" json:"spec_id"`
	RunID   string `gorm:"column:run_id;type:varchar(64);not null;uniqueIndex:uk_spec_run_version,priority:1" json:"run_id"`
	Version int    `gorm:"column:version;not null;uniqueIndex:uk_spec_run_version,priority:2" json:"version"`

	SpecHash string `gorm:"column:spec_hash;type:char(64);not null" json:"spec_hash"`

	Objective   string `gorm:"column:objective;type:varchar(1000);not null;default:''" json:"objective"`
	Topic       string `gorm:"column:topic;type:varchar(500);not null;default:''" json:"topic"`
	Audience    string `gorm:"column:audience;type:varchar(200);not null;default:''" json:"audience"`
	Language    string `gorm:"column:language;type:varchar(32);not null;default:''" json:"language"`
	DetailLevel string `gorm:"column:detail_level;type:varchar(32);not null;default:''" json:"detail_level"`

	// SpecJSON is the full structured spec (JSON). FieldSources is the
	// per-field provenance map (JSON). UserRequest is the original NL request.
	SpecJSON     string `gorm:"column:spec_json;type:json;not null" json:"spec_json"`
	FieldSources string `gorm:"column:field_sources;type:json;not null" json:"field_sources"`
	UserRequest  string `gorm:"column:user_request;type:text;not null" json:"user_request"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (AgentSummarySpec) TableName() string { return "agent_summary_spec" }
