package tokenizer

import (
	"strings"
)

// Tokenizer counts tokens for text.
type Tokenizer interface {
	Count(text string) int
	Estimate(text string) int
	IsExact() bool
	ModelName() string
}

// Config holds tokenizer configuration.
type Config struct {
	CharsPerTokenCJK   int
	CharsPerTokenASCII int
	KimiAPIKey         string // Deprecated: Kimi now uses local tokenizer
	ClaudeAPIKey       string
	GeminiAPIKey       string
	HTTPTimeout        int
}

// New creates a Tokenizer based on model name.
func New(model string, cfg Config) Tokenizer {
	m := strings.ToLower(model)

	// OpenAI models
	if strings.Contains(m, "gpt-4") || strings.Contains(m, "gpt-3.5") ||
		strings.Contains(m, "o1") || strings.Contains(m, "o3") ||
		strings.Contains(m, "text-davinci") || strings.Contains(m, "text-embedding") {
		return NewOpenAITokenizer(model, cfg)
	}

	// Kimi models
	if strings.Contains(m, "kimi-k2") || strings.Contains(m, "kimi_k2") {
		return NewKimiTokenizer(model, cfg)
	}

	// Qwen models - use HF tokenizer for exact counting
	if strings.Contains(m, "qwen") {
		return NewQwenHFTokenizer(model, cfg)
	}

	// DeepSeek models - use HF tokenizer for exact counting
	if strings.Contains(m, "deepseek") {
		return NewDeepSeekHFTokenizer(model, cfg)
	}

	// Claude models
	if strings.Contains(m, "claude") {
		return NewClaudeTokenizer(model, cfg)
	}

	// Gemini models
	if strings.Contains(m, "gemini") {
		return NewGeminiTokenizer(model, cfg)
	}

	// Fallback to estimate tokenizer
	return NewEstimateTokenizer(model, cfg)
}
