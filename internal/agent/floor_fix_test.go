package agent

import "testing"

// TestFloorFixKillsModelManufacturableCap pins the B-1a floor fix (Jerry-Xin
// 5338024667 §3 / mochashanyao 5338040645 P1 / yujiawei 5337380127 P1): the
// effective per-chunk message count is floored at ceil(len(msgMaps)/maxChunkCalls)
// BEFORE splitting, so a model-chosen chunk_size=1 can no longer manufacture a
// cap drop at ordinary input sizes. 257 messages @ chunk_size=1 must yield
// <= maxChunkCalls chunks with ZERO messages dropped by the cap.
func TestFloorFixKillsModelManufacturableCap(t *testing.T) {
	msgs := makeMsgMaps(257)
	processed, dropped, chunks, capped := ProbeChunkCoverage(msgs, 1, 1<<30, 1, 1)
	if capped != 0 {
		t.Fatalf("cap fired at 257 msgs / chunk_size=1: capped=%d dropped=%d chunks=%d — floor fix missing", capped, dropped, chunks)
	}
	if chunks > maxChunkCalls {
		t.Fatalf("chunks=%d exceeds maxChunkCalls=%d", chunks, maxChunkCalls)
	}
	if processed != 257 {
		t.Fatalf("processed=%d, want 257 (no message lost)", processed)
	}
}
