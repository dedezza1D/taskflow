-- When a task's message actually reached the queue.
--
-- Creating a task is two writes to two systems: the row commits to PostgreSQL,
-- then the message goes to JetStream. A crash between them leaves a task nobody
-- will ever deliver. The reconciler already rescues those, but it shares one
-- staleness window with the case of a worker that died mid-flight — and that
-- case must wait out the processing ceiling, or the sweep would rescue work
-- that is still legitimately running. A lost enqueue paid a ten-minute window
-- for a constraint that is not its own.
--
-- This column separates them. NULL means "committed, never confirmed on the
-- queue": no worker can be holding it, so it can be republished within seconds.
-- A task that reached the queue and is merely waiting for a free worker carries
-- a timestamp, so the fast sweep never touches it.
--
-- This is the transactional-outbox marker, on the row that already exists. A
-- separate outbox table stores an event that cannot be rebuilt from committed
-- state; the message here is {task_id, priority}, which the row already holds.
--
-- Idempotent like the other migrations — safe to re-run.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS enqueued_at TIMESTAMPTZ;

-- Existing rows predate the column. Treating them as published is the safe
-- reading: they were enqueued under the old path, and the staleness sweep still
-- covers anything that really was lost.
--
-- The trigger comes off for the backfill. trg_tasks_updated_at rewrites
-- updated_at on every UPDATE, and updated_at is what the reconciler's staleness
-- window reads: a backfill that touched every row would make every task look
-- freshly updated and hide genuinely stuck ones for a full window. A bookkeeping
-- column being filled in is not an update to the task.
ALTER TABLE tasks DISABLE TRIGGER trg_tasks_updated_at;

UPDATE tasks SET enqueued_at = created_at WHERE enqueued_at IS NULL;

ALTER TABLE tasks ENABLE TRIGGER trg_tasks_updated_at;

-- The fast sweep's access path: pending publishes, oldest first.
CREATE INDEX IF NOT EXISTS idx_tasks_unpublished
  ON tasks(created_at)
  WHERE enqueued_at IS NULL AND status = 'queued';
