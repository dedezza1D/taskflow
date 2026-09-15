-- Auth + multi-tenancy: organizations (the tenant), users (login identities
-- with a role), sessions (server-side, revocable), and tenant scoping on
-- documents. Idempotent like the other migrations — safe to re-run.

CREATE TABLE IF NOT EXISTS organizations (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS users (
  id UUID PRIMARY KEY,
  org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  -- Globally unique: email is the login key across tenants.
  email TEXT NOT NULL UNIQUE,
  -- bcrypt hash; the raw password never touches logs or errors.
  password_hash TEXT NOT NULL,
  -- viewer reads; analyst also uploads; admin also erases and manages users.
  role TEXT NOT NULL CHECK (role IN ('admin','analyst','viewer')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_users_org ON users(org_id);

-- Server-side sessions: the cookie carries a random token, the DB stores only
-- its SHA-256 — a DB leak does not leak usable session tokens. Deleting the
-- row revokes the session.
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- Tenant scoping for documents. Existing (pre-auth) rows are adopted by a
-- fixed "legacy" organization so they stay reachable and, more importantly,
-- ERASABLE — orphaning them would break the C4 enumerability guarantee.
-- NO ACTION on the FK: an organization cannot be deleted while it still has
-- documents (erasure must run first, so bytes never orphan).
INSERT INTO organizations (id, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'legacy')
ON CONFLICT (id) DO NOTHING;

ALTER TABLE documents ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id);
UPDATE documents SET org_id = '00000000-0000-0000-0000-000000000001' WHERE org_id IS NULL;
-- No default: every insert names its tenant explicitly, so a code path that
-- forgets fails loudly instead of quietly filing the document under a
-- catch-all organisation where the wrong people can read it.
ALTER TABLE documents ALTER COLUMN org_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_documents_org_created ON documents(org_id, created_at DESC);
