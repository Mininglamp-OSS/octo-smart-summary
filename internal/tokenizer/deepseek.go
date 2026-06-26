package tokenizer

const deepseekCharsPerTokenCJK = 2

// DeepSeekTokenizer estimates tokens for DeepSeek models.
type DeepSeekTokenizer struct {
	*EstimateTokenizer
}

// NewDeepSeekTokenizer creates a new DeepSeekTokenizer.
func NewDeepSeekTokenizer(model string, cfg Config) *DeepSeekTokenizer {
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = deepseekCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}
	return &DeepSeekTokenizer{
		EstimateTokenizer: &EstimateTokenizer{
			model:              model,
			charsPerTokenCJK:   cjk,
			charsPerTokenASCII: ascii,
		},
	}
}
