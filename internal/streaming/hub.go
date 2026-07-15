package streaming

import (
	"fmt"
	"sync"
	"time"
)

const defaultClientBuffer = 128

const defaultIdleTTLMul = 10

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
		h.mu.Lock()
		if cur := h.m[key]; cur != nil {
			delete(cur.clients, ch)
			if len(cur.clients) == 0 && !cur.done {
				h.scheduleIdleCleanupLocked(key, cur)
			}
		}
		h.mu.Unlock()
		close(ch)
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
		st.snapshot += ev.Delta
		ev.Content = st.snapshot
	case EventSnapshot:
		if ev.Content != "" {
			st.snapshot = ev.Content
		}
	case EventDone, EventError:
		st.done = true
		ev.Content = st.snapshot
		if st.cleanup != nil {
			st.cleanup.Stop()
		}
		st.cleanup = time.AfterFunc(h.ttl, func() { h.deleteKey(key) })
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
		select {
		case ch <- ev:
		default:
			// Slow SSE clients should not block worker token ingestion.
		}
	}
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
	st.cleanup = time.AfterFunc(h.idleTTL, func() { h.deleteKeyIfIdle(key, lastEventAt) })
}

func (h *Hub) deleteKeyIfIdle(key string, lastEventAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.m[key]
	if st == nil {
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

func (h *Hub) deleteKey(key string) {
	h.mu.Lock()
	delete(h.m, key)
	h.mu.Unlock()
}
