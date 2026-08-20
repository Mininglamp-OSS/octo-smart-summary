package finishgate

import "testing"

// a "good" complete run: everything resolved, no gaps.
func completeState() RunState {
	return RunState{
		ScopeResolved:            true,
		HasUsableEvidence:        true,
		SummaryGenerated:         true,
		CitationValidationPassed: true,
		FetchExpected:            true,
		CoverageMeasured:         true,
		ExpectedChannels:         []string{"ch-1", "ch-2"},
		AttemptedChannels:        []string{"ch-1", "ch-2"},
		SucceededChannels:        []string{"ch-1", "ch-2"},
	}
}

func TestEvaluateComplete(t *testing.T) {
	v, gaps := Evaluate(completeState())
	if v != Complete {
		t.Fatalf("verdict = %s, want COMPLETE", v)
	}
	if len(gaps) != 0 {
		t.Fatalf("COMPLETE should have no gaps, got %v", gaps)
	}
}

func TestEvaluateFailed(t *testing.T) {
	cases := map[string]RunState{
		"no evidence": func() RunState { s := completeState(); s.HasUsableEvidence = false; return s }(),
		"no summary":  func() RunState { s := completeState(); s.SummaryGenerated = false; return s }(),
		"bad citations": func() RunState {
			s := completeState()
			s.CitationValidationPassed = false
			return s
		}(),
		"critical tool error": func() RunState {
			s := completeState()
			s.CriticalToolErrors = []string{"PERMISSION_DENIED"}
			return s
		}(),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			v, gaps := Evaluate(s)
			if v != Failed {
				t.Fatalf("verdict = %s, want FAILED", v)
			}
			if len(gaps) == 0 {
				t.Fatal("FAILED must disclose at least one gap")
			}
		})
	}
}

func TestEvaluatePartial(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*RunState)
		gapKind string
	}{
		"truncated":         {func(s *RunState) { s.Truncated = true }, GapTruncation},
		"dropped messages":  {func(s *RunState) { s.DroppedMessages = 5 }, GapDropped},
		"coverage unknown":  {func(s *RunState) { s.CoverageMeasured = false }, GapCoverage},
		"channel shortfall": {func(s *RunState) { s.SucceededChannels = []string{"ch-1"} }, GapChannel},
		"failed channel":    {func(s *RunState) { s.FailedChannels = []string{"FETCH_TIMEOUT"} }, GapChannel},
		"scope unresolved":  {func(s *RunState) { s.ScopeResolved = false }, GapToolError},
		// Open scope: no spec pinned anything, so ExpectedChannels is empty and the
		// only signal of an under-fetch is what the run discovered but never tried.
		"open scope under-fetch": {func(s *RunState) {
			s.ExpectedChannels = nil
			s.AttemptedChannels = []string{"ch-1", "ch-2"}
			s.SucceededChannels = []string{"ch-1", "ch-2"}
			s.DiscoveredChannels = []string{"ch-1", "ch-2", "ch-3", "ch-4"}
		}, GapChannel},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := completeState()
			c.mutate(&s)
			v, gaps := Evaluate(s)
			if v != Partial {
				t.Fatalf("verdict = %s, want PARTIAL", v)
			}
			found := false
			for _, g := range gaps {
				if g.Kind == c.gapKind {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected a %q gap, got %v", c.gapKind, gaps)
			}
		})
	}
}

// The motivating case: a run that dropped 60% of messages must NOT be COMPLETE.
func TestEvaluateDroppedNotComplete(t *testing.T) {
	s := completeState()
	s.DroppedMessages = 300 // the "假平安" scenario
	if v, _ := Evaluate(s); v == Complete {
		t.Fatal("a run that dropped 300 messages must not be COMPLETE")
	}
}

