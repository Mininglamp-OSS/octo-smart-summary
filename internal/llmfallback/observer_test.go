package llmfallback

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// recorder captures every event for assertions.
type recorder struct {
	attempts []AttemptEvent
	switches []SwitchEvent
	results  []ResultEvent
}

func (r *recorder) ObserveAttempt(e AttemptEvent) { r.attempts = append(r.attempts, e) }
func (r *recorder) ObserveSwitch(e SwitchEvent)   { r.switches = append(r.switches, e) }
func (r *recorder) ObserveResult(e ResultEvent)   { r.results = append(r.results, e) }

// TestObserver_DoesNotAlterRunSemantics is the load-bearing guarantee from the
// brief: instrumentation must not change which model serves a request, what the
// caller gets back, or what the error says. internal/worker's
// sanitizeErrorForUser matches on error TEXT to choose the user-facing message,
// so an error-string drift would change what users see in their IM DM.
func TestObserver_DoesNotAlterRunSemantics(t *testing.T) {
	cases := []struct {
		name    string
		models  []string
		attempt Attempt[string]
	}{
		{
			name:   "primary success",
			models: []string{"a", "b"},
			attempt: func(_ context.Context, m string) (string, Outcome, error) {
				return "ok-" + m, Success, nil
			},
		},
		{
			name:   "fallback rescues after retries",
			models: []string{"a", "b"},
			attempt: func(_ context.Context, m string) (string, Outcome, error) {
				if m == "a" {
					return "", RetrySameModel, errors.New("rate limited")
				}
				return "ok-" + m, Success, nil
			},
		},
		{
			name:   "terminal stops immediately and preserves partial",
			models: []string{"a", "b"},
			attempt: func(_ context.Context, m string) (string, Outcome, error) {
				return "partial", Terminal, errors.New("bad request")
			},
		},
		{
			name:   "every model fails",
			models: []string{"a", "b"},
			attempt: func(_ context.Context, m string) (string, Outcome, error) {
				return "", RetrySameModel, errors.New("boom")
			},
		},
		{
			name:   "single model preserves the un-wrapped error contract",
			models: []string{"only"},
			attempt: func(_ context.Context, m string) (string, Outcome, error) {
				return "", RetrySameModel, errors.New("solo failure")
			},
		},
		{
			name:   "403 escalates without same-model retries",
			models: []string{"a", "b"},
			attempt: func(_ context.Context, m string) (string, Outcome, error) {
				if m == "a" {
					return "", ClassifyStatus(http.StatusForbidden), errors.New("explicit deny")
				}
				return "ok-" + m, Success, nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := Config{Models: tc.models, MaxAttempts: 3, Backoff: noBackoff}

			wantVal, wantModel, wantErr := Run(context.Background(), base, tc.attempt)

			instrumented := base
			instrumented.Observer = &recorder{}
			gotVal, gotModel, gotErr := Run(context.Background(), instrumented, tc.attempt)

			if gotVal != wantVal {
				t.Errorf("value drifted with observer: got %q want %q", gotVal, wantVal)
			}
			if gotModel != wantModel {
				t.Errorf("chosen model drifted with observer: got %q want %q", gotModel, wantModel)
			}
			switch {
			case wantErr == nil && gotErr != nil:
				t.Errorf("observer introduced an error: %v", gotErr)
			case wantErr != nil && gotErr == nil:
				t.Errorf("observer swallowed error %v", wantErr)
			case wantErr != nil && gotErr.Error() != wantErr.Error():
				// Error TEXT, not just identity: sanitizeErrorForUser greps it.
				t.Errorf("error text drifted with observer:\n got: %s\nwant: %s", gotErr, wantErr)
			}
		})
	}
}

