package llmobs

import (
	"log/slog"
	"sync/atomic"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

// defaultMetrics holds the process-wide metric set so the /metrics handler can
// render it without threading the Observer through the router signature.
var defaultMetrics atomic.Pointer[Metrics]

// Install builds the production Observer, registers it as llmfallback's
// process-wide default and returns it. Call once from a composition root
// (cmd/summary-api, cmd/summary-worker) before serving traffic.
//
// After this runs, every llmfallback.Run in the process is instrumented — the
// worker's Map/Reduce, agent chat, agent tools and API refine alike — with no
// per-call-site wiring to forget.
func Install(logger *slog.Logger) *Observer {
	m := NewMetrics(nil)
	obs := New(m, logger)
	defaultMetrics.Store(m)
	llmfallback.SetDefaultObserver(obs)
	return obs
}

// Default returns the installed metric set, or nil when Install has not run.
func Default() *Metrics { return defaultMetrics.Load() }

// ResetDefaultForTest clears the process-wide metric set. Tests need this
// because Install publishes a singleton that otherwise leaks across cases —
// a nil-guard test that cannot restore the un-installed state silently stops
// asserting once any earlier test has installed.
func ResetDefaultForTest() {
	defaultMetrics.Store(nil)
}
