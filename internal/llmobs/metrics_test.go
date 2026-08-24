package llmobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

func fixedNow() time.Time { return time.Unix(1700000000, 0) }

// advancingClock moves forward one second per read, so a gauge that is refreshed
// when it should not be produces a DIFFERENT value. A fixed clock cannot
// distinguish "refreshed wrongly" from "not refreshed" and makes any
// last-success assertion vacuous.
func advancingClock() func() time.Time {
	var n int64
	return func() time.Time {
		n++
		return time.Unix(1700000000+n, 0)
	}
}

// parseExposition splits Prometheus text output into metric families and sample
// lines so the assertions below read against structure rather than substrings.
type family struct {
	help    string
	typ     string
	samples []string
}

func parseExposition(t *testing.T, out string) map[string]*family {
	t.Helper()
	fams := map[string]*family{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "# HELP "):
			rest := strings.TrimPrefix(line, "# HELP ")
			name, help, ok := strings.Cut(rest, " ")
			if !ok {
				t.Errorf("HELP line has no help text: %q", line)
			}
			if fams[name] == nil {
				fams[name] = &family{}
			}
			if fams[name].help != "" {
				t.Errorf("duplicate HELP for %q", name)
			}
			fams[name].help = help
		case strings.HasPrefix(line, "# TYPE "):
			rest := strings.TrimPrefix(line, "# TYPE ")
			name, typ, ok := strings.Cut(rest, " ")
			if !ok {
				t.Errorf("TYPE line has no type: %q", line)
			}
			if fams[name] == nil {
				fams[name] = &family{}
			}
			fams[name].typ = typ
		case strings.HasPrefix(line, "#"):
			t.Errorf("unrecognised comment line: %q", line)
		default:
			name, _, ok := strings.Cut(line, "{")
			if !ok {
				t.Errorf("sample line is not name{labels} value: %q", line)
				continue
			}
			if fams[name] == nil {
				t.Errorf("sample for %q has no HELP/TYPE header", name)
				continue
			}
			fams[name].samples = append(fams[name].samples, line)
		}
	}
	return fams
}

func feedOneSwitchedCall(m *Metrics) {
	m.ObserveAttempt(llmfallback.AttemptEvent{
		Path: llmfallback.PathWorkerMap, Model: "primary-a", Position: llmfallback.PositionPrimary,
		Attempt: 1, Outcome: llmfallback.RetrySameModel,
	})
	m.ObserveSwitch(llmfallback.SwitchEvent{
		Path: llmfallback.PathWorkerMap, From: "primary-a", To: "fallback-b",
		FromPos: llmfallback.PositionPrimary, Reason: llmfallback.ReasonRetriesExhausted, Attempts: 3,
	})
	m.ObserveAttempt(llmfallback.AttemptEvent{
		Path: llmfallback.PathWorkerMap, Model: "fallback-b", Position: llmfallback.PositionFallback,
		Attempt: 1, Outcome: llmfallback.Success,
	})
	m.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathWorkerMap, Model: "fallback-b", Position: llmfallback.PositionFallback,
		Switches: 1, OK: true, Duration: 250 * time.Millisecond,
	})
}

// TestWritePrometheus_ExpositionFormat checks the output a scraper actually
// parses: every family carries exactly one HELP and one TYPE header before its
// samples, and every sample is well formed.
func TestWritePrometheus_ExpositionFormat(t *testing.T) {
	m := NewMetrics(fixedNow)
	feedOneSwitchedCall(m)

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	fams := parseExposition(t, buf.String())

	// Same vacuity guard as above: assert the set actually carries samples
	// before checking their shape.
	if got := len(fams["llm_attempts_total"].samples); got == 0 {
		t.Fatal("no attempt series emitted; the exposition assertions below would be vacuous")
	}

	want := map[string]string{
		"llm_attempts_total":                         "counter",
		"llm_model_switch_total":                     "counter",
		"llm_calls_total":                            "counter",
		"llm_call_duration_seconds_total":            "counter",
		"llm_primary_last_success_timestamp_seconds": "gauge",
	}
	for name, typ := range want {
		f := fams[name]
		if f == nil {
			t.Errorf("missing metric family %q", name)
			continue
		}
		if f.help == "" {
			t.Errorf("%s: missing HELP text", name)
		}
		if f.typ != typ {
			t.Errorf("%s: TYPE = %q, want %q", name, f.typ, typ)
		}
	}

	// The value on every sample line must parse as a float.
	for name, f := range fams {
		for _, s := range f.samples {
			_, val, _ := strings.Cut(s, "} ")
			if _, err := strconv.ParseFloat(val, 64); err != nil {
				t.Errorf("%s: sample value %q is not a float: %v", name, val, err)
			}
		}
	}
}

