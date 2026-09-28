package eval

import (
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

// TestCoverageGateExemptsDisclosedCap drives the gate over an input large enough
// to trip the 256-chunk fan-out cap and pins that a DISCLOSED cap is not counted
// as silent loss (#256 r8 N4). At the default ~200-msg chunk size, 52000 messages
// split into 260 chunks; the cap keeps 256 and drops the oldest 4 (800 messages).
// dropped is therefore non-zero, so a `NoSilentLoss: dropped == 0` reversion would
// wrongly fail here — the subtraction of the intentional, cap-fired count is what
// keeps the gate honest.
func TestCoverageGateExemptsDisclosedCap(t *testing.T) {
	const n = 52000
	cov := Coverage(GoldenCase{Messages: make([]GoldenMessage, n)})
	if cov.Chunks != 256 {
		t.Fatalf("chunks = %d, want 256 (fan-out cap must bind)", cov.Chunks)
	}
	if cov.TotalDroppedCount == 0 || cov.CappedDroppedCount == 0 {
		t.Fatalf("want a real cap drop, got dropped=%d capped=%d", cov.TotalDroppedCount, cov.CappedDroppedCount)
	}
	// All loss here is the intentional cap, so the two counts must match exactly —
	// no splitter/formatter loss leaked in.
	if cov.TotalDroppedCount != cov.CappedDroppedCount {
		t.Fatalf("dropped=%d capped=%d — the whole drop should be the disclosed cap", cov.TotalDroppedCount, cov.CappedDroppedCount)
	}
	if !cov.NoSilentLoss {
		t.Fatalf("a disclosed fan-out cap must NOT fail the no-silent-loss gate; dropped=%d capped=%d", cov.TotalDroppedCount, cov.CappedDroppedCount)
	}
}
