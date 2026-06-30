package tokenizer

import (
	"testing"
)

func TestNew_ModelMatching(t *testing.T) {
	cfg := Config{
		CharsPerTokenCJK:   1,
		CharsPerTokenASCII: 4,
	}

	tests := []struct {
		model    string
		wantType string
	}{
		{"gpt-4o", "*tokenizer.OpenAITokenizer"},
		{"gpt-4", "*tokenizer.OpenAITokenizer"},
		{"gpt-3.5-turbo", "*tokenizer.OpenAITokenizer"},
		{"o1-preview", "*tokenizer.OpenAITokenizer"},
		{"o3-mini", "*tokenizer.OpenAITokenizer"},
		{"kimi-k2.6", "*tokenizer.KimiTokenizer"},
		{"kimi_k2.5", "*tokenizer.KimiTokenizer"},
		{"mlamp/kimi-k2.6", "*tokenizer.KimiTokenizer"},
		{"qwen3.6-max", "*tokenizer.HFTokenizer"},
		{"qwen3.6-plus", "*tokenizer.HFTokenizer"},
		{"deepseek-v4-flash", "*tokenizer.HFTokenizer"},
		{"deepseek-v4-pro", "*tokenizer.HFTokenizer"},
		{"claude-sonnet-4-6", "*tokenizer.ClaudeTokenizer"},
		{"claude-opus-4-6", "*tokenizer.ClaudeTokenizer"},
		{"gemini-2.5-flash", "*tokenizer.GeminiTokenizer"},
		{"gemini-1.5-pro", "*tokenizer.GeminiTokenizer"},
		{"unknown-model", "*tokenizer.EstimateTokenizer"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			tok := New(tt.model, cfg)
			gotType := getTypeName(tok)
			if gotType != tt.wantType {
				t.Errorf("New(%q) = %s, want %s", tt.model, gotType, tt.wantType)
			}
		})
	}
}

func getTypeName(tok Tokenizer) string {
	switch tok.(type) {
	case *OpenAITokenizer:
		return "*tokenizer.OpenAITokenizer"
	case *KimiTokenizer:
		return "*tokenizer.KimiTokenizer"
	case *QwenTokenizer:
		return "*tokenizer.QwenTokenizer"
	case *DeepSeekTokenizer:
		return "*tokenizer.DeepSeekTokenizer"
	case *ClaudeTokenizer:
		return "*tokenizer.ClaudeTokenizer"
	case *GeminiTokenizer:
		return "*tokenizer.GeminiTokenizer"
	case *EstimateTokenizer:
		return "*tokenizer.EstimateTokenizer"
	case *HFTokenizer:
		return "*tokenizer.HFTokenizer"
	default:
		return "unknown"
	}
}

