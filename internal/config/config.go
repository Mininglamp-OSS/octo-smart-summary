package config

import (
	"log"
	"os"
	"strconv"
	"strings"
)

func getEnvFloat(key string, defaultVal float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("[config] invalid %s=%q, using default %.2f", key, v, defaultVal)
		return defaultVal
	}
	return f
}

type Config struct {
	// MySQL (summary DB)
	MySQLDSN string
	// IM MySQL (read-only, message tables)
	IMMySQLDSN string

	// Auth
	OctoAPIURL string

	// LLM
	LLMApiURL         string
	LLMApiKey         string
	LLMModel          string
	LLMTimeout        int
	LLMMaxToken       int
	LLMTemperature    float64
	LLMEnableThinking bool

	// API
	APIPort         string
	APIInternalPort string

	// Worker internal port (separate from API internal port)
	WorkerInternalPort        string
	WorkerListenAllInterfaces string

	// Worker
	WorkerMaxConcurrent  int
	WorkerMapConcurrency int
	WorkerPollInterval   int
	WorkerLeaseMinutes   int
	WorkerMaxRetry       int
	WorkerCallbackURL    string

	// Message table count
	MsgTableCount int

	// Context window for personal summary filtering
	ContextWindow             int
	MaxMessagesPerParticipant int
	MaxMessagesPerChannel     int
	MapMaxTokens              int
	CharsPerTokenCJK          int
	CharsPerTokenASCII        int

	// Worker trigger URL (API → Worker)
	WorkerTriggerURL string

	// Candidate search query limit (-1 = no limit, >0 = use as SQL LIMIT)
	CandidateQueryLimit int

	// Fetch concurrency for parallel channel message retrieval
	FetchConcurrency int

	// Channel scope narrowing
	ChannelScopeEnabled bool

	// Tool call per-attempt timeout (seconds)
	ToolCallTimeout int
}

func Load() *Config {
	return &Config{
		MySQLDSN:   envStr("MYSQL_DSN", ""),
		IMMySQLDSN: envStr("IM_MYSQL_DSN", ""),

		OctoAPIURL: envStr("OCTO_API_URL", ""),

		LLMApiURL:         envStr("LLM_API_URL", "https://api.example.com/v1"),
		LLMApiKey:         envStr("LLM_API_KEY", ""),
		LLMModel:          envStr("LLM_MODEL", "claude-sonnet-4-6"),
		LLMTimeout:        envInt("LLM_TIMEOUT", 180),
		LLMMaxToken:       envInt("LLM_MAX_TOKENS", 4096),
		LLMTemperature:    getEnvFloat("LLM_TEMPERATURE", 0.3),
		LLMEnableThinking: envBool("LLM_ENABLE_THINKING", false),

		APIPort:         envStr("API_PORT", "8080"),
		APIInternalPort: envStr("API_INTERNAL_PORT", "8081"),

		WorkerInternalPort:        envStr("WORKER_INTERNAL_PORT", "8082"),
		WorkerListenAllInterfaces: envStr("WORKER_LISTEN_ADDR", "0.0.0.0"),

		WorkerMaxConcurrent:  envInt("WORKER_MAX_CONCURRENT_TASKS", 20),
		WorkerMapConcurrency: envInt("WORKER_MAP_CONCURRENCY", 5),
		WorkerPollInterval:   envInt("WORKER_POLL_INTERVAL_SECONDS", 2),
		WorkerLeaseMinutes:   envInt("WORKER_TASK_LEASE_MINUTES", 20),
		WorkerMaxRetry:       envInt("WORKER_MAX_RETRY", 3),
		WorkerCallbackURL:    envStr("WORKER_API_CALLBACK_URL", ""),

		MsgTableCount: envInt("MSG_TABLE_COUNT", 5),

		ContextWindow:             envInt("CONTEXT_WINDOW", 2),
		MaxMessagesPerParticipant: envInt("MAX_MESSAGES_PER_PARTICIPANT", 5000),
		MaxMessagesPerChannel:     envInt("MAX_MESSAGES_PER_CHANNEL", -1),
		MapMaxTokens:             envInt("MAP_MAX_TOKENS", 0),
		CharsPerTokenCJK:         envInt("CHARS_PER_TOKEN_CJK", 1),
		CharsPerTokenASCII:       envInt("CHARS_PER_TOKEN_ASCII", 4),

		WorkerTriggerURL: envStr("WORKER_TRIGGER_URL", ""),

		CandidateQueryLimit: envInt("SUMMARY_CHAT_CANDIDATE_LIMIT", -1),

		FetchConcurrency: envInt("FETCH_CONCURRENCY", 10),

		ChannelScopeEnabled: envBool("CHANNEL_SCOPE_ENABLED", true),

		ToolCallTimeout: envInt("TOOL_CALL_TIMEOUT", 30),
	}
}

func ValidateRequired(fields map[string]string) {
	var missing []string
	for name, value := range fields {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		log.Fatalf("[config] required environment variables not set: %s", strings.Join(missing, ", "))
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// modelMaxTokensDefaults maps LLM model names to their recommended Map-phase token budget.
var modelMaxTokensDefaults = map[string]int{
	"claude-sonnet-4-6": 150000,
	"claude-opus-4-6":   150000,
	"claude-haiku-4-5":  150000,
	"qwen3.6-max":       400000,
	"qwen3.6-plus":      400000,
	"qwen3.6-flash":     400000,
	"deepseek-v4-flash": 400000,
	"deepseek-v4-pro":   400000,
}

const defaultMapMaxTokens = 100000

// ResolveMapMaxTokens returns the Map-phase token budget using three-tier fallback:
// 1. Explicit MapMaxTokens config (> 0)
// 2. Per-model default from modelMaxTokensDefaults
// 3. Global default (defaultMapMaxTokens)
func (c *Config) ResolveMapMaxTokens() int {
	if c.MapMaxTokens > 0 {
		return c.MapMaxTokens
	}
	model := strings.ToLower(c.LLMModel)
	for key, v := range modelMaxTokensDefaults {
		if strings.Contains(model, key) {
			return v
		}
	}
	return defaultMapMaxTokens
}

// ResolveCharsPerTokenCJK returns the CJK chars-per-token ratio.
// For qwen models, defaults to 2 if not explicitly configured.
// For other models, uses the configured value (default 1).
func (c *Config) ResolveCharsPerTokenCJK() int {
	if os.Getenv("CHARS_PER_TOKEN_CJK") != "" {
		// Explicitly configured, use as-is
		return c.CharsPerTokenCJK
	}
	if strings.Contains(strings.ToLower(c.LLMModel), "qwen3.6") || strings.Contains(strings.ToLower(c.LLMModel), "deepseek-v4") {
		return 2
	}
	return c.CharsPerTokenCJK
}
