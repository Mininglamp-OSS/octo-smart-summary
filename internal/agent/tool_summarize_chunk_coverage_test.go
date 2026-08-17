package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// SS-01 regression tests: prove the "chunk_size 500 / format 200" double cap no
// longer silently drops messages, and that coverage counts are honest. These are
// deterministic (no LLM) — they exercise the pure formatting/chunking path only.

func TestClampChunkSize(t *testing.T) {
	cases := []struct {
		name     string
		in, want int
	}{
		{"zero -> default", 0, defaultChunkSize},
		{"negative -> default", -1, defaultChunkSize},
		{"over max -> clamped", 500, maxChunkSize},
		{"just over max -> clamped", maxChunkSize + 1, maxChunkSize},
		{"at max -> kept", maxChunkSize, maxChunkSize},
		{"below max -> kept", 50, 50},
		{"one -> kept", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampChunkSize(c.in); got != c.want {
				t.Fatalf("clampChunkSize(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// makeMsgMaps builds n msgMap entries shaped exactly like the handler produces,
// with a dense global citation_index starting at 1.
func makeMsgMaps(n int) []map[string]interface{} {
	msgs := make([]map[string]interface{}, n)
	for i := 0; i < n; i++ {
		msgs[i] = map[string]interface{}{
			"sender_name":    fmt.Sprintf("user%d", i),
			"content":        fmt.Sprintf("message body %d", i),
			"citation_index": i + 1,
		}
	}
	return msgs
}

func TestFormatChunkForLLM_NoSilentDropAt200(t *testing.T) {
	chunk := makeMsgMaps(maxChunkSize) // exactly a full chunk
	formatted, processed, oversized := formatChunkForLLM(chunk)

	if processed != maxChunkSize {
		t.Fatalf("processed = %d, want %d (a full 200-chunk must not drop)", processed, maxChunkSize)
	}
	if oversized != 0 {
		t.Fatalf("oversized = %d, want 0", oversized)
	}
	// Every citation marker [1]..[200] must be present.
	for i := 1; i <= maxChunkSize; i++ {
		if !strings.Contains(formatted, fmt.Sprintf("[%d] ", i)) {
			t.Fatalf("formatted output missing citation marker [%d]", i)
		}
	}
}

func TestFormatChunkForLLM_OversizedCountedNotTruncated(t *testing.T) {
	long := strings.Repeat("x", oversizedMessageRunes+1)
	chunk := []map[string]interface{}{
		{"sender_name": "a", "content": "short", "citation_index": 1},
		{"sender_name": "b", "content": long, "citation_index": 2},
	}
	formatted, processed, oversized := formatChunkForLLM(chunk)

	if processed != 2 {
		t.Fatalf("processed = %d, want 2", processed)
	}
	if oversized != 1 {
		t.Fatalf("oversized = %d, want 1", oversized)
	}
	// The oversized message must NOT be truncated — its full content survives.
	if !strings.Contains(formatted, long) {
		t.Fatal("oversized message content was truncated; SS-01 requires no silent truncation")
	}
}

// aggregateCoverage mirrors the handler's per-chunk aggregation loop, but skips
// the LLM call so the funnel is deterministic. Kept in the test to assert the
// end-to-end "no messages lost across chunk boundaries" invariant.
func aggregateCoverage(t *testing.T, inputCount, requestedChunkSize int) chunkCoverage {
	t.Helper()
	msgMaps := makeMsgMaps(inputCount)
	size := clampChunkSize(requestedChunkSize)
	chunks := service.SplitIntoChunks(msgMaps, size)

	cov := chunkCoverage{InputCount: inputCount, ChunkSize: size}
	for _, chunk := range chunks {
		_, processed, oversized := formatChunkForLLM(chunk)
		cov.ProcessedCount += processed
		cov.OversizedMessageCount += oversized
	}
	cov.DroppedCount = cov.InputCount - cov.ProcessedCount
	cov.Truncated = cov.DroppedCount > 0
	return cov
}

func TestCoverage_NoSilentLoss(t *testing.T) {
	// The historical bug: 500 input, default chunk_size 500 -> 3 chunks were NOT
	// produced; a single 500-chunk was formatted to only its first 200, losing
	// 300 (60%). With SS-01, default clamps to 200 -> chunks [200,200,100],
	// every message processed.
	cases := []struct {
		name       string
		input      int
		requested  int
		wantChunks int
	}{
		{"201 default", 201, 0, 2},
		{"500 default", 500, 0, 3},
		{"500 with oversized request clamped", 500, 500, 3},
		{"below-limit request honored", 150, 50, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cov := aggregateCoverage(t, c.input, c.requested)
			if cov.ProcessedCount != c.input {
				t.Fatalf("processed = %d, want %d (silent message loss)", cov.ProcessedCount, c.input)
			}
			if cov.DroppedCount != 0 {
				t.Fatalf("dropped = %d, want 0", cov.DroppedCount)
			}
			if cov.Truncated {
				t.Fatal("truncated = true, want false when nothing dropped")
			}
			msgMaps := makeMsgMaps(c.input)
			if got := len(service.SplitIntoChunks(msgMaps, cov.ChunkSize)); got != c.wantChunks {
				t.Fatalf("chunk count = %d, want %d", got, c.wantChunks)
			}
		})
	}
}
