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
// that produced it, and an error. Terminal failures preserve any partial value
// returned by the attempt (notably streamed content and token accounting).
// Multiple models wrap the final error with the number of models attempted.
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
			log.Printf("[llmfallback] falling back from %q to %q: %s",
				cfg.Models[i-1], model, SafeErrorForLog(lastErr, 200))
		}
		hasNext := i < len(cfg.Models)-1

		val, outcome, err := runModel(ctx, model, maxAttempts, hasNext, cfg, attempt)
		switch outcome {
		case Success:
			return val, model, nil
		case Terminal:
			return val, model, err
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
						if lastErr == nil {
							lastErr = fmt.Errorf("insufficient budget for another %s attempt on %q", cfg.PerModelTimeout, model)
						}
						return zero, TryNextModel, lastErr
					}
				}
			}
			select {
			case <-ctx.Done():
				return zero, TryNextModel, ctx.Err()
			case <-time.After(delay):
			}
		}
		val, outcome, err := attempt(ctx, model)
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
			return zero, TryNextModel, err
		case RetrySameModel:
			lastErr = err
		}
	}
	return zero, TryNextModel, fmt.Errorf("model %q failed after %d attempt(s): %w", model, maxAttempts, lastErr)
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
