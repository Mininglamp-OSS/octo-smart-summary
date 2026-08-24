// Package llmfallback provides a shared cross-model fallback runner used by
// both the agent LLM client (internal/agent) and the summary worker/service
// LLM client (internal/service). It owns the retry / backoff / model-switch
// control flow so the two clients classify upstream failures identically and
// do not maintain two near-duplicate copies of the logic (issue #211).
//
// Failure-domain awareness (issue #211): a fallback only helps when the next
// model routes to a *different* backend than the one that failed. The classic
// example is an AWS Bedrock account-level denial (a Service Control Policy
// explicitly denying bedrock:InvokeModel for the caller's IAM user): every
// request to that account returns HTTP 403, and retrying the same model is
// pointless, but a fallback model the gateway routes to a different provider
// (e.g. gpt-4.1 / Kimi) can still succeed. A 401 remains terminal because all
// models share the gateway credential, so trying more models only multiplies
// denied-auth traffic.
package llmfallback

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

// Outcome classifies a single attempt so Run knows how to proceed.
type Outcome int

const (
	// Success: the attempt returned a usable result.
	Success Outcome = iota
	// RetrySameModel: transient failure (429 / 5xx / transport). Retrying the
	// same model may succeed; Run retries with backoff before falling back.
	RetrySameModel
	// TryNextModel: this model/provider is unavailable in a way retrying the
	// same model will not fix (403 — account-level denial), but
	// a fallback routed to a different backend may work. Run skips the
	// remaining same-model retries and switches to the next model immediately.
	TryNextModel
	// Terminal: a deterministic failure a different model would also hit
	// (400 / decode error / empty choices / caller cancellation). Run stops.
	Terminal
)

// ClassifyStatus maps an HTTP status code to an Outcome. Shared policy so the
// agent and worker clients agree on retry-vs-fallback-vs-stop.
//
//	< 400            → Success
//	429, 5xx         → RetrySameModel (transient overload / outage)
//	403              → TryNextModel   (account denial; a different backend
//	                   may succeed — see the Bedrock SCP-deny case)
//	other 4xx        → Terminal       (bad request / contract mismatch)
func ClassifyStatus(code int) Outcome {
	switch {
	case code < 400:
		return Success
	case code == http.StatusTooManyRequests || code >= 500:
		return RetrySameModel
	case code == http.StatusForbidden:
		return TryNextModel
	default:
		return Terminal
	}
}

// ClassifyNonOKStatus classifies an HTTP response after the caller has
// established that it is not 200 OK. Non-200 2xx and 3xx responses are
// protocol failures, not successful chat completions.
func ClassifyNonOKStatus(code int) Outcome {
	if code >= http.StatusBadRequest {
		return ClassifyStatus(code)
	}
	return Terminal
}

// Attempt performs a single upstream request against model. It returns the
// value, the Outcome classification and the underlying error (nil on Success).
// It must not implement its own retry/backoff/model-switch — Run owns that.
type Attempt[T any] func(ctx context.Context, model string) (T, Outcome, error)

// Config parameterises Run.
type Config struct {
	// Models is the ordered list of model identifiers to try: the primary
	// first, then fallbacks. Empty yields a zero value and a nil-safe error.
	Models []string
	// PerModelTimeout is one attempt's expected budget, used for the
	// deadline-aware early escalation (do not start another same-model retry
	// if it cannot fit before the parent deadline and a fallback is waiting).
	PerModelTimeout time.Duration
	// MaxAttempts is the per-model retry budget for RetrySameModel outcomes.
	// Values < 1 are treated as 1.
	MaxAttempts int
	// Backoff returns the sleep before retry attempt n (n >= 1). Nil defaults
	// to exponential 1s, 2s, 4s… Tests pass a zero-backoff to run fast.
	Backoff func(attempt int) time.Duration
	// Path labels which call site drove this Run, for metrics/logs. Optional;
	// defaults to PathUnknown.
	Path Path
	// Observer receives attempt/switch/result events. Optional; nil installs a
	// no-op. Not free: a nil observer still costs the timing calls Run makes
	// around each attempt (measured ~14ns -> ~200ns per Run, zero allocations),
	// which is noise against a network round trip but is not "nothing".
	Observer Observer
}

func (c Config) backoff(attempt int) time.Duration {
	if c.Backoff != nil {
		return c.Backoff(attempt)
	}
	return time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
}

