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
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

// labelSet is a bounded, ordered key/value list used as a map key. Every label
// value fed into it comes from configuration (model ids) or a closed constant
// set (path, position, outcome, reason) — never from request data — so the
// series count stays bounded by models * paths.
type labelSet string

func labels(pairs ...string) labelSet {
	if len(pairs)%2 != 0 {
		panic("llmobs: labels requires key/value pairs")
	}
	var b strings.Builder
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pairs[i])
		b.WriteString("=\"")
		b.WriteString(escapeLabelValue(pairs[i+1]))
		b.WriteString("\"")
	}
	return labelSet(b.String())
}

// labelEscaper is built once at package scope, NOT per call. strings.NewReplacer
// compiles a trie on construction, so building it inside escapeLabelValue cost
// ~3.5µs and 6.7KB of garbage per label value (measured) against ~26ns and zero
// allocations when hoisted — and escapeLabelValue runs on every label of every
// event on the LLM hot path.
//
// \r is escaped alongside the three the text format requires: it is not a
// separator so it cannot forge a line, but leaving a bare CR in a value makes
// log/terminal output overwrite itself, and SafeTextForLog already strips it
// elsewhere in this codebase.
// CR is replaced with a space, NOT escaped as \r. The Prometheus text format
// defines only \\, \" and \n inside a label value; \r is an invalid escape and
// the reference parser aborts the ENTIRE scrape on it, not just that line. So
// "hardening" a bare CR into \r traded one ugly character for the loss of every
// metric in the response. Flattening matches what SafeTextForLog already does.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", " ")

// escapeLabelValue applies the Prometheus text-format escaping rules.
func escapeLabelValue(v string) string {
	return labelEscaper.Replace(v)
}

// counterVec is a labelled monotonic counter.
type counterVec struct {
	name string
	help string
	mu   sync.Mutex
	vals map[labelSet]float64
}

func newCounterVec(name, help string) *counterVec {
	return &counterVec{name: name, help: help, vals: map[labelSet]float64{}}
}

func (c *counterVec) inc(l labelSet) { c.add(l, 1) }

func (c *counterVec) add(l labelSet, delta float64) {
	c.mu.Lock()
	c.vals[l] += delta
	c.mu.Unlock()
}

func (c *counterVec) write(w io.Writer) {
	c.mu.Lock()
	keys := make([]labelSet, 0, len(c.vals))
	for k := range c.vals {
		keys = append(keys, k)
	}
	snapshot := make(map[labelSet]float64, len(c.vals))
	for k, v := range c.vals {
		snapshot[k] = v
	}
	c.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s} %g\n", c.name, k, snapshot[k])
	}
}

// gaugeVec is a labelled last-write-wins gauge.
type gaugeVec struct {
	name string
	help string
	mu   sync.Mutex
	vals map[labelSet]float64
}

func newGaugeVec(name, help string) *gaugeVec {
	return &gaugeVec{name: name, help: help, vals: map[labelSet]float64{}}
}

func (g *gaugeVec) set(l labelSet, v float64) {
	g.mu.Lock()
	g.vals[l] = v
	g.mu.Unlock()
}

func (g *gaugeVec) write(w io.Writer) {
	g.mu.Lock()
	keys := make([]labelSet, 0, len(g.vals))
	for k := range g.vals {
		keys = append(keys, k)
	}
	snapshot := make(map[labelSet]float64, len(g.vals))
	for k, v := range g.vals {
		snapshot[k] = v
	}
	g.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s} %g\n", g.name, k, snapshot[k])
	}
}

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

// histogramVec is a labelled cumulative histogram.
//
// Buckets are stored as per-bucket hit counts and made cumulative only at
// render time: an observation touches exactly one slot, so the write path
// stays O(log n) on the boundary search and O(1) on the update, which matters
// because ObserveResult runs on every LLM call.
type histogramVec struct {
	name    string
	help    string
	buckets []float64
	mu      sync.Mutex
	counts  map[labelSet][]uint64
	sums    map[labelSet]float64
	totals  map[labelSet]uint64
}