// TestObserver_ClassifiesSwitchReasons pins the three reasons apart. Collapsing
// them into one "fallback happened" counter would page the wrong responder:
// denied needs billing/IAM, retries_exhausted usually self-heals, and
// budget_starved is a deadline misconfiguration nobody upstream can fix.
func TestObserver_ClassifiesSwitchReasons(t *testing.T) {
	t.Run("retries_exhausted", func(t *testing.T) {
		r := &recorder{}
		Run(context.Background(), Config{
			Models: []string{"a", "b"}, MaxAttempts: 3, Backoff: noBackoff, Observer: r,
		}, func(_ context.Context, m string) (string, Outcome, error) {
			if m == "a" {
				return "", RetrySameModel, errors.New("429")
			}
			return "ok", Success, nil
		})
		assertSwitch(t, r, ReasonRetriesExhausted, 3)
	})

	t.Run("denied", func(t *testing.T) {
		r := &recorder{}
		Run(context.Background(), Config{
			Models: []string{"a", "b"}, MaxAttempts: 3, Backoff: noBackoff, Observer: r,
		}, func(_ context.Context, m string) (string, Outcome, error) {
			if m == "a" {
				return "", ClassifyStatus(http.StatusForbidden), errors.New("SCP explicit deny")
			}
			return "ok", Success, nil
		})
		// A 403 must not burn the retry budget before escalating.
		assertSwitch(t, r, ReasonDenied, 1)
	})

	// budget_starved is reproduced with the SHIPPED agent defaults, so this test
	// fails the day someone retunes them into a sane relationship — which is the
	// point: the reason should stop firing once the config is fixed.
	t.Run("budget_starved under shipped 240s/180s defaults", func(t *testing.T) {
		const stepTimeout = 240 * time.Second // agent/profile.go StepTimeout
		const perModel = 180 * time.Second    // LLM_TIMEOUT default

		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		defer cancel()

		r := &recorder{}
		Run(ctx, Config{
			Models: []string{"a", "b"}, PerModelTimeout: perModel, MaxAttempts: 3,
			Backoff: noBackoff, Observer: r,
		}, func(_ context.Context, m string) (string, Outcome, error) {
			if m == "a" {
				return "", RetrySameModel, errors.New("429")
			}
			return "ok", Success, nil
		})
		// spent=1: the guard fires before the second attempt, which is exactly
		// the signature that distinguishes starvation from a real exhaustion.
		assertSwitch(t, r, ReasonBudgetStarved, 1)
	})
}

func assertSwitch(t *testing.T, r *recorder, want SwitchReason, wantAttempts int) {
	t.Helper()
	if len(r.switches) != 1 {
		t.Fatalf("expected exactly 1 switch, got %d: %+v", len(r.switches), r.switches)
	}
	got := r.switches[0]
	if got.Reason != want {
		t.Errorf("reason = %q, want %q (err: %v)", got.Reason, want, got.Err)
	}
	if got.Attempts != wantAttempts {
		t.Errorf("attempts spent on the abandoned model = %d, want %d", got.Attempts, wantAttempts)
	}
	if got.FromPos != PositionPrimary {
		t.Errorf("from_position = %q, want %q", got.FromPos, PositionPrimary)
	}
}

// TestObserver_ResultReportsServingPosition covers the headline degradation
// signal: whether the answer came from the primary or a fallback.
func TestObserver_ResultReportsServingPosition(t *testing.T) {
	r := &recorder{}
	Run(context.Background(), Config{
		Models: []string{"a", "b"}, MaxAttempts: 1, Backoff: noBackoff, Observer: r,
		Path: PathAgentChat,
	}, func(_ context.Context, m string) (string, Outcome, error) {
		if m == "a" {
			return "", RetrySameModel, errors.New("down")
		}
		return "ok", Success, nil
	})

	if len(r.results) != 1 {
		t.Fatalf("expected 1 result event, got %d", len(r.results))
	}
	res := r.results[0]
	if !res.OK || res.Position != PositionFallback || res.Model != "b" {
		t.Errorf("result = %+v; want OK on fallback model b", res)
	}
	if res.Switches != 1 {
		t.Errorf("switches = %d, want 1", res.Switches)
	}
	if res.Path != PathAgentChat {
		t.Errorf("path = %q, want %q", res.Path, PathAgentChat)
	}
}

