package config

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
)

func TestResolveMaxCitationsPerClaim(t *testing.T) {
	tests := []struct {
		name  string
		field int
		want  int
	}{
		{"shipped default", defaultMaxCitationsPerClaim, 3},
		{"explicit tune up", 5, 5},
		{"explicit tune down", 1, 1},
		{"zero disables", 0, citation.Disabled},
		{"negative disables", -1, citation.Disabled},
		{"large negative disables", -999, citation.Disabled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{SummaryMaxCitationsPerClaim: tc.field}
			if got := c.ResolveMaxCitationsPerClaim(); got != tc.want {
				t.Errorf("ResolveMaxCitationsPerClaim() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The Config field and the env-only read path must never disagree — that is
// the whole reason NormalizeMaxCitationsPerClaim exists (same argument as
// NormalizeAgentSummaryV2Mode).
func TestMaxCitationsPerClaimReadPathsAgree(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", 3},
		{"3", 3},
		{"5", 5},
		{"1", 1},
		{"0", citation.Disabled},
		{"-1", citation.Disabled},
		{"garbage", 3}, // envInt falls back to the default on a parse failure
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(MaxCitationsPerClaimEnvVar, tc.env)

			envPath := MaxCitationsPerClaim()
			if envPath != tc.want {
				t.Errorf("MaxCitationsPerClaim() = %d, want %d", envPath, tc.want)
			}

			// The Config field is populated by the same envInt call, so
			// resolving it must land on the same number.
			c := &Config{SummaryMaxCitationsPerClaim: envInt(MaxCitationsPerClaimEnvVar, defaultMaxCitationsPerClaim)}
			if fieldPath := c.ResolveMaxCitationsPerClaim(); fieldPath != envPath {
				t.Errorf("read paths disagree: config field -> %d, env -> %d", fieldPath, envPath)
			}
		})
	}
}

// The default must be the number the prompt states; a mismatch means the
// model is asked for one cap while the code enforces another.
func TestDefaultCapMatchesPromptRule(t *testing.T) {
	rule := citation.PromptRuleZH((&Config{SummaryMaxCitationsPerClaim: defaultMaxCitationsPerClaim}).ResolveMaxCitationsPerClaim())
	if rule == "" {
		t.Fatal("default cap produced an empty prompt rule")
	}
	if want := "3"; !contains(rule, want) {
		t.Errorf("prompt rule %q does not state the enforced default %s", rule, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
