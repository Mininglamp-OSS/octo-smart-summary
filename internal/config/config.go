package config

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// Agent-summary V2 rollout modes + normalizer live here (not in internal/agent)
// because agent already imports config for other wiring; putting the
// normalizer in agent would create an import cycle. Both read paths of
// AGENT_SUMMARY_V2_MODE (the config field and agent.SummaryV2Mode) go through
// one function so they can never disagree on case, whitespace, or unknown
// values.
const (
	AgentSummaryV2Off    = "off"
	AgentSummaryV2Shadow = "shadow"
	AgentSummaryV2On     = "on"
)

// NormalizeAgentSummaryV2Mode trims and lowercases the raw env value, mapping
// "shadow"/"on" to themselves and everything else (unset, empty, unknown) to
// "off" — fail safe, never fail into the new path.
func NormalizeAgentSummaryV2Mode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case AgentSummaryV2Shadow:
		return AgentSummaryV2Shadow
	case AgentSummaryV2On:
		return AgentSummaryV2On
	default:
		return AgentSummaryV2Off
	}
}

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
	LLMApiURL string
	LLMApiKey string
	LLMModel  string
	// LLMFallbackModels is an ordered list of model identifiers to try when
	// the primary LLMModel exhausts its retry budget on a Chat request.
	// Sourced from LLM_FALLBACK_MODELS as a comma-separated list. Empty by
	// default (no cross-model fallback), preserving current single-model
	// behavior. See issue #179.
	LLMFallbackModels []string
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

	// AgentMapConcurrency bounds how many Map-phase chunk summaries a single
	// summarize_chunk tool call runs in parallel (agent path). Deliberately
	// SEPARATE from WorkerMapConcurrency: the agent path already runs up to 4
	// tool calls concurrently (agent.NewPool(4) in agent_chat.go), so the two
	// knobs multiply and must be tuned independently.
	AgentMapConcurrency int

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
	// SummaryStreamInternalToken optionally authenticates worker -> API
	// POST /internal/summary-stream via X-Internal-Token. Empty preserves the
	// existing internal-network trust model used by other internal endpoints.
	SummaryStreamInternalToken string

	// AgentSummaryV2Mode gates the SS-03+ SummaryRun/SummarySpec persistence
	// path: "off" (default) keeps pre-SS-03 behavior, "shadow" persists runs
	// without changing the reply, "on" is the enabled path. Read from
	// AGENT_SUMMARY_V2_MODE and normalized through NormalizeAgentSummaryV2Mode
	// — the same function agent.SummaryV2Mode() uses, so the two read paths
	// can never disagree on case/whitespace/unknown values.
	AgentSummaryV2Mode string

	// Message table count
	MsgTableCount int

	// Context window for personal summary filtering
	ContextWindow             int
	MaxMessagesPerParticipant int
	MaxMessagesPerChannel     int
	MapMaxTokens              int
	CharsPerTokenCJK          int
	CharsPerTokenASCII        int

	// Worker trigger URL (API -> Worker)
	WorkerTriggerURL string

	// Candidate search query limit (-1 = no limit, >0 = use as SQL LIMIT)
	CandidateQueryLimit int

	// SummaryCustomTemplateLimit caps per-user custom summary templates per space.
	// Default 30. Values <=0 fall back to the default in handler.
	SummaryCustomTemplateLimit int

	// Fetch concurrency for parallel channel message retrieval
	FetchConcurrency int

	// Message content fetch backend: "batch" or "mysql".
	MessageFetchBackend string
	OctoSearchURL       string
	OctoSearchToken     string
	OctoSearchPollSec   int

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

	// --- task-terminal notification (delivery via octo-server internal-notify) ---

	// NotifyEnabled is the master switch for delivering a terminal-state
	// notification (Completed / Failed) to the task's recipients. Default OFF so
	// the feature can be dark-launched.
	NotifyEnabled bool
	// NotifyInternalToken authenticates POST /v1/internal/notify via the
	// X-Internal-Token header (constant-time compared server-side). SECRET: read
	// from env only, never printed/logged, never written to
	// summary_notification.last_error. Prod + NotifyEnabled but empty => startup
	// fatal (fail-fast on misconfig); dev only warns.
	NotifyInternalToken string
	// SummaryWebBaseURL is retained for compatibility; the server card path builds deep links.
	SummaryWebBaseURL string
	// AppEnv is the deployment environment ("prod" | "dev"). Drives the
	// prod-fatal / dev-warn handling of a missing NotifyInternalToken.
	AppEnv string
	// MaxNotifyAttempts caps same-row retries for a single (task_id, notify_kind)
	// notification before it is left in status='failed'. Default 3.
	MaxNotifyAttempts int
}

