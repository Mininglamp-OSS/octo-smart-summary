package eval

import (
	"strings"
	"testing"
)

const goldenDir = "golden"

// TestGoldenSet is the SS-02 entry point: `go test ./internal/agent/eval/`
// replays every golden case and asserts the deterministic Stage-1 thresholds.
// It prints each report so -v gives a reproducible metrics dump with no LLM.
func TestGoldenSet(t *testing.T) {
	cases, err := LoadGoldenCases(goldenDir)
	if err != nil {
		t.Fatalf("load golden cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no golden cases found")
	}

	for _, gc := range cases {
		gc := gc
		t.Run(gc.Name, func(t *testing.T) {
			rep := Evaluate(gc)
			t.Log("\n" + rep.String())

			// Sanity: the fixture's declared count matches its messages.
			if gc.Expected.MessageCount != len(gc.Messages) {
				t.Fatalf("fixture inconsistent: expected.message_count=%d but %d messages",
					gc.Expected.MessageCount, len(gc.Messages))
			}

			// Coverage: the P0 invariant — no snapshot message is silently lost.
			if !rep.Coverage.NoSilentLoss {
				t.Errorf("silent loss: input=%d processed=%d dropped=%d",
					rep.Coverage.InputCount, rep.Coverage.ProcessedCount, rep.Coverage.TotalDroppedCount)
			}
			if rep.Coverage.ProcessedCount != gc.Expected.MessageCount {
				t.Errorf("processed=%d, want %d", rep.Coverage.ProcessedCount, gc.Expected.MessageCount)
			}

			// Citation existence: every marker must resolve into the pool.
			if !rep.Citation.AllResolvable {
				t.Errorf("unresolvable citations: out_of_range=%v (pool=1..%d)",
					rep.Citation.OutOfRange, rep.Citation.MaxIndex)
			}

			// Format adherence: required sections present, language matches.
			if !rep.Format.Adherent {
				t.Errorf("format not adherent: missing=%v language_ok=%t",
					rep.Format.MissingSections, rep.Format.LanguageOK)
			}
		})
	}
}

// TestCitationValidatorCatchesOutOfRange guards the validator itself: a marker
// beyond the pool must be reported, not silently accepted. This is the offline
// analogue of the save-time citation drop the harness exists to prevent.
func TestCitationValidatorCatchesOutOfRange(t *testing.T) {
	m := Citations("valid [1] and bogus [9]", 3)
	if m.AllResolvable {
		t.Fatal("expected out-of-range citation to fail resolution")
	}
	if len(m.OutOfRange) != 1 || m.OutOfRange[0] != 9 {
		t.Fatalf("out_of_range = %v, want [9]", m.OutOfRange)
	}
	if m.Valid != 1 || m.Total != 2 {
		t.Fatalf("valid=%d total=%d, want 1 and 2", m.Valid, m.Total)
	}
}

// TestCoverageRegressionGuard fails loudly if the chunking defaults ever regress
// to a silent-drop cap. Post-PR-#196-fix-forward it drives the production
// chain (clamp -> token splitter -> formatter) through Coverage, so the
// assertion below is reachable: reintroducing a 200-style format cap makes
// dropped go non-zero and turns this test red (P1-2: the guard was previously
// arithmetic-only and could never fail).
func TestCoverageRegressionGuard(t *testing.T) {
	// 500 input at default chunk_size must feed all 500 to the model.
	fake := GoldenCase{Messages: make([]GoldenMessage, 500)}
	cov := Coverage(fake)
	if cov.TotalDroppedCount != 0 || cov.ProcessedCount != 500 {
		t.Fatalf("coverage regressed: processed=%d dropped=%d (chunk defaults likely back to 500)",
			cov.ProcessedCount, cov.TotalDroppedCount)
	}
}

// TestCoverageFloorPreservesSmallMessages pins the coverage gate over a huge default
// input. Post-floor-fix (B-1a, Jerry-Xin 5338024667 §3): floorMsgsPerChunk
// raises the per-chunk count to ceil(n/256) whenever the model-chosen size would
// fan out past the cap, so the MODEL-MANUFACTURABLE route (chunk_size=1 at
// ordinary sizes) is closed — 52000 small messages split into ~255 chunks with
// zero drop. (Round-5 P2 correction, yujiawei 5478968340: an earlier version of
// this comment claimed the cap "can no longer fire at ANY input size" — false.
// The residual route stays real: messages large enough that even floor-sized
// chunks bust the token budget force >256 chunks at ordinary sizes, e.g. 100k
// × 300-rune CJK messages drop ~18% to the disclosed cap. That route is pinned
// by TestProbeChunkCoverageDefaultReportsFiringCap; the fixtures here pin the
// floor's small-message invariant.)
func TestCoverageFloorPreservesSmallMessages(t *testing.T) {
	const n = 52000
	cov := Coverage(GoldenCase{Messages: make([]GoldenMessage, n)})
	if cov.Chunks > 256 {
		t.Fatalf("chunks = %d exceeds the fan-out cap", cov.Chunks)
	}
	if cov.TotalDroppedCount != 0 || cov.CappedDroppedCount != 0 {
		t.Fatalf("default config must not drop anything post-floor-fix, got dropped=%d capped=%d", cov.TotalDroppedCount, cov.CappedDroppedCount)
	}
	if cov.ProcessedCount != n {
		t.Fatalf("processed=%d, want %d", cov.ProcessedCount, n)
	}
	if !cov.NoSilentLoss {
		t.Fatal("no-silent-loss gate must hold on a clean huge input")
	}
}

// Exercise Coverage itself, including forwarding the probe's cap counters.
func TestCoverageGateExemptsDisclosedCap(t *testing.T) {
	messages := make([]GoldenMessage, 300)
	content := strings.Repeat("x", 240000)
	for i := range messages {
		messages[i].Content = content
	}
	cov := Coverage(GoldenCase{Messages: messages})
	if cov.InputCount != 300 || cov.ProcessedCount != 256 || cov.Chunks != 256 || cov.TotalDroppedCount != 44 || cov.CappedDroppedCount != 44 || !cov.NoSilentLoss {
		t.Fatalf("cap coverage=%+v, want 256 processed + 44 disclosed drops and no silent loss", cov)
	}
}

// A disclosed cap cannot excuse additional accidental loss. Current splitters
// are lossless; inject counters at the metric boundary to pin the negative case.
func TestCoverageMetricRejectsSilentLoss(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		processed, dropped, capped int
		want                       bool
	}{
		{"clean", 300, 0, 0, true},
		{"cap only", 256, 44, 44, true},
		{"accidental only", 299, 1, 0, false},
		{"cap plus accidental", 255, 45, 44, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cov := coverageMetricFromCounts(300, tc.processed, tc.dropped, 256, tc.capped)
			if cov.NoSilentLoss != tc.want {
				t.Fatalf("coverage=%+v, want no_silent_loss=%t", cov, tc.want)
			}
		})
	}
}
