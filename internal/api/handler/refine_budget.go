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
// hardcoded 90s context. That number is not independently meaningful: it is the
// PARENT deadline for an llmfallback.Run whose per-attempt timeout is
// LLM_TIMEOUT (default 180s). A parent smaller than a single attempt means a
// hanging primary consumes the whole budget and no fallback attempt can start,
// so LLM_FALLBACK_MODELS is effectively inert on refine.
//
// Sizing this correctly requires the runner's ACTUAL retry semantics, which are
// harsher than they look. Run gives the primary all MaxAttempts (3) attempts
// before it considers another model, and the early-escalation guard that would
// cut that short is deliberately not armed here (see service/llm.go). A hanging
// attempt is cut by http.Client.Timeout and classified RetrySameModel, so it
// costs a full LLM_TIMEOUT and the loop continues; only when the PARENT deadline
// fires inside an attempt does it classify Terminal, at which point Run returns
// without ever trying a fallback. So:
//
//	fallback gets one complete attempt  <=>  REFINE_TIMEOUT >= (MaxAttempts+1)*LLM_TIMEOUT + backoffs
//	                                                        =  4*180s + 3s = 723s at defaults
//	fallback cannot start at all         <=  REFINE_TIMEOUT <  MaxAttempts*LLM_TIMEOUT + backoffs
//	                                                        =  3*180s + 3s = 543s at defaults
//
// An earlier revision of this comment stated 2*LLM_TIMEOUT + backoff, which was
// only true under a budget-guard behaviour that was reverted. Following that
// number would have put an operator squarely in the second regime while
// believing they were in the first.
//
// This file deliberately does NOT raise the default to reach 723s: 90s is the
// latency users experience today on a path that currently errors at 90s, and an
// eightfold increase in worst-case refine latency is not a change to make
// blind. The right value depends on the refine percentiles that
// llm_run_duration_seconds now exposes (#220 defers the number to measured
// P95/P99). What changes here is only that the value stops being welded into
// four call sites.
//
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
// The ceiling comfortably clears the worst-case reachable run
// ((MaxAttempts+1)*LLM_TIMEOUT + backoffs = 723s at defaults) so it never
// silently truncates a legitimately-sized budget. There is no floor constant:
// the <= 0 rejection below already guarantees at least 1s, so a floor clamp
// would be unreachable code.
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