// TestObserver_PathFromContext covers the generic entry points: LLMClient.Call
// serves worker Map, worker Reduce and API refine, so the path has to ride the
// context rather than the Config.
func TestObserver_PathFromContext(t *testing.T) {
	r := &recorder{}
	ctx := WithPath(context.Background(), PathWorkerReduce)
	Run(ctx, Config{Models: []string{"a"}, MaxAttempts: 1, Observer: r},
		func(_ context.Context, m string) (string, Outcome, error) { return "ok", Success, nil })

	if len(r.results) != 1 || r.results[0].Path != PathWorkerReduce {
		t.Fatalf("path did not come from context: %+v", r.results)
	}

	// Config.Path must win over the context value.
	r2 := &recorder{}
	Run(ctx, Config{Models: []string{"a"}, MaxAttempts: 1, Observer: r2, Path: PathAgentChat},
		func(_ context.Context, m string) (string, Outcome, error) { return "ok", Success, nil })
	if r2.results[0].Path != PathAgentChat {
		t.Errorf("Config.Path should override context, got %q", r2.results[0].Path)
	}
}

// TestObserver_NilIsSafe guards the default path: no observer installed must not
// panic and must not cost a nil check at every call site.
func TestObserver_NilIsSafe(t *testing.T) {
	SetDefaultObserver(nil)
	val, model, err := Run(context.Background(), Config{Models: []string{"a"}, MaxAttempts: 1},
		func(_ context.Context, m string) (string, Outcome, error) { return "ok", Success, nil })
	if val != "ok" || model != "a" || err != nil {
		t.Fatalf("got (%q,%q,%v)", val, model, err)
	}
}

// TestRun_PreservesSanitizerMatchableText pins the user-facing contract that the
// error-text-is-user-facing rule describes. worker.sanitizeErrorForUser selects
// the Chinese message a user receives in their IM DM by substring-matching the
// internal error, so a wrapped error that loses the matched substring silently
// downgrades a specific message ("AI 处理超时，请稍后重试") to the generic one.
//
// The substrings below are copied from the whitelist in
// internal/worker/personal_processor.go; this test fails if wrapping in Run ever
// stops preserving them.
func TestRun_PreservesSanitizerMatchableText(t *testing.T) {
	t.Run("deadline text survives multi-model wrapping", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already-dead parent

		_, _, err := Run(ctx, Config{Models: []string{"a", "b"}, MaxAttempts: 3, Backoff: noBackoff},
			func(_ context.Context, _ string) (string, Outcome, error) {
				return "", RetrySameModel, errors.New("unused")
			})
		if err == nil {
			t.Fatal("expected an error from a cancelled parent")
		}
		if !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Errorf("cancellation text lost through Run: %q", err)
		}
	})

	t.Run("upstream API error text survives multi-model wrapping", func(t *testing.T) {
		// "LLM API error" is the substring the whitelist keys on for the
		// "AI 服务暂时不可用" message.
		_, _, err := Run(context.Background(), Config{Models: []string{"a", "b"}, MaxAttempts: 2, Backoff: noBackoff},
			func(_ context.Context, m string) (string, Outcome, error) {
				return "", RetrySameModel, fmt.Errorf("LLM API error: status=503 model=%s", m)
			})
		if err == nil {
			t.Fatal("expected an error when every model fails")
		}
		if !strings.Contains(err.Error(), "LLM API error") {
			t.Errorf("upstream error text lost through wrapping: %q", err)
		}
	})

	t.Run("upstream text survives the denied wrapper", func(t *testing.T) {
		// deniedErr prefixes the caller-visible text; nothing pinned that the
		// wrapped upstream body still shows through, so sanitizeErrorForUser's
		// "LLM API error" branch was unguarded for every 403.
		_, _, err := Run(context.Background(), Config{Models: []string{"a"}, MaxAttempts: 3, Backoff: noBackoff},
			func(_ context.Context, m string) (string, Outcome, error) {
				return "", ClassifyStatus(http.StatusForbidden),
					fmt.Errorf("LLM API error: status=403 body=AccessDenied on %s", m)
			})
		if err == nil {
			t.Fatal("expected an error from a denied model")
		}
		if !strings.Contains(err.Error(), "LLM API error") {
			t.Errorf("upstream text lost through the denied wrapper: %q", err)
		}
	})

	t.Run("deadline text survives the budget-starved guard", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer cancel()

		_, _, err := Run(ctx, Config{
			Models: []string{"a", "b"}, PerModelTimeout: 180 * time.Second,
			MaxAttempts: 3, Backoff: noBackoff,
		}, func(_ context.Context, _ string) (string, Outcome, error) {
			return "", RetrySameModel, context.DeadlineExceeded
		})
		if err == nil {
			t.Fatal("expected an error when every model fails")
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Errorf("deadline text lost through the starvation guard: %q", err)
		}
		// The Unwrap chain matters too, not just the text: internal/agent's
		// tool_error.go:70 classifies with errors.Is(err, context.DeadlineExceeded).
		// A guard error that stops unwrapping would silently reclassify a timeout.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("errors.Is chain broken through the starvation guard: %v", err)
		}
	})
}

