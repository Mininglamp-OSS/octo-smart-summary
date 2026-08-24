package llmfallback

import (
	"context"
	"sync/atomic"
)

type pathCtxKey struct{}

// WithPath tags ctx with the logical call site, so a generic entry point like
// LLMClient.Call reports whether it was serving worker Map, worker Reduce or an
// API refine without every signature growing a path parameter.
//
// Config.Path, when set, wins over the context value.
func WithPath(ctx context.Context, p Path) context.Context {
	return context.WithValue(ctx, pathCtxKey{}, p)
}

// PathFromContext returns the path tagged by WithPath, or PathUnknown.
func PathFromContext(ctx context.Context) Path {
	if ctx == nil {
		return PathUnknown
	}
	if p, ok := ctx.Value(pathCtxKey{}).(Path); ok && p != "" {
		return p
	}
	return PathUnknown
}

// defaultObserver is the process-wide Observer used when Config.Observer is
// nil. Telemetry is a cross-cutting concern here: routing it through every
// constructor would mean four call sites that can each silently forget to wire
// it, which is exactly how a fallback goes unmonitored. Set it once from a
// composition root (cmd/summary-api, cmd/summary-worker) before serving.
var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs the process-wide Observer. Passing nil clears it.
// Safe to call concurrently, but intended to run once at startup.
func SetDefaultObserver(o Observer) {
	if o == nil {
		defaultObserver.Store(nil)
		return
	}
	defaultObserver.Store(&o)
}

func (c Config) observer() Observer {
	if c.Observer != nil {
		return c.Observer
	}
	if p := defaultObserver.Load(); p != nil {
		return *p
	}
	return noopObserver{}
}

func (c Config) pathFor(ctx context.Context) Path {
	if c.Path != "" {
		return c.Path
	}
	return PathFromContext(ctx)
}
