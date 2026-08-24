package llmobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

// captureLogs returns an Observer writing JSON records into buf, so the log
// half of this package can be asserted. Before this, every log mutation
// survived: level policy, event names and the retry-record condition were all
// unasserted, and that half carries the "who gets paged" semantics.
func captureLogs(t *testing.T) (*Observer, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return New(NewMetrics(fixedNow), slog.New(h)), &buf
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestObserverLog_SwitchLevelsMatchWhoCanFixIt pins the level policy: a
// configuration or credential fault nobody upstream will fix is ERROR; ordinary
// upstream overload is WARN. Collapsing them makes a transient blip page the
// same as a broken deployment.
func TestObserverLog_SwitchLevelsMatchWhoCanFixIt(t *testing.T) {
	cases := []struct {
		reason    llmfallback.SwitchReason
		wantLevel string
	}{
		{llmfallback.ReasonBudgetStarved, "ERROR"},
		{llmfallback.ReasonDenied, "ERROR"},
		{llmfallback.ReasonRetriesExhausted, "WARN"},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			o, buf := captureLogs(t)
			o.ObserveSwitch(llmfallback.SwitchEvent{
				Path: llmfallback.PathAgentChat, From: "p", To: "f",
				FromPos: llmfallback.PositionPrimary, Reason: tc.reason,
				Attempts: 1, Err: errors.New("boom"),
			})
			recs := records(t, buf)
			if len(recs) != 1 {
				t.Fatalf("want exactly 1 record, got %d", len(recs))
			}
			if got := recs[0]["level"]; got != tc.wantLevel {
				t.Errorf("level = %v, want %v", got, tc.wantLevel)
			}
			if got := recs[0]["event"]; got != "llm.model.switch" {
				t.Errorf("event = %v, want llm.model.switch (alert rules match on it)", got)
			}
			if got := recs[0]["reason"]; got != string(tc.reason) {
				t.Errorf("reason = %v, want %v", got, tc.reason)
			}
		})
	}
}

// TestObserverLog_RetryRecordOnlyWhenARetryFollows covers the record that used
// to announce a retry on the final attempt, when the next step is abandoning
// the model entirely.
func TestObserverLog_RetryRecordOnlyWhenARetryFollows(t *testing.T) {
	o, buf := captureLogs(t)
	for attempt := 1; attempt <= 3; attempt++ {
		o.ObserveAttempt(llmfallback.AttemptEvent{
			Path: llmfallback.PathWorkerMap, Model: "m", Position: llmfallback.PositionPrimary,
			Attempt: attempt, MaxAttempts: 3, Outcome: llmfallback.RetrySameModel,
			Duration: time.Millisecond, Err: errors.New("503"),
		})
	}
	recs := records(t, buf)
	if len(recs) != 2 {
		t.Fatalf("want 2 retry records for a 3-attempt budget (the last attempt is not retried), got %d", len(recs))
	}
	for _, r := range recs {
		if r["event"] != "llm.attempt.retry" {
			t.Errorf("unexpected event %v", r["event"])
		}
	}
}

// TestObserverLog_SuccessAndTerminalAreQuiet keeps the retry record from
// becoming a log flood: only RetrySameModel is worth a line.
func TestObserverLog_SuccessAndTerminalAreQuiet(t *testing.T) {
	for _, oc := range []llmfallback.Outcome{llmfallback.Success, llmfallback.Terminal, llmfallback.TryNextModel} {
		o, buf := captureLogs(t)
		o.ObserveAttempt(llmfallback.AttemptEvent{
			Path: llmfallback.PathWorkerMap, Model: "m", Position: llmfallback.PositionPrimary,
			Attempt: 1, MaxAttempts: 3, Outcome: oc,
		})
		if recs := records(t, buf); len(recs) != 0 {
			t.Errorf("outcome %v produced %d log records, want 0", oc, len(recs))
		}
	}
}

// TestObserverLog_CancellationIsNotAnOutage is the log-side half of the
// exhaustion/cancellation split: a user closing a tab must not emit the ERROR
// record that means "no model could serve this".
func TestObserverLog_CancellationIsNotAnOutage(t *testing.T) {
	o, buf := captureLogs(t)
	o.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathAPIRefine, End: llmfallback.RunEndCancelled,
		Duration: 10 * time.Millisecond, Err: context.Canceled,
	})
	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if got := recs[0]["level"]; got == "ERROR" {
		t.Errorf("caller cancellation logged at ERROR; it is not an outage")
	}
	if got := recs[0]["event"]; got != "llm.call.cancelled" {
		t.Errorf("event = %v, want llm.call.cancelled", got)
	}

	// The genuine outage still is one.
	o2, buf2 := captureLogs(t)
	o2.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathAPIRefine, Duration: time.Second, Err: errors.New("all failed"),
	})
	recs2 := records(t, buf2)
	if len(recs2) != 1 || recs2[0]["level"] != "ERROR" || recs2[0]["event"] != "llm.call.exhausted" {
		t.Errorf("total exhaustion must stay an ERROR llm.call.exhausted record, got %+v", recs2)
	}
}

// TestObserverLog_NilLoggerIsSafe covers Install(nil): the guard is only
// exercised if an event actually flows afterwards.
func TestObserverLog_NilLoggerIsSafe(t *testing.T) {
	o := New(NewMetrics(fixedNow), nil)
	o.ObserveAttempt(llmfallback.AttemptEvent{Attempt: 1, MaxAttempts: 3, Outcome: llmfallback.RetrySameModel})
	o.ObserveSwitch(llmfallback.SwitchEvent{Reason: llmfallback.ReasonDenied})
	o.ObserveResult(llmfallback.ResultEvent{})
}

// TestObserverLog_TimeoutIsAlertableButNotAnOutage pins the third state apart
// from the other two. Our own deadline expiring is not a caller walking away
// (Info would bury a real incident) and not a proven provider outage (Error
// would page someone about a model that was never shown to be bad).
func TestObserverLog_TimeoutIsAlertableButNotAnOutage(t *testing.T) {
	o, buf := captureLogs(t)
	o.ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathAgentChat, End: llmfallback.RunEndTimedOut,
		Duration: 240 * time.Second, Err: context.DeadlineExceeded,
	})
	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if got := recs[0]["level"]; got != "WARN" {
		t.Errorf("level = %v, want WARN (Info buries it, Error reads as a provider outage)", got)
	}
	if got := recs[0]["event"]; got != "llm.call.timeout" {
		t.Errorf("event = %v, want llm.call.timeout", got)
	}
}
