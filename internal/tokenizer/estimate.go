package tokenizer

const defaultCharsPerTokenCJK = 1
const defaultCharsPerTokenASCII = 4
const overheadPerCall = 50
const defaultHTTPTimeout = 30 // seconds

// EstimateTokenizer estimates tokens based on character ratios.
type EstimateTokenizer struct {
	model              string
	charsPerTokenCJK   int
	charsPerTokenASCII int
}

// NewEstimateTokenizer creates a new EstimateTokenizer.
func NewEstimateTokenizer(model string, cfg Config) *EstimateTokenizer {
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = defaultCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}
	return &EstimateTokenizer{
		model:              model,
		charsPerTokenCJK:   cjk,
		charsPerTokenASCII: ascii,
	}
}

// Count returns estimated token count.
func (t *EstimateTokenizer) Count(text string) int {
	return t.Estimate(text)
}

// Estimate returns estimated token count based on character ratios.
func (t *EstimateTokenizer) Estimate(text string) int {
	cjkCount := 0
	asciiCount := 0
	for _, r := range text {
		if r > 0x7F {
			cjkCount++
		} else {
			asciiCount++
		}
	}
	return cjkCount/t.charsPerTokenCJK + asciiCount/t.charsPerTokenASCII + overheadPerCall
}

// ModelName returns the model name.
func (t *EstimateTokenizer) ModelName() string {
	return t.model
}

// IsExact returns false as this is an estimate.
func (t *EstimateTokenizer) IsExact() bool {
	return false
}
