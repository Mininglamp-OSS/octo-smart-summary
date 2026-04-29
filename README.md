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

| Variable | Description | Example |
|---|---|---|
| `MYSQL_DSN` | Summary database connection string | `user:pass@tcp(host:3306)/dmwork_summary?charset=utf8mb4&parseTime=True&loc=Local` |
| `IM_MYSQL_DSN` | IM database connection string (read-only) | `user:pass@tcp(host:3306)/im?charset=utf8mb4&parseTime=True&loc=Local` |
| `REDIS_ADDR` | Redis address | `localhost:6379` |
| `REDIS_DB` | Redis database number | `0` |
| `LLM_API_URL` | OpenAI-compatible API endpoint | `https://api.openai.com/v1` |
| `LLM_API_KEY` | LLM API key | |
| `LLM_MODEL` | LLM model name | `gpt-4o` |
| `LLM_MAX_TOKENS` | Max tokens for LLM response | `4096` |
| `WORKER_MAX_CONCURRENT_TASKS` | Max parallel summarization tasks | `20` |
| `WORKER_MAP_CONCURRENCY` | Concurrency for map-reduce stage | `5` |
| `WORKER_POLL_INTERVAL_SECONDS` | Task queue poll interval (seconds) | `2` |
| `WORKER_TASK_LEASE_MINUTES` | Task lease duration (minutes) | `10` |
| `WORKER_MAX_RETRY` | Max retry attempts for failed tasks | `3` |
| `WORKER_API_CALLBACK_URL` | Callback URL for task completion | `http://summary-api:8081/internal/task-event` |
