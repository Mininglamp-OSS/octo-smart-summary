# Configuration

All configuration is done via environment variables.

## Environment Variables

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `MYSQL_DSN` | MySQL DSN for the summary database | Yes | — |
| `IM_MYSQL_DSN` | MySQL DSN for the IM database (read-only channel/member metadata) | Yes | — |
| `MESSAGE_FETCH_BACKEND` | Message content backend: `batch` or `mysql` | No | `batch` |
| `OCTO_SEARCH_URL` | octo-search-batch base URL for message export (without `/v1`) | Yes when `MESSAGE_FETCH_BACKEND=batch` | — |
| `OCTO_SEARCH_TOKEN` | S2S bearer token for octo-search-batch | Yes when `MESSAGE_FETCH_BACKEND=batch` | — |
| `OCTO_API_URL` | Authentication API base URL | Yes (API) | — |
| `LLM_API_URL` | LLM gateway base URL (OpenAI-compatible) | Yes (Worker) | — |
| `LLM_API_KEY` | API key for the LLM gateway | Yes (Worker) | — |
| `LLM_MODEL` | Model identifier to use for summarization | Yes | — |
| `LLM_TIMEOUT` | LLM request timeout in seconds | No | `180` |
| `LLM_MAX_TOKENS` | Maximum tokens for LLM response | No | `4096` |
| `LLM_TEMPERATURE` | Sampling temperature for LLM | No | `0.3` |
| `LLM_ENABLE_THINKING` | Enable extended thinking mode | No | `false` |
| `API_PORT` | Port for the public API server | No | `8080` |
| `API_INTERNAL_PORT` | Port for the API internal server | No | `8081` |
| `WORKER_INTERNAL_PORT` | Port for the worker internal server | No | `8082` |
| `WORKER_LISTEN_ADDR` | Listen address for worker server | No | `0.0.0.0` |
| `WORKER_MAX_CONCURRENT_TASKS` | Max concurrent worker tasks | No | `20` |
| `WORKER_MAP_CONCURRENCY` | Concurrency for map-phase LLM calls | No | `5` |
| `WORKER_POLL_INTERVAL_SECONDS` | Task polling interval in seconds | No | `2` |
| `WORKER_TASK_LEASE_MINUTES` | Task lease duration in minutes | No | `20` |
| `WORKER_MAX_RETRY` | Maximum retry attempts for failed tasks | No | `3` |
| `WORKER_API_CALLBACK_URL` | Callback URL from worker to API | Yes (Worker) | — |
| `WORKER_TRIGGER_URL` | URL for API to trigger worker | Yes (API) | — |
| `MSG_TABLE_COUNT` | Number of message sharding tables | No | `5` |
| `CONTEXT_WINDOW` | Context window for personal summary filtering | No | `2` |
| `MAX_MESSAGES_PER_PARTICIPANT` | Max messages per participant in map phase | No | `5000` |
| `MAX_MESSAGES_PER_CHANNEL` | Max messages per channel (-1 = no limit) | No | `-1` |
| `MAP_MAX_TOKENS` | Override map-phase token budget (0 = auto) | No | `0` |
| `CHARS_PER_TOKEN_CJK` | Characters per token for CJK text | No | `1` |
| `CHARS_PER_TOKEN_ASCII` | Characters per token for ASCII text | No | `4` |
| `SUMMARY_CHAT_CANDIDATE_LIMIT` | Candidate query limit (-1 = no limit) | No | `-1` |
| `FETCH_CONCURRENCY` | Parallel channel message fetch concurrency | No | `10` |
| `CHANNEL_SCOPE_ENABLED` | Enable channel scope narrowing | No | `true` |
| `TOOL_CALL_TIMEOUT` | Tool call per-attempt timeout in seconds | No | `30` |

## LLM Gateway Options

The `LLM_API_URL` should point to any OpenAI-compatible chat completions endpoint. Supported gateway types:

- **OpenAI-compatible gateway** — Any proxy or gateway that implements the `/v1/chat/completions` API
- **Claude API (via OpenAI-compatible proxy)** — Anthropic Claude models accessed through a compatible adapter
- **Qwen API** — Alibaba Cloud Qwen models via their OpenAI-compatible endpoint
- **DeepSeek API** — DeepSeek models via their OpenAI-compatible endpoint

Set `LLM_API_URL` to your gateway's base URL (e.g., `https://your-gateway.example.com/v1`) and `LLM_API_KEY` to the corresponding API key.

## Supported Models

The following model identifiers are tested and supported:

| Model | Provider | Notes |
|-------|----------|-------|
| `claude-sonnet-4-6` | Anthropic | Balanced performance and cost |
| `claude-opus-4-6` | Anthropic | Highest capability |
| `claude-haiku-4-5` | Anthropic | Fast and cost-efficient |
| `qwen3.6-max` | Alibaba Cloud | Large context window |
| `qwen3.6-plus` | Alibaba Cloud | Balanced |
| `qwen3.6-flash` | Alibaba Cloud | Fast inference |
| `deepseek-v4-flash` | DeepSeek | Fast inference |
| `deepseek-v4-pro` | DeepSeek | Higher capability |

The system automatically adjusts token budgets and tokenization ratios based on the selected model.
