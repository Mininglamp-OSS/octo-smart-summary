package handler

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Refine LLM budget.
//
// The four refine entry points (personal_refine.go x2, edit.go x2) each had a
// hardcoded 90s context. That number is the PARENT deadline for an
// llmfallback.Run. Refine derives a per-attempt budget estimate from that
// parent, which can skip same-model retries after returned transient failures,
// but it is not itself a request timeout: a stalled primary can still consume
// the whole parent budget before fallback starts.
//
// Sizing this correctly requires the runner's ACTUAL retry semantics. Refine now
// derives `PerModelTimeout` from the remaining parent deadline (see service/llm.go),
// which arms the deadline-aware early-escalation guard between attempts. That
// lets a transient error returned before the parent deadline skip remaining
// same-model retries when the remaining budget should be saved for fallback.
//
// Important limitation: `PerModelTimeout` is an accounting estimate, not the
// request deadline. A stalled primary attempt is still bounded by the parent
// `REFINE_TIMEOUT` (or by `LLM_TIMEOUT` if smaller), so the common "hung primary"
// case can still consume the whole 90s default before any fallback starts.
// Enforcing a proportional per-attempt request timeout is a separate latency
// tradeoff that belongs with the measured-percentile work.
//
// An earlier revision of this comment assumed the early-escalation guard was not
// armed for refine at all and therefore required (MaxAttempts+1)*LLM_TIMEOUT +
// backoffs (723s at defaults) for a fallback to get one full 180s attempt. The
// shipped runtime is now in between: the guard is armed for returned transient
// failures, but hung attempts are not sliced by this PR.
//
// This file deliberately keeps the 90s default: it is the latency users
// experience today, and raising it is an explicit latency tradeoff best made
// against measured refine percentiles (`llm_run_duration_seconds`). What changes
// here is only that the value stops being welded into four call sites.
// Note also that on the two STREAMING refine handlers this bounds the LLM run,
// not the request: both clear the response write deadline before streaming, so
// a connected-but-not-reading client is bounded by neither this value nor a
// server WriteTimeout. That predates this knob.
const defaultRefineTimeout = 90 * time.Second

// maxRefineTimeout bounds REFINE_TIMEOUT.
//
// Parsing alone is not enough to make a typo safe. Two values parse cleanly and
// are still wrong in ways that are invisible in production:
//
//   - a wrong unit (REFINE_TIMEOUT=90000, assuming milliseconds) yields a
//     25-hour deadline, so a stuck refine holds its connection and goroutine
//     for effectively forever instead of failing at 90s;
//   - a huge value overflows time.Duration (seconds * time.Second wraps), which
//     produces a NEGATIVE duration and makes every refine request build an
//     already-expired context — all four handlers would fail instantly, and
//     silently.
//
// The ceiling comfortably clears any realistic parent budget an operator may set
// while still preventing effectively-forever stuck connections. There is no
// floor constant: the <= 0 rejection below already guarantees at least 1s, so a
// floor clamp would be unreachable code.
const maxRefineTimeout = 30 * time.Minute

// refineTimeoutEnvVar overrides the refine budget, in seconds.
const refineTimeoutEnvVar = "REFINE_TIMEOUT"

// refineTimeoutWarnOnce keeps a rejected or clamped value to a single log line
// per process. refineTimeout runs on every refine request, so an unguarded
// log.Printf here would reproduce the bad value on every call.
var refineTimeoutWarnOnce sync.Once

// refineTimeout returns the refine LLM budget.
//
// An unset value keeps the historical 90s. An unparsable or non-positive value
// also keeps 90s; a value above the ceiling is clamped to it. Every deviation
// from the configured input is logged once, so a typo is visible instead of
// silently reshaping every refine request.
//
// Read from the environment rather than threaded through config.Config,
// following agentStepTimeoutOverride's precedent in agent/profile.go: these
// handlers are constructed in tests that never build a full deps container.
func refineTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(refineTimeoutEnvVar))
	if raw == "" {
		return defaultRefineTimeout
	}
	// ParseInt with an explicit 64-bit width rather than Atoi: Atoi returns a
	// platform-width int, so on a 32-bit build a value like 9223372036854775807
	// would take the ErrRange path instead of being clamped, making the
	// behaviour architecture-dependent.
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		warnRefineTimeoutOnce("%s=%q is not a positive integer; using default %s", refineTimeoutEnvVar, raw, defaultRefineTimeout)
		return defaultRefineTimeout
	}
	// Check before multiplying: seconds beyond this bound cannot be represented
	// and would wrap to a negative duration.
	if secs > int64(maxRefineTimeout/time.Second) {
		warnRefineTimeoutOnce("%s=%q exceeds the maximum %s; clamped", refineTimeoutEnvVar, raw, maxRefineTimeout)
		return maxRefineTimeout
	}
	return time.Duration(secs) * time.Second
}

func warnRefineTimeoutOnce(format string, args ...any) {
	refineTimeoutWarnOnce.Do(func() {
		log.Printf("[refine] "+format, args...)
	})
}
