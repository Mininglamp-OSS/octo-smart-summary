package model

import "encoding/json"

// Snapshot represents version-specific generation provenance. Agent optimization
// may attach it to an originally traditional task. Legacy versions without
// recoverable provenance use ProvenanceStatus=unknown, never a newer scope.
//
// Design (see issue SUM-36 / 需求2 P0.1):
//
//   - snapshot_version: numeric, fixed at 1 for this first version
//   - task_id: numeric, references the SummaryTask primary key
//   - content_version: version of the result described by this snapshot
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
	ProvenanceStatus      string        `json:"provenance_status,omitempty"`
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
type SnapshotSource struct {
	SourceID   string `json:"source_id"`
	SourceType int    `json:"source_type"`
	SourceName string `json:"source_name"`
}

type SnapshotScope struct {
	Sources      []SnapshotSource `json:"sources,omitempty"`
	ChannelIDs   []string         `json:"channel_ids"`
	ChannelNames []string         `json:"channel_names"`
	DocumentIDs  []string         `json:"document_ids,omitempty"`
	TimeRange    TimeRangeJSON    `json:"time_range"`
}

// TimeRangeJSON represents a start/end timestamp pair.
type TimeRangeJSON struct {
	Start string `json:"start"` // RFC3339 format
	End   string `json:"end"`   // RFC3339 format
}

// UnknownGenerationSnapshot explicitly represents missing historical provenance.
// Never borrow a different version's source scope or tool trace.
func UnknownGenerationSnapshot(taskID int64, version int) *string {
	snapshot := Snapshot{SnapshotVersion: 1, TaskID: taskID, ContentVersion: version, ProvenanceStatus: "unknown", Scope: SnapshotScope{ChannelIDs: []string{}, ChannelNames: []string{}}, ToolSummary: []string{}, DataFreshnessNote: "此版本的生成来源未记录，不能据此判断来源范围或数据新鲜度。"}
	raw, _ := json.Marshal(snapshot)
	result := string(raw)
	return &result
}
