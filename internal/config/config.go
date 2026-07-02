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
	// ScheduleMaxWindowDays caps the start of a type=4 (incremental) summary
	// window at now-ScheduleMaxWindowDays. Pure defense: a frozen last_run_at
	// (e.g. a long Processing overlap) cannot blow up a single run's message
	// volume. <=0 disables the cap.
	ScheduleMaxWindowDays int
	WorkerCallbackURL     string

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

	// Message content fetch backend: "batch" or "mysql".
	MessageFetchBackend string
	OctoSearchURL       string
	OctoSearchToken     string

	// Channel scope narrowing
	ChannelScopeEnabled bool

	// Tool call per-attempt timeout (seconds)
	ToolCallTimeout int

	// FeatureTeamSchedule gates multi-participant scheduled summaries across both the
	// API guards and the worker scheduler. Default ON: API accepts multi-person
	// schedules and the worker runs them via the multi-person execution path. Set
	// FEATURE_TEAM_SCHEDULE=false to restore the legacy single-person-only behavior
	// (API rejects multi-person schedules with 40015, worker disables them).
	FeatureTeamSchedule bool

	// Intent recognition shortcut (skip LLM for simple topics)
	EnableIntentShortcut bool

	// Hardcoded limits (now configurable)
	MaxSafetyLimit       int // Max messages per channel before safety truncation, default 100000
	DefaultTimeRangeDays int // Default time range in days when not specified, default 31

	// Token calculation config
	SkipMapReduceThreshold int    // Skip Map-Reduce threshold (tokens), env SKIP_MAP_REDUCE_THRESHOLD
	KimiAPIKey             string // Kimi API key for exact token counting, env KIMI_API_KEY
	TokenizerHTTPTimeout   int    // HTTP timeout for tokenizer API calls, env TOKENIZER_HTTP_TIMEOUT
}