// Run tries the configured models in order and returns the value, the model
// that produced it, and an error. Terminal failures preserve any partial value
// returned by the attempt (notably streamed content and token accounting).
// Multiple models wrap the final error with the number of models attempted.
func Run[T any](ctx context.Context, cfg Config, attempt Attempt[T]) (T, string, error) {
	var zero T
	maxAttempts := cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	obs, path := cfg.observer(), cfg.pathFor(ctx)
	start := time.Now()
	switches := 0

	// Report the misconfiguration too. This used to return before any observer
	// call, so a deployment with an empty model list failed every request while
	// llm_calls_total stayed flat — the one case where a silent counter is
	// least affordable.
	if len(cfg.Models) == 0 {
		err := fmt.Errorf("llmfallback: no models configured")
		obs.ObserveResult(ResultEvent{Path: path, Duration: time.Since(start), Err: err})
		return zero, "", err
	}

	var lastErr error
	for i, model := range cfg.Models {
		// Respect caller cancellation before opening the next model's budget.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				err = fmt.Errorf("%w (last: %v)", err, lastErr)
			}
			obs.ObserveResult(ResultEvent{
				Path: path, Switches: switches, End: endOf(ctx),
				Duration: time.Since(start), Err: err,
			})
			return zero, "", err
		}
		if i > 0 {
			log.Printf("[llmfallback] falling back from %q to %q: %s",
				cfg.Models[i-1], model, SafeErrorForLog(lastErr, 200))
			switches++
			obs.ObserveSwitch(SwitchEvent{
				Path:     path,
				From:     cfg.Models[i-1],
				To:       model,
				FromPos:  PositionOf(i - 1),
				Reason:   reasonFor(lastErr),
				Attempts: attemptsSpent(lastErr, maxAttempts),
				Err:      lastErr,
			})
		}
		hasNext := i < len(cfg.Models)-1

		val, outcome, err := runModel(ctx, model, maxAttempts, hasNext, cfg, attempt, obs, path, PositionOf(i))
		switch outcome {
		case Success:
			obs.ObserveResult(ResultEvent{
				Path: path, Model: model, Position: PositionOf(i),
				Switches: switches, OK: true, Duration: time.Since(start),
			})
			return val, model, nil
		case Terminal:
			// endOf matters here too: both real clients turn a caller-cancelled
			// in-flight request into exactly this exit (they check the PARENT
			// ctx, so Terminal here means "the caller's context is dead"), and
			// without it a closed tab was counted result="failed" WITH a
			// position — invisible against genuine 400s on the same series.
			obs.ObserveResult(ResultEvent{
				Path: path, Model: model, Position: PositionOf(i), End: endOf(ctx),
				Switches: switches, Duration: time.Since(start), Err: err,
			})
			return val, model, err
		}
		// runModel exhausted retries or hit TryNextModel → advance.
		lastErr = err
	}

	if len(cfg.Models) > 1 {
		lastErr = fmt.Errorf("all %d model(s) failed: %w", len(cfg.Models), lastErr)
	}
	// The loop-end exit is reached on cancellation too: runModel returns
	// TryNextModel with ctx.Err() when the caller goes away during a backoff
	// sleep, and with no next model the loop simply ends here. In the default
	// single-model deployment that made a closed tab emit the ERROR
	// "exhausted every configured model" — the exact record this is meant to
	// keep for real outages.
	obs.ObserveResult(ResultEvent{
		Path: path, Switches: switches, End: endOf(ctx),
		Duration: time.Since(start), Err: lastErr,
	})
	return zero, "", lastErr
}

// endOf classifies how the caller's context ended a run, if it did.
//
// It keys on the sentinel rather than on ctx.Err() != nil because those are two
// different incidents: context.Canceled means the caller walked away and nobody
// should be paged, while context.DeadlineExceeded means OUR budget expired —
// usually because upstream was slow — which is exactly what an operator needs
// to see. This is the same distinction reasonFor makes for SwitchReason; it was
// missing one layer up, so a run that timed out was filed under a label whose
// documentation says "do not alert on it".
func endOf(ctx context.Context) RunEnd {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return RunEndCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return RunEndTimedOut
	default:
		return RunEndNone
	}
}

// budgetStarvedErr marks the error returned when the deadline guard abandons a
// model's remaining retries. It is a distinct type so ObserveSwitch can report
// ReasonBudgetStarved — a configuration problem — separately from a genuine
// upstream failure, without parsing error strings.
type budgetStarvedErr struct {
	model   string
	budget  time.Duration
	spent   int
	wrapped error
}

func (e *budgetStarvedErr) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("insufficient budget for another %s attempt on %q after %d attempt(s): %v",
			e.budget, e.model, e.spent, e.wrapped)
	}
	return fmt.Sprintf("insufficient budget for another %s attempt on %q after %d attempt(s)",
		e.budget, e.model, e.spent)
}

func (e *budgetStarvedErr) Unwrap() error { return e.wrapped }

