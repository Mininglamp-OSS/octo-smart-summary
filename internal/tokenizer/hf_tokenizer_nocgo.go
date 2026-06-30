//go:build !cgo

package tokenizer

import (
	"log"
)

// HFTokenizer is a stub for non-CGo builds that falls back to estimation.
type HFTokenizer struct {
	model    string
	fallback *EstimateTokenizer
}

// NewQwenHFTokenizer returns a fallback estimator when CGo is disabled.
func NewQwenHFTokenizer(model string, cfg Config) *HFTokenizer {
	log.Printf("[tokenizer] CGO disabled, Qwen tokenizer using estimate mode")
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = 2 // Qwen default
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}
	return &HFTokenizer{
		model: model,
		fallback: &EstimateTokenizer{
			model:              model,
			charsPerTokenCJK:   cjk,
			charsPerTokenASCII: ascii,
		},
	}
}

// NewDeepSeekHFTokenizer returns a fallback estimator when CGo is disabled.
func NewDeepSeekHFTokenizer(model string, cfg Config) *HFTokenizer {
	log.Printf("[tokenizer] CGO disabled, DeepSeek tokenizer using estimate mode")
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = 2 // DeepSeek default
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}
	return &HFTokenizer{
		model: model,
		fallback: &EstimateTokenizer{
			model:              model,
			charsPerTokenCJK:   cjk,
			charsPerTokenASCII: ascii,
		},
	}
}

func (t *HFTokenizer) Count(text string) int {
	return t.fallback.Estimate(text)
}

func (t *HFTokenizer) Estimate(text string) int {
	return t.fallback.Estimate(text)
}

func (t *HFTokenizer) IsExact() bool {
	return false // CGo disabled, always estimate
}

func (t *HFTokenizer) Model() string {
	return t.model
}

func (t *HFTokenizer) ModelName() string {
	return t.model
}
