// Package llmobs turns llmfallback's Run events into operational signal:
// structured logs and scrapeable counters.
//
// Why a hand-rolled registry instead of prometheus/client_golang: adding that
// module to this repo currently drags upgrades to golang.org/x/crypto,
// golang.org/x/net, golang.org/x/sys, golang.org/x/text and protobuf, which
// this repo's dependency-review / osv-scanner workflows must then re-clear.
// The counters below emit the standard Prometheus text exposition format, so a
// scraper cannot tell the difference. llmfallback.Observer is the seam: swapping
// in client_golang later is a change to this package only, with no edit to any
// call site.
package llmobs

import (
	"io"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/metrics"
)

// durationBuckets are the shared second-boundaries for every latency
// histogram in this package.
//
// Why a histogram at all, next to the existing callSecs counter: a cumulative
// seconds total divided by a call count yields a MEAN, and a mean cannot
// answer the question the timeout work actually asks. #220 defers the final
// LLM_TIMEOUT to per-scenario P95/P99, and long-context agent turns are
// documented at 60-100s while a refine is sub-second — an average over that
// mix describes no real request.
//
// The layout is fixed at package scope on purpose. Per-call-site buckets would
// make cross-path comparison meaningless, and bucket count is the cardinality
// cost here (series = paths x (len(buckets)+3)), so this list stays short and
// spans a sub-second refine through a 300s long-context turn in one series.
var durationBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 90, 120, 180, 300}

// Metrics is the LLM fallback metric set. The zero value is not usable; call
// NewMetrics. The registry primitives it is built from live in internal/metrics
// (extracted in #242); this struct owns only the LLM-specific names, help text
// and label choices.
type Metrics struct {
	attempts   *metrics.CounterVec
	switches   *metrics.CounterVec
	calls      *metrics.CounterVec
	callSecs   *metrics.CounterVec
	runDur     *metrics.HistogramVec
	attemptDur *metrics.HistogramVec
	lastOK     *metrics.GaugeVec
	reg        *metrics.Registry
	nowFn      func() time.Time
}

// NewMetrics builds the metric set. nowFn is injectable for tests; nil uses
// time.Now.
func NewMetrics(nowFn func() time.Time) *Metrics {
	if nowFn == nil {
		nowFn = time.Now
	}
	m := &Metrics{
		attempts: metrics.NewCounterVec("llm_attempts_total",
			"Upstream LLM attempts by call path, model, list position and classified outcome."),
		switches: metrics.NewCounterVec("llm_model_switch_total",
			"Cross-model fallback switches. reason=denied means the provider refused this request for this model (HTTP 403 — credentials, entitlement, a WAF rule or a regional restriction); retries_exhausted means the per-model retry budget ran out, usually upstream overload, though a deterministic contract failure (undecodable response, no choices) also lands here; budget_starved means the deadline guard abandoned the remaining retries, which is a configuration fault."),
		calls: metrics.NewCounterVec("llm_calls_total",
			"Completed LLM calls by call path, the position of the model that served them, and the outcome: ok; failed (no model could serve it); cancelled (the caller went away — not an upstream fault, do not alert on it); timeout (our own deadline expired before any model answered — nobody walked away, so this one IS alertable)."),
		callSecs: metrics.NewCounterVec("llm_call_duration_seconds_total",
			"DEPRECATED, prefer llm_run_duration_seconds: this counter is now exactly that histogram's _sum, fed from the same ResultEvent.Duration, and two hand-maintained copies of one quantity will diverge the first time somebody edits one observe site. Retained so existing dashboards keep working. Cumulative wall-clock seconds spent in llmfallback.Run, by call path; a mean needs sum by(path)(llm_call_duration_seconds_total) / sum by(path)(llm_calls_total)."),
		// Named llm_RUN_duration_seconds, not llm_call_duration_seconds: the
		// latter would collide with the existing llm_call_duration_seconds_total
		// counter under OpenMetrics, where a counter's family name is its name
		// minus _total. Both families would normalize to one name with
		// conflicting TYPEs for any consumer that normalizes (the OTel Collector
		// prometheus receiver, promtool, OpenMetrics-negotiating scrapers).
		// Prometheus text format 0.0.4 tolerates it; the rename is free now and
		// expensive after dashboards exist.
		runDur: metrics.NewHistogramVec("llm_run_duration_seconds",
			"Distribution of whole-llmfallback.Run wall-clock, by call path — INCLUDING backoff sleeps and every model tried. Use it to size PARENT budgets (REFINE_TIMEOUT, AGENT_STEP_TIMEOUT): histogram_quantile(0.95, sum by (path, le) (rate(llm_run_duration_seconds_bucket[1h]))). To size a per-attempt cap such as LLM_TIMEOUT, use llm_attempt_duration_seconds instead.",
			durationBuckets),
		// A run and an attempt diverge exactly where it matters. LLM_TIMEOUT is
		// applied per attempt (http.Client.Timeout in service/llm.go, the
		// per-attempt context in agent/llm.go), but a run's wall-clock also
		// carries backoffs and earlier models. On the happy path the two
		// coincide, so a run-level P95 is roughly usable; a run-level P99 is
		// not, because the P99 IS the retried runs. Sizing a per-attempt cap
		// from the run distribution therefore over-estimates systematically.
		attemptDur: metrics.NewHistogramVec("llm_attempt_duration_seconds",
			"Distribution of a SINGLE upstream attempt's wall-clock, by call path and classified outcome. Use it to size the per-attempt LLM_TIMEOUT; llm_run_duration_seconds includes backoffs and other models and will over-estimate it. CENSORED ON THE RIGHT: every attempt is already capped by the current LLM_TIMEOUT, so an upstream that would have taken longer is recorded at the cap and the top bucket is a pile-up, not a tail. Sound for deciding whether to LOWER the cap; it cannot tell you what raising it would recover.",
			durationBuckets),
		lastOK: metrics.NewGaugeVec("llm_primary_last_success_timestamp_seconds",
			"Unix timestamp of the most recent successful call served by the PRIMARY model, per call path. A stale value means sustained silent degradation onto a fallback."),
		nowFn: nowFn,
	}
	// Registration order IS exposition order (Registry renders in this order),
	// so it must match the previous hand-written WritePrometheus sequence to
	// keep scrape output byte-identical.
	m.reg = metrics.NewRegistry()
	m.reg.MustRegister(m.attempts, m.switches, m.calls, m.callSecs, m.runDur, m.attemptDur, m.lastOK)
	return m
}

