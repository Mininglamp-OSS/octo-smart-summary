package tokenizer

import (
	"log"
	"strings"

	"github.com/pkoukk/tiktoken-go"
)

// OpenAITokenizer counts tokens for OpenAI models using tiktoken.
type OpenAITokenizer struct {
	model    string
	encoding *tiktoken.Tiktoken
	fallback *EstimateTokenizer
}

// NewOpenAITokenizer creates a new OpenAITokenizer.
func NewOpenAITokenizer(model string, cfg Config) *OpenAITokenizer {
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = defaultCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}

	fallback := &EstimateTokenizer{
		model:              model,
		charsPerTokenCJK:   cjk,
		charsPerTokenASCII: ascii,
	}

	encodingName := resolveOpenAIEncoding(model)
	encoding, err := tiktoken.GetEncoding(encodingName)
	if err != nil {
		log.Printf("[tokenizer] failed to get tiktoken encoding %s: %v, using estimate", encodingName, err)
		return &OpenAITokenizer{
			model:    model,
			encoding: nil,
			fallback: fallback,
		}
	}

	return &OpenAITokenizer{
		model:    model,
		encoding: encoding,
		fallback: fallback,
	}
}

func resolveOpenAIEncoding(model string) string {
	m := strings.ToLower(model)
	if strings.Contains(m, "gpt-4o") || strings.Contains(m, "o1") || strings.Contains(m, "o3") {
		return "o200k_base"
	}
	return "cl100k_base"
}

// Count returns exact token count using tiktoken.
func (t *OpenAITokenizer) Count(text string) int {
	if t.encoding == nil {
		return t.Estimate(text)
	}
	tokens := t.encoding.Encode(text, nil, nil)
	return len(tokens)
}

// Estimate returns estimated token count.
func (t *OpenAITokenizer) Estimate(text string) int {
	return t.fallback.Estimate(text)
}

// IsExact returns true if tiktoken encoding is available.
func (t *OpenAITokenizer) IsExact() bool {
	return t.encoding != nil
}

// ModelName returns the model name.
func (t *OpenAITokenizer) ModelName() string {
	return t.model
}
