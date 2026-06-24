package tokenizer

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

const kimiCharsPerTokenCJK = 2

// KimiTokenizer counts tokens for Kimi models using local tiktoken.
type KimiTokenizer struct {
	model    string
	encoding *tiktoken.Tiktoken
	fallback *EstimateTokenizer
}

var (
	kimiEncoding     *tiktoken.Tiktoken
	kimiEncodingOnce sync.Once
	kimiEncodingErr  error
)

// kimiTokenizerConfig represents the tokenizer_config.json structure.
type kimiTokenizerConfig struct {
	AddedTokensDecoder map[string]struct {
		Content string `json:"content"`
		Special bool   `json:"special"`
	} `json:"added_tokens_decoder"`
	ChatTemplate string `json:"chat_template"`
}

// NewKimiTokenizer creates a new KimiTokenizer using local tiktoken files.
func NewKimiTokenizer(model string, cfg Config) *KimiTokenizer {
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = kimiCharsPerTokenCJK
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

	// Initialize encoding once (thread-safe)
	kimiEncodingOnce.Do(func() {
		kimiEncoding, kimiEncodingErr = loadKimiEncoding()
		if kimiEncodingErr != nil {
			log.Printf("[tokenizer] failed to load Kimi local tokenizer: %v", kimiEncodingErr)
		}
	})

	return &KimiTokenizer{
		model:    model,
		encoding: kimiEncoding,
		fallback: fallback,
	}
}

// loadKimiEncoding loads the Kimi tiktoken encoding from local files.
func loadKimiEncoding() (*tiktoken.Tiktoken, error) {
	// Get the directory of this source file
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, os.ErrNotExist
	}
	modelsDir := filepath.Join(filepath.Dir(currentFile), "models", "kimi")

	// Load BPE ranks from tiktoken.model
	tiktokenPath := filepath.Join(modelsDir, "tiktoken.model")
	mergeableRanks, err := loadTiktokenBPE(tiktokenPath)
	if err != nil {
		return nil, err
	}

	// Load special tokens from tokenizer_config.json
	configPath := filepath.Join(modelsDir, "tokenizer_config.json")
	specialTokens, err := loadKimiSpecialTokens(configPath)
	if err != nil {
		return nil, err
	}

	// Kimi uses a similar pattern to cl100k_base
	// Based on the tokenization_kimi.py from moonshotai/Kimi-K2.6
	patStr := `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

	// Create CoreBPE
	coreBPE, err := tiktoken.NewCoreBPE(mergeableRanks, specialTokens, patStr)
	if err != nil {
		return nil, err
	}

	// Create special tokens set
	specialTokensSet := make(map[string]any)
	for token := range specialTokens {
		specialTokensSet[token] = struct{}{}
	}

	// Create Tiktoken instance
	encoding := tiktoken.NewTiktoken(coreBPE, &tiktoken.Encoding{
		Name:           "kimi",
		PatStr:         patStr,
		MergeableRanks: mergeableRanks,
		SpecialTokens:  specialTokens,
	}, specialTokensSet)

	log.Printf("[tokenizer] Kimi local tokenizer loaded: %d tokens, %d special tokens",
		len(mergeableRanks), len(specialTokens))

	return encoding, nil
}

// loadTiktokenBPE loads BPE ranks from a tiktoken.model file.
func loadTiktokenBPE(path string) (map[string]int, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	bpeRanks := make(map[string]int)
	for _, line := range strings.Split(string(contents), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			continue
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, err
		}
		rank, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		bpeRanks[string(token)] = rank
	}
	return bpeRanks, nil
}

// loadKimiSpecialTokens loads special tokens from tokenizer_config.json.
func loadKimiSpecialTokens(path string) (map[string]int, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config kimiTokenizerConfig
	if err := json.Unmarshal(contents, &config); err != nil {
		return nil, err
	}

	specialTokens := make(map[string]int)
	for idStr, tokenInfo := range config.AddedTokensDecoder {
		if tokenInfo.Special {
			id, err := strconv.Atoi(idStr)
			if err != nil {
				continue
			}
			specialTokens[tokenInfo.Content] = id
		}
	}

	return specialTokens, nil
}

// Count returns exact token count using local tiktoken.
func (t *KimiTokenizer) Count(text string) int {
	if t.encoding == nil {
		return t.Estimate(text)
	}
	tokens := t.encoding.Encode(text, nil, nil)
	return len(tokens)
}

// Estimate returns estimated token count.
func (t *KimiTokenizer) Estimate(text string) int {
	return t.fallback.Estimate(text)
}

// IsExact returns true if local tiktoken is available.
func (t *KimiTokenizer) IsExact() bool {
	return t.encoding != nil
}

// ModelName returns the model name.
func (t *KimiTokenizer) ModelName() string {
	return t.model
}
