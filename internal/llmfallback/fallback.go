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
// (e.g. gpt-4.1 / Kimi) can still succeed. That is why 401/403 map to
// TryNextModel here rather than Terminal.
package llmfallback

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
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
	// same model will not fix (401 / 403 — auth or account-level denial), but
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
//	401, 403         → TryNextModel   (auth / account denial; a different
//	                   backend may succeed — see the Bedrock SCP-deny case)
//	other 4xx        → Terminal       (bad request / contract mismatch)
func ClassifyStatus(code int) Outcome {
	switch {
	case code < 400:
		return Success
	case code == http.StatusTooManyRequests || code >= 500:
		return RetrySameModel
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return TryNextModel
	default:
		return Terminal
	}
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
}

func (c Config) backoff(attempt int) time.Duration {
	if c.Backoff != nil {
		return c.Backoff(attempt)
	}
	return time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
}

// Run tries the configured models in order and returns the value, the model
// that produced it, and an error. A single configured model preserves the
// underlying single-model error verbatim (so log/alert patterns keep working);
// multiple models wrap the last error with a count when all fail.
func Run[T any](ctx context.Context, cfg Config, attempt Attempt[T]) (T, string, error) {
	var zero T
	maxAttempts := cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if len(cfg.Models) == 0 {
		return zero, "", fmt.Errorf("llmfallback: no models configured")
	}

	var lastErr error
	for i, model := range cfg.Models {
		// Respect caller cancellation before opening the next model's budget.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return zero, "", fmt.Errorf("%w (last: %v)", err, lastErr)
			}
			return zero, "", err
		}
		if i > 0 {
			log.Printf("[llmfallback] falling back from %q to %q: %v",
				cfg.Models[i-1], model, lastErr)
		}
		hasNext := i < len(cfg.Models)-1

		val, outcome, err := runModel(ctx, model, maxAttempts, hasNext, cfg, attempt)
		switch outcome {
		case Success:
			return val, model, nil
		case Terminal:
			return zero, "", err
		}
		// runModel exhausted retries or hit TryNextModel → advance.
		lastErr = err
	}

	if len(cfg.Models) == 1 {
		return zero, "", lastErr
	}
	return zero, "", fmt.Errorf("all %d model(s) failed: %w", len(cfg.Models), lastErr)
}

// runModel runs the per-model retry loop. Returns Success (with value),
// Terminal (stop everything), or TryNextModel (advance to the next model,
// either because retries were exhausted on RetrySameModel or because the
// attempt classified TryNextModel).
func runModel[T any](ctx context.Context, model string, maxAttempts int, hasNext bool, cfg Config, attempt Attempt[T]) (T, Outcome, error) {
	var zero T
	var lastErr error
	for a := 0; a < maxAttempts; a++ {
		if a > 0 {
			select {
			case <-ctx.Done():
				return zero, TryNextModel, ctx.Err()
			case <-time.After(cfg.backoff(a)):
			}
			// Deadline-aware early escalation: if another full attempt cannot
			// fit before the parent deadline and a fallback is waiting, stop
			// retrying this model so the fallback gets a real budget.
			if hasNext {
				if dl, ok := ctx.Deadline(); ok && time.Until(dl) < cfg.PerModelTimeout {
					if lastErr == nil {
						lastErr = fmt.Errorf("insufficient budget for another %s attempt on %q", cfg.PerModelTimeout, model)
					}
					return zero, TryNextModel, lastErr
				}
			}
		}
		val, outcome, err := attempt(ctx, model)
		switch outcome {
		case Success:
			return val, Success, nil
		case Terminal:
			return zero, Terminal, err
		case TryNextModel:
			return zero, TryNextModel, err
		case RetrySameModel:
			lastErr = err
		}
	}
	return zero, TryNextModel, fmt.Errorf("model %q failed after %d attempt(s): %w", model, maxAttempts, lastErr)
}
