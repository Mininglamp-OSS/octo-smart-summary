package config

import (
	"testing"
)

func TestResolveMapMaxTokens_QwenModels(t *testing.T) {
	// Note: token limits updated based on 2026-06 model testing (see docs/模型测试结果.md)
	// - Qwen: 160K+ causes knowledge leakage → reduced to 150K
	// - DeepSeek: 250K+ causes truncation → reduced to 150K/200K
	tests := []struct {
		model string
		want  int
	}{
		{"qwen3.6-max", 150000},
		{"qwen3.6-plus", 150000},
		{"qwen3.6-flash", 150000},
		{"deepseek-v4-flash", 150000},
		{"deepseek-v4-pro", 200000},
		{"claude-sonnet-4-6", 150000},
		{"mlamp/deepseek-v4-flash", 150000},
		{"tencent/deepseek-v4-pro", 200000},
		{"Qwen3.6-Max", 150000},
		{"DEEPSEEK-V4-FLASH", 150000},
		{"ali/Qwen3.6-Flash", 150000},
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
