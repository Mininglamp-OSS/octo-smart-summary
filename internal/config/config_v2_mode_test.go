package config

import "testing"

// TestNormalizeAgentSummaryV2Mode pins the single normalizer both read paths of
// AGENT_SUMMARY_V2_MODE (config.AgentSummaryV2Mode and agent.SummaryV2Mode)
// now share: trim+lowercase, shadow/on pass through, everything else (unset,
// empty, unknown) maps to off — fail safe, never fail into the new path.
func TestNormalizeAgentSummaryV2Mode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", AgentSummaryV2Off},
		{"off", AgentSummaryV2Off},
		{"ON", AgentSummaryV2On},
		{" On ", AgentSummaryV2On},
		{"SHADOW", AgentSummaryV2Shadow},
		{"shadow ", AgentSummaryV2Shadow},
		{"bogus", AgentSummaryV2Off},
	}
	for _, tc := range cases {
		if got := NormalizeAgentSummaryV2Mode(tc.in); got != tc.want {
			t.Errorf("NormalizeAgentSummaryV2Mode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLoad_AgentSummaryV2ModeNormalized covers the old divergence: config used
// to store the raw env value (" ON ") while agent.SummaryV2Mode normalized it
// to "on" — the first consumer of the field would have compared the raw value
// and silently stayed on the legacy path. Load() must now normalize.
func TestLoad_AgentSummaryV2ModeNormalized(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", " ON ")
	cfg := Load()
	if cfg.AgentSummaryV2Mode != AgentSummaryV2On {
		t.Errorf("cfg.AgentSummaryV2Mode = %q, want %q", cfg.AgentSummaryV2Mode, AgentSummaryV2On)
	}
}

func TestLoad_AgentSummaryV2ModeUnknownFallsToOff(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "bogus")
	cfg := Load()
	if cfg.AgentSummaryV2Mode != AgentSummaryV2Off {
		t.Errorf("cfg.AgentSummaryV2Mode = %q, want off", cfg.AgentSummaryV2Mode)
	}
}