func newHistogramVec(name, help string, buckets []float64) *histogramVec {
	// Copy: observe relies on sort.SearchFloat64s, which is only correct on a
	// strictly increasing slice. Keeping the caller's backing array would let a
	// later append/sort elsewhere silently corrupt every bucket assignment.
	b := make([]float64, len(buckets))
	copy(b, buckets)
	return &histogramVec{
		name:    name,
		help:    help,
		buckets: b,
		counts:  map[labelSet][]uint64{},
		sums:    map[labelSet]float64{},
		totals:  map[labelSet]uint64{},
	}
}

// observe records one sample. Values above the last boundary still count
// toward _sum and _count (and the +Inf bucket), so a pathological outlier is
// never silently dropped from the total.
func (h *histogramVec) observe(l labelSet, v float64) {
	// A negative duration cannot happen from a monotonic clock, but a bad
	// caller must not corrupt _sum for everyone else on this series.
	if v < 0 {
		v = 0
	}
	i := sort.SearchFloat64s(h.buckets, v)
	h.mu.Lock()
	if h.counts[l] == nil {
		h.counts[l] = make([]uint64, len(h.buckets))
	}
	if i < len(h.buckets) {
		h.counts[l][i]++
	}
	h.sums[l] += v
	h.totals[l]++
	h.mu.Unlock()
}

func (h *histogramVec) write(w io.Writer) {
	h.mu.Lock()
	keys := make([]labelSet, 0, len(h.counts))
	snapCounts := make(map[labelSet][]uint64, len(h.counts))
	for k, v := range h.counts {
		keys = append(keys, k)
		c := make([]uint64, len(v))
		copy(c, v)
		snapCounts[k] = c
	}
	snapSums := make(map[labelSet]float64, len(h.sums))
	for k, v := range h.sums {
		snapSums[k] = v
	}
	snapTotals := make(map[labelSet]uint64, len(h.totals))
	for k, v := range h.totals {
		snapTotals[k] = v
	}
	h.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	for _, k := range keys {
		var cum uint64
		for i, b := range h.buckets {
			cum += snapCounts[k][i]
			fmt.Fprintf(w, "%s_bucket{%s} %d\n", h.name, withLE(k, formatBucket(b)), cum)
		}
		total := snapTotals[k]
		fmt.Fprintf(w, "%s_bucket{%s} %d\n", h.name, withLE(k, "+Inf"), total)
		fmt.Fprintf(w, "%s_sum{%s} %g\n", h.name, k, snapSums[k])
		fmt.Fprintf(w, "%s_count{%s} %d\n", h.name, k, total)
	}
}

// withLE appends the le dimension that the histogram exposition format
// requires. It is built here rather than by the caller so no observe path can
// forget it and emit a bucket line a scraper will reject.
func withLE(l labelSet, le string) labelSet {
	if l == "" {
		return labelSet(`le="` + le + `"`)
	}
	return l + labelSet(`,le="`+le+`"`)
}

// formatBucket renders a boundary the way the text format expects: the
// shortest representation that round-trips, so 0.5 stays "0.5" and 60 stays
// "60" rather than "60.000000".
func formatBucket(b float64) string {
	return strconv.FormatFloat(b, 'g', -1, 64)
}

// Metrics is the LLM fallback metric set. The zero value is not usable; call
// NewMetrics.
type Metrics struct {
	attempts   *counterVec
	switches   *counterVec
	calls      *counterVec
	callSecs   *counterVec
	runDur     *histogramVec
	attemptDur *histogramVec
	lastOK     *gaugeVec
	nowFn      func() time.Time
}

