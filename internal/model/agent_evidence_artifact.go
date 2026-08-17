package model

import "time"

// AgentEvidenceArtifact is an immutable snapshot of the message pool a run
// summarizes at one revision (SS-04). Fetch results become an artifact
// revision; a later re-fetch (e.g. a targeted backfill) creates a NEW revision
// rather than mutating the old one, which is what keeps citation numbering
// stable within a revision.
//
// Idempotency: UNIQUE(run_id, content_hash) — re-freezing the same pool returns
// the existing artifact instead of writing a duplicate. ContentHash is the
// sha256 of the canonical, ordered (channel_id, message_seq) list, so two
// fetches yielding the same messages converge on one artifact.
type AgentEvidenceArtifact struct {
	ArtifactID string `gorm:"column:artifact_id;type:varchar(64);not null;primaryKey" json:"artifact_id"`
	RunID      string `gorm:"column:run_id;type:varchar(64);not null;uniqueIndex:uk_artifact_hash,priority:1;index:idx_artifact_run_rev,priority:1" json:"run_id"`
	UserID     string `gorm:"column:user_id;type:varchar(64);not null" json:"user_id"`
	SessionID  string `gorm:"column:session_id;type:varchar(128);not null;default:''" json:"session_id"`
	Revision   int    `gorm:"column:revision;not null;index:idx_artifact_run_rev,priority:2" json:"revision"`

	ContentHash string `gorm:"column:content_hash;type:char(64);not null;uniqueIndex:uk_artifact_hash,priority:2" json:"content_hash"`

	MessageCount int `gorm:"column:message_count;not null;default:0" json:"message_count"`
	ChannelCount int `gorm:"column:channel_count;not null;default:0" json:"channel_count"`

	// ActualTimeRange / FailedChannels are JSON (evolving shape → checklist A1).
	ActualTimeRange string `gorm:"column:actual_time_range;type:json;not null" json:"actual_time_range"`
	FailedChannels  string `gorm:"column:failed_channels;type:json;not null" json:"failed_channels"`
	Truncated       bool   `gorm:"column:truncated;not null;default:false" json:"truncated"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (AgentEvidenceArtifact) TableName() string { return "agent_evidence_artifact" }

// AgentCitationManifest freezes the citation ordinal mapping for one artifact
// revision (SS-04). Entries is the JSON array of {channel_id, message_seq} in
// frozen citation order; the 1-based ordinal of a message is its index+1.
//
// This is the fix for the citation-drift defect: instead of both the mid-run
// pool and the save-time pool independently re-sorting the (possibly changed)
// evidence set and re-deriving CitationIndex, they read the frozen ordinals
// here. The number a [n] marker points to can no longer shift.
//
// UNIQUE(artifact_id): exactly one manifest per artifact revision.
type AgentCitationManifest struct {
	ManifestID string `gorm:"column:manifest_id;type:varchar(64);not null;primaryKey" json:"manifest_id"`
	ArtifactID string `gorm:"column:artifact_id;type:varchar(64);not null;uniqueIndex:uk_manifest_artifact" json:"artifact_id"`
	RunID      string `gorm:"column:run_id;type:varchar(64);not null;index:idx_manifest_run_rev,priority:1" json:"run_id"`
	UserID     string `gorm:"column:user_id;type:varchar(64);not null" json:"user_id"`
	Revision   int    `gorm:"column:revision;not null;index:idx_manifest_run_rev,priority:2" json:"revision"`

	// Entries is a JSON array of stable ids in frozen citation order.
	Entries    string `gorm:"column:entries;type:mediumtext;not null" json:"entries"`
	EntryCount int    `gorm:"column:entry_count;not null;default:0" json:"entry_count"`

	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (AgentCitationManifest) TableName() string { return "agent_citation_manifest" }
