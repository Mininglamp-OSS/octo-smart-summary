# Smart Summary (智能总结)

AI-powered chat summary service for the dmwork IM platform. Generates intelligent summaries of group chat conversations using LLM APIs.

## Architecture

The service consists of two components:

- **summary-api** — HTTP API server that receives summary requests and serves results
- **summary-worker** — Background task processor that handles LLM-based summarization

## Tech Stack

Go, Gin, GORM, Redis, MySQL, LLM API (OpenAI-compatible)

## Build

```bash
go build -o bin/summary-api ./cmd/summary-api
go build -o bin/summary-worker ./cmd/summary-worker
```

## Docker Build

```bash
docker build -f Dockerfile.api -t summary-api:local .
docker build -f Dockerfile.worker -t summary-worker:local .
```

## Environment Variables

> For the full canonical reference of all configuration options, see [CONFIGURATION.md](CONFIGURATION.md).

| Variable | Description | Service | Required |
|---|---|---|---|
| `MYSQL_DSN` | Summary database connection string | both | yes |
| `IM_MYSQL_DSN` | IM database connection string (read-only) | both | yes |
| `OCTO_API_URL` | Auth/API server base URL | api | yes |
| `LLM_API_URL` | OpenAI-compatible API endpoint | worker | yes (no default) |
| `LLM_API_KEY` | LLM API key | worker | yes |
| `LLM_MODEL` | LLM model name | worker | yes (no default) |
| `LLM_TIMEOUT` | LLM request timeout in seconds | worker | no (default: `180`) |
| `LLM_MAX_TOKENS` | Max tokens for LLM response | worker | no (default: `4096`) |
| `API_PORT` | Public API listen port | api | no (default: `8080`) |
| `API_INTERNAL_PORT` | Internal API listen port (callbacks) | api | no (default: `8081`) |
| `WORKER_INTERNAL_PORT` | Worker internal listen port | worker | no (default: `8082`) |
| `WORKER_LISTEN_ADDR` | Worker bind address | worker | no (default: `0.0.0.0`) |
| `WORKER_TRIGGER_URL` | URL for API to trigger worker tasks | api | yes |
| `WORKER_API_CALLBACK_URL` | Callback URL for task completion | worker | yes |
| `WORKER_MAX_CONCURRENT_TASKS` | Max parallel summarization tasks | worker | no (default: `20`) |
| `WORKER_MAP_CONCURRENCY` | Concurrency for map-reduce stage | worker | no (default: `5`) |
| `WORKER_POLL_INTERVAL_SECONDS` | Task queue poll interval (seconds) | worker | no (default: `2`) |
| `WORKER_TASK_LEASE_MINUTES` | Task lease duration (minutes) | worker | no (default: `20`) |
| `WORKER_MAX_RETRY` | Max retry attempts for failed tasks | worker | no (default: `3`) |
| `MSG_TABLE_COUNT` | Number of message sharding tables | both | no (default: `5`) |
| `CONTEXT_WINDOW` | Context window days for personal summary | worker | no (default: `2`) |
| `MAX_MESSAGES_PER_PARTICIPANT` | Max messages per participant to process | worker | no (default: `5000`) |
