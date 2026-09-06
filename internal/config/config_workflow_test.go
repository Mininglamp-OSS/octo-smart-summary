package config

import (
	"strings"
	"testing"
)

func TestLoad_WorkflowConfigs(t *testing.T) {
	// Test default values
	t.Run("defaults", func(t *testing.T) {
		// Clear any existing env vars
		t.Setenv("ENABLE_INTENT_SHORTCUT", "")
		t.Setenv("MAX_SAFETY_LIMIT", "")
		t.Setenv("DEFAULT_TIME_RANGE_DAYS", "")
		t.Setenv("MAX_TIME_RANGE_DAYS", "")
		t.Setenv("SKIP_MAP_REDUCE_THRESHOLD", "")
		t.Setenv("TOKENIZER_HTTP_TIMEOUT", "")
		t.Setenv("MESSAGE_FETCH_BACKEND", "")
		t.Setenv("OCTO_SEARCH_POLL_INTERVAL_SECONDS", "")

		cfg := Load()

		if cfg.MessageFetchBackend != "batch" {
			t.Errorf("MessageFetchBackend = %q, want batch", cfg.MessageFetchBackend)
		}
		if cfg.OctoSearchPollSec != 1 {
			t.Errorf("OctoSearchPollSec = %d, want 1", cfg.OctoSearchPollSec)
		}
		if cfg.EnableIntentShortcut != true {
			t.Errorf("EnableIntentShortcut = %v, want true", cfg.EnableIntentShortcut)
		}
		if cfg.MaxSafetyLimit != 100000 {
			t.Errorf("MaxSafetyLimit = %d, want 100000", cfg.MaxSafetyLimit)
		}
		if cfg.DefaultTimeRangeDays != 31 {
			t.Errorf("DefaultTimeRangeDays = %d, want 31", cfg.DefaultTimeRangeDays)
		}
		if cfg.MaxTimeRangeDays != 90 {
			t.Errorf("MaxTimeRangeDays = %d, want 90", cfg.MaxTimeRangeDays)
		}
		if cfg.SkipMapReduceThreshold != 0 {
			t.Errorf("SkipMapReduceThreshold = %d, want 0 (uses fallback)", cfg.SkipMapReduceThreshold)
		}
		if cfg.TokenizerHTTPTimeout != 10 {
			t.Errorf("TokenizerHTTPTimeout = %d, want 10", cfg.TokenizerHTTPTimeout)
		}
	})

	// Test env var overrides
	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("ENABLE_INTENT_SHORTCUT", "false")
		t.Setenv("MAX_SAFETY_LIMIT", "50000")
		t.Setenv("DEFAULT_TIME_RANGE_DAYS", "14")
		t.Setenv("MAX_TIME_RANGE_DAYS", "60")
		t.Setenv("SKIP_MAP_REDUCE_THRESHOLD", "150000")
		t.Setenv("TOKENIZER_HTTP_TIMEOUT", "20")
		t.Setenv("MESSAGE_FETCH_BACKEND", "mysql")
		t.Setenv("OCTO_SEARCH_POLL_INTERVAL_SECONDS", "2")

		cfg := Load()

		if cfg.MessageFetchBackend != "mysql" {
			t.Errorf("MessageFetchBackend = %q, want mysql", cfg.MessageFetchBackend)
		}
		if cfg.OctoSearchPollSec != 2 {
			t.Errorf("OctoSearchPollSec = %d, want 2", cfg.OctoSearchPollSec)
		}
		if cfg.EnableIntentShortcut != false {
			t.Errorf("EnableIntentShortcut = %v, want false", cfg.EnableIntentShortcut)
		}
		if cfg.MaxSafetyLimit != 50000 {
			t.Errorf("MaxSafetyLimit = %d, want 50000", cfg.MaxSafetyLimit)
		}
		if cfg.DefaultTimeRangeDays != 14 {
			t.Errorf("DefaultTimeRangeDays = %d, want 14", cfg.DefaultTimeRangeDays)
		}
		if cfg.MaxTimeRangeDays != 60 {
			t.Errorf("MaxTimeRangeDays = %d, want 60", cfg.MaxTimeRangeDays)
		}
		if cfg.SkipMapReduceThreshold != 150000 {
			t.Errorf("SkipMapReduceThreshold = %d, want 150000", cfg.SkipMapReduceThreshold)
		}
		if cfg.TokenizerHTTPTimeout != 20 {
			t.Errorf("TokenizerHTTPTimeout = %d, want 20", cfg.TokenizerHTTPTimeout)
		}
	})
}