func TestEstimateTokenizer_Count(t *testing.T) {
	cfg := Config{
		CharsPerTokenCJK:   1,
		CharsPerTokenASCII: 4,
	}
	tok := NewEstimateTokenizer("test-model", cfg)

	tests := []struct {
		name    string
		text    string
		wantMin int
		wantMax int
	}{
		{"empty", "", 50, 50},
		{"ascii only", "hello world", 50 + 11/4, 60},
		{"cjk only", "你好世界", 50 + 4, 60},
		{"mixed", "hello你好world世界", 50 + 10/4 + 4, 70},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tok.Count(tt.text)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("Count(%q) = %d, want in range [%d, %d]", tt.text, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestEstimateTokenizer_IsExact(t *testing.T) {
	cfg := Config{
		CharsPerTokenCJK:   1,
		CharsPerTokenASCII: 4,
	}
	tok := NewEstimateTokenizer("test-model", cfg)

	if tok.IsExact() {
		t.Error("EstimateTokenizer.IsExact() should return false")
	}
}

func TestQwenTokenizer_DefaultCJKRatio(t *testing.T) {
	cfg := Config{
		CharsPerTokenASCII: 4,
	}
	tok := NewQwenTokenizer("qwen3.6-max", cfg)

	cjkText := "你好世界测试文本" // 8 CJK chars
	tokens := tok.Count(cjkText)

	expected := 8/qwenCharsPerTokenCJK + overheadPerCall
	if tokens != expected {
		t.Errorf("QwenTokenizer.Count() = %d, want %d (CJK chars=8, ratio=%d)",
			tokens, expected, qwenCharsPerTokenCJK)
	}
}

func TestDeepSeekTokenizer_DefaultCJKRatio(t *testing.T) {
	cfg := Config{
		CharsPerTokenASCII: 4,
	}
	tok := NewDeepSeekTokenizer("deepseek-v4-flash", cfg)

	cjkText := "你好世界测试文本" // 8 CJK chars
	tokens := tok.Count(cjkText)

	expected := 8/deepseekCharsPerTokenCJK + overheadPerCall
	if tokens != expected {
		t.Errorf("DeepSeekTokenizer.Count() = %d, want %d (CJK chars=8, ratio=%d)",
			tokens, expected, deepseekCharsPerTokenCJK)
	}
}

func TestKimiTokenizer_LocalTiktoken(t *testing.T) {
	cfg := Config{
		CharsPerTokenCJK:   2,
		CharsPerTokenASCII: 4,
	}
	tok := NewKimiTokenizer("kimi-k2.6", cfg)

	// Skip if local tiktoken is not available (models not downloaded)
	if !tok.IsExact() {
		t.Skip("Skipping: local tiktoken not available (TOKENIZER_MODELS_DIR not set or models not downloaded)")
	}

	// Test Chinese text
	text := "测试文本"
	count := tok.Count(text)
	if count <= 0 {
		t.Errorf("KimiTokenizer.Count() should return positive value, got %d", count)
	}
	// Chinese characters typically use 1-2 tokens each in Kimi's tokenizer
	if count > 20 {
		t.Errorf("KimiTokenizer.Count() for 4 Chinese chars should be small, got %d", count)
	}

	// Test English text
	englishText := "Hello, world!"
	englishCount := tok.Count(englishText)
	if englishCount <= 0 {
		t.Errorf("KimiTokenizer.Count() for English should return positive value, got %d", englishCount)
	}
}

func TestOpenAITokenizer_IsExact(t *testing.T) {
	cfg := Config{}
	tok := NewOpenAITokenizer("gpt-4o", cfg)

	if !tok.IsExact() {
		t.Error("OpenAITokenizer should return IsExact()=true when tiktoken is available")
	}
}

func TestOpenAITokenizer_Count(t *testing.T) {
	cfg := Config{}
	tok := NewOpenAITokenizer("gpt-4o", cfg)

	text := "Hello, world!"
	count := tok.Count(text)

	if count <= 0 {
		t.Errorf("OpenAITokenizer.Count() should return positive value, got %d", count)
	}

	if count > 50 {
		t.Errorf("OpenAITokenizer.Count() for short text should be small, got %d", count)
	}
}

func TestClaudeTokenizer_WithEnvConfig(t *testing.T) {
	cfg := Config{}
	tok := NewClaudeTokenizer("claude-sonnet-4-6", cfg)

	// Claude uses API-based counting which can fail and fall back to estimation.
	// IsExact() should always return false for API-based tokenizers.
	if tok.IsExact() {
		t.Error("ClaudeTokenizer should return IsExact()=false (API-based tokenizers are never exact)")
	}

	// Test token counting - uses estimate since API-based
	text := "Hello, world!"
	count := tok.Count(text)
	if count <= 0 {
		t.Errorf("ClaudeTokenizer.Count() should return positive value, got %d", count)
	}
	t.Logf("ClaudeTokenizer.Count() returned %d tokens for '%s' (apiKey set=%v, countURL set=%v)",
		count, text, tok.apiKey != "", tok.countURL != "")
}

func TestGeminiTokenizer_WithEnvConfig(t *testing.T) {
	cfg := Config{}
	tok := NewGeminiTokenizer("gemini-2.5-flash", cfg)

	// Gemini uses API-based counting which can fail and fall back to estimation.
	// IsExact() should always return false for API-based tokenizers.
	if tok.IsExact() {
		t.Error("GeminiTokenizer should return IsExact()=false (API-based tokenizers are never exact)")
	}

	// Test token counting - uses estimate since API-based
	text := "Hello, world!"
	count := tok.Count(text)
	if count <= 0 {
		t.Errorf("GeminiTokenizer.Count() should return positive value, got %d", count)
	}
	t.Logf("GeminiTokenizer.Count() returned %d tokens for '%s' (apiKey set=%v, baseURL set=%v)",
		count, text, tok.apiKey != "", tok.baseURL != "")
}
