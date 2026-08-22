package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// ErrSummaryHandleCapacity marks a request-scoped store cap that has been hit.
// It is a typed sentinel rather than another error-string pattern on purpose:
// these messages spell "summary handle" with a SPACE, while the tool-error
// classifier matched "summary_handle" with an UNDERSCORE, so capacity failures
// silently fell through to the critical-tool default (retryable=true) and the
// nudged retry hit the identical cap every step until MaxSteps, ending the run
// with zero output. Adding a second substring pattern would fix today's spelling
// and leave the same trap armed for the next one; errors.Is cannot drift.
//
// Semantics: a deterministic dead end for THIS request. Retrying changes
// nothing — the caps are per-request and the store only grows.
var ErrSummaryHandleCapacity = errors.New("summary handle capacity exceeded")

const (
	maxSummaryHandles         = 128
	maxSummaryHandleText      = 8 << 20 // 8 MiB per request
	anonymousMapFailurePrefix = "tool_call_id="
)

// summaryHandleEntry is one Map result kept out of the planner transcript.
// It lives only for one RunWithHistory call; the opaque handle is the only part
// returned to the model.
type summaryHandleEntry struct {
	Handle     string
	Text       string
	ChunkCount int
	MapStep    int
}

type summaryHandleResolution struct {
	Entries    []summaryHandleEntry
	Generation uint64
}

type summaryMapFailure struct {
	Step int
}

// summaryHandleStore is request-scoped. It deliberately is not a Runner field
// or global cache: either would let handles leak across concurrent requests and
// would require owner binding, expiry and cleanup. RunWithHistory installs one
// store in its derived context and all tool calls share that context.
type summaryHandleStore struct {
	mu                sync.RWMutex
	prefix            string
	next              uint64
	generation        uint64
	reducedGeneration uint64
	totalTextBytes    int
	items             map[string]summaryHandleEntry
	mapFailures       map[string]summaryMapFailure
}

type summaryHandleStoreContextKey struct{}
type summaryToolStepContextKey struct{}

func newSummaryHandleStore() *summaryHandleStore {
	return &summaryHandleStore{
		prefix:      strings.ReplaceAll(uuid.NewString(), "-", ""),
		items:       make(map[string]summaryHandleEntry),
		mapFailures: make(map[string]summaryMapFailure),
	}
}

func withSummaryHandleStore(ctx context.Context) context.Context {
	// A context can outlive one Runner invocation (tests, jobs, or callers that
	// reuse a request-derived context). Always shadow any parent store so handles,
	// pending failures, and Reduce state cannot bleed into the next run.
	return context.WithValue(ctx, summaryHandleStoreContextKey{}, newSummaryHandleStore())
}

func summaryHandleStoreFromContext(ctx context.Context) (*summaryHandleStore, error) {
	store, ok := ctx.Value(summaryHandleStoreContextKey{}).(*summaryHandleStore)
	if !ok || store == nil {
		return nil, fmt.Errorf("missing request-scoped summary handle store")
	}
	return store, nil
}

func (s *summaryHandleStore) Put(text string, chunkCount int) (string, error) {
	return s.PutAtStep(text, chunkCount, 0)
}

func (s *summaryHandleStore) PutAtStep(text string, chunkCount, mapStep int) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("summary text is empty")
	}
	if chunkCount < 0 {
		return "", fmt.Errorf("chunk_count must be non-negative")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) >= maxSummaryHandles {
		return "", fmt.Errorf("%w: too many summary handles in one request (max %d)", ErrSummaryHandleCapacity, maxSummaryHandles)
	}
	if s.totalTextBytes+len(text) > maxSummaryHandleText {
		return "", fmt.Errorf("%w: summary handle text exceeds per-request limit (%d bytes)", ErrSummaryHandleCapacity, maxSummaryHandleText)
	}

	s.next++
	handle := fmt.Sprintf("map_%s_%d", s.prefix, s.next)
	s.generation++
	s.totalTextBytes += len(text)
	s.items[handle] = summaryHandleEntry{
		Handle:     handle,
		Text:       text,
		ChunkCount: chunkCount,
		MapStep:    mapStep,
	}
	return handle, nil
}

