// Package metrics is a tiny hand-rolled Prometheus text-exposition registry:
// labelled counters, gauges and cumulative histograms plus a Registry that
// renders them.
//
// Why hand-rolled instead of prometheus/client_golang: adding that module to
// this repo currently drags upgrades to golang.org/x/crypto, golang.org/x/net,
// golang.org/x/sys, golang.org/x/text and protobuf, which this repo's
// dependency-review / osv-scanner workflows must then re-clear. These vectors
// emit the standard Prometheus text exposition format, so a scraper cannot tell
// the difference.
//
// This package was lifted out of internal/llmobs (its first consumer) so the
// worker and agent paths can emit their own metrics without importing the
// LLM-fallback-specific package — see #242. It carries no domain knowledge: the
// caller owns metric names, help text, bucket layouts and — crucially — label
// cardinality. Only ever build a LabelSet from configuration or a closed
// constant set, never from request data, or the series count is unbounded.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// LabelSet is a bounded, ordered key/value list used as a map key and rendered
// verbatim inside the {} of an exposition line. Build it with Labels; the
// caller is responsible for keeping the value domain bounded.
type LabelSet string

// Labels builds a LabelSet from alternating key/value arguments, escaping each
// value per the text format. It panics on an odd argument count — a programming
// error, never runtime data.
func Labels(pairs ...string) LabelSet {
	if len(pairs)%2 != 0 {
		panic("metrics: Labels requires key/value pairs")
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
	return LabelSet(b.String())
}

// labelEscaper is built once at package scope, NOT per call. strings.NewReplacer
// compiles a trie on construction, so building it inside escapeLabelValue cost
// ~3.5µs and 6.7KB of garbage per label value (measured) against ~26ns and zero
// allocations when hoisted — and escapeLabelValue runs on every label of every
// event on a hot path.
//
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

// Collector is anything the Registry can render. The three vec types below
// implement it; a consumer may implement it too and register the result.
type Collector interface {
	WriteProm(w io.Writer)
}

// Registry renders a set of Collectors in registration order, so exposition
// output is deterministic across scrapes.
type Registry struct {
	mu sync.Mutex
	cs []Collector
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// Default is the process-wide registry that application subsystems (worker
// timing, agent trace) register their collectors into, rendered by the
// /internal/metrics scrape handler alongside the llmobs LLM-fallback registry.
// llmobs keeps its own registry rather than sharing this one so its exposition
// output stays independent and stable.
var Default = NewRegistry()

// MustRegister appends collectors in order. Later WritePrometheus renders them
// in exactly this order.
func (r *Registry) MustRegister(cs ...Collector) {
	r.mu.Lock()
	r.cs = append(r.cs, cs...)
	r.mu.Unlock()
}

// WritePrometheus renders every registered collector, in registration order.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	cs := make([]Collector, len(r.cs))
	copy(cs, r.cs)
	r.mu.Unlock()
	for _, c := range cs {
		c.WriteProm(w)
	}
}

// CounterVec is a labelled monotonic counter.
type CounterVec struct {
	name string
	help string
	mu   sync.Mutex
	vals map[LabelSet]float64
}

func NewCounterVec(name, help string) *CounterVec {
	return &CounterVec{name: name, help: help, vals: map[LabelSet]float64{}}
}

func (c *CounterVec) Inc(l LabelSet) { c.Add(l, 1) }

func (c *CounterVec) Add(l LabelSet, delta float64) {
	c.mu.Lock()
	c.vals[l] += delta
	c.mu.Unlock()
}

func (c *CounterVec) WriteProm(w io.Writer) {
	c.mu.Lock()
	keys := make([]LabelSet, 0, len(c.vals))
	for k := range c.vals {
		keys = append(keys, k)
	}
	snapshot := make(map[LabelSet]float64, len(c.vals))
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

// GaugeVec is a labelled last-write-wins gauge.
type GaugeVec struct {
	name string
	help string
	mu   sync.Mutex
	vals map[LabelSet]float64
}

func NewGaugeVec(name, help string) *GaugeVec {
	return &GaugeVec{name: name, help: help, vals: map[LabelSet]float64{}}
}

func (g *GaugeVec) Set(l LabelSet, v float64) {
	g.mu.Lock()
	g.vals[l] = v
	g.mu.Unlock()
}

func (g *GaugeVec) WriteProm(w io.Writer) {
	g.mu.Lock()
	keys := make([]LabelSet, 0, len(g.vals))
	for k := range g.vals {
		keys = append(keys, k)
	}
	snapshot := make(map[LabelSet]float64, len(g.vals))
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

// HistogramVec is a labelled cumulative histogram.
//
// Buckets are stored as per-bucket hit counts and made cumulative only at
// render time: an observation touches exactly one slot, so the write path
// stays O(log n) on the boundary search and O(1) on the update, which matters
// because Observe can run on every request.
type HistogramVec struct {
	name    string
	help    string
	buckets []float64
	mu      sync.Mutex
	counts  map[LabelSet][]uint64
	sums    map[LabelSet]float64
	totals  map[LabelSet]uint64
}

func NewHistogramVec(name, help string, buckets []float64) *HistogramVec {
	// Copy: Observe relies on sort.SearchFloat64s, which is only correct on a
	// strictly increasing slice. Keeping the caller's backing array would let a
	// later append/sort elsewhere silently corrupt every bucket assignment.
	b := make([]float64, len(buckets))
	copy(b, buckets)
	return &HistogramVec{
		name:    name,
		help:    help,
		buckets: b,
		counts:  map[LabelSet][]uint64{},
		sums:    map[LabelSet]float64{},
		totals:  map[LabelSet]uint64{},
	}
}

// Observe records one sample. Values above the last boundary still count
// toward _sum and _count (and the +Inf bucket), so a pathological outlier is
// never silently dropped from the total.
func (h *HistogramVec) Observe(l LabelSet, v float64) {
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

func (h *HistogramVec) WriteProm(w io.Writer) {
	h.mu.Lock()
	keys := make([]LabelSet, 0, len(h.counts))
	snapCounts := make(map[LabelSet][]uint64, len(h.counts))
	for k, v := range h.counts {
		keys = append(keys, k)
		c := make([]uint64, len(v))
		copy(c, v)
		snapCounts[k] = c
	}
	snapSums := make(map[LabelSet]float64, len(h.sums))
	for k, v := range h.sums {
		snapSums[k] = v
	}
	snapTotals := make(map[LabelSet]uint64, len(h.totals))
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
// requires. It is built here rather than by the caller so no Observe path can
// forget it and emit a bucket line a scraper will reject.
func withLE(l LabelSet, le string) LabelSet {
	if l == "" {
		return LabelSet(`le="` + le + `"`)
	}
	return l + LabelSet(`,le="`+le+`"`)
}

// formatBucket renders a boundary the way the text format expects: the
// shortest representation that round-trips, so 0.5 stays "0.5" and 60 stays
// "60" rather than "60.000000".
func formatBucket(b float64) string {
	return strconv.FormatFloat(b, 'g', -1, 64)
}
