-- Self-service password recovery.
--
-- Same shape as sessions, and for the same reason: the token the user receives
-- by email is never stored. Only its SHA-256 lives here, so a database leak
-- yields nothing an attacker can put in a reset link. The token is 256 bits from
-- a CSPRNG, so there is no dictionary to attack and a fast hash is correct.
--
-- Single use is enforced by used_at rather than by deleting the row: a consumed
-- token that still exists lets a second attempt be answered "already used"
-- instead of silently behaving like an unknown token, and leaves an audit trail
-- of when recovery actually happened.
CREATE TABLE IF NOT EXISTS password_reset_tokens (
  token_hash TEXT PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL,
  used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Supports both the expiry sweep and the per-user throttle, which asks for the
-- most recent token issued to one user.
CREATE INDEX IF NOT EXISTS idx_password_reset_user_created
  ON password_reset_tokens (user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_password_reset_expires
  ON password_reset_tokens (expires_at);
