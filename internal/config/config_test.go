package config

import (
	"testing"
)

func TestResolveMapMaxTokens_QwenModels(t *testing.T) {
	// Token limits tuned per-model to balance summary quality and LLM call efficiency
	tests := []struct {
		model string
		want  int
	}{
		{"qwen3.6-max", 300000},
		{"qwen3.6-plus", 300000},
		{"qwen3.6-flash", 300000},
		{"deepseek-v4-flash", 300000},
		{"deepseek-v4-pro", 300000},
		{"claude-sonnet-4-6", 250000},
		{"claude-opus-4-6", 350000},
		{"claude-haiku-4-5", 200000},
		{"mlamp/deepseek-v4-flash", 300000},
		{"tencent/deepseek-v4-pro", 300000},
		{"Qwen3.6-Max", 300000},
		{"DEEPSEEK-V4-FLASH", 300000},
		{"ali/Qwen3.6-Flash", 300000},
		{"kimi-k2.6", 150000},
		{"kimi-k2.5", 150000},
		{"mlamp/kimi-k2.6", 150000},
		{"KIMI-K2.6", 150000},
		{"unknown-model", 100000},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			cfg := &Config{LLMModel: tt.model}
			if got := cfg.ResolveMapMaxTokens(); got != tt.want {
				t.Errorf("ResolveMapMaxTokens() for model %q = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestResolveMapMaxTokens_ExplicitOverride(t *testing.T) {
	cfg := &Config{LLMModel: "qwen3.6-max", MapMaxTokens: 200000}
	if got := cfg.ResolveMapMaxTokens(); got != 200000 {
		t.Errorf("ResolveMapMaxTokens() with explicit override = %d, want 200000", got)
	}
}

func TestResolveCharsPerTokenCJK(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  int
	}{
		{"qwen3.6-flash defaults to 2", "qwen3.6-flash", 2},
		{"qwen3.6-max defaults to 2", "qwen3.6-max", 2},
		{"deepseek-v4-flash defaults to 2", "deepseek-v4-flash", 2},
		{"deepseek-v4-pro defaults to 2", "deepseek-v4-pro", 2},
		{"kimi-k2.6 defaults to 2", "kimi-k2.6", 2},
		{"kimi-k2.5 defaults to 2", "kimi-k2.5", 2},
		{"mlamp/kimi-k2.6 defaults to 2", "mlamp/kimi-k2.6", 2},
		{"kimi_k2.6 defaults to 2", "kimi_k2.6", 2},
		{"claude model defaults to 1", "claude-haiku-4-5", 1},
		{"unknown model defaults to 1", "some-model", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CHARS_PER_TOKEN_CJK", "")
			cfg := &Config{LLMModel: tt.model, CharsPerTokenCJK: 1}
			if got := cfg.ResolveCharsPerTokenCJK(); got != tt.want {
				t.Errorf("ResolveCharsPerTokenCJK() for model %q = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestResolveCharsPerTokenCJK_ExplicitEnvOverride(t *testing.T) {
	t.Setenv("CHARS_PER_TOKEN_CJK", "3")

	cfg := &Config{LLMModel: "kimi-k2.6", CharsPerTokenCJK: 3}
	if got := cfg.ResolveCharsPerTokenCJK(); got != 3 {
		t.Errorf("ResolveCharsPerTokenCJK() with explicit env = %d, want 3", got)
	}
}