func Load() *Config {
	return &Config{
		MySQLDSN:   envStr("MYSQL_DSN", ""),
		IMMySQLDSN: envStr("IM_MYSQL_DSN", ""),

		OctoAPIURL: envStr("OCTO_API_URL", ""),

		LLMApiURL: envStr("LLM_API_URL", ""),
		LLMApiKey: envStr("LLM_API_KEY", ""),
		LLMModel:  envStr("LLM_MODEL", ""),

		AgentSummaryV2Mode: NormalizeAgentSummaryV2Mode(os.Getenv("AGENT_SUMMARY_V2_MODE")),
		LLMFallbackModels:  envStrList("LLM_FALLBACK_MODELS", nil),
		LLMTimeout:         envInt("LLM_TIMEOUT", 180),
		LLMMaxToken:        envInt("LLM_MAX_TOKENS", 4096),
		LLMTemperature:     getEnvFloat("LLM_TEMPERATURE", 0.3),
		LLMEnableThinking:  envBool("LLM_ENABLE_THINKING", false),
		APIPort:            envStr("API_PORT", "8080"),
		APIInternalPort:    envStr("API_INTERNAL_PORT", "8081"),

		WorkerInternalPort:        envStr("WORKER_INTERNAL_PORT", "8082"),
		WorkerListenAllInterfaces: envStr("WORKER_LISTEN_ADDR", "0.0.0.0"),

		AgentMapConcurrency: envInt("AGENT_MAP_CONCURRENCY", defaultAgentMapConcurrency),

		WorkerMaxConcurrent:        envInt("WORKER_MAX_CONCURRENT_TASKS", 20),
		WorkerMapConcurrency:       envInt("WORKER_MAP_CONCURRENCY", 5),
		WorkerPollInterval:         envInt("WORKER_POLL_INTERVAL_SECONDS", 2),
		WorkerLeaseMinutes:         envInt("WORKER_TASK_LEASE_MINUTES", 20),
		WorkerMaxRetry:             envInt("WORKER_MAX_RETRY", 3),
		ScheduleMaxWindowDays:      envInt("SCHEDULE_MAX_WINDOW_DAYS", 30),
		WorkerCallbackURL:          envStr("WORKER_API_CALLBACK_URL", ""),
		SummaryStreamInternalToken: envStr("SUMMARY_STREAM_INTERNAL_TOKEN", ""),

		MsgTableCount: envInt("MSG_TABLE_COUNT", 5),

		ContextWindow:             envInt("CONTEXT_WINDOW", 2),
		MaxMessagesPerParticipant: envInt("MAX_MESSAGES_PER_PARTICIPANT", 5000),
		MaxMessagesPerChannel:     envInt("MAX_MESSAGES_PER_CHANNEL", -1),
		MapMaxTokens:              envInt("MAP_MAX_TOKENS", 0),
		CharsPerTokenCJK:          envInt("CHARS_PER_TOKEN_CJK", 1),
		CharsPerTokenASCII:        envInt("CHARS_PER_TOKEN_ASCII", 4),

		WorkerTriggerURL: envStr("WORKER_TRIGGER_URL", ""),

		CandidateQueryLimit: envInt("SUMMARY_CHAT_CANDIDATE_LIMIT", -1),

		SummaryCustomTemplateLimit: envInt("SUMMARY_CUSTOM_TEMPLATE_LIMIT", 30),

		FetchConcurrency: envInt("FETCH_CONCURRENCY", 10),

		MessageFetchBackend: strings.ToLower(strings.TrimSpace(envStr("MESSAGE_FETCH_BACKEND", "batch"))),
		OctoSearchURL:       envStr("OCTO_SEARCH_URL", ""),
		OctoSearchToken:     envStr("OCTO_SEARCH_TOKEN", ""),
		OctoSearchPollSec:   envInt("OCTO_SEARCH_POLL_INTERVAL_SECONDS", 1),

		ChannelScopeEnabled: envBool("CHANNEL_SCOPE_ENABLED", true),

		ToolCallTimeout: envInt("TOOL_CALL_TIMEOUT", 30),

		// keep upstream main default (true, #112)
		FeatureTeamSchedule: envBool("FEATURE_TEAM_SCHEDULE", true),

		EnableIntentShortcut: envBool("ENABLE_INTENT_SHORTCUT", true),

		MaxSafetyLimit:       envInt("MAX_SAFETY_LIMIT", 100000),
		DefaultTimeRangeDays: envInt("DEFAULT_TIME_RANGE_DAYS", 31),

		SkipMapReduceThreshold: envInt("SKIP_MAP_REDUCE_THRESHOLD", 0),
		KimiAPIKey:             envStr("KIMI_API_KEY", ""),
		TokenizerHTTPTimeout:   envInt("TOKENIZER_HTTP_TIMEOUT", 10),

		NotifyEnabled: envBool("SUMMARY_NOTIFY_ENABLED", false),
		// Custom env name (老板拍板): distinct from matter's NOTIFY_INTERNAL_TOKEN.
		// Header contract X-Internal-Token is unchanged; only the source env differs.
		NotifyInternalToken: envStr("SUMMARY_NOTIFY_TOKEN", ""),
		SummaryWebBaseURL:   envStr("SUMMARY_WEB_BASE_URL", ""),
		AppEnv:              envStr("APP_ENV", "dev"),
		MaxNotifyAttempts:   envInt("MAX_NOTIFY_ATTEMPTS", 3),
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

// envStrList reads a comma-separated environment variable into a slice of
// trimmed, non-empty strings. Returns def when the variable is unset or
// contains only whitespace / empty entries.
//
// Consumers that treat "no value" and "explicit empty list" the same (which
// is currently the case for LLM_FALLBACK_MODELS) can rely on this returning
// def in both cases.
func envStrList(key string, def []string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
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

const (
	// defaultAgentMapConcurrency is the agent Map-phase fan-out default.
	//
	// 3, not the worker path's 5: the agent runner executes up to 4 tool calls
	// from one LLM turn concurrently (agent.NewPool(4)), and each of those can
	// be a summarize_chunk. The knobs multiply, so the real ceiling of in-flight
	// Map calls is poolSize * AgentMapConcurrency = 12 at this default. The
	// worker path has no such outer fan-out, which is why its default can be
	// higher.
	defaultAgentMapConcurrency = 3
	// maxAgentMapConcurrency caps operator input. Above this the gateway sees
	// 4*N concurrent completions from a single user request.
	maxAgentMapConcurrency = 5
)

// ResolveAgentMapConcurrency returns the effective agent Map fan-out, clamped
// to [1, maxAgentMapConcurrency].
//
// 1 is the serial fallback: it makes the concurrent path byte-equivalent to the
// pre-change loop (one worker, one in-flight call, same order, same error), so
// an operator can revert the behaviour with an env var instead of a rollback.
// Values <= 0 (unset / malformed / explicitly disabled) resolve to the default
// rather than to 0, which would deadlock a zero-capacity semaphore.
func (c *Config) ResolveAgentMapConcurrency() int {
	n := c.AgentMapConcurrency
	if n <= 0 {
		n = defaultAgentMapConcurrency
	}
	if n > maxAgentMapConcurrency {
		n = maxAgentMapConcurrency
	}
	return n
}

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
