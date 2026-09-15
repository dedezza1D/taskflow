# TaskFlow Compliance Document Pipeline

> **The contract:** TaskFlow runs each document through OCR → PII → compliance-report as checkpointed, independently-retryable stages. Every document reaches a terminal outcome — report generated, or dead-lettered at stage X — with at-least-once idempotent execution, bounded durable-attempt retries, and a full per-stage audit, and with no document content or PII leaking into the queue, DLQ, logs, traces, or audit error fields.

## Architecture

One task type, **`document.process`**, runs as a **checkpointed monolith**: OCR → PII → report inline on the existing engine. Each stage writes its artifact atomically to object storage and records a row in `document_artifacts`; on retry, a stage whose artifact exists is skipped, so a worker killed mid-OCR redelivers and **resumes**, never redoes.

A stage graduates into its own task/consumer only when it earns an independent lifecycle (slot-occupancy, or a real priority/scaling need). OCR is the first candidate; the per-stage checkpoint layout is what makes that split cheap later.

### Data model

| Piece | Role |
|---|---|
| `documents` | The pipeline-run entity: status, `failed_stage`, link to the driving task. Optimistically locked like `tasks`. |
| `document_artifacts` | One row per completed stage checkpoint, unique on `(document_id, stage, kind)` — the idempotency guard and the erasure inventory. |
| Object storage (`internal/objects`) | Where bytes live: `documents/{id}/original`, `ocr.txt`, `findings.json`, `report.json`. The v1 store is a filesystem root shared via compose volume; Put is atomic (temp + fsync + rename), which is the checkpoint invariant — "artifact object exists" always means "stage complete". S3/MinIO slots in behind the same interface. |

### Payload is a reference, never bytes (C1)

The task payload is `{document_id, storage_uri, content_type}`. Document bytes never touch the payload, the queue, or the 7-day DLQ — so there is nothing PII-bearing to purge from the stream, ever.

## Stages (v1: real but simple)

- **OCR** — `tesseract` via `exec.CommandContext` under its **own sub-ceiling** (`WORKER_OCR_TIMEOUT`, validated `< WORKER_MAX_PROCESSING`): a hung binary fails the stage cooperatively instead of burning the lease. Images (png/jpeg/tiff/bmp) supported; `text/plain` passes through (also what keeps CI green without tesseract). **PDF** is handled in pure Go, layered: the **text layer** is read first (`ledongthuc/pdf`, BSD-3) and, when a document has one, it is the answer — no OCR at all, which is both faster and more accurate than rasterising text that was never ambiguous. Only a **scan** falls through, and then the pages' *embedded images* are extracted (`pdfcpu`, Apache-2.0) and OCR'd — extracting what is already in the file rather than re-rendering the page to guess at it. pdfcpu also supplies the page count, gating the fan-out: a PDF over 500 pages is a **permanent** error, never a silent truncation. One ceiling over the whole stage, pages joined by form feed into **one atomically-written text artifact**, so the checkpoint invariant is untouched. Dropping poppler also dropped its GPL-2 obligations, which is what made a redistributable desktop installer possible. A tool failure that isn't the ceiling firing is treated as a **poison pill → permanent → dead-letter**, never a crash-loop.
- **PII** — the low-false-positive set: **Luhn-validated** cards, **mod-97-validated** IBANs, **check-digit-validated** German Steuer-IDs and **Brazilian CPF/CNPJ** (mod-11 double check digits; all-same-digit runs rejected — they satisfy the arithmetic but are invalid), plus best-effort email/phone (`internal/pii`). Findings record **category and location, never the raw value** — the pipeline's own artifacts must not become a secondary PII store. Upgrade path: Presidio NER sidecar layered on top; `detector_version` is stamped into every artifact.
- **Report** — detection is jurisdiction-independent; the report carries **one section per regulation (GDPR and LGPD)** over the same findings: per-category classification (ordinary personal data vs. special/sensitive — v1 detectors are all ordinary; the `special` flag is structural for the NER upgrade; CNPJ is `context_dependent`, a legal-entity identifier that is personal data only when it identifies a natural person) plus triggered obligations (GDPR: Art. 5/6, 13/14, 15/17, 30, with Art. 32/33/34 emphasis when financial identifiers are present; LGPD: Art. 6/7, 18, 37, with Art. 46/48 emphasis). `report.json`; `report.md` optional later.

