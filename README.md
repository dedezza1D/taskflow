# TaskFlow Compliance

**Find the personal data in a document, and be able to prove what happened to it afterwards.** Upload a PDF or a scan; it comes back as a GDPR/LGPD report naming the categories of personal data found and the obligations they trigger — and the document's own bytes are destroyed the moment that report exists.

The engine underneath is a distributed task queue built for the job (**Go**, **NATS JetStream**, **PostgreSQL**), and the same code ships two ways: a served deployment behind nginx, and a single-binary desktop app where no document ever leaves the machine.

[![CI](https://github.com/dedezza1D/taskflow/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/dedezza1D/taskflow/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> **One engine, one domain, two deployments.** The queue is not a side project next to the compliance tool: it is what makes the compliance claims possible — checkpointed stages, bounded retries, a per-attempt audit trail, and a dead-letter path that still ends with the document marked failed *at a named stage*. The desktop build is the same engine with two seams swapped (`store.DB`, `queue.Broker`), because a scanner for personal data is most useful where the data already is.

The contract, in one sentence:

> TaskFlow runs each document through **OCR → PII → compliance-report** as checkpointed, independently-retryable stages. Every document reaches a terminal outcome — report generated, or dead-lettered at stage X — with at-least-once idempotent execution, bounded durable-attempt retries, and a full per-stage audit, and with **no document content or PII leaking into the queue, DLQ, logs, traces, or audit error fields**.

See [docs/MANUAL.md](docs/MANUAL.md) for the user manual — the three jobs the
tool exists for, what each role can do, how to read a verdict, and the known
limitations. See [docs/PIPELINE.md](docs/PIPELINE.md) for the pipeline design (checkpoints, C1–C4 compliance guarantees, erasure order, deferred scope) and the `/api/v1/documents` endpoints.

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
- [Design decisions](#-design-decisions-and-what-they-cost)
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

The upload path carries **bytes** to object storage and a **reference** to the
queue. That split is what every compliance guarantee rests on: nothing
PII-bearing is ever in a message, a DLQ entry, a log line or a span.

```mermaid
flowchart LR
    UI["React SPA"]
    NGINX["nginx<br/>TLS"]
    API["API"]
    OBJ[("Object storage")]
    PG[("PostgreSQL")]
    JS{{"JetStream"}}
    W["Worker"]
    STAGES["OCR → PII → report<br/>checkpointed"]
    DLQ{{"tasks.dlq"}}

    UI --> NGINX --> API
    API -->|bytes| OBJ
    API -->|rows| PG
    API -->|reference| JS
    JS -->|pull| W --> STAGES
    STAGES -->|artifacts| OBJ
    W -->|"attempts, status"| PG
    W -->|"failed for good"| DLQ
```

The **desktop build** is the same diagram with two boxes replaced: SQLite for
PostgreSQL, the `tasks` table itself for JetStream, and no nginx — one process,
one file, loopback only. See [Desktop build](#-desktop-build-local-only).

### Task lifecycle

1. Client creates a task (`POST /api/v1/tasks`)
2. Task is persisted in PostgreSQL (`status=queued`)
3. Task ID is published to JetStream (`tasks.*`)
4. Worker fetches + processes the task
5. Worker records an execution attempt (`task_executions`)
6. Task is marked `completed` or `failed`
7. Permanent failures publish a DLQ message (`tasks.dlq`)

---

## 🧭 Design decisions, and what they cost

Three choices shaped most of the code. Each one had a cheaper alternative that
was wrong for a specific, findable reason.

**1. One checkpointed task, not one task per stage.** Splitting OCR, PII and
report into three queues is the obvious "distributed" answer, and it buys an
independent lifecycle per stage — which nothing here needs yet. Instead the
three run inline in one `document.process` task, with each stage writing its
artifact atomically *before* recording its row, so "the row exists" means "the
stage finished". A worker killed mid-OCR redelivers and **resumes** rather than
redoing. *The cost:* a slow OCR occupies a worker slot for the whole document,
and splitting later means moving a stage out — which the per-stage checkpoint
layout is designed to make cheap.

**2. The queue carries references; the bytes never move.** A payload of
`{document_id, storage_uri}` means no message, DLQ entry (7-day retention), log
line or trace span can hold personal data — a guarantee by construction rather
than by discipline. Errors are the leak nobody plans for, so every error passes
through one chokepoint, `worker.ScrubError`, which redacts using **the same
detector set the PII stage uses**: one pattern set, two enforcement points, no
drift. *The cost:* the guarantee is only as strong as the detectors, and a
regex cannot see free-text personal data — which is why the honest statement is
in [docs/PIPELINE.md](docs/PIPELINE.md) instead of a claim of completeness.

**3. Two storage backends behind one seam, with a conformance suite.** The
desktop build needs SQLite; the served one needs PostgreSQL. Every store method
is written once in PostgreSQL idiom and must behave identically on both, which
only stays true while **both are exercised by the same cases** (`eachBackend`).
That suite has already caught what the SQLite-only version missed — a query
PostgreSQL rejected outright, returning 500 on every list in production.
Related: **no test in this repo skips**. With the database down, the API package
once reported `ok` having run 4 of its 38 tests, with authentication and
password recovery quietly uncovered. A test that cannot run has not passed, and
the suite now says so by failing.

A fourth, learned the hard way: the erasure path and the pipeline are
**concurrent**, and a correct-looking sequence can still be outrun. Deleting a
document while its OCR stage was running left the extracted text in a directory
no row pointed at any more — invisible to both erasure and the retention sweep.
The fix is a fence with an ordering argument, written out in
[docs/PIPELINE.md](docs/PIPELINE.md#compliance-guarantees).

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

**Sign-in is throttled.** Five attempts per address in fifteen minutes, then
`429` with `Retry-After` — even for the right password, which would otherwise
reveal the correct guess. Known and unknown addresses are counted identically,
so the limit is not a directory of accounts; a successful sign-in clears the
count. The limit lives in each API process; nginx adds a per-IP limit on the
sign-in and recovery endpoints for one address spraying many accounts. Typed
addresses are never logged.

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
