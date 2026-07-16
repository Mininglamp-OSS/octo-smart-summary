package streaming

import (
	"testing"
	"time"
)

func readEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for stream event")
	}
	return Event{}
}

func TestHubDeltaCarriesSnapshotAndLateSubscribe(t *testing.T) {
	h := NewHub(time.Second)
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "run-1", Scope: ScopePersonal, TargetUserID: "u1"})

	ch, snapshot, done, cancel := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()
	if snapshot != "" || done {
		t.Fatalf("initial snapshot=%q done=%v, want empty/false", snapshot, done)
	}

	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "run-1", Scope: ScopePersonal, TargetUserID: "u1", Delta: "你"})
	if ev := readEvent(t, ch); ev.Content != "你" || ev.Delta != "你" {
		t.Fatalf("first delta content=%q delta=%q, want snapshot delta", ev.Content, ev.Delta)
	}
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "run-1", Scope: ScopePersonal, TargetUserID: "u1", Delta: "好"})
	if ev := readEvent(t, ch); ev.Content != "你好" || ev.Delta != "好" {
		t.Fatalf("second delta content=%q delta=%q, want accumulated snapshot", ev.Content, ev.Delta)
	}

	_, lateSnapshot, lateDone, lateCancel := h.Subscribe(1, ScopePersonal, "u1")
	defer lateCancel()
	if lateSnapshot != "你好" || lateDone {
		t.Fatalf("late snapshot=%q done=%v, want accumulated/false", lateSnapshot, lateDone)
	}
}

func TestHubDropsOldRunEvents(t *testing.T) {
	h := NewHub(time.Second)
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "old", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "old", Scope: ScopePersonal, TargetUserID: "u1", Delta: "old"})
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "new", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "old", Scope: ScopePersonal, TargetUserID: "u1", Delta: "-stale"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "new", Scope: ScopePersonal, TargetUserID: "u1", Delta: "new"})

	_, snapshot, done, cancel := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()
	if snapshot != "new" || done {
		t.Fatalf("snapshot=%q done=%v, want new/false", snapshot, done)
	}
}

func TestHubDoneKeepsShortSnapshotThenExpires(t *testing.T) {
	h := NewHub(20 * time.Millisecond)
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1", Delta: "done"})
	h.Publish(Event{Type: EventDone, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1"})

	_, snapshot, done, cancel := h.Subscribe(1, ScopePersonal, "u1")
	cancel()
	if snapshot != "done" || !done {
		t.Fatalf("snapshot=%q done=%v, want done snapshot/true", snapshot, done)
	}

	time.Sleep(60 * time.Millisecond)
	_, snapshot, done, cancel = h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()
	if snapshot != "" || done {
		t.Fatalf("expired snapshot=%q done=%v, want empty/false", snapshot, done)
	}
}

func TestHubUnfinishedIdleCleanup(t *testing.T) {
	h := NewHub(time.Second)
	h.idleTTL = 20 * time.Millisecond
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1", Delta: "leak"})

	time.Sleep(60 * time.Millisecond)
	_, snapshot, done, cancel := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()
	if snapshot != "" || done {
		t.Fatalf("idle-cleaned snapshot=%q done=%v, want empty/false", snapshot, done)
	}
}

func TestHubCancelThenPublishDoesNotPanic(t *testing.T) {
	h := NewHub(time.Second)
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1"})
	_, _, _, cancel := h.Subscribe(1, ScopePersonal, "u1")
	cancel()
	cancel() // idempotent

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Publish after cancel panicked: %v", r)
		}
	}()
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1", Delta: "x"})
}

func TestHubTerminalEventMakesRoomWhenBufferFull(t *testing.T) {
	h := NewHub(time.Second)
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1"})
	ch, _, _, cancel := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()

	for i := 0; i < defaultClientBuffer+10; i++ {
		h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1", Delta: "x"})
	}
	h.Publish(Event{Type: EventDone, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1"})

	seenDone := false
	for i := 0; i < defaultClientBuffer; i++ {
		select {
		case ev := <-ch:
			if ev.Type == EventDone {
				seenDone = true
			}
		default:
		}
	}
	if !seenDone {
		t.Fatal("terminal event was not retained for a full client buffer")
	}
}

func TestHubDoneCleanupDoesNotDeleteNewRun(t *testing.T) {
	h := NewHub(20 * time.Millisecond)
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "old", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "old", Scope: ScopePersonal, TargetUserID: "u1", Delta: "old"})
	h.Publish(Event{Type: EventDone, TaskID: 1, RunID: "old", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "new", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "new", Scope: ScopePersonal, TargetUserID: "u1", Delta: "new"})

	time.Sleep(60 * time.Millisecond)
	_, snapshot, done, cancel := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()
	if snapshot != "new" || done {
		t.Fatalf("new run was deleted by stale cleanup: snapshot=%q done=%v", snapshot, done)
	}
}

func TestHubSnapshotIsCapped(t *testing.T) {
	h := NewHub(time.Second)
	h.Publish(Event{Type: EventStart, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1"})
	h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "run", Scope: ScopePersonal, TargetUserID: "u1", Delta: string(make([]byte, maxSnapshotBytes+1024))})

	_, snapshot, _, cancel := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()
	if len(snapshot) > maxSnapshotBytes {
		t.Fatalf("snapshot len=%d exceeds cap=%d", len(snapshot), maxSnapshotBytes)
	}
}
