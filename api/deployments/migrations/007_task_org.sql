-- Tenant scoping for tasks.
--
-- 003 scoped documents but left tasks global, so GET /tasks answered every
-- organisation's rows to anyone signed in: document ids, content types, status
-- and the per-attempt error history of other tenants' pipelines. The documents
-- were 404; their tasks were an index of them.
--
-- Idempotent like the other migrations — safe to re-run.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;

-- Backfill, most specific evidence first. A pipeline task belongs to its
-- document's tenant: through the link the API writes at upload...
UPDATE tasks t
SET org_id = d.org_id
FROM documents d
WHERE t.org_id IS NULL AND d.task_id = t.id;

-- ...or, where that link was never written, through the payload reference.
UPDATE tasks t
SET org_id = d.org_id
FROM documents d
WHERE t.org_id IS NULL
  AND t.type = 'document.process'
  AND t.payload->>'document_id' = d.id::text;

-- Whatever is left predates tenancy or outlived its document. The legacy
-- organisation adopts it, exactly as 003 did for documents.
INSERT INTO organizations (id, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'legacy')
ON CONFLICT (id) DO NOTHING;

UPDATE tasks SET org_id = '00000000-0000-0000-0000-000000000001' WHERE org_id IS NULL;

-- No default, for the reason 005 gives for documents: a code path that forgets
-- its tenant must fail the insert, not file the task where others can read it.
ALTER TABLE tasks ALTER COLUMN org_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_tasks_org_created ON tasks(org_id, created_at DESC);