func Load() *Config {
	return &Config{
		MySQLDSN:   envStr("MYSQL_DSN", ""),
		IMMySQLDSN: envStr("IM_MYSQL_DSN", ""),

		OctoAPIURL: envStr("OCTO_API_URL", ""),

		LLMApiURL:         envStr("LLM_API_URL", ""),
		LLMApiKey:         envStr("LLM_API_KEY", ""),
		LLMModel:          envStr("LLM_MODEL", ""),
		LLMTimeout:        envInt("LLM_TIMEOUT", 180),
		LLMMaxToken:       envInt("LLM_MAX_TOKENS", 4096),
		LLMTemperature:    getEnvFloat("LLM_TEMPERATURE", 0.3),
		LLMEnableThinking: envBool("LLM_ENABLE_THINKING", false),

		APIPort:         envStr("API_PORT", "8080"),
		APIInternalPort: envStr("API_INTERNAL_PORT", "8081"),

		WorkerInternalPort:        envStr("WORKER_INTERNAL_PORT", "8082"),
		WorkerListenAllInterfaces: envStr("WORKER_LISTEN_ADDR", "0.0.0.0"),

		WorkerMaxConcurrent:   envInt("WORKER_MAX_CONCURRENT_TASKS", 20),
		WorkerMapConcurrency:  envInt("WORKER_MAP_CONCURRENCY", 5),
		WorkerPollInterval:    envInt("WORKER_POLL_INTERVAL_SECONDS", 2),
		WorkerLeaseMinutes:    envInt("WORKER_TASK_LEASE_MINUTES", 20),
		WorkerMaxRetry:        envInt("WORKER_MAX_RETRY", 3),
		ScheduleMaxWindowDays: envInt("SCHEDULE_MAX_WINDOW_DAYS", 30),
		WorkerCallbackURL:     envStr("WORKER_API_CALLBACK_URL", ""),

		MsgTableCount: envInt("MSG_TABLE_COUNT", 5),

		ContextWindow:             envInt("CONTEXT_WINDOW", 2),
		MaxMessagesPerParticipant: envInt("MAX_MESSAGES_PER_PARTICIPANT", 5000),
		MaxMessagesPerChannel:     envInt("MAX_MESSAGES_PER_CHANNEL", -1),
		MapMaxTokens:              envInt("MAP_MAX_TOKENS", 0),
		CharsPerTokenCJK:          envInt("CHARS_PER_TOKEN_CJK", 1),
		CharsPerTokenASCII:        envInt("CHARS_PER_TOKEN_ASCII", 4),

		WorkerTriggerURL: envStr("WORKER_TRIGGER_URL", ""),

		CandidateQueryLimit: envInt("SUMMARY_CHAT_CANDIDATE_LIMIT", -1),

		FetchConcurrency: envInt("FETCH_CONCURRENCY", 10),

		MessageFetchBackend: strings.ToLower(strings.TrimSpace(envStr("MESSAGE_FETCH_BACKEND", "batch"))),
		OctoSearchURL:       envStr("OCTO_SEARCH_URL", ""),
		OctoSearchToken:     envStr("OCTO_SEARCH_TOKEN", ""),

		ChannelScopeEnabled: envBool("CHANNEL_SCOPE_ENABLED", true),

		ToolCallTimeout: envInt("TOOL_CALL_TIMEOUT", 30),

		FeatureTeamSchedule: envBool("FEATURE_TEAM_SCHEDULE", true),

		EnableIntentShortcut: envBool("ENABLE_INTENT_SHORTCUT", true),

		MaxSafetyLimit:       envInt("MAX_SAFETY_LIMIT", 100000),
		DefaultTimeRangeDays: envInt("DEFAULT_TIME_RANGE_DAYS", 31),

		SkipMapReduceThreshold: envInt("SKIP_MAP_REDUCE_THRESHOLD", 0),
		KimiAPIKey:             envStr("KIMI_API_KEY", ""),
		TokenizerHTTPTimeout:   envInt("TOKENIZER_HTTP_TIMEOUT", 10),
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

// modelMapThresholds is an ordered list of model patterns for Map-phase token budget.
// Thresholds are tuned per-model based on production testing to balance summary quality
// and LLM call efficiency:
// - Claude: tiered by model capacity (Opus > Sonnet > Haiku)
// - Qwen/DeepSeek: optimized for large context with safety margin
// - Kimi: conservative due to 265K context limit
// More specific patterns must come before less specific ones.
var modelMapThresholds = []modelThreshold{
	// Claude models - specific versions first
	{"claude-sonnet-4-6", 250000},
	{"claude-opus-4-6", 350000},
	{"claude-haiku-4-5", 200000},
	// Qwen models
	{"qwen3.6-max", 300000},
	{"qwen3.6-plus", 300000},
	{"qwen3.6-flash", 300000},
	// DeepSeek models
	{"deepseek-v4-flash", 300000},
	{"deepseek-v4-pro", 300000},
	// Kimi models
	{"kimi-k2", 150000},
	{"kimi_k2", 150000},
	// GLM models
	{"glm-5.2", 300000},
}

const defaultMapMaxTokens = 100000

// ResolveCharsPerTokenCJK returns the CJK chars-per-token ratio.
// For qwen/deepseek/kimi models, defaults to 2 if not explicitly configured.
// For other models, uses the configured value (default 1).
func (c *Config) ResolveCharsPerTokenCJK() int {
	if os.Getenv("CHARS_PER_TOKEN_CJK") != "" {
		return c.CharsPerTokenCJK
	}
	m := strings.ToLower(c.LLMModel)
	if IsKimiModel(m) || IsQwenOrDeepSeekModel(m) {
		return 2
	}
	return c.CharsPerTokenCJK
}

// ResolveMapMaxTokens returns the Map-phase token budget using three-tier fallback:
// 1. Explicit MapMaxTokens config (> 0)
// 2. Per-model default from modelMapThresholds (ordered, first match wins)
// 3. Global default (defaultMapMaxTokens)
func (c *Config) ResolveMapMaxTokens() int {
	if c.MapMaxTokens > 0 {
		return c.MapMaxTokens
	}
	model := strings.ToLower(c.LLMModel)
	for _, mt := range modelMapThresholds {
		if strings.Contains(model, mt.pattern) {
			return mt.threshold
		}
	}
	return defaultMapMaxTokens
}

// modelThreshold is a model name pattern and its associated token threshold.
type modelThreshold struct {
	pattern   string
	threshold int
}

// modelSkipThresholds is an ordered list of model patterns for skip-Map-Reduce threshold.
// More specific patterns must come before less specific ones (e.g., "gpt-4o" before "gpt-4").
var modelSkipThresholds = []modelThreshold{
	// Qwen models
	{"qwen3.6-max", 500000},
	{"qwen3.6-plus", 500000},
	{"qwen3.6-flash", 500000},
	// DeepSeek models
	{"deepseek-v4-flash", 500000},
	{"deepseek-v4-pro", 500000},
	// Claude models
	{"claude-sonnet", 500000},
	{"claude-opus", 500000},
	{"claude-haiku", 500000},
	// OpenAI models - more specific first
	{"gpt-4o", 500000},
	{"gpt-4", 500000},
	// Kimi models
	{"kimi-k2", 200000}, // Leave headroom for Kimi's 265K context.
	{"kimi_k2", 200000},
}

const defaultSkipMapReduceThreshold = 500000

// ResolveSkipMapReduceThreshold returns the skip-Map-Reduce token threshold using three-tier fallback:
// 1. Explicit SkipMapReduceThreshold config (> 0)
// 2. Per-model default from modelSkipThresholds (ordered, first match wins)
// 3. Global default (defaultSkipMapReduceThreshold)
func (c *Config) ResolveSkipMapReduceThreshold() int {
	if c.SkipMapReduceThreshold > 0 {
		return c.SkipMapReduceThreshold
	}
	model := strings.ToLower(c.LLMModel)
	for _, mt := range modelSkipThresholds {
		if strings.Contains(model, mt.pattern) {
			return mt.threshold
		}
	}
	return defaultSkipMapReduceThreshold
}
