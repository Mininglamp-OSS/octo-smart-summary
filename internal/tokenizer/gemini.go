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
	geminiCharsPerTokenCJK = 1 // Gemini uses ~1 char per token for CJK
)

// GeminiTokenizer counts tokens for Gemini models using Vertex AI API via LiteLLM proxy.
type GeminiTokenizer struct {
	model      string
	apiKey     string
	baseURL    string
	projectID  string
	location   string
	httpClient *http.Client
	fallback   *EstimateTokenizer
}

// NewGeminiTokenizer creates a new GeminiTokenizer.
func NewGeminiTokenizer(model string, cfg Config) *GeminiTokenizer {
	cjk := cfg.CharsPerTokenCJK
	if cjk <= 0 {
		cjk = geminiCharsPerTokenCJK
	}
	ascii := cfg.CharsPerTokenASCII
	if ascii <= 0 {
		ascii = defaultCharsPerTokenASCII
	}

	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}

	// Get configuration from environment (like deepmining-agent)
	apiKey := cfg.GeminiAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	baseURL := os.Getenv("OPENAI_API_BASE")

	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		projectID = "clau-so37-20250526" // Default project
	}

	location := os.Getenv("GOOGLE_CLOUD_LOCATION")
	if location == "" {
		location = "us-east5" // Default location
	}

	return &GeminiTokenizer{
		model:     model,
		apiKey:    apiKey,
		baseURL:   baseURL,
		projectID: projectID,
		location:  location,
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

// Gemini model mapping
var geminiModelMapping = map[string]string{
	"gemini-3-pro":       "gemini-3-pro-preview",
	"gemini-3.5-flash":   "gemini-2.5-flash-preview-05-20",
	"gemini-2.5-flash":   "gemini-2.5-flash-preview-05-20",
	"gemini-2-flash":     "gemini-2.0-flash",
	"gemini-1.5-pro":     "gemini-1.5-pro",
	"gemini-1.5-flash":   "gemini-1.5-flash",
}

// Gemini API request/response structures
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiCountTokensRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiCountTokensResponse struct {
	TotalTokens int `json:"totalTokens"`
}

// Count returns token count using Gemini Vertex AI API if available, otherwise estimates.
func (t *GeminiTokenizer) Count(text string) int {
	if t.apiKey == "" || t.baseURL == "" {
		return t.Estimate(text)
	}

	count, err := t.countViaAPI(text)
	if err != nil {
		log.Printf("[tokenizer] gemini API error, falling back to estimate: %v", err)
		return t.Estimate(text)
	}
	return count
}

func (t *GeminiTokenizer) countViaAPI(text string) (int, error) {
	// Map model name to Vertex AI format
	model := toVertexGeminiModel(t.model)

	// Build Vertex AI countTokens URL (like deepmining-agent)
	// Format: {base}/vertex_ai/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:countTokens
	baseURL := strings.TrimSuffix(t.baseURL, "/v1")
	url := fmt.Sprintf("%s/vertex_ai/v1/projects/%s/locations/%s/publishers/google/models/%s:countTokens",
		baseURL, t.projectID, t.location, model)

	reqBody := geminiCountTokensRequest{
		Contents: []geminiContent{
			{
				Role: "user",
				Parts: []geminiPart{
					{Text: text},
				},
			},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
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

	var result geminiCountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}

	return result.TotalTokens, nil
}

// toVertexGeminiModel maps model names to Vertex AI format.
func toVertexGeminiModel(model string) string {
	if mapped, ok := geminiModelMapping[model]; ok {
		return mapped
	}
	return model
}

// Estimate returns estimated token count.
func (t *GeminiTokenizer) Estimate(text string) int {
	return t.fallback.Estimate(text)
}

// IsExact returns true if API key and base URL are available.
func (t *GeminiTokenizer) IsExact() bool {
	return t.apiKey != "" && t.baseURL != ""
}

// ModelName returns the model name.
func (t *GeminiTokenizer) ModelName() string {
	return t.model
}
