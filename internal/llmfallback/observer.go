package llmfallback

import "time"

// Path identifies which logical call site drove a Run. It is a low-cardinality
// label: keep it to the fixed set below so metrics series stay bounded. Never
// derive it from request data (task ids, chunk indexes, user ids).
type Path string

const (
	PathUnknown      Path = "unknown"
	PathAgentChat    Path = "agent_chat"
	PathAgentTool    Path = "agent_tool"
	PathWorkerMap    Path = "worker_map"
	PathWorkerReduce Path = "worker_reduce"
	PathAPIRefine    Path = "api_refine"
	PathToolCall     Path = "tool_call"
)

// SwitchReason explains why Run advanced from one model to the next. The three
// reasons demand different remediations, so they must not be collapsed into a
// single "fallback happened" counter:
//
//   - ReasonDenied: the account/credential is refused for this model (403).
//     Will not self-heal; someone has to fix billing/IAM/SCP.
//   - ReasonRetriesExhausted: genuine upstream overload/outage (429/5xx) that
//     survived the per-model retry budget. Usually transient.
//   - ReasonBudgetStarved: the runner gave up the remaining same-model retries
//     because the parent deadline could not fit them plus a fallback attempt.
//     This is a CONFIGURATION signal, not an upstream one: it means the
//     deployment's step deadline is too tight for MaxAttempts*PerModelTimeout,
//     so the primary is being abandoned early on every transient blip.
type SwitchReason string

const (
	ReasonDenied           SwitchReason = "denied"
	ReasonRetriesExhausted SwitchReason = "retries_exhausted"
	ReasonBudgetStarved    SwitchReason = "budget_starved"
)

// Position labels a model by its rank in the configured list.
const (
	PositionPrimary  = "primary"
	PositionFallback = "fallback"
)

// PositionOf maps a model index to a bounded position label.
func PositionOf(i int) string {
	if i == 0 {
		return PositionPrimary
	}
	return PositionFallback
}

// AttemptEvent reports the result of one upstream request.
type AttemptEvent struct {
	Path     Path
	Model    string
	Position string
	// Attempt is 1-based within the current model's retry budget.
	Attempt int
	// MaxAttempts is that budget, so a consumer can tell "failed, will retry"
	// from "failed on the last attempt". Without it the retry log claimed a
	// retry was coming even on the final attempt, when the next step is
	// abandoning the model.
	MaxAttempts int
	Outcome     Outcome
	Duration    time.Duration
	Err         error
}

// SwitchEvent reports Run advancing from one model to the next. This is the
// event that makes silent quality drift visible.
type SwitchEvent struct {
	Path     Path
	From     string
	To       string
	FromPos  string
	Reason   SwitchReason
	Attempts int // attempts spent on From before giving up
	Err      error
}

// ResultEvent reports the terminal outcome of a whole Run.
type ResultEvent struct {
	Path Path
	// Model is the model that produced the result (empty when every model
	// failed).
	Model string
	// Position is the rank of Model; empty when every model failed.
	Position string
	// Switches counts how many times Run advanced to another model.
	Switches int
	// OK is true when a model returned a usable result.
	OK bool
	// End records whether the caller's context ended the run, and how. Without
	// it an ordinary disconnect is indistinguishable from "every configured
	// model failed": both arrive with OK=false and an empty Model, which is the
	// series a real provider outage lands on.
	End      RunEnd
	Duration time.Duration
	Err      error
}

// RunEnd says how the caller's context ended a run, when it did. Cancellation
// and deadline expiry are separate values because they page different people: a
// client closing a tab is not actionable, while exhausting our own time budget
// is a real incident that must stay alertable. A single boolean forced those
// two into one bucket, and whichever label it carried was wrong for the other.
type RunEnd string

const (
	// RunEndNone: the run finished on its own terms — success, a terminal
	// upstream error, or every model failing. The caller was still waiting.
	RunEndNone RunEnd = ""
	// RunEndCancelled: the caller went away (SSE disconnect, shutdown). Not an
	// upstream fault.
	RunEndCancelled RunEnd = "cancelled"
	// RunEndTimedOut: our own deadline expired (AGENT_STEP_TIMEOUT, the refine
	// budget). Nobody walked away — we ran out of time, usually because
	// upstream was slow. Alertable.
	RunEndTimedOut RunEnd = "timeout"
)

// Observer receives Run's lifecycle events. Implementations MUST be safe for
// concurrent use (the worker runs Map chunks in parallel) and MUST NOT block —
// they run on the request path. A nil Observer disables instrumentation.
//
// Keeping this an interface owned by llmfallback lets the metrics backend live
// in a separate package, so this leaf package stays dependency-free and a
// future swap to prometheus/client_golang is a drop-in at the composition root.
type Observer interface {
	ObserveAttempt(AttemptEvent)
	ObserveSwitch(SwitchEvent)
	ObserveResult(ResultEvent)
}

// noopObserver is used when Config.Observer is nil so Run needs no nil checks
// on the hot path.
type noopObserver struct{}

func (noopObserver) ObserveAttempt(AttemptEvent) {}
func (noopObserver) ObserveSwitch(SwitchEvent)   {}
func (noopObserver) ObserveResult(ResultEvent)   {}
