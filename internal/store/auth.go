package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Role is the authorisation level, ordered viewer < analyst < admin. The same
// three values the users table's CHECK constraint allows.
type Role string

const (
	RoleViewer  Role = "viewer"
	RoleAnalyst Role = "analyst"
	RoleAdmin   Role = "admin"
)

// rank orders the roles for "at least this role" checks. Unknown roles rank 0,
// so a value that somehow escapes the CHECK constraint authorises nothing.
func (r Role) rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleAnalyst:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// AtLeast reports whether r satisfies a requirement of need.
func (r Role) AtLeast(need Role) bool {
	return r.rank() >= need.rank() && r.rank() > 0
}

func (r Role) Valid() bool { return r.rank() > 0 }

type Organization struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// User never carries PasswordHash into JSON — the hash is a credential, and the
// API returns users to clients.
type User struct {
	ID           uuid.UUID `json:"id"`
	OrgID        uuid.UUID `json:"org_id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         Role      `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
}

const userColumns = `id, org_id, email, password_hash, role, created_at`

func scanUser(row Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.OrgID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) CreateOrganization(ctx context.Context, id uuid.UUID, name string) (*Organization, error) {
	q := `INSERT INTO organizations (id, name) VALUES ($1, $2)
	      RETURNING id, name, created_at;`
	var o Organization
	if err := s.db.QueryRow(ctx, q, id, name).Scan(&o.ID, &o.Name, &o.CreatedAt); err != nil {
		return nil, err
	}
	return &o, nil
}

func (s *Store) GetOrganization(ctx context.Context, id uuid.UUID) (*Organization, error) {
	q := `SELECT id, name, created_at FROM organizations WHERE id = $1;`
	var o Organization
	err := s.db.QueryRow(ctx, q, id).Scan(&o.ID, &o.Name, &o.CreatedAt)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

type CreateUserParams struct {
	OrgID        uuid.UUID
	Email        string
	PasswordHash string
	Role         Role
}

// ErrEmailTaken is returned instead of a raw constraint violation so callers can
// answer "that address is already registered" without parsing driver errors.
var ErrEmailTaken = errors.New("email already registered")

func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) (*User, error) {
	q := `
INSERT INTO users (id, org_id, email, password_hash, role)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (email) DO NOTHING
RETURNING ` + userColumns + `;`

	u, err := scanUser(s.db.QueryRow(ctx, q, uuid.New(), p.OrgID, p.Email, p.PasswordHash, p.Role))
	if errors.Is(err, ErrNotFound) {
		// DO NOTHING suppressed the insert: the address is taken.
		return nil, ErrEmailTaken
	}
	return u, err
}

// GetUserByEmail is the login lookup. Email is globally unique, so it resolves
// the organisation too.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE email = $1;`
	return scanUser(s.db.QueryRow(ctx, q, email))
}

func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE id = $1;`
	return scanUser(s.db.QueryRow(ctx, q, id))
}

func (s *Store) ListUsers(ctx context.Context, orgID uuid.UUID) ([]User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE org_id = $1 ORDER BY email;`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.OrgID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) error {
	tag, err := s.db.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1;`, id, passwordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user; their sessions cascade away with the row, so
// deletion is also immediate revocation.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM users WHERE id = $1;`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- sessions ------------------------------------------------------------

// Session is server-side and revocable. The DB stores only the SHA-256 of the
// token the client holds, so a database leak yields no usable sessions.
type Session struct {
	TokenHash string
	UserID    uuid.UUID
	ExpiresAt time.Time
	CreatedAt time.Time
}

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID uuid.UUID, expiresAt time.Time) error {
	q := `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3);`
	_, err := s.db.Exec(ctx, q, tokenHash, userID, expiresAt)
	return err
}

// GetSessionUser resolves a session token hash to its user in one round trip,
// rejecting expired rows in SQL so an expired session can never authenticate
// even if the sweep has not reached it yet.
func (s *Store) GetSessionUser(ctx context.Context, tokenHash string) (*User, error) {
	q := `
SELECT u.id, u.org_id, u.email, u.password_hash, u.role, u.created_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > $2;`
	return scanUser(s.db.QueryRow(ctx, q, tokenHash, time.Now()))
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1;`, tokenHash)
	return err
}

// DeleteUserSessions revokes every session for a user — used on password change,
// so a stolen session dies with the credential it came from.
func (s *Store) DeleteUserSessions(ctx context.Context, userID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1;`, userID)
	return err
}

// DeleteExpiredSessions is housekeeping: expired rows never authenticate (the
// lookup filters them), this just stops the table growing forever.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1;`, time.Now())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---- password reset -------------------------------------------------------

// ErrTokenUsed distinguishes a token that was already spent from one that never
// existed, so the user gets "that link was already used" instead of a blanket
// "invalid link" that leaves them guessing.
var ErrTokenUsed = errors.New("reset token already used")

func (s *Store) CreatePasswordResetToken(ctx context.Context, tokenHash string, userID uuid.UUID, expiresAt time.Time) error {
	q := `INSERT INTO password_reset_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3);`
	_, err := s.db.Exec(ctx, q, tokenHash, userID, expiresAt)
	return err
}

// LastPasswordResetRequest reports when this user last asked for a reset, used
// to throttle. Zero time means never.
func (s *Store) LastPasswordResetRequest(ctx context.Context, userID uuid.UUID) (time.Time, error) {
	var at time.Time
	err := s.db.QueryRow(ctx,
		`SELECT created_at FROM password_reset_tokens
		 WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1;`, userID).Scan(&at)
	if errors.Is(err, ErrNoRows) {
		return time.Time{}, nil
	}
	return at, err
}

// ConsumePasswordResetToken atomically claims a token and returns its user.
//
// The UPDATE ... WHERE used_at IS NULL is the whole concurrency story: two
// requests racing the same link produce exactly one winner, because only one
// UPDATE can match. Doing this as SELECT-then-UPDATE would let both through.
func (s *Store) ConsumePasswordResetToken(ctx context.Context, tokenHash string) (*User, error) {
	var userID uuid.UUID
	err := s.db.QueryRow(ctx, `
UPDATE password_reset_tokens
SET used_at = $2
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
RETURNING user_id;`, tokenHash, time.Now()).Scan(&userID)

	if errors.Is(err, ErrNoRows) {
		// Nothing was claimed. Separate "spent" from "never existed / expired"
		// so the caller can explain which it was.
		var used bool
		probe := s.db.QueryRow(ctx,
			`SELECT used_at IS NOT NULL FROM password_reset_tokens WHERE token_hash = $1;`, tokenHash)
		if probeErr := probe.Scan(&used); probeErr == nil && used {
			return nil, ErrTokenUsed
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, userID)
}

// InvalidatePasswordResetTokens spends every outstanding token for a user.
// Called after any password change, so a recovery link that was requested and
// then abandoned cannot be redeemed later against a password the user has
// since chosen.
func (s *Store) InvalidatePasswordResetTokens(ctx context.Context, userID uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`UPDATE password_reset_tokens SET used_at = $2
		 WHERE user_id = $1 AND used_at IS NULL;`, userID, time.Now())
	return err
}

// DeleteExpiredPasswordResetTokens drops spent tokens older than the grace
// period. The cutoff is computed here rather than in SQL: date arithmetic is
// where dialects diverge hardest, and a retention window is application policy
// anyway, not something the database should be deciding.
func (s *Store) DeleteExpiredPasswordResetTokens(ctx context.Context, grace time.Duration) (int64, error) {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM password_reset_tokens WHERE expires_at <= $1;`, time.Now().Add(-grace))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
