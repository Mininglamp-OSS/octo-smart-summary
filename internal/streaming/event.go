package streaming

const (
	EventStart    = "start"
	EventStage    = "stage"
	EventDelta    = "delta"
	EventSnapshot = "snapshot"
	EventDone     = "done"
	EventError    = "error"
)

const (
	ScopePersonal = "personal"
	ScopeTeam     = "team"
)

// Event is the internal worker→api stream payload and the api→web SSE payload.
// Phase 1 treats api as the source of truth for Snapshot: api appends Delta into
// an in-memory snapshot; worker-sent Snapshot is accepted only as a best-effort
// reconciliation hint for degraded paths.
type Event struct {
	Type         string `json:"type"`
	TaskID       int64  `json:"task_id"`
	RunID        string `json:"run_id,omitempty"`
	Scope        string `json:"scope,omitempty"`
	TargetUserID string `json:"target_user_id,omitempty"`
	Stage        string `json:"stage,omitempty"`
	Delta        string `json:"delta,omitempty"`
	Content      string `json:"content,omitempty"`
	Message      string `json:"message,omitempty"`
	Status       int    `json:"status,omitempty"`
}

func NormalizeScope(scope string) string {
	if scope == ScopeTeam {
		return ScopeTeam
	}
	return ScopePersonal
}
