-- Documents: the pipeline-run entity. One row per uploaded document; the row is
-- the queryable surface (status, failed stage) and — critically — the anchor that
-- makes a document fully ENUMERABLE for right-to-erasure (GDPR Art. 17): from
-- this row we can reach every artifact row, every object-store key (the
-- documents/{id}/ prefix), and the task/execution rows via task_id.
CREATE TABLE IF NOT EXISTS documents (
  id UUID PRIMARY KEY,
  filename TEXT NOT NULL,
  content_type TEXT NOT NULL,
  -- Reference, never bytes (C1): the original lives in object storage. This URI
  -- is the only thing that travels in the task payload / queue / DLQ.
  storage_uri TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'uploaded'
    CHECK (status IN ('uploaded','processing','completed','failed','erased')),
  -- Set when the document dead-letters: which stage it died at (ledger-derived).
  failed_stage TEXT NULL,
  -- The document.process task driving this document. SET NULL so erasing the
  -- task (part of C4) never blocks on this FK.
  task_id UUID NULL REFERENCES tasks(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  -- optimistic locking, same discipline as tasks
  version INT NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_documents_status_created ON documents(status, created_at DESC);

-- Per-stage artifacts side table. One row = one atomically-visible checkpoint
-- object ("artifact row exists" is only written AFTER the object is atomically
-- visible in the store, so it always means "stage complete").
CREATE TABLE IF NOT EXISTS document_artifacts (
  id UUID PRIMARY KEY,
  document_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  stage TEXT NOT NULL CHECK (stage IN ('ocr','pii','report')),
  kind TEXT NOT NULL,
  storage_uri TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Idempotent checkpointing: a stage may run more than once (at-least-once), but
-- only one artifact row per (document, stage, kind) ever lands.
CREATE UNIQUE INDEX IF NOT EXISTS ux_document_artifacts_doc_stage_kind
ON document_artifacts(document_id, stage, kind);

CREATE INDEX IF NOT EXISTS idx_document_artifacts_doc ON document_artifacts(document_id);

-- Auto-update updated_at on documents (same trigger discipline as tasks)
CREATE OR REPLACE FUNCTION set_documents_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_documents_updated_at ON documents;

CREATE TRIGGER trg_documents_updated_at
BEFORE UPDATE ON documents
FOR EACH ROW
EXECUTE FUNCTION set_documents_updated_at();
