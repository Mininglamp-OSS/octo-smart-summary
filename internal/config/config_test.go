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

func TestLoad_SummaryCustomTemplateLimit(t *testing.T) {
	t.Setenv("SUMMARY_CUSTOM_TEMPLATE_LIMIT", "50")
	cfg := Load()
	if cfg.SummaryCustomTemplateLimit != 50 {
		t.Fatalf("SummaryCustomTemplateLimit=%d want 50", cfg.SummaryCustomTemplateLimit)
	}
}

func TestLoad_SummaryCustomTemplateLimitDefault(t *testing.T) {
	t.Setenv("SUMMARY_CUSTOM_TEMPLATE_LIMIT", "")
	cfg := Load()
	if cfg.SummaryCustomTemplateLimit != 30 {
		t.Fatalf("SummaryCustomTemplateLimit=%d want 30", cfg.SummaryCustomTemplateLimit)
	}
}

func TestLoad_SummaryWorkbenchEnabledDefaultsOff(t *testing.T) {
	t.Setenv("SUMMARY_WORKBENCH_ENABLED", "")
	if cfg := Load(); cfg.SummaryWorkbenchEnabled {
		t.Fatal("SummaryWorkbenchEnabled=true, want false by default")
	}
}

func TestLoad_SummaryWorkbenchEnabledFromEnvironment(t *testing.T) {
	t.Setenv("SUMMARY_WORKBENCH_ENABLED", "true")
	if cfg := Load(); !cfg.SummaryWorkbenchEnabled {
		t.Fatal("SummaryWorkbenchEnabled=false, want true")
	}
}

// TestMapWindowReserve is the #241 item-3 contract: the Map window reserve is
// the shared system-prompt reserve PLUS the completion budget (LLMMaxToken)
// that shares the same context window, so both Map paths hold back room for the
// response. A single source keeps the agent and worker paths from drifting
// apart (they used 800 vs 3000 before).
func TestMapWindowReserve(t *testing.T) {
	c := &Config{LLMMaxToken: 4096}
	if got, want := c.MapWindowReserve(), MapSystemPromptReserve+4096; got != want {
		t.Errorf("MapWindowReserve() = %d, want %d (system prompt %d + completion %d)",
			got, want, MapSystemPromptReserve, 4096)
	}
	// The completion term tracks LLMMaxToken, so changing LLM_MAX_TOKENS keeps
	// the reserve correct rather than hard-coding 4096.
	c2 := &Config{LLMMaxToken: 8192}
	if got, want := c2.MapWindowReserve(), MapSystemPromptReserve+8192; got != want {
		t.Errorf("MapWindowReserve() with LLMMaxToken=8192 = %d, want %d", got, want)
	}
}

// TestResolveMapInputBudget is the #241 contract shared by both Map paths: the
// per-chunk input budget is window - (system prompt + completion) reserve, with
// a loud fallback to the default window when the configured window cannot hold
// the reserve plus a minimal input, and a positive floor so the packer never
// runs with a non-positive budget.
func TestResolveMapInputBudget(t *testing.T) {
	const llm = 8192
	reserve := MapSystemPromptReserve + llm // 11192

	t.Run("healthy explicit window", func(t *testing.T) {
		c := Config{MapMaxTokens: 50000, LLMMaxToken: llm}
		got, fell := c.ResolveMapInputBudget()
		if fell || got != 50000-reserve {
			t.Errorf("got (%d, fellBack=%v), want (%d, false)", got, fell, 50000-reserve)
		}
	})

	t.Run("zero config resolves to default window, no fallback", func(t *testing.T) {
		c := Config{LLMMaxToken: llm} // ResolveMapMaxTokens -> defaultMapMaxTokens
		got, fell := c.ResolveMapInputBudget()
		if fell || got != defaultMapMaxTokens-reserve {
			t.Errorf("got (%d, fellBack=%v), want (%d, false)", got, fell, defaultMapMaxTokens-reserve)
		}
	})

	t.Run("window too small for reserve falls back to default", func(t *testing.T) {
		// 10000 - 11192 < minMapInputBudget -> degenerate -> default window.
		c := Config{MapMaxTokens: 10000, LLMMaxToken: llm}
		got, fell := c.ResolveMapInputBudget()
		if !fell {
			t.Errorf("expected fellBack=true for degenerate window")
		}
		if got != defaultMapMaxTokens-reserve {
			t.Errorf("got budget %d, want %d (default window - reserve)", got, defaultMapMaxTokens-reserve)
		}
	})

	t.Run("budget never non-positive even if the default cannot fit the reserve", func(t *testing.T) {
		// A pathological completion budget larger than the default window: the
		// floor must still return a positive budget rather than <= 0.
		c := Config{MapMaxTokens: 10000, LLMMaxToken: defaultMapMaxTokens}
		got, _ := c.ResolveMapInputBudget()
		if got < minMapInputBudget {
			t.Errorf("budget %d below floor %d", got, minMapInputBudget)
		}
	})
}
