-- SQLite schema for the desktop build: one file, one process, no server.
--
-- This is a consolidated schema rather than a replay of the numbered Postgres
-- migrations. A local database is created fresh by the application that owns
-- it, so there is no history to migrate through — and keeping six migrations in
-- two dialects in step by hand is a standing invitation to drift.
--
-- Type mapping, and why each is safe here:
--   UUID        -> TEXT       google/uuid implements Scanner/Valuer
--   TIMESTAMPTZ -> TIMESTAMP  the declared type is what makes the driver hand
--                             back a time.Time instead of a string
--   JSONB       -> TEXT       payloads are read back as bytes; SQLite has no
--                             JSON storage type, and the store never queries
--                             *into* the payload
--   INT         -> INTEGER

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS organizations (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now'))
);

CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  email TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('admin','analyst','viewer')),
  created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now'))
);

CREATE INDEX IF NOT EXISTS idx_users_org ON users(org_id);

CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMP NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now'))
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS password_reset_tokens (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMP NOT NULL,
  used_at TIMESTAMP,
  created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now'))
);

CREATE INDEX IF NOT EXISTS idx_password_reset_user_created
  ON password_reset_tokens (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  payload TEXT NOT NULL,
  priority TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('low','normal','high')),
  status TEXT NOT NULL DEFAULT 'queued'
    CHECK (status IN ('queued','processing','completed','failed','cancelled')),
  created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
  updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
  version INTEGER NOT NULL DEFAULT 1,
  -- The org index lives in upgradeSQLite, not here: on a file created before
  -- this column existed, CREATE TABLE IF NOT EXISTS is a no-op and an index on
  -- org_id would fail the whole script before the upgrade could add it.
  org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_tasks_status_created ON tasks(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_type_created ON tasks(type, created_at DESC);

CREATE TABLE IF NOT EXISTS task_executions (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt INTEGER NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('started','succeeded','failed')),
  error TEXT NULL,
  started_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
  finished_at TIMESTAMP NULL
);

CREATE INDEX IF NOT EXISTS idx_exec_task_attempt ON task_executions(task_id, attempt DESC);

-- The concurrency guard: two deliveries racing the same attempt produce one
-- winner and one unique-violation, which the store maps to ErrAlreadyExists.
CREATE UNIQUE INDEX IF NOT EXISTS ux_task_executions_task_attempt
  ON task_executions(task_id, attempt);

CREATE TABLE IF NOT EXISTS documents (
  id TEXT PRIMARY KEY,
  filename TEXT NOT NULL,
  content_type TEXT NOT NULL,
  storage_uri TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'uploaded'
    CHECK (status IN ('uploaded','processing','completed','failed','erased')),
  failed_stage TEXT NULL,
  task_id TEXT NULL REFERENCES tasks(id) ON DELETE SET NULL,
  org_id TEXT NOT NULL REFERENCES organizations(id),
  created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
  updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
  version INTEGER NOT NULL DEFAULT 1,
  raw_shredded_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_documents_status_created ON documents(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_documents_org_created ON documents(org_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_documents_raw_pending
  ON documents (status, updated_at)
  WHERE raw_shredded_at IS NULL;

CREATE TABLE IF NOT EXISTS document_artifacts (
  id TEXT PRIMARY KEY,
  document_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  stage TEXT NOT NULL CHECK (stage IN ('ocr','pii','report')),
  kind TEXT NOT NULL,
  storage_uri TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_document_artifacts_doc_stage_kind
  ON document_artifacts(document_id, stage, kind);

CREATE INDEX IF NOT EXISTS idx_document_artifacts_doc ON document_artifacts(document_id);

-- updated_at triggers, mirroring the plpgsql ones on Postgres.
--
-- AFTER UPDATE, and the body updates the same table: safe because SQLite's
-- recursive_triggers defaults to OFF, so the inner write does not re-fire this.
-- The WHEN guard stops the trigger from firing on its own bookkeeping write.
CREATE TRIGGER IF NOT EXISTS trg_tasks_updated_at
AFTER UPDATE ON tasks
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
  UPDATE tasks SET updated_at = strftime('%Y-%m-%d %H:%M:%f','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS trg_documents_updated_at
AFTER UPDATE ON documents
FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN
  UPDATE documents SET updated_at = strftime('%Y-%m-%d %H:%M:%f','now') WHERE id = NEW.id;
END;