// ResolveAll resolves handles in the caller-provided order and rejects partial,
// duplicate or cross-request sets. Reduce must consume every Map result from the
// current request; silently omitting one would recreate the completeness bug in
// a smaller prompt.
func (s *summaryHandleStore) ResolveAll(handles []string) (summaryHandleResolution, error) {
	return s.ResolveAllBefore(handles, 0)
}

func (s *summaryHandleStore) ResolveAllBefore(handles []string, reduceStep int) (summaryHandleResolution, error) {
	if len(handles) == 0 {
		return summaryHandleResolution{}, fmt.Errorf("summary_handles must contain at least one handle")
	}
	if len(handles) > maxSummaryHandles {
		// NOT ErrSummaryHandleCapacity: this is the model handing over more
		// handles than can exist, which it CAN fix by passing the real set.
		return summaryHandleResolution{}, fmt.Errorf("too many summary_handles (max %d)", maxSummaryHandles)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.mapFailures) > 0 {
		return summaryHandleResolution{}, fmt.Errorf("cannot Reduce while %d summarize_chunk call(s) still need a successful retry", len(s.mapFailures))
	}
	seen := make(map[string]bool, len(handles))
	entries := make([]summaryHandleEntry, 0, len(handles))
	for _, handle := range handles {
		if handle == "" {
			return summaryHandleResolution{}, fmt.Errorf("summary_handle must not be empty")
		}
		if seen[handle] {
			return summaryHandleResolution{}, fmt.Errorf("duplicate summary_handle: %s", handle)
		}
		seen[handle] = true
		entry, ok := s.items[handle]
		if !ok {
			return summaryHandleResolution{}, fmt.Errorf("invalid or expired summary_handle: %s", handle)
		}
		if reduceStep > 0 && entry.MapStep >= reduceStep {
			return summaryHandleResolution{}, fmt.Errorf("summary_handle %s was produced in the same or a later tool step; run Reduce after all Map calls finish", handle)
		}
		entries = append(entries, entry)
	}
	if len(handles) != len(s.items) {
		return summaryHandleResolution{}, fmt.Errorf(
			"summary_handles must include all Map results from this request: got %d of %d",
			len(handles), len(s.items),
		)
	}
	return summaryHandleResolution{Entries: entries, Generation: s.generation}, nil
}

func withSummaryToolStep(ctx context.Context, step int) context.Context {
	return context.WithValue(ctx, summaryToolStepContextKey{}, step)
}

func summaryToolStepFromContext(ctx context.Context) int {
	step, _ := ctx.Value(summaryToolStepContextKey{}).(int)
	return step
}

// MarkMapFailed records a summarize_chunk failure that did not produce a
// summary handle. A valid messages_handle gives the failure durable lineage;
// malformed/handle-less calls use an anonymous tool_call_id key because there
// is no message set to bind yet.
func (s *summaryHandleStore) MarkMapFailed(target string, step int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mapFailures[target] = summaryMapFailure{Step: step}
}

// MarkMapSucceeded clears only a failure recovered in a LATER tool step. This
// prevents a parallel success in the same fan-out from masking another Map
// call's failure. A known messages_handle clears only its exact failure because
// different handles may represent unrelated channels. If there is no exact
// match, one older anonymous argument failure may be cleared: a malformed call
// has no lineage to match, and without this bounded fallback a corrected retry
// with a new tool-call ID could never recover the request.
func (s *summaryHandleStore) MarkMapSucceeded(target string, step int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if failure, ok := s.mapFailures[target]; ok && failure.Step < step {
		delete(s.mapFailures, target)
		return
	}
	for failedTarget, failure := range s.mapFailures {
		if strings.HasPrefix(failedTarget, anonymousMapFailurePrefix) && failure.Step < step {
			delete(s.mapFailures, failedTarget)
			return
		}
	}
}

func (s *summaryHandleStore) PendingMapFailures() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.mapFailures)
}

func (s *summaryHandleStore) PendingAnonymousMapFailures() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for target := range s.mapFailures {
		if strings.HasPrefix(target, anonymousMapFailurePrefix) {
			count++
		}
	}
	return count
}

// MarkReduced records a successful Reduce only if no new Map result appeared
// while the Reduce LLM was running. A concurrent/new Map increments generation,
// so the Runner will require another merge before accepting a final answer.
func (s *summaryHandleStore) MarkReduced(generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == s.generation {
		s.reducedGeneration = generation
	}
}

func (s *summaryHandleStore) NeedsReduce() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation > s.reducedGeneration
}
