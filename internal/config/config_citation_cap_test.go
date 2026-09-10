package config

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
)

func TestMaxCitationsPerClaimReadPathsAgree(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", 3}, {"5", 5}, {"1", 1}, {"0", citation.Disabled},
		{"-1", citation.Disabled}, {"garbage", 3},
	} {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(MaxCitationsPerClaimEnvVar, tc.env)
			envPath := MaxCitationsPerClaim()
			if envPath != tc.want {
				t.Fatalf("MaxCitationsPerClaim() = %d, want %d", envPath, tc.want)
			}
			cfg := &Config{SummaryMaxCitationsPerClaim: envInt(MaxCitationsPerClaimEnvVar, defaultMaxCitationsPerClaim)}
			if got := cfg.ResolveMaxCitationsPerClaim(); got != envPath {
				t.Fatalf("config path = %d, env path = %d", got, envPath)
			}
		})
	}
}

func TestDefaultCapMatchesPromptRule(t *testing.T) {
	rule := citation.PromptRuleZH(defaultMaxCitationsPerClaim)
	if !strings.Contains(rule, "3") {
		t.Fatalf("default prompt rule does not state cap 3: %q", rule)
	}
}
