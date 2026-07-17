package model

// Snapshot represents the complete generation context for an agent-produced
// PersonalResult. It is stored as JSON in PersonalResult.SnapshotJSON and is
// only populated for trigger_type=Agent summaries (traditional workflow leaves
// it NULL).
//
// Design (see issue SUM-36 / 需求2 P0.1):
//
//   - snapshot_version: numeric, fixed at 1 for this first version
//   - task_id: numeric, references the SummaryTask primary key
//   - content_version: numeric, always 1 in this phase (future refine PR will
//     increment this when the user requests an edit)
//   - requirement: the user's original request as expressed in the agent chat
//     (can be extracted from SummaryTask.Title or the first user message)
//   - scope: object containing channel_ids (string array), channel_names (string
//     array — may be empty if SummaryTask does not store names yet), and
//     time_range (object with start/end)
//   - tool_summary: string array, a coarse log of which tools the agent invoked
//     (e.g. ["fetch_channel x 3", "search_messages x 1"]) — not a full trace
//   - data_freshness_note: fixed string explaining that tool_summary is only a
//     record of THIS generation's tool calls, not a guarantee of what data will
//     be present next time
//   - parent_snapshot_version: numeric or null, used in future refine PR to link
//     back to the previous snapshot when the user requests an edit
//   - user_instruction: string or null, used in future refine PR to record the
//     user's edit request ("make it shorter", etc.)
//
// For the initial v1 (first-generation summary via POST /summaries/agent):
//   - parent_snapshot_version = null
//   - user_instruction = null
//   - content_version = 1
type Snapshot struct {
	SnapshotVersion       int           `json:"snapshot_version"`
	TaskID                int64         `json:"task_id"`
	ContentVersion        int           `json:"content_version"`
	Requirement           string        `json:"requirement"`
	Scope                 SnapshotScope `json:"scope"`
	ToolSummary           []string      `json:"tool_summary"`
	DataFreshnessNote     string        `json:"data_freshness_note"`
	ParentSnapshotVersion *int          `json:"parent_snapshot_version"`
	UserInstruction       *string       `json:"user_instruction"`
}

// SnapshotScope represents the channel + time range that the agent summarized.
type SnapshotScope struct {
	ChannelIDs   []string      `json:"channel_ids"`
	ChannelNames []string      `json:"channel_names"`
	TimeRange    TimeRangeJSON `json:"time_range"`
}

// TimeRangeJSON represents a start/end timestamp pair.
type TimeRangeJSON struct {
	Start string `json:"start"` // RFC3339 format
	End   string `json:"end"`   // RFC3339 format
}
