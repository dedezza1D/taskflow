-- Tasks
CREATE TABLE IF NOT EXISTS tasks (
  id UUID PRIMARY KEY,
  type TEXT NOT NULL,
  payload JSONB NOT NULL,
  priority TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('low','normal','high')),
  status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','processing','completed','failed','cancelled')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  -- optimistic locking
  version INT NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_tasks_status_created ON tasks(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_type_created ON tasks(type, created_at DESC);

-- Execution history / audit trail
CREATE TABLE IF NOT EXISTS task_executions (
  id UUID PRIMARY KEY,
  task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt INT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('started','succeeded','failed')),
  error TEXT NULL,
  started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at TIMESTAMPTZ NULL
);

CREATE INDEX IF NOT EXISTS idx_exec_task_attempt ON task_executions(task_id, attempt DESC);

-- Idempotency: prevent duplicate executions for the same task+attempt
CREATE UNIQUE INDEX IF NOT EXISTS ux_task_executions_task_attempt
ON task_executions(task_id, attempt);

-- Auto-update updated_at on tasks
CREATE OR REPLACE FUNCTION set_tasks_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_tasks_updated_at ON tasks;

CREATE TRIGGER trg_tasks_updated_at
BEFORE UPDATE ON tasks
FOR EACH ROW
EXECUTE FUNCTION set_tasks_updated_at();