func TestValidateSummaryTimeRanges(t *testing.T) {
	tests := []struct {
		name        string
		defaultDays int
		maxDays     int
		wantError   string
	}{
		{name: "valid defaults", defaultDays: 31, maxDays: 90},
		{name: "equal values", defaultDays: 31, maxDays: 31},
		{name: "non-positive default", defaultDays: 0, maxDays: 90, wantError: "DEFAULT_TIME_RANGE_DAYS"},
		{name: "non-positive max", defaultDays: 31, maxDays: 0, wantError: "MAX_TIME_RANGE_DAYS"},
		{name: "max below default", defaultDays: 31, maxDays: 7, wantError: "greater than or equal"},
		{name: "max below workspace default", defaultDays: 5, maxDays: 29, wantError: "at least 30"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSummaryTimeRanges(tt.defaultDays, tt.maxDays)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestLoadPreservesLegacyTimeRangeConfiguration(t *testing.T) {
	t.Run("zero default falls back instead of crash looping", func(t *testing.T) {
		t.Setenv("DEFAULT_TIME_RANGE_DAYS", "0")
		t.Setenv("MAX_TIME_RANGE_DAYS", "")
		cfg := Load()
		if cfg.DefaultTimeRangeDays != 31 || cfg.MaxTimeRangeDays != 90 {
			t.Fatalf("ranges=(%d,%d), want (31,90)", cfg.DefaultTimeRangeDays, cfg.MaxTimeRangeDays)
		}
		if err := ValidateSummaryTimeRanges(cfg.DefaultTimeRangeDays, cfg.MaxTimeRangeDays); err != nil {
			t.Fatalf("normalized legacy config must validate: %v", err)
		}
	})

	t.Run("unset max expands around an existing larger default", func(t *testing.T) {
		t.Setenv("DEFAULT_TIME_RANGE_DAYS", "120")
		t.Setenv("MAX_TIME_RANGE_DAYS", "")
		cfg := Load()
		if cfg.DefaultTimeRangeDays != 120 || cfg.MaxTimeRangeDays != 120 {
			t.Fatalf("ranges=(%d,%d), want (120,120)", cfg.DefaultTimeRangeDays, cfg.MaxTimeRangeDays)
		}
		if err := ValidateSummaryTimeRanges(cfg.DefaultTimeRangeDays, cfg.MaxTimeRangeDays); err != nil {
			t.Fatalf("legacy config with unset new ceiling must validate: %v", err)
		}
	})
}

func TestResolveSkipMapReduceThreshold(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		threshold int
		want      int
	}{
		{"explicit threshold", "any-model", 100000, 100000},
		{"qwen3.6-max default", "qwen3.6-max", 0, 500000},
		{"qwen3.6-plus default", "qwen3.6-plus", 0, 500000},
		{"deepseek-v4-flash default", "deepseek-v4-flash", 0, 500000},
		{"claude-sonnet default", "claude-sonnet-4-6", 0, 500000},
		{"kimi-k2 default", "kimi-k2.6", 0, 200000},
		{"unknown model global default", "unknown-model", 0, defaultSkipMapReduceThreshold},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				LLMModel:               tt.model,
				SkipMapReduceThreshold: tt.threshold,
			}
			got := cfg.ResolveSkipMapReduceThreshold()
			if got != tt.want {
				t.Errorf("ResolveSkipMapReduceThreshold() = %d, want %d", got, tt.want)
			}
		})
	}
}
