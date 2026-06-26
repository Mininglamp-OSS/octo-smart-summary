package tokenizer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	claudeCharsPerTokenCJK = 1 // Claude uses ~1 char per token for CJK
)

// ClaudeTokenizer counts tokens for Claude models using Vertex AI API via LiteLLM proxy.
type ClaudeTokenizer struct {
	model      string
	apiKey     string
	countURL   string
	httpClient *http.Client
	fallback   *EstimateTokenizer
}

// NewClaudeTokenizer creates a new ClaudeTokenizer.
func NewClaudeTokenizer(model string, cfg Config) *ClaudeTokenizer {
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = claudeCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}

	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}

	// Get API key from config or environment
	apiKey := cfg.ClaudeAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	// Get count tokens URL from environment (like deepmining-agent)
	countURL := os.Getenv("CLAUDE_COUNT_TOKENS_URL")

	return &ClaudeTokenizer{
		model:    model,
		apiKey:   apiKey,
		countURL: countURL,
		httpClient: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
		fallback: &EstimateTokenizer{
			model:              model,
			charsPerTokenCJK:   cjk,
			charsPerTokenASCII: ascii,
		},
	}
}

// Claude model mapping to Vertex AI format
var claudeModelMapping = map[string]string{
	"claude-sonnet-4-5":          "claude-sonnet-4-5@20250929",
	"claude-sonnet-4":            "claude-sonnet-4@20250514",
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-opus-4-5":            "claude-opus-4-5-20251101",
	"claude-opus-4-6":            "claude-opus-4-6",
	"claude-3-7-sonnet":          "claude-3-7-sonnet@20250219",
	"claude-3-5-sonnet":          "claude-3-5-sonnet-v2@20241022",
	"claude-sonnet-4-20250514":   "claude-sonnet-4@20250514",
	"claude-3-7-sonnet-20250219": "claude-3-7-sonnet@20250219",
	"claude-3-5-sonnet-20241022": "claude-3-5-sonnet-v2@20241022",
}

type claudeCountTokensRequest struct {
	Model    string               `json:"model"`
	Messages []claudeTokenMessage `json:"messages"`
}

type claudeTokenMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeCountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// Count returns token count using Claude Vertex AI API if available, otherwise estimates.
func (t *ClaudeTokenizer) Count(text string) int {
	if t.apiKey == "" || t.countURL == "" {
		return t.Estimate(text)
	}

	count, err := t.countViaAPI(text)
	if err != nil {
		log.Printf("[tokenizer] claude API error, falling back to estimate: %v", err)
		return t.Estimate(text)
	}
	return count
}

func (t *ClaudeTokenizer) countViaAPI(text string) (int, error) {
	// Map model name to Vertex AI format
	model := toVertexClaudeModel(t.model)

	reqBody := claudeCountTokensRequest{
		Model: model,
		Messages: []claudeTokenMessage{
			{Role: "user", Content: text},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", t.countURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-litellm-api-key", "Bearer "+t.apiKey)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	var result claudeCountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}

	return result.InputTokens, nil
}

// toVertexClaudeModel maps model names to Vertex AI format.
func toVertexClaudeModel(model string) string {
	if mapped, ok := claudeModelMapping[model]; ok {
		return mapped
	}

	// Fallback pattern matching
	m := strings.ToLower(model)
	if strings.Contains(m, "claude-sonnet-4-6") {
		return "claude-sonnet-4-6"
	}
	if strings.Contains(m, "claude-opus-4-6") {
		return "claude-opus-4-6"
	}
	if strings.Contains(m, "claude-opus-4-5") {
		return "claude-opus-4-5-20251101"
	}
	if strings.Contains(m, "claude-sonnet-4-5") {
		return "claude-sonnet-4-5@20250929"
	}
	if strings.Contains(m, "claude-sonnet-4") {
		return "claude-sonnet-4@20250514"
	}
	if strings.Contains(m, "claude-3-7-sonnet") {
		return "claude-3-7-sonnet@20250219"
	}
	if strings.Contains(m, "claude-3-5-sonnet") {
		return "claude-3-5-sonnet-v2@20241022"
	}

	return model
}

// Estimate returns estimated token count.
func (t *ClaudeTokenizer) Estimate(text string) int {
	return t.fallback.Estimate(text)
}

// IsExact returns false because Claude uses API-based counting which can fail
// and fall back to estimation. Only local tokenizers (HF, Kimi) are considered exact.
func (t *ClaudeTokenizer) IsExact() bool {
	return false
}

// ModelName returns the model name.
func (t *ClaudeTokenizer) ModelName() string {
	return t.model
}
