package config

import (
	"os"
	"testing"
)

func TestResolveMapMaxTokens_QwenModels(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"qwen3.6-max", 400000},
		{"qwen3.6-plus", 400000},
		{"qwen3.6-flash", 400000},
		{"deepseek-v4-flash", 400000},
		{"deepseek-v4-pro", 400000},
		{"claude-sonnet-4-6", 150000},
		{"mlamp/deepseek-v4-flash", 400000},
		{"tencent/deepseek-v4-pro", 400000},
		{"Qwen3.6-Max", 400000},
		{"DEEPSEEK-V4-FLASH", 400000},
		{"ali/Qwen3.6-Flash", 400000},
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
	// Ensure env is clean
	os.Unsetenv("CHARS_PER_TOKEN_CJK")

	tests := []struct {
		name  string
		model string
		want  int
	}{
		{"qwen3.6-flash defaults to 2", "qwen3.6-flash", 2},
		{"qwen3.6-max defaults to 2", "qwen3.6-max", 2},
		{"deepseek-v4-flash defaults to 2", "deepseek-v4-flash", 2},
		{"deepseek-v4-pro defaults to 2", "deepseek-v4-pro", 2},
		{"claude model defaults to 1", "claude-haiku-4-5", 1},
		{"unknown model defaults to 1", "some-model", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{LLMModel: tt.model, CharsPerTokenCJK: 1}
			if got := cfg.ResolveCharsPerTokenCJK(); got != tt.want {
				t.Errorf("ResolveCharsPerTokenCJK() for model %q = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestResolveCharsPerTokenCJK_ExplicitEnvOverride(t *testing.T) {
	os.Setenv("CHARS_PER_TOKEN_CJK", "3")
	defer os.Unsetenv("CHARS_PER_TOKEN_CJK")

	cfg := &Config{LLMModel: "qwen3.6-flash", CharsPerTokenCJK: 3}
	if got := cfg.ResolveCharsPerTokenCJK(); got != 3 {
		t.Errorf("ResolveCharsPerTokenCJK() with explicit env = %d, want 3", got)
	}
}
