package streaming

import (
	"testing"
	"time"
)

// The worker streams model output live, but the persisted body is the model
// output AFTER the citation pipeline (dedup, orphan strip, per-claim cap). A
// live client therefore renders text the worker is about to rewrite.
//
// The reconciliation contract is: the worker publishes the final body as an
// EventSnapshot before EventDone, the hub REPLACES its delta-accumulated
// snapshot with it, and the Done frame carries that reconciled body. This
// test pins that contract at the hub, because it is what makes the fix
// require no wire-protocol change and no octo-web change.
func TestSnapshotBeforeDoneReconcilesTheDeltaAccumulatedBody(t *testing.T) {
	h := NewHub(time.Minute)
	ch, _, _, cancel := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel()

	// What the model streamed: over-cited, pre-pipeline.
	streamed := []string{"结论一：范围已确认[1][2][3]\n", "结论二：负责人已定[1][2][3][10][11][12]"}
	for _, d := range streamed {
		h.Publish(Event{Type: EventDelta, TaskID: 1, RunID: "r", Scope: ScopePersonal, TargetUserID: "u1", Delta: d})
	}

	// What is actually persisted, after the citation pipeline + cap.
	const persisted = "结论一：范围已确认[1][2][3]\n结论二：负责人已定[10][11][12]"

	h.Publish(Event{Type: EventSnapshot, TaskID: 1, RunID: "r", Scope: ScopePersonal, TargetUserID: "u1", Content: persisted})
	h.Publish(Event{Type: EventDone, TaskID: 1, RunID: "r", Scope: ScopePersonal, TargetUserID: "u1", Status: 2})

	var last Event
	var sawSnapshot bool
	for {
		select {
		case ev := <-ch:
			if ev.Type == EventSnapshot {
				sawSnapshot = true
				if ev.Content != persisted {
					t.Fatalf("snapshot forwarded %q, want the persisted body %q", ev.Content, persisted)
				}
			}
			last = ev
			if ev.Type == EventDone {
				goto check
			}
		default:
			t.Fatal("stream ended without a done frame")
		}
	}

check:
	if !sawSnapshot {
		t.Error("no snapshot frame was delivered; live clients never learn the body changed")
	}
	if last.Content != persisted {
		t.Errorf("done frame carries %q, want the persisted body %q\n"+
			"A late subscriber and a refresh would disagree with the live view.",
			last.Content, persisted)
	}

	// A client that connects AFTER completion must also get the persisted
	// body, not the raw streamed text.
	_, snap, done, cancel2 := h.Subscribe(1, ScopePersonal, "u1")
	defer cancel2()
	if !done {
		t.Fatal("late subscriber did not see the stream as done")
	}
	if snap != persisted {
		t.Errorf("late subscriber replay = %q, want %q", snap, persisted)
	}
}

// Mutation evidence: WITHOUT the snapshot frame, the terminal body is the raw
// streamed text and disagrees with what is persisted. This is the divergence
// the fix removes.
func TestMutationNoSnapshotLeavesTheStreamDivergedFromThePersistedBody(t *testing.T) {
	h := NewHub(time.Minute)
	ch, _, _, cancel := h.Subscribe(2, ScopePersonal, "u1")
	defer cancel()

	const streamed = "结论二：负责人已定[1][2][3][10][11][12]"
	const persisted = "结论二：负责人已定[10][11][12]"

	h.Publish(Event{Type: EventDelta, TaskID: 2, RunID: "r", Scope: ScopePersonal, TargetUserID: "u1", Delta: streamed})
	// No EventSnapshot — the pre-fix behaviour.
	h.Publish(Event{Type: EventDone, TaskID: 2, RunID: "r", Scope: ScopePersonal, TargetUserID: "u1", Status: 2})

	var last Event
	for i := 0; i < 8; i++ {
		select {
		case ev := <-ch:
			last = ev
		default:
		}
		if last.Type == EventDone {
			break
		}
	}
	if last.Type != EventDone {
		t.Fatal("no done frame")
	}
	if last.Content == persisted {
		t.Fatal("MUTATION CHECK FAILED: the stream matched the persisted body with no " +
			"snapshot frame, so the snapshot is not what fixes the divergence")
	}
	t.Logf("MUTATION EVIDENCE: without a snapshot frame the client's terminal body is %q "+
		"but the persisted body is %q — markers [1][2][3] are rendered with no backing "+
		"Citation row", last.Content, persisted)
}