// TestWritePrometheus_SeriesAreSorted keeps scrape output stable across writes,
// so a diff of two scrapes shows real changes rather than map iteration order.
func TestWritePrometheus_SeriesAreSorted(t *testing.T) {
	m := NewMetrics(fixedNow)
	for _, model := range []string{"zeta", "alpha", "mu"} {
		m.ObserveAttempt(llmfallback.AttemptEvent{
			Path: llmfallback.PathAgentChat, Model: model,
			Position: llmfallback.PositionPrimary, Attempt: 1, Outcome: llmfallback.Success,
		})
	}
	var a, b bytes.Buffer
	m.WritePrometheus(&a)
	m.WritePrometheus(&b)
	if a.String() != b.String() {
		t.Fatal("two consecutive scrapes differ; series are not deterministically ordered")
	}

	fams := parseExposition(t, a.String())
	got := fams["llm_attempts_total"].samples
	if !sortedStrings(got) {
		t.Errorf("samples are not sorted:\n%s", strings.Join(got, "\n"))
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

// TestWritePrometheus_EscapesLabelValues guards against a model id containing a
// quote, backslash or newline breaking the exposition format — an unescaped
// value would corrupt every following line of the scrape, not just its own.
func TestWritePrometheus_EscapesLabelValues(t *testing.T) {
	m := NewMetrics(fixedNow)
	m.ObserveAttempt(llmfallback.AttemptEvent{
		Path:     llmfallback.PathAgentChat,
		Model:    `we"ird\model` + "\nllm_injected_total{a=\"b\"} 99",
		Position: llmfallback.PositionPrimary, Attempt: 1, Outcome: llmfallback.Success,
	})

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()

	if strings.Contains(out, "llm_injected_total") && !strings.Contains(out, `\nllm_injected_total`) {
		t.Error("a newline in a model id injected a forged metric line")
	}
	for _, frag := range []string{`we\"ird`, `\\model`, `\n`} {
		if !strings.Contains(out, frag) {
			t.Errorf("expected escaped fragment %q in output:\n%s", frag, out)
		}
	}
	// The forged line must not survive as its own parsed sample.
	fams := parseExposition(t, out)
	if fams["llm_injected_total"] != nil {
		t.Error("forged metric family parsed out of an escaped label value")
	}
}

// TestMetrics_LabelCardinalityIsBounded is the guard the brief calls for: series
// count must be a function of (models x paths x closed constant sets) only.
// Feeding a thousand distinct task-shaped strings through the legitimate fields
// must not create a thousand series.
func TestMetrics_LabelCardinalityIsBounded(t *testing.T) {
	m := NewMetrics(fixedNow)
	paths := []llmfallback.Path{llmfallback.PathWorkerMap, llmfallback.PathAgentChat}
	models := []string{"primary-a", "fallback-b"}

	for i := 0; i < 1000; i++ {
		for _, p := range paths {
			for j, model := range models {
				m.ObserveAttempt(llmfallback.AttemptEvent{
					Path: p, Model: model, Position: llmfallback.PositionOf(j),
					// Per-call varying data must never reach a label.
					Attempt:  i % 3,
					Duration: time.Duration(i) * time.Millisecond,
					Err:      fmt.Errorf("task-%d chunk-%d user-%d failed", i, j, i),
					Outcome:  llmfallback.RetrySameModel,
				})
			}
		}
	}

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()
	fams := parseExposition(t, out)

	// 2 paths x 2 (model,position) pairs x 1 outcome = 4 series.
	if got := len(fams["llm_attempts_total"].samples); got != 4 {
		t.Errorf("llm_attempts_total has %d series, want 4 — cardinality is not bounded", got)
	}
	// Inspect the label sets only: a metric NAME may legitimately contain a word
	// like "duration", but no LABEL may carry per-call data.
	allowedKeys := map[string]bool{
		"path": true, "model": true, "position": true,
		"outcome": true, "reason": true, "from": true, "to": true, "result": true,
	}
	for name, f := range fams {
		for _, sample := range f.samples {
			_, rest, _ := strings.Cut(sample, "{")
			labelPart, _, _ := strings.Cut(rest, "} ")
			for _, kv := range strings.Split(labelPart, `","`) {
				key, value, _ := strings.Cut(strings.Trim(kv, `"`), `="`)
				if !allowedKeys[key] {
					t.Errorf("%s: unexpected label key %q — only closed-set keys may be labels", name, key)
				}
				for _, leak := range []string{"task-", "chunk-", "user-"} {
					if strings.Contains(value, leak) {
						t.Errorf("%s: per-call data %q leaked into label %s=%q", name, leak, key, value)
					}
				}
			}
		}
	}
}

// TestMetrics_PrimaryLastSuccessTracksSilentDrift covers the signal that answers
// "how long have we been degraded?": the gauge advances only on a success served
// by the PRIMARY, so a stale value means sustained fallback dependence even
// while calls keep succeeding.
func TestMetrics_PrimaryLastSuccessTracksSilentDrift(t *testing.T) {
	m := NewMetrics(advancingClock())

	m.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathAgentChat, Model: "p", Position: llmfallback.PositionPrimary, OK: true,
	})
	first := gaugeValue(t, m, "llm_primary_last_success_timestamp_seconds")

	// A fallback success must NOT refresh it — that is the whole point.
	m.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathAgentChat, Model: "f", Position: llmfallback.PositionFallback, OK: true, Switches: 1,
	})
	if got := gaugeValue(t, m, "llm_primary_last_success_timestamp_seconds"); got != first {
		t.Errorf("a fallback success advanced the primary-health gauge (%v -> %v)", first, got)
	}

	// A failed primary must not refresh it either.
	m.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathAgentChat, Model: "p", Position: llmfallback.PositionPrimary, OK: false,
	})
	if got := gaugeValue(t, m, "llm_primary_last_success_timestamp_seconds"); got != first {
		t.Errorf("a failed primary advanced the gauge (%v -> %v)", first, got)
	}
}

