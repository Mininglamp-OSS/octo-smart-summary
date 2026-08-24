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

// Metrics is the LLM fallback metric set. The zero value is not usable; call
// NewMetrics.
type Metrics struct {
	attempts *counterVec
	switches *counterVec
	calls    *counterVec
	callSecs *counterVec
	lastOK   *gaugeVec
	nowFn    func() time.Time
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
			"Cumulative wall-clock seconds spent in llmfallback.Run, by call path. Labelled by path only, so a mean needs sum by(path)(llm_call_duration_seconds_total) / sum by(path)(llm_calls_total) — dividing the raw series returns an empty vector."),
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