// NewMetrics builds the metric set. nowFn is injectable for tests; nil uses
// time.Now.
func NewMetrics(nowFn func() time.Time) *Metrics {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Metrics{
		attempts: newCounterVec("llm_attempts_total",
			"Upstream LLM attempts by call path, model, list position and classified outcome."),
		switches: newCounterVec("llm_model_switch_total",
			"Cross-model fallback switches. reason=denied means the provider refused this request for this model (HTTP 403 — credentials, entitlement, a WAF rule or a regional restriction); retries_exhausted means the per-model retry budget ran out, usually upstream overload, though a deterministic contract failure (undecodable response, no choices) also lands here; budget_starved means the deadline guard abandoned the remaining retries, which is a configuration fault."),
		calls: newCounterVec("llm_calls_total",
			"Completed LLM calls by call path, the position of the model that served them, and the outcome: ok; failed (no model could serve it); cancelled (the caller went away — not an upstream fault, do not alert on it); timeout (our own deadline expired before any model answered — nobody walked away, so this one IS alertable)."),
		callSecs: newCounterVec("llm_call_duration_seconds_total",
			"DEPRECATED, prefer llm_run_duration_seconds: this counter is now exactly that histogram's _sum, fed from the same ResultEvent.Duration, and two hand-maintained copies of one quantity will diverge the first time somebody edits one observe site. Retained so existing dashboards keep working. Cumulative wall-clock seconds spent in llmfallback.Run, by call path; a mean needs sum by(path)(llm_call_duration_seconds_total) / sum by(path)(llm_calls_total)."),
		// Named llm_RUN_duration_seconds, not llm_call_duration_seconds: the
		// latter would collide with the existing llm_call_duration_seconds_total
		// counter under OpenMetrics, where a counter's family name is its name
		// minus _total. Both families would normalize to one name with
		// conflicting TYPEs for any consumer that normalizes (the OTel Collector
		// prometheus receiver, promtool, OpenMetrics-negotiating scrapers).
		// Prometheus text format 0.0.4 tolerates it; the rename is free now and
		// expensive after dashboards exist.
		runDur: newHistogramVec("llm_run_duration_seconds",
			"Distribution of whole-llmfallback.Run wall-clock, by call path — INCLUDING backoff sleeps and every model tried. Use it to size PARENT budgets (REFINE_TIMEOUT, AGENT_STEP_TIMEOUT): histogram_quantile(0.95, sum by (path, le) (rate(llm_run_duration_seconds_bucket[1h]))). To size a per-attempt cap such as LLM_TIMEOUT, use llm_attempt_duration_seconds instead.",
			durationBuckets),
		// A run and an attempt diverge exactly where it matters. LLM_TIMEOUT is
		// applied per attempt (http.Client.Timeout in service/llm.go, the
		// per-attempt context in agent/llm.go), but a run's wall-clock also
		// carries backoffs and earlier models. On the happy path the two
		// coincide, so a run-level P95 is roughly usable; a run-level P99 is
		// not, because the P99 IS the retried runs. Sizing a per-attempt cap
		// from the run distribution therefore over-estimates systematically.
		attemptDur: newHistogramVec("llm_attempt_duration_seconds",
			"Distribution of a SINGLE upstream attempt's wall-clock, by call path and classified outcome. Use it to size the per-attempt LLM_TIMEOUT; llm_run_duration_seconds includes backoffs and other models and will over-estimate it. CENSORED ON THE RIGHT: every attempt is already capped by the current LLM_TIMEOUT, so an upstream that would have taken longer is recorded at the cap and the top bucket is a pile-up, not a tail. Sound for deciding whether to LOWER the cap; it cannot tell you what raising it would recover.",
			durationBuckets),
		lastOK: newGaugeVec("llm_primary_last_success_timestamp_seconds",
			"Unix timestamp of the most recent successful call served by the PRIMARY model, per call path. A stale value means sustained silent degradation onto a fallback."),
		nowFn: nowFn,
	}
}

// ObserveAttempt implements llmfallback.Observer.
func (m *Metrics) ObserveAttempt(e llmfallback.AttemptEvent) {
	m.attempts.inc(labels(
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
	m.attemptDur.observe(labels(
		"path", string(e.Path),
		"outcome", outcomeLabel(e.Outcome),
	), e.Duration.Seconds())
}

// ObserveSwitch implements llmfallback.Observer.
func (m *Metrics) ObserveSwitch(e llmfallback.SwitchEvent) {
	m.switches.inc(labels(
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
	m.calls.inc(labels("path", string(e.Path), "position", position, "result", result))
	m.callSecs.add(labels("path", string(e.Path)), e.Duration.Seconds())
	// Labelled by path only, exactly like callSecs. Adding result/position here
	// would multiply every bucket series by those dimensions for a question the
	// counters already answer.
	m.runDur.observe(labels("path", string(e.Path)), e.Duration.Seconds())

	if e.OK && e.Position == llmfallback.PositionPrimary {
		m.lastOK.set(labels("path", string(e.Path)), float64(m.nowFn().Unix()))
	}
}

// WritePrometheus renders the metric set in Prometheus text exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	m.attempts.write(w)
	m.switches.write(w)
	m.calls.write(w)
	m.callSecs.write(w)
	m.runDur.write(w)
	m.attemptDur.write(w)
	m.lastOK.write(w)
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