func gaugeValue(t *testing.T, m *Metrics, name string) float64 {
	t.Helper()
	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, name+"{") {
			_, val, _ := strings.Cut(line, "} ")
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				t.Fatalf("bad gauge value %q: %v", val, err)
			}
			return f
		}
	}
	t.Fatalf("gauge %q not found in:\n%s", name, buf.String())
	return 0
}

// TestMetrics_ExhaustedCallIsCounted covers total failure: no model served the
// call, so it must land on position="none" rather than silently vanishing.
func TestMetrics_ExhaustedCallIsCounted(t *testing.T) {
	m := NewMetrics(fixedNow)
	m.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathWorkerReduce, Switches: 2, OK: false, Duration: time.Second,
	})
	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()
	if !strings.Contains(out, `position="none"`) || !strings.Contains(out, `result="failed"`) {
		t.Errorf("exhausted call not counted as none/failed:\n%s", out)
	}
}

// TestMetrics_ConcurrentUse matters because the worker runs Map chunks in
// parallel against one process-wide Metrics. Run with -race.
func TestMetrics_ConcurrentUse(t *testing.T) {
	m := NewMetrics(fixedNow)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				feedOneSwitchedCall(m)
				if j%10 == 0 {
					m.WritePrometheus(&bytes.Buffer{}) // concurrent scrape
				}
			}
		}(i)
	}
	wg.Wait()

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	fams := parseExposition(t, buf.String())
	// 32 goroutines x 50 iterations, one switch each.
	// Without this the assertion below is vacuous: WritePrometheus emits the
	// HELP/TYPE header even with no data, so a fully disabled switch counter
	// still yields a present family with zero samples and the loop body never
	// runs. Verified by mutation.
	if got := len(fams["llm_model_switch_total"].samples); got == 0 {
		t.Fatalf("no switch series emitted; the counter is not recording")
	}
	for _, s := range fams["llm_model_switch_total"].samples {
		_, val, _ := strings.Cut(s, "} ")
		if val != "1600" {
			t.Errorf("lost increments under concurrency: switch counter = %s, want 1600", val)
		}
	}
}

