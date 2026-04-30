package config

import (
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// MySQL (summary DB)
	MySQLDSN string
	// IM MySQL (read-only, message tables)
	IMMySQLDSN string

	// Auth
	OctoAPIURL string

	// LLM
	LLMApiURL   string
	LLMApiKey   string
	LLMModel    string
	LLMTimeout  int
	LLMMaxToken int

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
	MapMaxTokens              int
	CharsPerTokenCJK          int
	CharsPerTokenASCII        int

	// Worker trigger URL (API → Worker)
	WorkerTriggerURL string
}

func Load() *Config {
	return &Config{
		MySQLDSN:   envStr("MYSQL_DSN", ""),
		IMMySQLDSN: envStr("IM_MYSQL_DSN", ""),

		OctoAPIURL: envStr("OCTO_API_URL", ""),

		LLMApiURL:   envStr("LLM_API_URL", "https://api.example.com/v1"),
		LLMApiKey:   envStr("LLM_API_KEY", ""),
		LLMModel:    envStr("LLM_MODEL", "claude-sonnet-4-6"),
		LLMTimeout:  envInt("LLM_TIMEOUT", 120),
		LLMMaxToken: envInt("LLM_MAX_TOKENS", 4096),

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
		MapMaxTokens:             envInt("MAP_MAX_TOKENS", 0),
		CharsPerTokenCJK:         envInt("CHARS_PER_TOKEN_CJK", 1),
		CharsPerTokenASCII:       envInt("CHARS_PER_TOKEN_ASCII", 4),

		WorkerTriggerURL: envStr("WORKER_TRIGGER_URL", ""),
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

// modelMaxTokensDefaults maps LLM model names to their recommended Map-phase token budget.
var modelMaxTokensDefaults = map[string]int{
	"claude-sonnet-4-6": 150000,
	"claude-opus-4-6":   150000,
	"claude-haiku-4-5":  150000,
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
	if v, ok := modelMaxTokensDefaults[c.LLMModel]; ok {
		return v
	}
	return defaultMapMaxTokens
}
