// Package auth is authentication and authorisation for the HTTP API: password
// verification, server-side sessions, and the Principal that handlers read from
// the request context.
//
// The central design constraint: handlers are auth-UNAWARE. They ask the
// context for a Principal and always get one. Whether it arrived from a session
// cookie or from a single local identity (the desktop build, where there is
// nobody to authenticate against) is the middleware's business, not theirs.
// That is what lets the same handlers serve both deployments.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// SessionCookieName is the cookie carrying the session token.
const SessionCookieName = "taskflow_session"

// bcryptCost is deliberately above the library default (10): these hashes guard
// documents full of personal data, and login is not a hot path.
const bcryptCost = 12

// MinPasswordLen is a floor, not a policy. Long passphrases beat composition
// rules, so length is the only thing enforced here.
const MinPasswordLen = 12

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrPasswordTooShort   = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
)

// HashPassword returns a bcrypt hash, rejecting passwords under the floor.
func HashPassword(plain string) (string, error) {
	if len(plain) < MinPasswordLen {
		return "", ErrPasswordTooShort
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword compares a candidate against a stored hash. bcrypt's compare
// is constant-time for a given hash.
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// NewSessionToken mints a 256-bit token and returns it with its SHA-256.
// The plaintext goes to the client once, in the cookie; only the hash is
// stored, so a database leak yields nothing that can be replayed.
//
// SHA-256 (not bcrypt) is correct here: the token is 256 bits of entropy from a
// CSPRNG, so there is no dictionary to attack — only speed of lookup matters.
func NewSessionToken() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NormalizeEmail lower-cases and trims, so "DPO@Acme.com " and "dpo@acme.com"
// are the same login and cannot become two accounts.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Principal is the authenticated caller as handlers see it.
type Principal struct {
	UserID uuid.UUID
	OrgID  uuid.UUID
	Email  string
	Role   store.Role
	// Local marks the synthetic principal used when authentication is disabled
	// (the desktop build). Handlers never branch on it; it exists for logging
	// and for the /auth/me response.
	Local bool
}

// Can reports whether the principal holds at least the required role.
func (p *Principal) Can(need store.Role) bool {
	return p != nil && p.Role.AtLeast(need)
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal, if the auth middleware ran.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok && p != nil
}

// LocalOrgID is the organisation every document belongs to when authentication
// is disabled. It is the same "legacy" organisation 003_auth.sql seeds, so a
// deployment can switch auth on without stranding existing documents.
var LocalOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// LocalPrincipal is the identity used when auth is disabled: one machine, one
// user, full rights. Not a fallback for the web build — enabling it there would
// silently open the API, which is why the server requires an explicit setting.
func LocalPrincipal() *Principal {
	return &Principal{
		UserID: uuid.Nil,
		OrgID:  LocalOrgID,
		Email:  "local",
		Role:   store.RoleAdmin,
		Local:  true,
	}
}
