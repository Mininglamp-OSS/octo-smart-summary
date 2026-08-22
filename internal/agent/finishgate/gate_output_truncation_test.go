package finishgate

import "testing"

// Output truncation is the model's OWN completion hitting its token ceiling —
// the summary text stops mid-sentence. It is a different fact from Truncated,
// which means the fetched message pool was clipped before the model ever saw it.
//
// These tests pin the separation. The reason it matters is remediation: a pool
// truncation is fixed by narrowing the time range or raising the per-channel
// cap, an output truncation by lowering the detail level. Applying the first
// remedy to the second problem makes it strictly worse, so a single boolean
// with one detail string would necessarily give wrong advice for one of them.

func TestEvaluateOutputTruncatedIsPartial(t *testing.T) {
	s := completeState()
	s.OutputTruncated = true

	v, gaps := Evaluate(s)
	if v != Partial {
		t.Fatalf("verdict = %s, want PARTIAL", v)
	}
	if !hasKind(gaps, GapOutputTruncation) {
		t.Fatalf("gaps = %v, want a %s gap", gaps, GapOutputTruncation)
	}
	// The pool was never truncated here; claiming it was would send the user to
	// the wrong remedy.
	if hasKind(gaps, GapTruncation) {
		t.Fatalf("output truncation must not report a fetch-pool gap: %v", gaps)
	}
}

// The two truncations are independent and can co-occur: a run may both clip its
// input pool AND run out of output budget. A single shared boolean could only
// ever report one of them; both must survive.
func TestEvaluateBothTruncationsReportedSeparately(t *testing.T) {
	s := completeState()
	s.Truncated = true
	s.OutputTruncated = true

	v, gaps := Evaluate(s)
	if v != Partial {
		t.Fatalf("verdict = %s, want PARTIAL", v)
	}
	if !hasKind(gaps, GapTruncation) || !hasKind(gaps, GapOutputTruncation) {
		t.Fatalf("both truncation kinds must be disclosed, got %v", gaps)
	}

	// And their details must differ — the whole point of the split is that the
	// user is told which failure they hit.
	var poolDetail, outputDetail string
	for _, g := range gaps {
		switch g.Kind {
		case GapTruncation:
			poolDetail = g.Detail
		case GapOutputTruncation:
			outputDetail = g.Detail
		}
	}
	if poolDetail == outputDetail {
		t.Fatalf("the two truncation gaps share detail %q; the split buys nothing", poolDetail)
	}
}

// NO FALSE POSITIVES. gate.go's comments document a prior bug where one half of
// a change reported the other half's correct behaviour as a defect. A run with
// no truncation of either kind must stay COMPLETE — adding a disclosure path
// must not make PARTIAL the standing verdict.
func TestEvaluateNoOutputTruncationStaysComplete(t *testing.T) {
	s := completeState()
	s.OutputTruncated = false

	v, gaps := Evaluate(s)
	if v != Complete {
		t.Fatalf("verdict = %s, want COMPLETE (gaps: %v)", v, gaps)
	}
	if len(gaps) != 0 {
		t.Fatalf("clean run must disclose nothing, got %v", gaps)
	}
}

// An output truncation is a property of the GENERATION, not of the fetch, so it
// must be disclosed even on a turn that legitimately fetched nothing. A
// confident rewrite (SS-08b) has its fetch tools physically removed —
// FetchExpected=false, CoverageMeasured=false — and can still overrun its token
// ceiling. Gating the output check behind CoverageMeasured would silence it on
// exactly the turns that produce the longest prose.
func TestEvaluateOutputTruncatedDisclosedOnFetchFreeTurn(t *testing.T) {
	s := completeState()
	s.FetchExpected = false
	s.CoverageMeasured = false
	s.ExpectedChannels = nil
	s.AttemptedChannels = nil
	s.SucceededChannels = nil

	// Baseline: without the truncation this shape is COMPLETE by design.
	if v, gaps := Evaluate(s); v != Complete {
		t.Fatalf("baseline rewrite verdict = %s, want COMPLETE (gaps: %v)", v, gaps)
	}

	s.OutputTruncated = true
	v, gaps := Evaluate(s)
	if v != Partial {
		t.Fatalf("verdict = %s, want PARTIAL", v)
	}
	if !hasKind(gaps, GapOutputTruncation) {
		t.Fatalf("gaps = %v, want a %s gap", gaps, GapOutputTruncation)
	}
}

func hasKind(gaps []Gap, kind string) bool {
	for _, g := range gaps {
		if g.Kind == kind {
			return true
		}
	}
	return false
}