// reasonFor classifies why a model was abandoned, for the switch metric.
func reasonFor(err error) SwitchReason {
	var starved *budgetStarvedErr
	switch {
	case err == nil:
		return ReasonRetriesExhausted
	case errors.As(err, &starved):
		return ReasonBudgetStarved
	// Deliberately NOT special-cased here: an error wrapping
	// context.DeadlineExceeded. Every production call site gives each attempt
	// its own request timeout (agent/llm.go attemptChat, service/llm.go
	// CallWithTools), so a merely slow upstream trips the INNER deadline while
	// the parent Run context is still healthy. Classifying on the sentinel
	// reported those as "cancelled" and emptied retries_exhausted during
	// exactly the incident this metric exists for. Genuine caller cancellation
	// never reaches this function: Run returns at the top of the model loop
	// before any switch is observed.
	case isDenied(err):
		return ReasonDenied
	default:
		return ReasonRetriesExhausted
	}
}

// attemptsSpent reports how many attempts were actually spent on the abandoned
// model, so the switch metric distinguishes "gave up after 1" (the starvation
// signature) from "gave up after the full budget".
func attemptsSpent(err error, maxAttempts int) int {
	var starved *budgetStarvedErr
	if errors.As(err, &starved) {
		return starved.spent
	}
	var denied *deniedErr
	if errors.As(err, &denied) {
		return denied.spent
	}
	return maxAttempts
}

// runModel runs the per-model retry loop. Returns Success (with value),
// Terminal (stop everything), or TryNextModel (advance to the next model,
// either because retries were exhausted on RetrySameModel or because the
// attempt classified TryNextModel).
func runModel[T any](ctx context.Context, model string, maxAttempts int, hasNext bool, cfg Config, attempt Attempt[T], obs Observer, path Path, position string) (T, Outcome, error) {
	var zero T
	var lastErr error
	for a := 0; a < maxAttempts; a++ {
		if a > 0 {
			delay := cfg.backoff(a)
			// Reserve the backoff, the pending same-model retry, and one complete
			// attempt for a fallback.
			// Checking before sleep prevents the backoff itself from consuming
			// the budget that this branch exists to protect. If the parent will
			// expire during backoff, let cancellation win instead of contacting a
			// fallback that cannot receive a meaningful attempt budget.
			if hasNext && cfg.PerModelTimeout > 0 {
				if dl, ok := ctx.Deadline(); ok {
					remaining := time.Until(dl)
					if remaining > delay && remaining < delay+2*cfg.PerModelTimeout {
						return zero, TryNextModel, &budgetStarvedErr{
							model: model, budget: cfg.PerModelTimeout, spent: a, wrapped: lastErr,
						}
					}
				}
			}
			select {
			case <-ctx.Done():
				return zero, TryNextModel, ctx.Err()
			case <-time.After(delay):
			}
		}
		attemptStart := time.Now()
		val, outcome, err := attempt(ctx, model)
		obs.ObserveAttempt(AttemptEvent{
			Path: path, Model: model, Position: position,
			Attempt: a + 1, MaxAttempts: maxAttempts,
			Outcome: outcome, Duration: time.Since(attemptStart), Err: err,
		})
		switch outcome {
		case Success:
			// Never let an inconsistent attempt result (Success plus error)
			// become a silent empty success at the caller.
			if err != nil {
				return val, Terminal, err
			}
			return val, Success, nil
		case Terminal:
			return val, Terminal, err
		case TryNextModel:
			return zero, TryNextModel, &deniedErr{model: model, spent: a + 1, wrapped: err}
		case RetrySameModel:
			lastErr = err
		}
	}
	return zero, TryNextModel, fmt.Errorf("model %q failed after %d attempt(s): %w", model, maxAttempts, lastErr)
}

// deniedErr marks an attempt-classified TryNextModel (today: HTTP 403 account
// denial) so the switch metric can separate "this credential is refused" from
// "upstream is overloaded". The two need different responders.
type deniedErr struct {
	model   string
	spent   int
	wrapped error
}

func (e *deniedErr) Error() string {
	return fmt.Sprintf("model %q refused the credential after %d attempt(s): %v", e.model, e.spent, e.wrapped)
}

func (e *deniedErr) Unwrap() error { return e.wrapped }

func isDenied(err error) bool {
	var d *deniedErr
	return errors.As(err, &d)
}

// SafeTextForLog flattens and bounds upstream-controlled text before it is
// written to logs or embedded in an error that callers may log.
func SafeTextForLog(s string, maxRunes int) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "...(truncated)"
}

// SafeErrorForLog is SafeTextForLog for errors.
func SafeErrorForLog(err error, maxRunes int) string {
	if err == nil {
		return "unknown error"
	}
	return SafeTextForLog(err.Error(), maxRunes)
}