// TestReasonFor_RealProductionErrorShapes pins the classification against the
// error shapes production actually produces, rather than the bare sentinels a
// unit test reaches for first.
//
// The regression this exists for: every call site wraps its attempt in its own
// per-attempt context (agent attemptChat, service CallWithTools). A merely slow
// upstream trips that INNER deadline while the parent Run context is healthy,
// so the attempt error wraps context.DeadlineExceeded even though nothing was
// cancelled. Classifying on the sentinel reported those switches as
// "cancelled" and left retries_exhausted reading ~0 during exactly the incident
// the metric exists to surface.
func TestReasonFor_RealProductionErrorShapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want SwitchReason
	}{
		{
			name: "per-attempt timeout on a healthy parent is upstream overload",
			err:  fmt.Errorf("http do: %w", context.DeadlineExceeded),
			want: ReasonRetriesExhausted,
		},
		{
			name: "url.Error wrapping the inner deadline, as net/http returns it",
			err:  fmt.Errorf("model %q failed after 3 attempt(s): %w", "primary", fmt.Errorf("Post \"http://gw\": %w", context.DeadlineExceeded)),
			want: ReasonRetriesExhausted,
		},
		{
			name: "plain 429",
			err:  errors.New("LLM API error: status=429"),
			want: ReasonRetriesExhausted,
		},
		{
			name: "starvation still wins even though it wraps a deadline",
			err:  &budgetStarvedErr{model: "p", budget: time.Minute, spent: 1, wrapped: context.DeadlineExceeded},
			want: ReasonBudgetStarved,
		},
		{
			name: "denial still wins over a wrapped deadline",
			err:  &deniedErr{model: "p", spent: 1, wrapped: fmt.Errorf("403: %w", context.DeadlineExceeded)},
			want: ReasonDenied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasonFor(tc.err); got != tc.want {
				t.Errorf("reasonFor = %q, want %q (err: %v)", got, tc.want, tc.err)
			}
		})
	}
}

