# TaskFlow

**TaskFlow** is a production-style distributed task queue and execution-tracking platform built with **Go**, **NATS JetStream**, and **PostgreSQL**.

It demonstrates reliable asynchronous processing with retries, execution audit history, and operational patterns like **Dead Letter Queues (DLQ)**.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## ✨ Highlights

- **Distributed task queue** with JetStream (**at-least-once delivery**)
- **Best-effort exactly-once behavior** via DB idempotency + task state transitions
- **Priority routing**: `tasks.high`, `tasks.normal`, `tasks.low`
- **Retries with backoff** + max attempts
- **Dead Letter Queue**: permanent failures published to `tasks.dlq`
- **Execution audit trail**: each attempt recorded (`started`, `succeeded`, `failed`)
- **Optimistic locking** to prevent duplicate/competing processing
- **REST API** for tasks + execution history
- **Graceful shutdown** (worker drains in-flight tasks)

---

## 📋 Table of Contents

- [Architecture](#-architecture)
- [Quick Start](#-quick-start)
- [API](#-api)
- [Configuration](#️-configuration)
- [Testing](#-testing)
- [Troubleshooting](#-troubleshooting)
- [Contributing](#-contributing)
- [License](#-license)

---

## 🏗 Architecture

```text
┌─────────────┐
│   Client    │
└──────┬──────┘
       │ HTTP
       ▼
┌─────────────┐      ┌──────────────┐
│  API Server │─────▶│  PostgreSQL   │
└──────┬──────┘      └──────────────┘
       │ publish task message
       ▼
┌─────────────┐
│    NATS     │
│ JetStream   │
└──────┬──────┘
       │ pull subscribe
       ▼
┌─────────────┐      ┌──────────────┐
│   Workers   │─────▶│  PostgreSQL   │
│  (Scaled)   │      └──────────────┘
└─────────────┘
```

### Task lifecycle

1. Client creates a task (`POST /api/v1/tasks`)
2. Task is persisted in PostgreSQL (`status=queued`)
3. Task ID is published to JetStream (`tasks.*`)
4. Worker fetches + processes the task
5. Worker records an execution attempt (`task_executions`)
6. Task is marked `completed` or `failed`
7. Permanent failures publish a DLQ message (`tasks.dlq`)

---

## 🚀 Quick Start

### 1) Start infrastructure (Docker)

```bash
docker compose up -d postgres nats
```

### 2) Run migrations

```bash
psql "postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable"   -f api/deployments/migrations/001_init.sql
```

### 3) Start API

```bash
go run ./cmd/api
```

### 4) Start worker (new terminal)

```bash
go run ./cmd/worker
```

### 5) Health check

```bash
curl -i http://localhost:8080/api/v1/health
```

### 6) Create a demo task

```bash
curl -X POST http://localhost:8080/api/v1/tasks   -H "Content-Type: application/json"   -d '{
    "type": "demo",
    "payload": {"hello":"world"},
    "priority": "normal"
  }'
```

---

## 📚 API

- `GET /api/v1/health`
- `POST /api/v1/tasks`
- `GET /api/v1/tasks/{id}`
- `GET /api/v1/tasks/{id}/executions`

For request/response examples, see the handlers in `api/httpapi/`.

---

## ⚙️ Configuration

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `HTTP_PORT` | `8080` | API server port |
| `DATABASE_URL` | required | PostgreSQL DSN |
| `NATS_URL` | `nats://localhost:4222` | NATS server URL |
| `NATS_STREAM_NAME` | `TASKFLOW` | Stream name |
| `NATS_CONSUMER_NAME` | `taskflow-worker` | Durable consumer |
| `WORKER_CONCURRENCY` | `10` | Concurrent executions |
| `WORKER_POLL_TIMEOUT` | `2s` | Pull fetch timeout |
| `WORKER_MAX_ATTEMPTS` | `5` | Retry limit |
| `WORKER_BACKOFF_BASE` | `500ms` | Base backoff |
| `WORKER_BACKOFF_MAX` | `10s` | Max backoff |
| `LOG_LEVEL` | `info` | Zap log level |

---

## 🧪 Testing

### Unit + integration tests

> Requires `postgres` and `nats` running.

```bash
docker compose up -d postgres nats
go test ./... -v
```

### Smoke test

```bash
./scripts/smoke.sh
```

---

## 🐛 Troubleshooting

### API works but tasks don't process
- Ensure worker is running: `go run ./cmd/worker`
- Inspect JetStream: `go run ./cmd/js-info`
- Watch worker logs for errors

### Ports already in use (Windows)
```powershell
netstat -ano | findstr :8080
```

---

## 🤝 Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## 📄 License

MIT License. See [LICENSE](LICENSE).