// TestEvaluateFetchFreeTurnIsNotAGap pins the round-2 regression: a turn that was
// never supposed to fetch must not be reported as having a coverage gap.
//
// SS-08b strips the fetch tools from a confident rewrite, so CoverageMeasured
// physically cannot be set. Judging that absence as "coverage was not measured"
// made PARTIAL the standing verdict for every correct rewrite — one half of the
// PR disclosing the other half's intended behaviour as a defect. The same shape
// covers answer-from-history turns and peek-only answers.
func TestEvaluateFetchFreeTurnIsNotAGap(t *testing.T) {
	s := completeState()
	s.FetchExpected = false
	s.CoverageMeasured = false
	s.ExpectedChannels = nil
	s.AttemptedChannels = nil
	s.SucceededChannels = nil

	v, gaps := Evaluate(s)
	if v != Complete {
		t.Fatalf("verdict = %s, want COMPLETE for a turn that was never supposed to fetch (gaps=%v)", v, gaps)
	}
	if len(gaps) != 0 {
		t.Fatalf("a fetch-free turn has nothing to disclose, got %v", gaps)
	}
}

// TestEvaluateUnmeasuredCoverageStillDisclosedWhenExpected is the counterpart:
// the exemption above is scoped to turns that were never allowed to fetch. When
// fetching WAS expected and no coverage was recorded, that is unknown — not zero
// — and must still be disclosed rather than asserted away as COMPLETE.
func TestEvaluateUnmeasuredCoverageStillDisclosedWhenExpected(t *testing.T) {
	s := completeState()
	s.FetchExpected = true
	s.CoverageMeasured = false

	v, gaps := Evaluate(s)
	if v != Partial {
		t.Fatalf("verdict = %s, want PARTIAL when a fetch was expected but nothing was measured", v)
	}
	found := false
	for _, g := range gaps {
		if g.Kind == GapCoverage {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a coverage gap, got %v", gaps)
	}
}

// TestEvaluateOpenScopeFullCoverageIsComplete guards the other direction of the
// open-scope check: fetching everything that was discovered is COMPLETE, so the
// new signal cannot turn into a blanket PARTIAL for open-scope runs.
func TestEvaluateOpenScopeFullCoverageIsComplete(t *testing.T) {
	s := completeState()
	s.ExpectedChannels = nil
	s.AttemptedChannels = []string{"ch-1", "ch-2"}
	s.SucceededChannels = []string{"ch-1", "ch-2"}
	s.DiscoveredChannels = []string{"ch-1", "ch-2"}

	v, gaps := Evaluate(s)
	if v != Complete {
		t.Fatalf("verdict = %s, want COMPLETE when every discovered channel was fetched (gaps=%v)", v, gaps)
	}
}

// TestEvaluateOpenScopeGapNamesTheChannel pins that the disclosure identifies
// WHICH channel was missed — SS-11 ships gaps to clients, and "some channel was
// missed" is not actionable.
func TestEvaluateOpenScopeGapNamesTheChannel(t *testing.T) {
	s := completeState()
	s.ExpectedChannels = nil
	s.AttemptedChannels = []string{"ch-1"}
	s.SucceededChannels = []string{"ch-1"}
	s.DiscoveredChannels = []string{"ch-1", "ch-7"}

	_, gaps := Evaluate(s)
	for _, g := range gaps {
		if g.Kind == GapChannel && g.ChannelID == "ch-7" {
			return
		}
	}
	t.Fatalf("expected a channel gap naming ch-7, got %v", gaps)
}

// TestEvaluateClosedScopeIgnoresDiscoveredChannels pins the round-4 P0-1
// (yujiawei / Jerry-Xin): when a spec pins a scope (ExpectedChannels), the
// discovered union — which can hold the user's whole visible surface — must NOT
// manufacture gaps for channels the run deliberately left out. Before the guard,
// a user who pinned one channel and got a perfect single-channel summary was told
// four other discovered channels "were never fetched", and SS-11 shipped that
// fabricated gap list to the client.
func TestEvaluateClosedScopeIgnoresDiscoveredChannels(t *testing.T) {
	s := completeState()
	s.ExpectedChannels = []string{"ch-1"}
	s.AttemptedChannels = []string{"ch-1"}
	s.SucceededChannels = []string{"ch-1"}
	// The visible surface leaked in via a discovery tool, but scope is closed.
	s.DiscoveredChannels = []string{"ch-1", "vis-2", "vis-3", "vis-4", "vis-5"}

	v, gaps := Evaluate(s)
	if v != Complete {
		t.Fatalf("verdict = %s, want COMPLETE — a pinned closed scope was fully fetched; discovered channels outside it are not gaps (gaps=%v)", v, gaps)
	}
}

// TestEvaluateClosedScopeStillCatchesUnfetchedExpected pins that the guard only
// suppresses the discovered-union path, not the authoritative ExpectedChannels
// check: a pinned channel that was never fetched is still a real gap.
func TestEvaluateClosedScopeStillCatchesUnfetchedExpected(t *testing.T) {
	s := completeState()
	s.ExpectedChannels = []string{"ch-1", "ch-2"}
	s.AttemptedChannels = []string{"ch-1"}
	s.SucceededChannels = []string{"ch-1"}
	s.DiscoveredChannels = []string{"vis-9"} // must not appear as a gap

	v, gaps := Evaluate(s)
	if v != Partial {
		t.Fatalf("verdict = %s, want PARTIAL for an unfetched pinned channel", v)
	}
	var sawCh2, sawVis9 bool
	for _, g := range gaps {
		if g.ChannelID == "ch-2" {
			sawCh2 = true
		}
		if g.ChannelID == "vis-9" {
			sawVis9 = true
		}
	}
	if !sawCh2 {
		t.Fatalf("expected a gap naming the unfetched pinned channel ch-2, got %v", gaps)
	}
	if sawVis9 {
		t.Fatalf("discovered-but-out-of-scope vis-9 must not be reported on a closed scope, got %v", gaps)
	}
}

// TestEvaluateRecordedFailureSurvivesFetchNotExpected pins the round-5 finding:
// FACT BEFORE EXPECTATION.
//
// FetchExpected used to short-circuit the entire coverage audit, so a run with a
// real failure sitting in FailedChannels reported COMPLETE purely because the
// turn "was not supposed to fetch". The reachable shape is a SOFT rewrite:
// route.Fetch=false ⇒ FetchExpected=false, but only a CONFIDENT rewrite strips
// the fetch tools, so a soft rewrite can and does fetch — and that fetch can
// fail. Coverage was measured; the flag then erased it.
func TestEvaluateRecordedFailureSurvivesFetchNotExpected(t *testing.T) {
	s := completeState()
	s.FetchExpected = false // soft rewrite: not expected to fetch...
	s.CoverageMeasured = true
	s.ExpectedChannels = nil
	s.AttemptedChannels = []string{"ch-1"}
	s.SucceededChannels = nil
	s.FailedChannels = []string{"ch-1"} // ...but it did, and it failed

	v, gaps := Evaluate(s)
	if v != Partial {
		t.Fatalf("verdict = %s, want PARTIAL — a recorded failure cannot be erased by fetch_expected=false", v)
	}
	named := false
	for _, g := range gaps {
		if g.Kind == GapChannel && g.ChannelID == "ch-1" {
			named = true
		}
	}
	if !named {
		t.Fatalf("the failed channel must be disclosed by id, got %v", gaps)
	}
}

// TestEvaluateTruncationAndDropSurviveFetchNotExpected covers the other two
// facts on the same path. Truncation and dropped messages are recorded by
// summarize_chunk, which a soft rewrite retains, so they can occur on a turn
// whose FetchExpected is false and must not be silently dropped either.
func TestEvaluateTruncationAndDropSurviveFetchNotExpected(t *testing.T) {
	for name, mutate := range map[string]func(*RunState){
		"truncated": func(s *RunState) { s.Truncated = true },
		"dropped":   func(s *RunState) { s.DroppedMessages = 7 },
	} {
		t.Run(name, func(t *testing.T) {
			s := completeState()
			s.FetchExpected = false
			s.CoverageMeasured = false
			s.ExpectedChannels = nil
			s.AttemptedChannels = nil
			s.SucceededChannels = nil
			mutate(&s)

			if v, gaps := Evaluate(s); v != Partial {
				t.Fatalf("verdict = %s, want PARTIAL — %s is a recorded fact (gaps=%v)", v, name, gaps)
			}
		})
	}
}

// TestEvaluateExpectationOnlyExplainsAbsence states the invariant directly:
// FetchExpected may only ever change the verdict when NOTHING was recorded. Once
// coverage exists, flipping the expectation must make no difference at all.
//
// Written as a property over both values rather than as another example, because
// the two previous rounds each fixed one example of this class and left the class
// open (round 2 added the flag as a veto; round 4 changed where the flag came
// from, which moved the hole rather than closing it).
func TestEvaluateExpectationOnlyExplainsAbsence(t *testing.T) {
	base := func() RunState {
		s := completeState()
		s.CoverageMeasured = true
		s.ExpectedChannels = []string{"ch-1", "ch-2"}
		s.AttemptedChannels = []string{"ch-1", "ch-2"}
		s.SucceededChannels = []string{"ch-1"}
		s.FailedChannels = []string{"ch-2"}
		return s
	}

	expected := base()
	expected.FetchExpected = true
	vExpected, gapsExpected := Evaluate(expected)

	unexpected := base()
	unexpected.FetchExpected = false
	vUnexpected, gapsUnexpected := Evaluate(unexpected)

	if vExpected != vUnexpected {
		t.Fatalf("verdict differs on fetch_expected alone with coverage recorded: %s vs %s", vExpected, vUnexpected)
	}
	if len(gapsExpected) != len(gapsUnexpected) {
		t.Fatalf("gaps differ on fetch_expected alone: %v vs %v", gapsExpected, gapsUnexpected)
	}
}

// TestEvaluateOpportunisticFetchDoesNotCreateScopeObligation pins the round-6
// over-correction — a regression the previous commit introduced.
//
// Fact-before-expectation is right, but it was applied to the WHOLE coverage
// block instead of to the recorded-outcome half. "In scope but never fetched" is
// itself an ABSENCE, so it stays explainable by the expectation exactly like the
// unmeasured case. A soft rewrite KEEPS its fetch tools, so a turn with channels
// still pinned in the UI whose model touched one to check a detail became PARTIAL
// with bogus "was not fetched" gaps shipped to the client — strictly worse than
// the COMPLETE it reported before. One voluntary fetch does not create an
// obligation to cover a scope the turn never owed.
func TestEvaluateOpportunisticFetchDoesNotCreateScopeObligation(t *testing.T) {
	t.Run("closed scope", func(t *testing.T) {
		s := completeState()
		s.FetchExpected = false
		s.CoverageMeasured = true
		s.ExpectedChannels = []string{"c1", "c2", "c3"}
		s.AttemptedChannels = []string{"c1"}
		s.SucceededChannels = []string{"c1"}

		if v, gaps := Evaluate(s); v != Complete {
			t.Fatalf("verdict = %s, want COMPLETE — the turn never owed this scope (gaps=%v)", v, gaps)
		}
	})

	t.Run("open scope", func(t *testing.T) {
		s := completeState()
		s.FetchExpected = false
		s.CoverageMeasured = true
		s.ExpectedChannels = nil
		s.DiscoveredChannels = []string{"d1", "d2", "d3"}
		s.AttemptedChannels = []string{"d1"}
		s.SucceededChannels = []string{"d1"}

		if v, gaps := Evaluate(s); v != Complete {
			t.Fatalf("verdict = %s, want COMPLETE — discovery is not an obligation here either (gaps=%v)", v, gaps)
		}
	})

	// The recorded-outcome half must stay unconditional: this is the round-5
	// invariant, and the fix above must not walk it back.
	t.Run("a real failure is still audited", func(t *testing.T) {
		s := completeState()
		s.FetchExpected = false
		s.CoverageMeasured = true
		s.ExpectedChannels = []string{"c1", "c2"}
		s.AttemptedChannels = []string{"c1", "c2"}
		s.SucceededChannels = []string{"c1"}
		s.FailedChannels = []string{"c2"}

		v, gaps := Evaluate(s)
		if v != Partial {
			t.Fatalf("verdict = %s, want PARTIAL — a recorded failure is a fact", v)
		}
		named := false
		for _, g := range gaps {
			if g.Kind == GapChannel && g.ChannelID == "c2" {
				named = true
			}
		}
		if !named {
			t.Fatalf("the failed channel must be named, got %v", gaps)
		}
	})

	// And a turn that WAS obliged to cover the scope still gets the shortfall.
	t.Run("expected turn still owes its scope", func(t *testing.T) {
		s := completeState()
		s.FetchExpected = true
		s.CoverageMeasured = true
		s.ExpectedChannels = []string{"c1", "c2"}
		s.AttemptedChannels = []string{"c1"}
		s.SucceededChannels = []string{"c1"}

		if v, _ := Evaluate(s); v != Partial {
			t.Fatalf("verdict = %s, want PARTIAL — this turn did owe c2", v)
		}
	})
}