// TestRun_ContextEndIsClassifiedAtEveryExit pins the second half of the same
// problem: a caller walking away must not look like a total outage, and our own
// deadline expiring must not look like a caller walking away.
//
// The earlier version of this test cancelled BEFORE calling Run, so the attempt
// function never ran and only the top-of-loop exit was ever exercised — the one
// exit production almost never takes. Each case below drives a real exit:
// both clients turn a mid-flight cancel into Terminal (they check the parent
// ctx), and a cancel during a backoff sleep falls out of the model loop.
func TestRun_ContextEndIsClassifiedAtEveryExit(t *testing.T) {
	t.Run("cancelled mid-attempt reaches the Terminal exit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		r := &recorder{}
		Run(ctx, Config{Models: []string{"a", "b"}, MaxAttempts: 3, Backoff: noBackoff, Observer: r},
			func(actx context.Context, _ string) (string, Outcome, error) {
				cancel() // the client closed the tab while the request was open
				if actx.Err() != nil {
					// Exactly what agent attemptChat and service Call do.
					return "", Terminal, actx.Err()
				}
				return "", RetrySameModel, nil
			})
		assertEnd(t, r, RunEndCancelled)
	})

	t.Run("cancelled during backoff reaches the loop-end exit", func(t *testing.T) {
		// A single model is the default deployment (LLM_FALLBACK_MODELS empty),
		// and it is the shape that used to emit the "exhausted every configured
		// model" ERROR for a plain disconnect.
		ctx, cancel := context.WithCancel(context.Background())
		r := &recorder{}
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		Run(ctx, Config{
			Models: []string{"only"}, MaxAttempts: 3, Observer: r,
			Backoff: func(int) time.Duration { return 50 * time.Millisecond },
		}, func(context.Context, string) (string, Outcome, error) {
			return "", RetrySameModel, errors.New("upstream 503")
		})
		assertEnd(t, r, RunEndCancelled)
	})

	t.Run("our own deadline expiring is a timeout, not a cancellation", func(t *testing.T) {
		// Upstream is slow and our budget runs out. Filing this under
		// "cancelled" would mark a real incident "do not alert on it".
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		r := &recorder{}
		Run(ctx, Config{Models: []string{"a", "b"}, MaxAttempts: 1, Backoff: noBackoff, Observer: r},
			func(context.Context, string) (string, Outcome, error) {
				time.Sleep(50 * time.Millisecond)
				return "", ClassifyStatus(http.StatusServiceUnavailable), errors.New("503 slow")
			})
		assertEnd(t, r, RunEndTimedOut)
	})

	t.Run("an ordinary upstream failure is neither", func(t *testing.T) {
		r := &recorder{}
		Run(context.Background(), Config{Models: []string{"a"}, MaxAttempts: 1, Backoff: noBackoff, Observer: r},
			func(context.Context, string) (string, Outcome, error) {
				return "", ClassifyStatus(http.StatusBadRequest), errors.New("400 bad request")
			})
		assertEnd(t, r, RunEndNone)
	})
}

func assertEnd(t *testing.T, r *recorder, want RunEnd) {
	t.Helper()
	if len(r.results) != 1 {
		t.Fatalf("expected exactly 1 result event, got %d: %+v", len(r.results), r.results)
	}
	got := r.results[0]
	if got.End != want {
		t.Errorf("End = %q, want %q (err: %v)", got.End, want, got.Err)
	}
	if got.OK {
		t.Error("a failed run must not report OK")
	}
}

// TestRun_EmptyModelListIsObserved covers the one exit that used to return
// before any observer call: a deployment configured with no models failed every
// request while the counters stayed flat.
func TestRun_EmptyModelListIsObserved(t *testing.T) {
	r := &recorder{}
	_, _, err := Run(context.Background(), Config{Models: nil, MaxAttempts: 3, Observer: r},
		func(_ context.Context, _ string) (string, Outcome, error) {
			t.Fatal("attempt must not run with no models configured")
			return "", Terminal, nil
		})
	if err == nil {
		t.Fatal("expected an error for an empty model list")
	}
	if len(r.results) != 1 {
		t.Fatalf("misconfiguration produced %d result events, want 1", len(r.results))
	}
	if r.results[0].OK {
		t.Error("empty model list reported OK")
	}
}

// TestRun_AttemptEventCarriesBudget lets a consumer tell "will retry" from
// "this was the last attempt" — the retry log claimed a retry was coming even
// on the final attempt before this was plumbed through.
func TestRun_AttemptEventCarriesBudget(t *testing.T) {
	r := &recorder{}
	Run(context.Background(), Config{Models: []string{"solo"}, MaxAttempts: 3, Backoff: noBackoff, Observer: r},
		func(_ context.Context, _ string) (string, Outcome, error) {
			return "", RetrySameModel, errors.New("503")
		})

	if len(r.attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(r.attempts))
	}
	for i, a := range r.attempts {
		if a.MaxAttempts != 3 {
			t.Errorf("attempt %d: MaxAttempts = %d, want 3", i+1, a.MaxAttempts)
		}
		if a.Attempt != i+1 {
			t.Errorf("attempt %d: Attempt = %d, want %d (1-based)", i+1, a.Attempt, i+1)
		}
	}
	// The last one must be identifiable as final: no retry follows it.
	last := r.attempts[len(r.attempts)-1]
	if last.Attempt < last.MaxAttempts {
		t.Errorf("final attempt %d/%d does not read as final", last.Attempt, last.MaxAttempts)
	}
}
