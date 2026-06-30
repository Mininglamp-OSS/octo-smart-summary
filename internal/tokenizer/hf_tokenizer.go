//go:build cgo

package tokenizer

import (
	"log"
	"os"
	"path/filepath"
	"sync"

	hf "github.com/daulet/tokenizers"
)

// HFTokenizer wraps a HuggingFace tokenizer for exact token counting.
type HFTokenizer struct {
	model    string
	tk       *hf.Tokenizer
	fallback *EstimateTokenizer
}

var (
	// Singleton tokenizers - loaded once and reused
	qwenHFTokenizer     *hf.Tokenizer
	qwenHFTokenizerOnce sync.Once
	qwenHFTokenizerErr  error

	deepseekHFTokenizer     *hf.Tokenizer
	deepseekHFTokenizerOnce sync.Once
	deepseekHFTokenizerErr  error
)

// getModelsDir returns the path to the tokenizer models directory.
// It checks TOKENIZER_MODELS_DIR env var first, then fallback paths.
// Returns empty string if no valid directory found.
func getModelsDir() string {
	// Check environment variable first (recommended for production)
	if dir := os.Getenv("TOKENIZER_MODELS_DIR"); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		log.Printf("[tokenizer] TOKENIZER_MODELS_DIR=%s but directory not found", dir)
	}

	// Check relative to current working directory (for development)
	if _, err := os.Stat("internal/tokenizer/models"); err == nil {
		return "internal/tokenizer/models"
	}

	// Check relative to executable (for some deployment scenarios)
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), "models")
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}

	// No valid directory found - will cause tokenizer load to fail
	return ""
}

// loadQwenHFTokenizer loads the Qwen HF tokenizer (singleton).
func loadQwenHFTokenizer() (*hf.Tokenizer, error) {
	qwenHFTokenizerOnce.Do(func() {
		path := filepath.Join(getModelsDir(), "qwen", "tokenizer.json")
		qwenHFTokenizer, qwenHFTokenizerErr = hf.FromFile(path)
		if qwenHFTokenizerErr != nil {
			log.Printf("[tokenizer] failed to load Qwen HF tokenizer from %s: %v", path, qwenHFTokenizerErr)
		} else {
			log.Printf("[tokenizer] loaded Qwen HF tokenizer from %s", path)
		}
	})
	return qwenHFTokenizer, qwenHFTokenizerErr
}

// loadDeepSeekHFTokenizer loads the DeepSeek HF tokenizer (singleton).
func loadDeepSeekHFTokenizer() (*hf.Tokenizer, error) {
	deepseekHFTokenizerOnce.Do(func() {
		path := filepath.Join(getModelsDir(), "deepseek", "tokenizer.json")
		deepseekHFTokenizer, deepseekHFTokenizerErr = hf.FromFile(path)
		if deepseekHFTokenizerErr != nil {
			log.Printf("[tokenizer] failed to load DeepSeek HF tokenizer from %s: %v", path, deepseekHFTokenizerErr)
		} else {
			log.Printf("[tokenizer] loaded DeepSeek HF tokenizer from %s", path)
		}
	})
	return deepseekHFTokenizer, deepseekHFTokenizerErr
}

// NewQwenHFTokenizer creates a Qwen tokenizer with HF backend.
func NewQwenHFTokenizer(model string, cfg Config) *HFTokenizer {
	tk, err := loadQwenHFTokenizer()

	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = qwenCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}

	t := &HFTokenizer{
		model: model,
		tk:    tk,
		fallback: &EstimateTokenizer{
			model:              model,
			charsPerTokenCJK:   cjk,
			charsPerTokenASCII: ascii,
		},
	}

	if err != nil {
		log.Printf("[tokenizer] Qwen HF tokenizer unavailable, using estimate fallback")
	}

	return t
}

// NewDeepSeekHFTokenizer creates a DeepSeek tokenizer with HF backend.
func NewDeepSeekHFTokenizer(model string, cfg Config) *HFTokenizer {
	tk, err := loadDeepSeekHFTokenizer()

	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = deepseekCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}

	t := &HFTokenizer{
		model: model,
		tk:    tk,
		fallback: &EstimateTokenizer{
			model:              model,
			charsPerTokenCJK:   cjk,
			charsPerTokenASCII: ascii,
		},
	}

	if err != nil {
		log.Printf("[tokenizer] DeepSeek HF tokenizer unavailable, using estimate fallback")
	}

	return t
}

// Count returns exact token count using HF tokenizer, or estimate if unavailable.
func (t *HFTokenizer) Count(text string) int {
	if t.tk == nil {
		return t.Estimate(text)
	}

	// EncodeErr returns error on encode failure; fall back to estimate.
	ids, _, err := t.tk.EncodeErr(text, false)
	if err != nil {
		return t.Estimate(text)
	}
	return len(ids)
}

// Estimate returns estimated token count (always available).
func (t *HFTokenizer) Estimate(text string) int {
	return t.fallback.Estimate(text)
}

// IsExact returns true if HF tokenizer is available.
func (t *HFTokenizer) IsExact() bool {
	return t.tk != nil
}

// ModelName returns the model name.
func (t *HFTokenizer) ModelName() string {
	return t.model
}
