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
