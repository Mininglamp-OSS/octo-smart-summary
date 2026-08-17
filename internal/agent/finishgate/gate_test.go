package finishgate

import "testing"

// a "good" complete run: everything resolved, no gaps.
func completeState() RunState {
	return RunState{
		ScopeResolved:            true,
		HasUsableEvidence:        true,
		SummaryGenerated:         true,
		CitationValidationPassed: true,
		ChannelsExpected:         2,
		ChannelsFetched:          2,
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
		"channel shortfall": {func(s *RunState) { s.ChannelsFetched = 1 }, GapChannel},
		"failed channel":    {func(s *RunState) { s.FailedChannels = []string{"FETCH_TIMEOUT"} }, GapChannel},
		"scope unresolved":  {func(s *RunState) { s.ScopeResolved = false }, GapToolError},
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
