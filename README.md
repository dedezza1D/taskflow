# TaskFlow

**TaskFlow** is a production-style distributed task queue and execution-tracking platform built with **Go**, **NATS JetStream**, and **PostgreSQL** — and, built on that engine, a **GDPR compliance document pipeline**:

> TaskFlow runs each document through **OCR → PII → compliance-report** as checkpointed, independently-retryable stages. Every document reaches a terminal outcome — report generated, or dead-lettered at stage X — with at-least-once idempotent execution, bounded durable-attempt retries, and a full per-stage audit, and with **no document content or PII leaking into the queue, DLQ, logs, traces, or audit error fields**.

See [docs/MANUAL.md](docs/MANUAL.md) for the user manual — the three jobs the
tool exists for, what each role can do, how to read a verdict, and the known
limitations. See [docs/PIPELINE.md](docs/PIPELINE.md) for the pipeline design (checkpoints, C1–C4 compliance guarantees, erasure order, deferred scope) and the `/api/v1/documents` endpoints. The engine below demonstrates reliable asynchronous processing with retries, execution audit history, and operational patterns like **Dead Letter Queues (DLQ)**.

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
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
- [Frontend](#-frontend)
- [Desktop build](#-desktop-build-local-only)
- [TLS](#-tls)
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

### Everything at once (Docker)

Brings up the full stack — API, worker, Postgres, NATS, the built frontend, and
the observability services.

```bash
bash scripts/gen-dev-certs.sh
docker compose up -d --build
```

The UI is at **`https://localhost:8443`** (Grafana at `/grafana/`); plain HTTP on
`:8080` answers with a 308 to it. The dev certificate is self-signed, so the
browser will warn once — that is the correct reaction to a certificate nobody
vouched for, and clicking through is fine locally.

TLS in dev is not ceremony: the session cookie is marked `Secure`, and a browser
silently discards a `Secure` cookie that arrives over plain HTTP. Testing over
HTTP would test a different application.

### Or run the Go services on the host

Prerequisites: **Go 1.25+**, **Node 22+** (frontend), and — if you run the
worker outside Docker — **tesseract** for the OCR stage (PDF handling is pure Go).

#### 1) Start infrastructure

```bash
docker compose up -d postgres nats
```

#### 2) Run migrations

They are ordered and idempotent; apply all of them.

```bash
for f in api/deployments/migrations/*.sql; do psql "postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable" -f "$f"; done
```

#### 3) Start API

```bash
go run ./cmd/api
```

#### 4) Start worker (new terminal)

```bash
go run ./cmd/worker
```

#### 5) Start the frontend (new terminal)

Vite proxies `/api` to the API, so the browser sees a single origin.

```bash
cd web && npm install && npm run dev
```

The Vite dev server speaks plain HTTP, so a `Secure` session cookie will not
survive it. Run the API with `SECURE_COOKIES=false` for this flow, or use the
Docker stack over `https://localhost:8443` instead.

#### 6) Create the first admin

The API is closed by default, so nothing works until an account exists. Omit
`--password` to have one generated and printed once.

```bash
go run ./cmd/seed-admin --org "Acme" --email dpo@acme.com
```

#### 7) Health check

`-k` accepts the self-signed dev certificate.

```bash
curl -ik https://localhost:8443/api/v1/health
```

#### 8) Create a demo task

Everything below `/api/v1/` needs a session, so sign in first and keep the cookie.

```bash
curl -k -c cookies.txt -X POST https://localhost:8443/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"dpo@acme.com","password":"YOUR_PASSWORD"}'

curl -k -b cookies.txt -X POST https://localhost:8443/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"type":"demo","payload":{"hello":"world"},"priority":"normal"}'
```

#### 9) Upload a document through the compliance pipeline

```bash
curl -k -b cookies.txt -X POST https://localhost:8443/api/v1/documents -F "file=@sample.png"
```

---

## 📚 API

**Auth**

Session cookies, no public sign-up. `cmd/seed-admin` creates the first
organisation and admin; every account after that is created by an admin from the
**Usuários** panel in the app (header → Usuários), which generates the initial
password and shows it once — the server only ever stores the bcrypt hash.

Anyone signed in can change their own password from **Senha**. Doing so revokes
every session, including the current one.

**Recovery by email** (`POST /auth/forgot-password`, `POST /auth/reset-password`)
is on when SMTP is configured. The link is single-use, expires in an hour, and
redeeming it revokes every session and every other outstanding link for that
account. `GET /auth/config` tells the login screen whether to offer it.

Two deliberate asymmetries: **requesting** a link always answers 204 — an
unknown address must be indistinguishable from a known one, or the endpoint
becomes a directory of who has an account — while **redeeming** one answers
honestly (410 `token_used` vs `token_invalid`), since the caller already holds
the token and learns nothing new. Requests are throttled per account so the
endpoint cannot be used to flood someone's inbox.

In dev, `docker compose` runs **Mailpit** as a local catcher: nothing leaves the
machine, and the mail appears at `http://localhost:8025`.

- `POST /api/v1/auth/login` — sets an `HttpOnly; SameSite=Lax` session cookie
- `POST /api/v1/auth/logout` — deletes the session row, so a copied token dies too
- `GET /api/v1/auth/me` — who am I (and whether this build has auth at all)
- `POST /api/v1/auth/password` — change password; revokes every existing session
- `GET|POST /api/v1/users`, `DELETE /api/v1/users/{id}` — admin only

Roles are ordered **viewer < analyst < admin**: a viewer reads, an analyst also
uploads, an admin also erases and manages users. Erasure is irreversible, so it
is the one document action reserved for admin.

Documents and tasks are scoped to the caller's organisation. A document or
task belonging to another tenant reads as **404, not 403**, so an id cannot be
probed to confirm that it exists elsewhere. `document.process` tasks can only be
created by uploading through `/documents`; `POST /tasks` refuses that type.

**Tasks (the queue engine)**

- `GET /api/v1/health` — public, so healthchecks work before anyone signs in
- `POST /api/v1/tasks`
- `GET /api/v1/tasks`
- `GET /api/v1/tasks/{id}`
- `GET /api/v1/tasks/{id}/executions`

**Documents (the compliance pipeline)**

- `POST /api/v1/documents` — multipart upload (`file`), starts OCR → PII → report
- `GET /api/v1/documents` — list
- `GET /api/v1/documents/{id}` — status, failed stage, artifacts
- `GET /api/v1/documents/{id}/report` — GDPR/LGPD report (`404 report_not_ready` until generated)
- `DELETE /api/v1/documents/{id}` — right-to-erasure; removes row and every stored byte

For request/response examples, see the handlers in `api/httpapi/`, and
[docs/PIPELINE.md](docs/PIPELINE.md) for the erasure ordering and C1–C4 guarantees.

---

## 🖥 Frontend

React + Vite + TypeScript, in [`web/`](web/). In production it is compiled by
`Dockerfile.web` and served by the same nginx that proxies `/api` and
`/grafana`; there is no Node runtime in the deployed image.

```bash
cd web
npm install
npm run dev      # dev server with /api proxy
npm run build    # tsc -b && vite build → web/dist
npm run lint
```

---

## 💻 Desktop build (local-only)

`cmd/taskflow-desktop` is the whole application in one process: SQLite in a file
the user owns, the queue backed by that same file, and the HTTP API bound to
loopback. No Postgres, no NATS, no container — and **no document ever leaves the
machine**, which is the reason to ship a compliance tool this way. The scanner
does not become another copy of the personal data it finds.

```bash
go build -o taskflow-desktop ./cmd/taskflow-desktop
./taskflow-desktop --web-dir web/dist
```

It prints the URL to open, including a one-time launch token (`--addr` defaults
to `127.0.0.1:0`, letting the kernel pick a free port; non-loopback addresses
are refused). Data lives under the per-user application directory
unless `--data-dir` says otherwise.

### Packaged as a native app

[`desktop/`](desktop/) wraps that binary in a Tauri window and an installer.
Tauri launches the Go binary as a sidecar, reads the port off its stdout, and
points the window there — so the SPA and the API share one origin and nothing is
hardcoded. See [desktop/README.md](desktop/README.md).

```bash
bash scripts/build-desktop.sh          # installer
bash scripts/build-desktop.sh --dev    # run without packaging
```

The installer (~52 MB) carries **tesseract** with **Portuguese and English**
language data, so a scanned Brazilian document works on a machine where nothing
was installed. Only the derived dependency closure ships, not the whole install.
A PDF with a text layer never touches OCR at all.

Set `TASKFLOW_SIGN_THUMBPRINT` (or `TASKFLOW_SIGN_COMMAND`) and the same build
signs the whole tree — shell, sidecar, OCR binaries, uninstaller, installer —
then verifies the files rather than trusting the bundler's log. Without it the
installer still builds and every user is told its publisher is unknown. Which
certificate to get — two of the options cost nothing — why timestamping
is not optional, and what the `SHA256SUMS` every build emits does and does
not prove:
[desktop/README.md](desktop/README.md#signing).

What it deliberately drops, and why:

| | Served | Desktop |
|---|---|---|
| Database | PostgreSQL | SQLite file |
| Queue | NATS JetStream | the `tasks` table + an in-process wake-up |
| Auth | sessions, roles, tenants | no accounts — a per-launch token and a strict `Host` check |
| TLS | nginx terminates | none — loopback, and no CA vouches for localhost |

User accounts and TLS are *server* concerns. On a single-user install there is
nobody to sign in as, so every request runs as a local admin principal.
`config.Validate` refuses that combination whenever `ENV=prod`, so it cannot
reach a served deployment by accident.

Loopback alone is not the boundary, though: web pages can reach `127.0.0.1`
(DNS rebinding lets them read the answers), and so can every other account on
the machine. The binary therefore listens only on loopback, rejects any `Host`
but the address it bound, and requires a cookie obtained from the one-time
launch link it prints — `http://127.0.0.1:<port>/?launch=<token>`. Open that
URL as printed; see [desktop/README.md](desktop/README.md#loopback-is-not-access-control).

The same store methods, pipeline, and worker loop run in both — the differences
live behind two seams (`store.DB` and `queue.Broker`), not in forked code.

---

## 🔐 TLS

nginx terminates TLS and is the only thing listening publicly. Port 80 serves
nothing — it answers every request with a 308 to HTTPS, because a session cookie
must never cross a connection anyone can read.

- **TLS 1.2 and 1.3 only.** 1.0 and 1.1 are refused.
- **HSTS** `max-age=31536000; includeSubDomains`, deliberately **without**
  `preload` — preloading is a one-way submission to browser vendors and belongs
  to whoever owns the real domain, not to a config file in a repository.
- Plus `X-Content-Type-Options`, `X-Frame-Options: DENY`, and
  `Referrer-Policy: same-origin`.

**Development:** `scripts/gen-dev-certs.sh` writes a self-signed certificate to
`deploy/nginx/certs/` (git-ignored). Browsers warn; that is correct.

**Production:** mount real certificates by pointing `TLS_CERT_DIR` at a
directory holding `cert.pem` and `key.pem`. Where they come from — ACME/Let's
Encrypt, or a corporate CA — is still an open deployment decision; nginx does
not care, and nothing in the repository assumes one.

If TLS terminates upstream instead (a cloud load balancer), keep
`SECURE_COOKIES=true` and let that proxy set `X-Forwarded-Proto`.

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
| `OBJECTS_DIR` | `data/objects` | Object storage root, shared by API and worker. Must be absolute with `ENV=prod` — the relative default would give each container its own private copy |
| `MAX_UPLOAD_BYTES` | `26214400` (25 MiB) | Upload ceiling; larger bodies get `413` |
| `WORKER_OCR_TIMEOUT` | `2m` | OCR sub-ceiling; must be `< WORKER_MAX_PROCESSING` |
| `WORKER_RAW_RETENTION` | `24h` | Safety net for the raw-material sweep. A completed document is shredded **inline**; this bounds only dead-lettered ones |
| `AUTH_ENABLED` | `true` | `false` runs every request as a single local admin — for the desktop build only; refused when `ENV=prod` |
| `SECURE_COOKIES` | `false` | Marks the session cookie `Secure`. Required with `ENV=prod`; a `Secure` cookie over plain HTTP is dropped by the browser, so leave it off for the Vite dev server |
| `TLS_CERT_DIR` | `./deploy/nginx/certs` | Host directory holding `cert.pem` + `key.pem`, mounted into nginx (prod compose) |
| `SMTP_HOST` | *(empty)* | Enables password recovery. Empty disables it — endpoints answer 404 and the login screen hides the link |
| `SMTP_PORT` | `587` | |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | *(empty)* | Setting a username without `SMTP_STARTTLS` is refused at startup: the credential would cross the network in cleartext |
| `SMTP_FROM` | *(empty)* | Envelope sender; required when SMTP is configured |
| `SMTP_STARTTLS` | `true` | |
| `APP_BASE_URL` | *(empty)* | Public origin for recovery links. Required when recovery is on — never taken from the request `Host`, which an attacker controls |
| `RESET_TOKEN_TTL` | `1h` | Lifetime of a recovery link |
| `MAIL_LOG_ONLY` | `false` | Prints recovery links to the log instead of sending. Development only; refused with `ENV=prod` |
| `SESSION_TTL` | `12h` | Absolute session lifetime |
| `OCR_LANGS` | `eng` | Tesseract language packs. Both compose files set `por+eng`, and the worker image ships `por`, `eng` and `deu`; outside Docker, install the packs you name |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty)* | Disables tracing when unset |
| `GRAFANA_ADMIN_PASSWORD` | required (prod compose) | Grafana admin password. `docker-compose.prod.yml` refuses to start without it, because Grafana is served publicly at `/grafana/`. Read only when Grafana first creates its database — on an existing `grafana_data` volume use `grafana cli admin reset-admin-password` |

`OBJECTS_DIR` must point at the **same** storage for the API and the worker —
the API writes the original there and the worker reads it back. Both compose
files mount the `objects_data` volume at `/data/objects` in both containers.

---

## 🧪 Testing

### Go: unit + integration

**Nothing skips.** These are integration tests of a system that needs a real
database and a real OCR engine; without them the tests have not run, and the
suite says so by failing rather than by reporting `ok`.

```bash
docker compose up -d postgres
go test ./... -v
```

Two prerequisites, each of which fails loudly and names the command that fixes
it:

| | needed by | if missing |
|---|---|---|
| PostgreSQL | `api/httpapi`, `internal/store`, `internal/pipeline` | `docker compose up -d postgres` |
| tesseract | the one PDF test covering the scanned path | `winget install UB-Mannheim.TesseractOCR` — resolved on PATH or in the usual install directory |

NATS is **not** among them: the queue and worker tests use the in-process broker
and pass with the broker unreachable. The smoke test below does need it.

**The tests never touch the development database.** They create and migrate a
separate `taskflow_test` database on the same server (`internal/testdb`), so the
compose worker cannot pick up their rows — it once did, reprocessing test tasks
against temp-dir objects and filling the development UI with failed documents
nobody had uploaded. Set `TEST_DATABASE_URL` to use a different database; the
migrations are applied to it on every run.

The store suite is a **conformance suite**: one set of cases, run against both
SQLite and PostgreSQL by `eachBackend`. Every store method is written once in
PostgreSQL idiom and has to behave identically on the desktop backend, and that
only holds while both are exercised — see the note at the top of
`internal/store/backends_test.go` for what shipped the last time they were not.

The reason for the no-skip rule is not tidiness. With the database down, this
suite used to report `ok` for `api/httpapi` having run **4 of its 38 tests** —
authentication, authorisation and password recovery silently uncovered, green
the whole way. A skipped test has not passed.

### Frontend

```bash
cd web && npm test        # vitest, once
cd web && npm run test:watch
```

Covers the two pieces that decide what the operator sees: the inventory
arithmetic in `lib/compliance.ts` and the report-fetching pool in `hooks.ts`.

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
