package streaming

import (
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

const defaultClientBuffer = 128

const defaultIdleTTLMul = 10

// maxSnapshotBytes matches the current persisted-result size guard used by the
// summary handlers. Normal summaries stay fully replayable; abnormal streams are
// capped so a single in-memory hub entry cannot grow without bound.
const maxSnapshotBytes = 512 * 1024

type streamState struct {
	activeRunID string
	snapshot    string
	done        bool
	lastEventAt time.Time
	clients     map[chan Event]struct{}
	cleanup     *time.Timer
}

// Hub is an in-memory, single-api-instance bridge from worker NDJSON streams to
// browser SSE streams. Phase 1 deployment contract: summary-api runs as a single
// replica (or traffic is sticky). Multi-replica requires Redis Pub/Sub.
type Hub struct {
	mu      sync.Mutex
	ttl     time.Duration
	idleTTL time.Duration
	m       map[string]*streamState
}

func NewHub(ttl time.Duration) *Hub {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &Hub{ttl: ttl, idleTTL: ttl * defaultIdleTTLMul, m: make(map[string]*streamState)}
}

func Key(taskID int64, scope, targetUserID string) string {
	return fmt.Sprintf("%d:%s:%s", taskID, NormalizeScope(scope), targetUserID)
}

func (h *Hub) Subscribe(taskID int64, scope, targetUserID string) (<-chan Event, string, bool, func()) {
	key := Key(taskID, scope, targetUserID)
	ch := make(chan Event, defaultClientBuffer)
	var once sync.Once

	h.mu.Lock()
	st := h.ensureLocked(key)
	if st.cleanup != nil && !st.done {
		st.cleanup.Stop()
		st.cleanup = nil
	}
	st.clients[ch] = struct{}{}
	snapshot := st.snapshot
	done := st.done
	h.mu.Unlock()

	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			if cur := h.m[key]; cur != nil {
				delete(cur.clients, ch)
				if len(cur.clients) == 0 && !cur.done {
					h.scheduleIdleCleanupLocked(key, cur)
				}
			}
			h.mu.Unlock()
			// Do not close ch here. Publish copies subscriber channels and sends
			// after releasing h.mu; closing from cancel races with that send and can
			// panic. The SSE handler exits via request context cancellation, and the
			// unreferenced channel is garbage-collected.
		})
	}
	return ch, snapshot, done, cancel
}

func (h *Hub) Publish(ev Event) {
	if ev.TaskID == 0 || ev.Type == "" {
		return
	}
	ev.Scope = NormalizeScope(ev.Scope)
	key := Key(ev.TaskID, ev.Scope, ev.TargetUserID)

	h.mu.Lock()
	st := h.ensureLocked(key)

	if ev.Type == EventStart {
		if st.cleanup != nil {
			st.cleanup.Stop()
			st.cleanup = nil
		}
		st.activeRunID = ev.RunID
		st.snapshot = ""
		st.done = false
	} else if st.activeRunID != "" && ev.RunID != "" && ev.RunID != st.activeRunID {
		// Old worker run arrived late after a newer run started; discard to avoid
		// mixing regenerated content with a previous run.
		h.mu.Unlock()
		return
	}

	st.lastEventAt = time.Now()

	switch ev.Type {
	case EventDelta:
		st.snapshot = trimSnapshot(st.snapshot + ev.Delta)
		// Keep the accumulated content on delta events so slow clients that miss a
		// previous delta can self-heal on the next delivered frame. The snapshot is
		// capped above to keep memory and wire size bounded.
		ev.Content = st.snapshot
	case EventSnapshot:
		if ev.Content != "" {
			st.snapshot = trimSnapshot(ev.Content)
		}
	case EventDone, EventError:
		st.done = true
		ev.Content = st.snapshot
		if st.cleanup != nil {
			st.cleanup.Stop()
		}
		state := st
		runID := st.activeRunID
		st.cleanup = time.AfterFunc(h.ttl, func() { h.deleteKeyIfState(key, state, runID) })
	}
	if !st.done {
		h.scheduleIdleCleanupLocked(key, st)
	}

	clients := make([]chan Event, 0, len(st.clients))
	for ch := range st.clients {
		clients = append(clients, ch)
	}
	h.mu.Unlock()

	for _, ch := range clients {
		deliver(ch, ev)
	}
}

func deliver(ch chan Event, ev Event) {
	select {
	case ch <- ev:
		return
	default:
	}

	// Delta backpressure is best-effort: the next delta carries a bounded snapshot
	// and repairs missed text. Terminal events should not be silently dropped; make
	// room by discarding one stale buffered event and try once more.
	if ev.Type != EventDone && ev.Type != EventError {
		return
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- ev:
	default:
	}
}

func trimSnapshot(s string) string {
	if len(s) <= maxSnapshotBytes {
		return s
	}
	start := len(s) - maxSnapshotBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func (h *Hub) ensureLocked(key string) *streamState {
	st := h.m[key]
	if st == nil {
		st = &streamState{clients: make(map[chan Event]struct{})}
		h.m[key] = st
	}
	return st
}

func (h *Hub) scheduleIdleCleanupLocked(key string, st *streamState) {
	if h.idleTTL <= 0 || st.done {
		return
	}
	if st.cleanup != nil {
		st.cleanup.Stop()
	}
	lastEventAt := st.lastEventAt
	state := st
	st.cleanup = time.AfterFunc(h.idleTTL, func() { h.deleteKeyIfIdle(key, state, lastEventAt) })
}

func (h *Hub) deleteKeyIfIdle(key string, state *streamState, lastEventAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.m[key]
	if st == nil || st != state {
		return
	}
	if st.done {
		delete(h.m, key)
		return
	}
	if len(st.clients) > 0 || !st.lastEventAt.Equal(lastEventAt) {
		h.scheduleIdleCleanupLocked(key, st)
		return
	}
	delete(h.m, key)
}

func (h *Hub) deleteKeyIfState(key string, state *streamState, runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.m[key]
	if st == nil || st != state || st.activeRunID != runID {
		return
	}
	delete(h.m, key)
}