// TestInstall_EndToEndFromRealRun closes the gap between the two halves tested
// above: llmfallback -> Observer is covered in the llmfallback package, and
// Observer -> exposition is covered here, but neither proves the COMPOSED path
// works. Without this, SetDefaultObserver could silently fail to install and
// every test would still pass while production reported nothing.
func TestInstall_EndToEndFromRealRun(t *testing.T) {
	obs := Install(slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := scrape(t, obs.Metrics())

	// A real Run: primary exhausts its budget, fallback succeeds.
	_, model, err := llmfallback.Run(
		llmfallback.WithPath(context.Background(), llmfallback.PathWorkerReduce),
		llmfallback.Config{
			Models: []string{"primary-e2e", "fallback-e2e"}, MaxAttempts: 2,
			Backoff: func(int) time.Duration { return 0 },
		},
		func(_ context.Context, m string) (string, llmfallback.Outcome, error) {
			if m == "primary-e2e" {
				return "", llmfallback.ClassifyStatus(http.StatusTooManyRequests), errors.New("429")
			}
			return "ok", llmfallback.Success, nil
		})
	if err != nil || model != "fallback-e2e" {
		t.Fatalf("Run did not fall back as set up: model=%q err=%v", model, err)
	}

	after := scrape(t, obs.Metrics())
	if after == before {
		t.Fatal("a real Run produced no metric change; the default observer is not wired")
	}
	for _, want := range []string{
		`llm_model_switch_total{path="worker_reduce",from="primary-e2e",to="fallback-e2e",reason="retries_exhausted"}`,
		`llm_calls_total{path="worker_reduce",position="fallback",result="ok"}`,
		`llm_attempts_total{path="worker_reduce",model="primary-e2e",position="primary",outcome="retry_same_model"}`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("expected series missing after a real Run:\n  %s\ngot:\n%s", want, after)
		}
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	return buf.String()
}

// sampleValue pulls the numeric value of one exact series out of a scrape.
// Assertions built on strings.Contains of the label set alone cannot see a
// counter that increments by 2, accumulates into the wrong metric, or records
// milliseconds into a _seconds_total — every one of those keeps the label set
// byte-identical. Mutation testing showed that class of defect surviving, so
// the E2E assertions below read values, not substrings.
func sampleValue(t *testing.T, scrape, series string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(scrape, "\n") {
		labels, val, ok := strings.Cut(line, "} ")
		if !ok || labels+"}" != series {
			continue
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			t.Fatalf("series %s has non-numeric value %q", series, val)
		}
		return f, true
	}
	return 0, false
}

func mustSample(t *testing.T, scrape, series string) float64 {
	t.Helper()
	v, ok := sampleValue(t, scrape, series)
	if !ok {
		t.Fatalf("series not present in scrape:\n  %s\n--- scrape ---\n%s", series, scrape)
	}
	return v
}

// TestInstall_EndToEndValuesAreExact drives a real Run and asserts the exact
// counter values it must produce, not merely that some series bearing the right
// labels exists.
func TestInstall_EndToEndValuesAreExact(t *testing.T) {
	obs := Install(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { llmfallback.SetDefaultObserver(nil) })

	const primary, fallback = "p-exact", "f-exact"
	var primaryTries int
	_, model, err := llmfallback.Run(
		llmfallback.WithPath(context.Background(), llmfallback.PathWorkerMap),
		llmfallback.Config{
			Models: []string{primary, fallback}, MaxAttempts: 2,
			Backoff: func(int) time.Duration { return 0 },
		},
		func(_ context.Context, m string) (string, llmfallback.Outcome, error) {
			if m == primary {
				primaryTries++
				return "", llmfallback.ClassifyStatus(http.StatusTooManyRequests), errors.New("429")
			}
			return "ok", llmfallback.Success, nil
		})
	if err != nil || model != fallback {
		t.Fatalf("setup: model=%q err=%v", model, err)
	}

	var buf bytes.Buffer
	obs.Metrics().WritePrometheus(&buf)
	out := buf.String()

	// The primary burned its full 2-attempt budget; the fallback answered first try.
	if got := mustSample(t, out, `llm_attempts_total{path="worker_map",model="p-exact",position="primary",outcome="retry_same_model"}`); got != 2 {
		t.Errorf("primary retry attempts = %v, want 2 (attempt fn ran %d times)", got, primaryTries)
	}
	if got := mustSample(t, out, `llm_attempts_total{path="worker_map",model="f-exact",position="fallback",outcome="success"}`); got != 1 {
		t.Errorf("fallback success attempts = %v, want 1", got)
	}
	if got := mustSample(t, out, `llm_model_switch_total{path="worker_map",from="p-exact",to="f-exact",reason="retries_exhausted"}`); got != 1 {
		t.Errorf("switches = %v, want 1", got)
	}
	if got := mustSample(t, out, `llm_calls_total{path="worker_map",position="fallback",result="ok"}`); got != 1 {
		t.Errorf("calls = %v, want exactly 1 per Run", got)
	}

	// A unit slip (ms or ns accumulated into a _seconds_total) leaves the label
	// set untouched but moves the value by 3-9 orders of magnitude. This Run is
	// sub-second, so anything at or above 1 is a unit bug rather than slowness.
	secs := mustSample(t, out, `llm_call_duration_seconds_total{path="worker_map"}`)
	if secs <= 0 || secs >= 1 {
		t.Errorf("duration = %v seconds; expected a sub-second value — a value >= 1 means ms or ns were accumulated as seconds", secs)
	}
}

// TestInstall_ExhaustionAndCancellationAreDistinct pins the two failure shapes
// apart at the series level, driven through a real Run. Before this, the only
// exhaustion coverage hand-fed a ResultEvent, so it could not prove Run emits
// one at all — and a caller who merely walked away was counted as a total
// provider outage.
func TestInstall_ExhaustionAndCancellationAreDistinct(t *testing.T) {
	obs := Install(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { llmfallback.SetDefaultObserver(nil) })

	// 1. Everything down: no model can serve the call.
	llmfallback.Run(
		llmfallback.WithPath(context.Background(), llmfallback.PathWorkerReduce),
		llmfallback.Config{
			Models: []string{"a-down", "b-down"}, MaxAttempts: 1,
			Backoff: func(int) time.Duration { return 0 },
		},
		func(_ context.Context, _ string) (string, llmfallback.Outcome, error) {
			return "", llmfallback.ClassifyStatus(http.StatusServiceUnavailable), errors.New("503")
		})

	// 2. The caller goes away mid-request — an SSE client closing the tab.
	// Cancelled from INSIDE the attempt, because that is the exit production
	// takes: both clients check the parent ctx and return Terminal. Cancelling
	// before Run only ever exercised the top-of-loop guard.
	cancelled, cancel := context.WithCancel(context.Background())
	llmfallback.Run(
		llmfallback.WithPath(cancelled, llmfallback.PathAPIRefine),
		llmfallback.Config{
			Models: []string{"a-live", "b-live"}, MaxAttempts: 1,
			Backoff: func(int) time.Duration { return 0 },
		},
		func(actx context.Context, _ string) (string, llmfallback.Outcome, error) {
			cancel()
			return "", llmfallback.Terminal, actx.Err()
		})

	// 3. Our own budget expires while upstream is slow. This must NOT share a
	// series with either the outage or the disconnect: nobody walked away, and
	// no model was proven bad — it is its own alertable condition.
	timedOut, cancelTimeout := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelTimeout()
	llmfallback.Run(
		llmfallback.WithPath(timedOut, llmfallback.PathAgentChat),
		llmfallback.Config{
			Models: []string{"slow-a", "slow-b"}, MaxAttempts: 1,
			Backoff: func(int) time.Duration { return 0 },
		},
		func(context.Context, string) (string, llmfallback.Outcome, error) {
			time.Sleep(40 * time.Millisecond)
			return "", llmfallback.ClassifyStatus(http.StatusServiceUnavailable), errors.New("503 slow")
		})

	var buf bytes.Buffer
	obs.Metrics().WritePrometheus(&buf)
	out := buf.String()

	if got := mustSample(t, out, `llm_calls_total{path="worker_reduce",position="none",result="failed"}`); got != 1 {
		t.Errorf("exhausted call = %v, want 1 — Run must emit a result event when every model fails", got)
	}
	// The cancelled run reached the Terminal exit, so it carries the position of
	// the model that was in flight — what matters is the result label.
	if got := mustSample(t, out, `llm_calls_total{path="api_refine",position="primary",result="cancelled"}`); got != 1 {
		t.Errorf("cancelled call = %v, want 1", got)
	}
	if v, ok := sampleValue(t, out, `llm_calls_total{path="api_refine",position="primary",result="failed"}`); ok {
		t.Errorf("caller cancellation was counted as an upstream failure (%v); it must not land on the series operators alert on", v)
	}
	if got := mustSample(t, out, `llm_calls_total{path="agent_chat",position="none",result="timeout"}`); got != 1 {
		t.Errorf("timed-out call = %v, want 1", got)
	}
	if v, ok := sampleValue(t, out, `llm_calls_total{path="agent_chat",position="none",result="cancelled"}`); ok {
		t.Errorf("our own deadline expiring was labelled cancelled (%v) — that label documents itself as 'do not alert on it', which would bury a real incident", v)
	}
}

// TestInstall_TerminalFailureIsNotCountedOK covers the shape a 400 produces:
// a model DID serve the call, and it failed. Recording that as ok would hide
// contract errors entirely.
func TestInstall_TerminalFailureIsNotCountedOK(t *testing.T) {
	obs := Install(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { llmfallback.SetDefaultObserver(nil) })

	llmfallback.Run(
		llmfallback.WithPath(context.Background(), llmfallback.PathAgentChat),
		llmfallback.Config{Models: []string{"only-m"}, MaxAttempts: 2, Backoff: func(int) time.Duration { return 0 }},
		func(_ context.Context, _ string) (string, llmfallback.Outcome, error) {
			return "", llmfallback.ClassifyStatus(http.StatusBadRequest), errors.New("400 malformed")
		})

	var buf bytes.Buffer
	obs.Metrics().WritePrometheus(&buf)
	out := buf.String()

	if got := mustSample(t, out, `llm_calls_total{path="agent_chat",position="primary",result="failed"}`); got != 1 {
		t.Errorf("terminal failure = %v, want 1 on result=failed", got)
	}
	if v, ok := sampleValue(t, out, `llm_calls_total{path="agent_chat",position="primary",result="ok"}`); ok {
		t.Errorf("a terminal failure was counted as ok (%v)", v)
	}
}

// TestWritePrometheus_CarriageReturnIsFlattenedNotEscaped pins the one escape
// that must NOT be produced. The Prometheus text format defines only \\, \" and
// \n inside a label value; \r is an invalid escape sequence and the reference
// parser aborts the ENTIRE scrape on it — so "hardening" a bare CR into \r
// trades one ugly character for the loss of every metric in the response.
// Nothing covered this, which is how it shipped.
func TestWritePrometheus_CarriageReturnIsFlattenedNotEscaped(t *testing.T) {
	m := NewMetrics(fixedNow)
	m.ObserveAttempt(llmfallback.AttemptEvent{
		Path:     llmfallback.PathAgentChat,
		Model:    "we\rird-model",
		Position: llmfallback.PositionPrimary, Attempt: 1, MaxAttempts: 3,
		Outcome: llmfallback.Success,
	})

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()

	if strings.Contains(out, `\r`) {
		t.Errorf("emitted the invalid escape sequence \\r; the reference parser aborts the whole scrape on it:\n%s", out)
	}
	if strings.ContainsRune(out, '\r') {
		t.Errorf("emitted a bare CR inside a label value:\n%q", out)
	}
	// And the series is still there, flattened rather than dropped.
	if !strings.Contains(out, `model="we ird-model"`) {
		t.Errorf("CR should flatten to a space, keeping the series intact:\n%s", out)
	}
}
