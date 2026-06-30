package tokenizer

const qwenCharsPerTokenCJK = 2

// QwenTokenizer estimates tokens for Qwen models.
type QwenTokenizer struct {
	*EstimateTokenizer
}

// NewQwenTokenizer creates a new QwenTokenizer.
func NewQwenTokenizer(model string, cfg Config) *QwenTokenizer {
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = qwenCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}
	return &QwenTokenizer{
		EstimateTokenizer: &EstimateTokenizer{
			model:              model,
			charsPerTokenCJK:   cjk,
			charsPerTokenASCII: ascii,
		},
	}
}
