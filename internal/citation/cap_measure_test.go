package citation

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// syntheticPathologicalBody reconstructs the shape measured on one real
// production run, so the PR's central claim is a number rather than an
// assertion:
//
//	1227 [n] markers in the body, 794 distinct
//	longest unbroken marker run: 1026 characters
//
// The generator is seeded and deterministic. It is NOT a captured production
// body — no user content is committed — but it reproduces the two properties
// that drive the cost: total marker volume and the long unbroken runs.
//
// LIMITATION, stated because it hid a bug for a review round: repeats are
// inserted NEAR their original (`at := src + 1 + rng.Intn(6)`), so they land
// inside the same claim's run and two claims almost never share a source.
// This corpus therefore cannot exhibit CROSS-CLAIM SOURCE REUSE — one
// decisive message supporting several conclusions — which is what a real
// summary looks like and what is required to surface the interaction between
// the cap and the whole-document dedup. That case is measured against the
// real three-stage pipeline in worker.TestMeasuredUncitedClaims, which needs
// buildCitations/dedupCitations and so cannot live in this package.
func syntheticPathologicalBody() string {
	const (
		totalMarkers  = 1227
		distinct      = 794
		longestRunLen = 1026 // characters
	)
	rng := rand.New(rand.NewSource(1227794))

	// Build the exact measured multiset: 794 distinct ordinals, each present
	// at least once, plus 1227-794 = 433 repeats. Each repeat is inserted
	// NEAR its original rather than at a uniform random position, because
	// that is how repeats actually arise — a model listing sources for one
	// claim re-lists the same message inside that claim's run. Scattering the
	// repeats uniformly would place nearly all of them in different runs and
	// understate dedup; clustering them all would overstate it. This models
	// the middle and the resulting dedup number is reported, not asserted as
	// a target.
	markers := make([]int, 0, totalMarkers)
	for i := 1; i <= distinct; i++ {
		markers = append(markers, i)
	}
	rng.Shuffle(len(markers), func(i, j int) { markers[i], markers[j] = markers[j], markers[i] })
	for len(markers) < totalMarkers {
		src := rng.Intn(len(markers))
		at := src + 1 + rng.Intn(6) // within the same claim's run, typically
		if at > len(markers) {
			at = len(markers)
		}
		markers = append(markers, 0)
		copy(markers[at+1:], markers[at:])
		markers[at] = markers[src]
	}

	var b strings.Builder
	b.WriteString("# 项目周会总结\n\n## 关键结论\n\n")
	next := 0
	emit := func() {
		fmt.Fprintf(&b, "[%d]", markers[next])
		next++
	}

	// One catastrophic wall: a single claim whose marker run reaches the
	// measured 1026-character peak.
	b.WriteString("- 团队一致同意推进二期方案并在下周冻结接口")
	wallStart := b.Len()
	for b.Len()-wallStart < longestRunLen && next < len(markers) {
		emit()
	}
	b.WriteString("\n")

	// The rest of the body: ordinary-looking claims that each over-cite by a
	// realistic amount (2..16 markers), which is how the remaining markers
	// accumulate.
	claim := 0
	for next < len(markers) {
		claim++
		n := 2 + rng.Intn(15)
		if next+n > len(markers) {
			n = len(markers) - next
		}
		fmt.Fprintf(&b, "- 讨论要点%d：明确了范围与负责人", claim)
		for i := 0; i < n; i++ {
			emit()
		}
		b.WriteString("\n")
		if claim%12 == 0 {
			b.WriteString("\n## 待办\n\n")
		}
	}
	return b.String()
}

