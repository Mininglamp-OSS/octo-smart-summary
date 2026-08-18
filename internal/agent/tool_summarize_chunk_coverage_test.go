package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
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
		{"way over backstop -> clamped", 5000, hardMessageBackstop},
		{"over backstop -> clamped", hardMessageBackstop + 1, hardMessageBackstop},
		{"at backstop -> kept", hardMessageBackstop, hardMessageBackstop},
		{"below backstop -> kept", 50, 50},
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

func TestFormatChunkForLLM_NoSilentDropAtFullChunk(t *testing.T) {
	chunk := makeMsgMaps(defaultChunkSize) // exactly a full default chunk
	formatted, processed, oversized := formatChunkForLLM(chunk)

	if processed != defaultChunkSize {
		t.Fatalf("processed = %d, want %d (a full default chunk must not drop)", processed, defaultChunkSize)
	}
	if oversized != 0 {
		t.Fatalf("oversized = %d, want 0", oversized)
	}
	// Every citation marker [1]..[defaultChunkSize] must be present.
	for i := 1; i <= defaultChunkSize; i++ {
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

// aggregateCoverage drives the PRODUCTION chunking chain for the funnel:
// clampChunkSize -> splitMsgMapsByTokenBudget -> formatChunkForLLM, exactly as
// the handler does (default config, no LLM call). PR #196 review P1-3: the
// previous version of this helper exercised service.SplitIntoChunks — a
// count-based splitter with zero production callers — so it validated a dead
// path. The SS-02 no-silent-loss guarantee must hold for the splitter the
// handler actually runs.
func aggregateCoverage(t *testing.T, inputCount, requestedChunkSize int) chunkCoverage {
	t.Helper()
	msgMaps := makeMsgMaps(inputCount)
	size := clampChunkSize(requestedChunkSize)
	processed, dropped, _ := ProbeChunkCoverageDefault(msgMaps, requestedChunkSize)

	cov := chunkCoverage{InputCount: inputCount, ChunkSize: size}
	cov.ProcessedCount = processed
	cov.DroppedCount = dropped
	cov.Truncated = dropped > 0
	return cov
}

func TestCoverage_NoSilentLoss(t *testing.T) {
	// The historical bug: 500 input, default chunk_size 500 -> 3 chunks were NOT
	// produced; a single 500-chunk was formatted to only its first 200, losing
	// 300 (60%). SS-01 clamped the default to 200; SS-06b then switched to
	// token-budget chunking. This now drives the production token splitter
	// (PR #196 review P1-3) and asserts that the funnel stays honest under the
	// default budget regardless of the requested chunk_size.
	cases := []struct {
		name      string
		input     int
		requested int
	}{
		{"201 default", 201, 0},
		{"500 default", 500, 0},
		{"over-backstop request clamped", 500, 5000}, // clamp -> 500 -> zero loss
		{"below-limit request honored", 150, 50},
		{"5000 requested", 1200, 5000}, // backstop must cap, not lose
		{"large default", 12345, 0},
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
			// And every chunk the production splitter emitted respects the
			// hard backstop — the bound the handler depends on.
			msgMaps := makeMsgMaps(c.input)
			cfg := config.Config{}
			for i, chunk := range splitMsgMapsByTokenBudget(msgMaps,
				chunkTokenBudget(cfg), cov.ChunkSize,
				cfg.ResolveCharsPerTokenCJK(), cfg.CharsPerTokenASCII) {
				if len(chunk) > hardMessageBackstop {
					t.Fatalf("chunk %d has %d messages > backstop %d", i, len(chunk), hardMessageBackstop)
				}
			}
		})
	}
}
