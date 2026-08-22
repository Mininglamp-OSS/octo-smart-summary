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
| `OCTO_SEARCH_POLL_INTERVAL_SECONDS` | Poll interval for octo-search-batch task status | No | `1` |
| `OCTO_API_URL` | Authentication API base URL | Yes (API) | — |
| `LLM_API_URL` | LLM gateway base URL (OpenAI-compatible) | Yes (Worker) | — |
| `LLM_API_KEY` | API key for the LLM gateway | Yes (Worker) | — |
| `LLM_MODEL` | Model identifier to use for summarization | Yes | — |
| `LLM_FALLBACK_MODELS` | Comma-separated ordered list of models to try when the primary `LLM_MODEL` fails. Applies to the summary worker's Map/Reduce (`Call`/`CallStream`), agent chat, agent tools (merge/narrow/summarize), and API refine. Each model retries transient network/429/5xx failures up to 3 times before the next model is tried; **403 account-level denial** (for example a Bedrock SCP explicit-deny) switches models immediately, while 401 and other terminal failures stop. Empty preserves single-model behavior. Example: `claude-sonnet-4-6,gpt-4.1`. Fallback only survives a provider/account outage when at least one fallback is routed by `LLM_API_URL` to a **different backend/credential** (all models otherwise share `LLM_API_KEY`). Map/Reduce selects independently per chunk, so mixed-model output is possible; cross-chunk stickiness is not provided. Worker/API paths have no aggregate fallback deadline, so latency grows with the model count. API refine has a 90-second request deadline: if a hanging primary consumes it, no fallback attempt can start. Keep the list short and size upstream/task deadlines accordingly. For agent chat, keep `maxAttempts*LLM_TIMEOUT + backoffs (3s) <= AGENT_STEP_TIMEOUT`; it escalates early when the remaining deadline cannot fit both the pending retry and a fallback attempt. | No | — |
| `LLM_TIMEOUT` | LLM request timeout in seconds | No | `180` |
| `AGENT_STEP_TIMEOUT` | Maximum duration in seconds for one agent planning step; streaming requests remain capped by the 300-second request deadline | No | `240` |
| `AGENT_SUMMARY_V2_MODE` | Rollout flag for the Stage-2 agent-summary contract. `off` (default) keeps the pre-SS-03 path byte-identical: no run row is persisted and no new query runs. `shadow` / `on` enable the V2 path: each submit persists a SummaryRun/SummarySpec keyed by `(user_id, session_id, request_id)`, the citation manifest is frozen once per run, the save-time citation pass adopts the frozen ordinals of the run named by the request's `request_id` (falling back to the legacy recompute when the request carries none), and messages fetched after the freeze are dropped from chunk input instead of reaching the model as citation `[0]`. `shadow` and `on` behave identically today; the split is reserved for later stages. Unrecognized values fall back to `off` (fail safe — never fail into the new path). See `internal/agent/summary_v2.go`. | No | `off` |
| `LLM_MAX_TOKENS` | Maximum tokens for LLM response | No | `4096` |
| `LLM_TEMPERATURE` | Sampling temperature for LLM | No | `0.3` |
| `LLM_ENABLE_THINKING` | Enable extended thinking mode | No | `false` |
| `API_PORT` | Port for the public API server | No | `8080` |
| `API_INTERNAL_PORT` | Port for the API internal server | No | `8081` |
| `WORKER_INTERNAL_PORT` | Port for the worker internal server | No | `8082` |
| `WORKER_LISTEN_ADDR` | Listen address for worker server | No | `0.0.0.0` |
| `WORKER_MAX_CONCURRENT_TASKS` | Max concurrent worker tasks | No | `20` |
| `WORKER_MAP_CONCURRENCY` | Concurrency for map-phase LLM calls | No | `3` |
| `WORKER_POLL_INTERVAL_SECONDS` | Task polling interval in seconds | No | `2` |
| `WORKER_TASK_LEASE_MINUTES` | Task lease duration in minutes | No | `20` |
| `WORKER_MAX_RETRY` | Maximum retry attempts for failed tasks | No | `3` |
| `WORKER_API_CALLBACK_URL` | Callback URL from worker to API | Yes (Worker) | — |
| `WORKER_TRIGGER_URL` | URL for API to trigger worker | Yes (API) | — |
| `MSG_TABLE_COUNT` | Number of message sharding tables | No | `3` |
| `CONTEXT_WINDOW` | Context window for personal summary filtering | No | `2` |
| `MAX_MESSAGES_PER_PARTICIPANT` | Max messages per participant in map phase | No | `5000` |
| `MAX_MESSAGES_PER_CHANNEL` | Max messages per channel (-1 = no limit) | No | `-1` |
| `MAP_MAX_TOKENS` | Override map-phase token budget (0 = auto) | No | `0` |
| `SUMMARY_MAX_CITATIONS_PER_CLAIM` | Maximum number of `[n]` citation markers a single claim may carry. A "claim" is a maximal consecutive marker run (`[3][7][12]`, spaces/tabs tolerated); a newline starts a new claim. Duplicate markers inside one claim are removed first (lossless, keyed on the parsed number so `[1]` and `[01]` are one source) and only then is the cap applied; a capped claim always keeps at least one marker. Enforced by post-processing MODEL OUTPUT at exactly three sites, all stated in `internal/citation/cap.go`: the agent Map tool output, the agent planner's final answer, and the worker's final body (where it runs AFTER dedup/orphan-strip and the citation rows are then re-derived). Prompt input is never capped, and stream deltas are not capped — the worker reconciles the live view by publishing the final body as a stream snapshot before `done`. The same resolved number is rendered into the Map prompts and the agent planner prompt, so the model is asked for exactly what is enforced. `0` or negative disables capping entirely and restores the previous behavior byte-for-byte, prompt included (pinned by `TestDisabledCapRestoresTheLegacyMapPrompt` and `TestPlannerPromptDropsTheCapRuleWhenDisabled`). | No | `3` |
| `AGENT_TRACE` | Emit a per-request agent latency trace (`[agent-trace]` log lines): total wall clock split into planning / tools / unaccounted, per-step planner latency and prompt size, slowest tool spans, and the citation cap's effect. Roughly a few dozen lines per request, so it is off by default and intended for diagnosing a specific slow request. Logs sizes, counts, durations, step numbers and tool names only — never message content, prompt text, tool arguments, or user/channel names. | No | `false` |
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
