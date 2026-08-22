package finishgate

import "testing"

// MissingChannels is the shared set diff behind both the finish gate's coverage
// audit and the SS-12 repair loop's "what do I re-fetch" decision, so its edge
// cases are pinned here rather than only exercised through Evaluate.
func TestMissingChannels(t *testing.T) {
	cases := map[string]struct {
		expected, attempted, want []string
	}{
		"all attempted":   {[]string{"a", "b"}, []string{"a", "b"}, nil},
		"one missing":     {[]string{"a", "b", "c"}, []string{"a", "c"}, []string{"b"}},
		"none attempted":  {[]string{"a", "b"}, nil, []string{"a", "b"}},
		"empty expected":  {nil, []string{"a"}, nil},
		"extra attempted": {[]string{"a"}, []string{"a", "z"}, nil},
		"dup expected":    {[]string{"a", "a", "b"}, []string{"a"}, []string{"b"}},
		"blank ids":       {[]string{"a", "", "b"}, []string{"a"}, []string{"b"}},
		// The point of a SET diff over a count compare: an expected channel that
		// was tried but held nothing is NOT a gap, even though it contributes no
		// messages and so is invisible to any count of "channels with content".
		"attempted but empty is not missing": {[]string{"a", "quiet"}, []string{"a", "quiet"}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := MissingChannels(c.expected, c.attempted)
			if len(got) != len(c.want) {
				t.Fatalf("MissingChannels(%v, %v) = %v, want %v", c.expected, c.attempted, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("MissingChannels(%v, %v) = %v, want %v (order must follow expected)", c.expected, c.attempted, got, c.want)
				}
			}
		})
	}
}

// The repair loop and Evaluate must never disagree about what is missing: a
// channel MissingChannels reports is a channel Evaluate must have flagged as a
// gap (given a closed scope that owed a fetch).
func TestMissingChannelsAgreesWithEvaluate(t *testing.T) {
	s := RunState{
		ScopeResolved:            true,
		SummaryGenerated:         true,
		CitationValidationPassed: true,
		HasUsableEvidence:        true,
		FetchExpected:            true,
		CoverageMeasured:         true,
		ExpectedChannels:         []string{"c1", "c2", "c3"},
		AttemptedChannels:        []string{"c1", "c2"},
		SucceededChannels:        []string{"c1", "c2"},
	}
	missing := MissingChannels(s.ExpectedChannels, s.AttemptedChannels)
	if len(missing) != 1 || missing[0] != "c3" {
		t.Fatalf("MissingChannels = %v, want [c3]", missing)
	}

	verdict, gaps := Evaluate(s)
	if verdict != Partial {
		t.Fatalf("verdict = %s, want PARTIAL when an expected channel was never attempted", verdict)
	}
	found := false
	for _, g := range gaps {
		if g.Kind == GapChannel && g.ChannelID == "c3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Evaluate gaps = %v, want a channel gap naming c3 (the repair loop would re-fetch it)", gaps)
	}
}
