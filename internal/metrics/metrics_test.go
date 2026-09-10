package metrics

import (
	"bytes"
	"strings"
	"testing"
)

// TestLabels_EscapesValues checks the three characters the text format defines
// plus the CR-to-space flattening, and that an odd argument count panics.
func TestLabels_EscapesValues(t *testing.T) {
	got := string(Labels("model", `we"ird\x`+"\n"+"y", "path", "a\rb"))
	for _, want := range []string{`model="we\"ird\\x\ny"`, `path="a b"`} {
		if !strings.Contains(got, want) {
			t.Errorf("Labels escaping: %q missing %q", got, want)
		}
	}
	if strings.ContainsRune(got, '\r') {
		t.Errorf("bare CR survived: %q", got)
	}

	defer func() {
		if recover() == nil {
			t.Error("odd pair count did not panic")
		}
	}()
	Labels("only-key")
}

// TestCounterAndGauge_Exposition covers the counter (monotonic add) and gauge
// (last-write-wins) render paths and their HELP/TYPE headers.
func TestCounterAndGauge_Exposition(t *testing.T) {
	c := NewCounterVec("things_total", "count of things")
	c.Inc(Labels("kind", "a"))
	c.Inc(Labels("kind", "a"))
	c.Add(Labels("kind", "b"), 3)

	g := NewGaugeVec("level", "a level")
	g.Set(Labels("kind", "a"), 1)
	g.Set(Labels("kind", "a"), 9) // last write wins

	var buf bytes.Buffer
	c.WriteProm(&buf)
	g.WriteProm(&buf)
	out := buf.String()

	for _, want := range []string{
		"# TYPE things_total counter",
		`things_total{kind="a"} 2`,
		`things_total{kind="b"} 3`,
		"# TYPE level gauge",
		`level{kind="a"} 9`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestHistogram_CumulativeAndInf pins the load-bearing histogram semantics:
// inclusive le boundary, cumulative buckets, +Inf == _count, and _sum.
func TestHistogram_CumulativeAndInf(t *testing.T) {
	h := NewHistogramVec("dur_seconds", "durations", []float64{0.5, 5, 300})
	for _, v := range []float64{0.5, 4, 250, 400} { // 400 is over-range → +Inf only
		h.Observe(Labels("path", "p"), v)
	}
	var buf bytes.Buffer
	h.WriteProm(&buf)
	out := buf.String()

	for _, want := range []string{
		"# TYPE dur_seconds histogram",
		`dur_seconds_bucket{path="p",le="0.5"} 1`, // 0.5 lands here (inclusive)
		`dur_seconds_bucket{path="p",le="5"} 2`,   // +4
		`dur_seconds_bucket{path="p",le="300"} 3`, // +250; 400 excluded
		`dur_seconds_bucket{path="p",le="+Inf"} 4`,
		`dur_seconds_sum{path="p"} 654.5`,
		`dur_seconds_count{path="p"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestHistogram_NegativeClamped guards the _sum-poisoning path: a negative
// observation is clamped to 0 rather than corrupting the running total.
func TestHistogram_NegativeClamped(t *testing.T) {
	h := NewHistogramVec("d", "", []float64{1})
	h.Observe(Labels("p", "x"), -5)
	var buf bytes.Buffer
	h.WriteProm(&buf)
	if !strings.Contains(buf.String(), `d_sum{p="x"} 0`) {
		t.Errorf("negative not clamped:\n%s", buf.String())
	}
}

// TestRegistry_RendersInRegistrationOrder is the new surface this package adds:
// the Registry must render collectors in the order registered, so exposition
// output is stable across scrapes and callers can control family ordering.
func TestRegistry_RendersInRegistrationOrder(t *testing.T) {
	first := NewCounterVec("zzz_total", "z")
	first.Inc(Labels("a", "1"))
	second := NewCounterVec("aaa_total", "a")
	second.Inc(Labels("a", "1"))

	r := NewRegistry()
	r.MustRegister(first, second) // registered zzz before aaa, on purpose

	var a, b bytes.Buffer
	r.WritePrometheus(&a)
	r.WritePrometheus(&b)
	if a.String() != b.String() {
		t.Fatal("two scrapes differ; registry order is not deterministic")
	}
	if iz, ia := strings.Index(a.String(), "zzz_total"), strings.Index(a.String(), "aaa_total"); iz > ia {
		t.Errorf("registry did not preserve registration order (zzz should precede aaa):\n%s", a.String())
	}
}

// TestHistogram_EmptyLabelSetRenders covers a label-free histogram (as used by
// agent_steps / agent_prompt_chars): the le dimension must still render as a
// well-formed {le="…"} with no leading comma, and _sum/_count carry no braces
// content.
func TestHistogram_EmptyLabelSetRenders(t *testing.T) {
	h := NewHistogramVec("things", "no labels", []float64{1, 5})
	h.Observe(Labels(), 3) // Labels() → empty LabelSet
	var buf bytes.Buffer
	h.WriteProm(&buf)
	out := buf.String()

	for _, want := range []string{
		`things_bucket{le="1"} 0`,
		`things_bucket{le="5"} 1`,
		`things_bucket{le="+Inf"} 1`,
		"things_sum{} 3",
		"things_count{} 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A leading comma would mean withLE glued le onto an empty set wrong.
	if strings.Contains(out, `{,le=`) {
		t.Errorf("empty label set produced a leading comma:\n%s", out)
	}
}