// ObserveAttempt implements llmfallback.Observer.
func (m *Metrics) ObserveAttempt(e llmfallback.AttemptEvent) {
	m.attempts.Inc(metrics.Labels(
		"path", string(e.Path),
		"model", e.Model,
		"position", e.Position,
		"outcome", outcomeLabel(e.Outcome),
	))
	// Deliberately narrower than the counter's label set: no model, no position.
	// Bucket series multiply by len(buckets)+3, so carrying the model dimension
	// here would scale the series count with the configured model list. outcome
	// is kept because a timed-out attempt and a fast 403 have different
	// distributions and folding them together is what hides a degrading model.
	//
	// outcome is not free either, and the arithmetic belongs here so the next
	// person adding a label sees the real cost: paths x outcomes x (buckets+3)
	// = 8 x 4 x 15 = ~480 series for this family, against 8 x 15 = 120 for
	// runDur. That is affordable; a third dimension likely is not.
	m.attemptDur.Observe(metrics.Labels(
		"path", string(e.Path),
		"outcome", outcomeLabel(e.Outcome),
	), e.Duration.Seconds())
}

// ObserveSwitch implements llmfallback.Observer.
func (m *Metrics) ObserveSwitch(e llmfallback.SwitchEvent) {
	m.switches.Inc(metrics.Labels(
		"path", string(e.Path),
		"from", e.From,
		"to", e.To,
		"reason", string(e.Reason),
	))
}

// ObserveResult implements llmfallback.Observer.
func (m *Metrics) ObserveResult(e llmfallback.ResultEvent) {
	// "cancelled" is kept apart from "failed" on purpose. A caller walking away
	// (an SSE client closing the tab, a shutdown) arrives with OK=false and an
	// empty Model — byte-identical to a total provider outage. Folding them
	// together put user-initiated disconnects onto the series operators page on.
	result := "failed"
	switch {
	case e.OK:
		result = "ok"
	case e.End == llmfallback.RunEndCancelled:
		result = "cancelled"
	case e.End == llmfallback.RunEndTimedOut:
		result = "timeout"
	}
	position := e.Position
	if position == "" {
		position = "none"
	}
	m.calls.Inc(metrics.Labels("path", string(e.Path), "position", position, "result", result))
	m.callSecs.Add(metrics.Labels("path", string(e.Path)), e.Duration.Seconds())
	// Labelled by path only, exactly like callSecs. Adding result/position here
	// would multiply every bucket series by those dimensions for a question the
	// counters already answer.
	m.runDur.Observe(metrics.Labels("path", string(e.Path)), e.Duration.Seconds())

	if e.OK && e.Position == llmfallback.PositionPrimary {
		m.lastOK.Set(metrics.Labels("path", string(e.Path)), float64(m.nowFn().Unix()))
	}
}

// WritePrometheus renders the metric set in Prometheus text exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	m.reg.WritePrometheus(w)
}

func outcomeLabel(o llmfallback.Outcome) string {
	switch o {
	case llmfallback.Success:
		return "success"
	case llmfallback.RetrySameModel:
		return "retry_same_model"
	case llmfallback.TryNextModel:
		return "try_next_model"
	case llmfallback.Terminal:
		return "terminal"
	default:
		return "unknown"
	}
}
