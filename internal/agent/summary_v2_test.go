package agent

import (
	"os"
	"testing"
)

func TestSummaryV2ModeDefaultOff(t *testing.T) {
	os.Unsetenv("AGENT_SUMMARY_V2_MODE")
	if got := SummaryV2Mode(); got != V2ModeOff {
		t.Fatalf("default mode = %q, want off", got)
	}
	if SummaryV2Enabled() {
		t.Fatal("V2 must be disabled by default")
	}
}

func TestSummaryV2ModeParsing(t *testing.T) {
	cases := map[string]string{
		"shadow":  V2ModeShadow,
		"on":      V2ModeOn,
		" ON ":    V2ModeOn, // trimmed + case-insensitive
		"Shadow":  V2ModeShadow,
		"off":     V2ModeOff,
		"garbage": V2ModeOff, // unknown fails safe to off
		"":        V2ModeOff,
	}
	for in, want := range cases {
		t.Setenv("AGENT_SUMMARY_V2_MODE", in)
		if got := SummaryV2Mode(); got != want {
			t.Errorf("mode(%q) = %q, want %q", in, got, want)
		}
	}
}