// TestMeasuredReduction is the PR's quantified before/after. It separates the
// lossless dedup contribution from the lossy cap contribution, because those
// are two different arguments: dedup is free, the cap is a tradeoff.
//
// It prints a table (visible with -v) and asserts only the properties that
// must hold for the change to be worth shipping, so it does not turn into a
// brittle golden-number test.
func TestMeasuredReduction(t *testing.T) {
	body := syntheticPathologicalBody()

	_, base := CapRuns(body, Disabled)
	t.Logf("BEFORE: markers=%d distinct=%d runs=%d longest_run=%d marks / %d chars bytes=%d",
		base.MarkersBefore, len(Numbers(body)), base.Runs,
		base.LongestRunBefore, base.LongestRunCharsBefore, base.BytesBefore)

	// Dedup-only isolation: a cap larger than the longest run can never
	// trigger the cap branch, so everything it removes is duplicates.
	dedupOnlyText, dedupOnly := CapRuns(body, base.LongestRunBefore)
	if dedupOnly.RemovedByCap != 0 {
		t.Fatalf("dedup-only probe still capped %d markers; probe is not isolating dedup", dedupOnly.RemovedByCap)
	}
	t.Logf("DEDUP ONLY (cap=%d, i.e. never binding): markers %d -> %d (-%d, %.1f%%) bytes %d -> %d (-%d, %.1f%%)",
		base.LongestRunBefore,
		dedupOnly.MarkersBefore, dedupOnly.MarkersAfter, dedupOnly.RemovedByDedup,
		pctOf(dedupOnly.RemovedByDedup, dedupOnly.MarkersBefore),
		dedupOnly.BytesBefore, dedupOnly.BytesAfter, dedupOnly.BytesBefore-dedupOnly.BytesAfter,
		pctOf(dedupOnly.BytesBefore-dedupOnly.BytesAfter, dedupOnly.BytesBefore))

	baseUncited := uncitedClaims(body)
	t.Logf("BEFORE: uncited claims = %d", baseUncited)

	for _, max := range []int{5, 3, 2, 1} {
		out, st := CapRuns(body, max)
		t.Logf("CAP=%d: markers %d -> %d (-%d total: dedup -%d, cap -%d) | bytes %d -> %d (-%d, %.1f%%) | longest run %d -> %d marks (%d -> %d chars) | capped_runs %d/%d | uncited_claims %d -> %d",
			max,
			st.MarkersBefore, st.MarkersAfter, st.MarkersBefore-st.MarkersAfter,
			st.RemovedByDedup, st.RemovedByCap,
			st.BytesBefore, st.BytesAfter, st.BytesBefore-st.BytesAfter,
			pctOf(st.BytesBefore-st.BytesAfter, st.BytesBefore),
			st.LongestRunBefore, st.LongestRunAfter,
			st.LongestRunCharsBefore, st.LongestRunCharsAfter,
			st.CappedRuns, st.Runs,
			baseUncited, uncitedClaims(out))

		// Citation COVERAGE is a quality metric the cap can regress, it is
		// cheap to compute, and it belongs next to the byte counts. Bytes
		// bought at the price of an uncited claim are not a win.
		if got := uncitedClaims(out); got > baseUncited {
			t.Errorf("cap=%d left %d uncited claims, up from %d: the cap destroyed "+
				"citations that exist without it", max, got, baseUncited)
		}

		// Invariants that must hold at every cap value.
		if runs, _, _ := findRuns(out); true {
			for _, r := range runs {
				if len(r) > max {
					t.Errorf("cap=%d: a run of %d markers survived", max, len(r))
				}
			}
		}
		for i, line := range strings.Split(body, "\n") {
			if !MarkerRe.MatchString(line) {
				continue
			}
			outLines := strings.Split(out, "\n")
			if i >= len(outLines) || !MarkerRe.MatchString(outLines[i]) {
				t.Fatalf("cap=%d: claim line %d lost every citation", max, i)
			}
		}
		if st.MarkersAfter != st.MarkersBefore-st.RemovedByDedup-st.RemovedByCap {
			t.Errorf("cap=%d: stats do not balance: %+v", max, st)
		}
	}

	// The shipped default must actually solve the measured problem.
	_, def := CapRuns(body, 3)
	if def.LongestRunCharsAfter > 40 {
		t.Errorf("cap=3 leaves a %d-char marker run; the 1026-char wall is the thing being fixed", def.LongestRunCharsAfter)
	}
	if def.MarkersAfter >= def.MarkersBefore/2 {
		t.Errorf("cap=3 only reduced markers %d -> %d; expected at least a halving on this shape",
			def.MarkersBefore, def.MarkersAfter)
	}
	if dedupOnly.RemovedByDedup >= def.MarkersBefore-def.MarkersAfter {
		t.Errorf("dedup alone (%d) accounts for the entire reduction (%d) — then the cap is not earning its risk",
			dedupOnly.RemovedByDedup, def.MarkersBefore-def.MarkersAfter)
	}
	_ = dedupOnlyText
}

func pctOf(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}

// TestMutationCapDisabledFailsEnforcement is the mutation evidence: with the
// cap disabled the enforcement assertion FAILS, proving the assertion is
// actually driven by the code under test and not passing vacuously.
//
// Structured as a table over (max, wantViolation) so the failing condition is
// asserted rather than merely observed.
func TestMutationCapDisabledFailsEnforcement(t *testing.T) {
	body := syntheticPathologicalBody()

	check := func(max int) (violations int, longestRunChars int) {
		out, st := CapRuns(body, max)
		runs, _, _ := findRuns(out)
		effective := max
		if effective < 1 {
			effective = 3 // what the enforcement WOULD require
		}
		for _, r := range runs {
			if len(r) > effective {
				violations++
			}
		}
		return violations, st.LongestRunCharsAfter
	}

	// Cap ENABLED at the shipped default: zero runs exceed 3.
	if v, chars := check(3); v != 0 {
		t.Fatalf("cap=3 left %d over-cap runs (longest %d chars) — enforcement is broken", v, chars)
	}

	// Cap DISABLED: the same assertion must fail, and loudly. If this block
	// ever reports zero violations, the test above proves nothing.
	v, chars := check(Disabled)
	if v == 0 {
		t.Fatal("MUTATION CHECK FAILED: with the cap disabled the enforcement assertion still passed, " +
			"so it is not actually testing the cap")
	}
	t.Logf("MUTATION EVIDENCE: cap disabled -> %d runs exceed the 3-marker contract, longest run %d chars "+
		"(with cap=3: 0 runs, %d chars)", v, chars, mustChars(body, 3))
}

func mustChars(body string, max int) int {
	_, st := CapRuns(body, max)
	return st.LongestRunCharsAfter
}

// uncitedClaims counts claim lines that carry no citable marker. A claim line
// is any line the generator emits with a marker run; a line that never had one
// is not a claim and is skipped, so this measures LOSS rather than prose.
func uncitedClaims(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		if len(Numbers(line)) == 0 {
			n++
		}
	}
	return n
}