## Compliance guarantees

- **C1** — no PII in queue/DLQ: by construction (references only).
- **C2/C3** — no PII in `task_executions.error`, DLQ error fields, logs, or spans: `worker.ScrubError` is the single chokepoint, now doing **pattern redaction with the same `internal/pii` detector set the PII stage uses** (one pattern set, two enforcement points, zero drift), plus a hard length cap. Honest scope: redaction is as strong as the detectors; free-text PII a regex can't see still relies on the handler rule of never embedding document content in errors — the Presidio upgrade strengthens both sides at once.
- **C4** — right to erasure (GDPR Art. 17): `DELETE /api/v1/documents/{id}` runs, in order: **(1) tombstone** the row (`status=erased`, optimistic) so an in-flight worker acks without touching bytes; **(2) remove every object** under `documents/{id}/`; **(3) delete the task row** (executions cascade — a later redelivery hits the engine's existing "task not found → ack"; DLQ entries need no purge because they hold references and the bytes are gone); **(4) delete the document row** (artifact rows cascade). Every step is idempotent — a crash mid-sequence is fixed by calling DELETE again.
- **C5 — data minimisation (raw material does not outlive the run)**: the "raw material" is exactly two objects, `original` and `ocr.txt`; `findings.json` and `report.json` are not, because they hold category and location and never values. Both raw objects are destroyed the instant the report commits, by `pipeline.ShredRaw` — not on a timer, because the pipeline needs them for one run (milliseconds for text, up to the OCR ceiling for a PDF) and never again. `documents.raw_shredded_at` stamps when, so minimisation is verifiable rather than claimed, and the API exposes it.

  Two design points that are not obvious. **Shredding happens at pipeline completion, never stage-by-stage**: `loadCheckpoint` treats a missing artifact object as "recompute this stage", so dropping `ocr.txt` as soon as `findings.json` landed would make a redelivery arriving before the report committed try to re-run OCR against an already-deleted original, dead-lettering a document one stage from done. Completion is safe because `Handle` short-circuits on `DocCompleted`. And **only objects are removed, never artifact rows** — the rows are the stage ledger that `failedStageFromLedger` and the UI timeline read, and they carry no content.

  `cmd/worker` runs a sweep (`WORKER_RAW_RETENTION`, default 24h) as the safety net for the two cases the inline path cannot reach: documents that **dead-lettered**, which have no completion event to shred against and are the ones nobody revisits; and documents whose inline shred failed, where `raw_shredded_at` is deliberately left NULL so the sweep retries instead of stranding bytes.

- **Dead-letter → document status**: every terminal path in the engine (permanent error, exhausted attempts, reconciler reap) calls `pipeline.MarkDeadLettered`, which sets `status=failed` and derives `failed_stage` from the **checkpoint ledger** (first stage without an artifact) — correct even for a crash-pill reaped with no error in hand.

## API

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/documents` | multipart upload (`file`, optional `priority`) → stores bytes, creates document + task, enqueues. Unsupported types fail fast with 415. |
| `GET /api/v1/documents/{id}` | status + artifact list |
| `GET /api/v1/documents/{id}/report` | streams `report.json` (404 `report_not_ready` until generated) |
| `DELETE /api/v1/documents/{id}` | erasure (C4) |

## Deferred (agreed, unbuilt)

- **Backup-resident copies** — every guarantee here describes live storage. A shredded original and an erased document both survive in Postgres snapshots and object-store versioning for as long as the backup policy keeps them, so C4 and C5 are honest only up to the backup boundary. Closing it means crypto-shredding (per-document key, destroy the key rather than the bytes) so a backup holds unusable ciphertext — the only approach that works when "delete" is not something the storage layer can promise.
- **Presidio NER sidecar** — layered onto `internal/pii`, upgrading the PII stage and `ScrubError` together.
- **`report.md`** — human-readable rendering of `report.json`.
