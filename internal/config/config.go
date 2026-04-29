package config

import (
	"os"
	"strconv"
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
	WorkerInternalPort         string
	WorkerListenAllInterfaces  string

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
	ContextWindow            int
	MaxMessagesPerParticipant int

	// Frontend
	FrontendBaseURL string

	// Worker trigger URL (API → Worker)
	WorkerTriggerURL string
}

func Load() *Config {
	return &Config{
		MySQLDSN:   envStr("MYSQL_DSN", "root:tsdd123456@tcp(localhost:3306)/dmwork_summary?charset=utf8mb4&parseTime=True&loc=Local"),
		IMMySQLDSN: envStr("IM_MYSQL_DSN", "root:tsdd123456@tcp(localhost:3306)/im?charset=utf8mb4&parseTime=True&loc=Local"),

		OctoAPIURL: envStr("OCTO_API_URL", "http://tangsengdaodaoserver:8090"),

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
		WorkerLeaseMinutes:   envInt("WORKER_TASK_LEASE_MINUTES", 10),
		WorkerMaxRetry:       envInt("WORKER_MAX_RETRY", 3),
		WorkerCallbackURL:    envStr("WORKER_API_CALLBACK_URL", "http://127.0.0.1:8081/internal/task-event"),

		MsgTableCount: envInt("MSG_TABLE_COUNT", 5),

		ContextWindow:            envInt("CONTEXT_WINDOW", 2),
		MaxMessagesPerParticipant: envInt("MAX_MESSAGES_PER_PARTICIPANT", 5000),

		FrontendBaseURL:  envStr("FRONTEND_BASE_URL", "http://localhost:3000"),
		WorkerTriggerURL: envStr("WORKER_TRIGGER_URL", "http://summary-worker:8082/internal/worker-trigger"),
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
