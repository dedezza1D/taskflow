package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

// dummyHash is a real bcrypt hash of a random value, compared against when the
// email is unknown so a failed login costs the same time whether or not the
// account exists. Without it, response timing tells an attacker which addresses
// are registered — itself disclosure, in a tool whose users are named people.
const dummyHash = "$2a$12$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// principalMiddleware resolves the caller once per request and puts a Principal
// in the context. It never rejects: authorisation is the route's job, so an
// anonymous request reaches the handler chain and is turned away by
// requireRole with a 401 it can act on.
//
// When auth is disabled (the local desktop build) every request gets the local
// principal, which is what keeps handlers free of "if auth enabled" branches.
func (s *Server) principalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled {
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.LocalPrincipal())))
			return
		}

		cookie, err := r.Cookie(auth.SessionCookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		user, err := s.store.GetSessionUser(r.Context(), auth.HashToken(cookie.Value))
		if err != nil {
			// Unknown or expired session: clear the stale cookie so the browser
			// stops sending it, then continue as anonymous.
			if errors.Is(err, store.ErrNotFound) {
				http.SetCookie(w, s.sessionCookie("", -1))
			} else {
				s.logger.Error("session lookup failed", zap.Error(err))
			}
			next.ServeHTTP(w, r)
			return
		}

		p := &auth.Principal{
			UserID: user.ID,
			OrgID:  user.OrgID,
			Email:  user.Email,
			Role:   user.Role,
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// requireRole wraps a handler with an authorisation check.
//
// 401 means "you are not signed in" and 403 means "you are, but this is not
// yours to do" — the distinction matters to the SPA, which redirects on the
// first and shows a message on the second.
func (s *Server) requireRole(need store.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
			return
		}
		if !p.Can(need) {
			writeErr(w, http.StatusForbidden, "forbidden",
				"your role ("+string(p.Role)+") does not allow this action")
			return
		}
		h(w, r)
	}
}

// principal is the handler-side accessor. Every route that reads it is behind
// requireRole, so the principal is always present by the time it runs.
func principal(r *http.Request) *auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

func (s *Server) sessionCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:  auth.SessionCookieName,
		Value: token,
		Path:  "/",
		// HttpOnly keeps the token away from any XSS that lands in the SPA;
		// SameSite=Lax blocks the cross-site POST that CSRF needs, which is what
		// lets this API skip CSRF tokens.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies,
		MaxAge:   maxAge,
	}
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type meResponse struct {
	Email string     `json:"email"`
	Role  store.Role `json:"role"`
	OrgID string     `json:"org_id"`
	Local bool       `json:"local"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled {
		writeErr(w, http.StatusNotFound, "auth_disabled", "authentication is not enabled")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	email := auth.NormalizeEmail(req.Email)

	// Before the lookup, so a throttled attempt reveals nothing — not even
	// through timing — about whether the account exists. See login_throttle.go.
	if ok, retryAfter := s.loginLimiter.reserve(email); !ok {
		s.logger.Info("login throttled")
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Round(time.Second).Seconds())))
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts",
			"too many sign-in attempts for this account; try again later")
		return
	}

	user, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("login lookup failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "login failed")
		return
	}

	// Always run a comparison, so an unknown address costs the same as a wrong
	// password. Neither the credentials nor the typed address are logged: the
	// address is personal data, and on a rejection it is whatever the caller
	// chose to type.
	hash := dummyHash
	if user != nil {
		hash = user.PasswordHash
	}
	if !auth.VerifyPassword(hash, req.Password) || user == nil {
		if user != nil {
			s.logger.Info("login rejected", zap.String("user_id", user.ID.String()))
		} else {
			s.logger.Info("login rejected: no such account")
		}
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	s.loginLimiter.reset(email)

	token, tokenHash, err := auth.NewSessionToken()
	if err != nil {
		s.logger.Error("session token generation failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "login failed")
		return
	}
	if err := s.store.CreateSession(r.Context(), tokenHash, user.ID, time.Now().Add(s.sessionTTL)); err != nil {
		s.logger.Error("session create failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "login failed")
		return
	}

	http.SetCookie(w, s.sessionCookie(token, int(s.sessionTTL.Seconds())))
	s.logger.Info("login", zap.String("user_id", user.ID.String()), zap.String("role", string(user.Role)))

	writeJSON(w, http.StatusOK, meResponse{
		Email: user.Email,
		Role:  user.Role,
		OrgID: user.OrgID.String(),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
		// Delete the row, not just the cookie: the session must die server-side
		// even if the token was copied elsewhere.
		if err := s.store.DeleteSession(r.Context(), auth.HashToken(cookie.Value)); err != nil {
			s.logger.Error("session delete failed", zap.Error(err))
		}
	}
	http.SetCookie(w, s.sessionCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// handleMe is how the SPA learns who it is talking to, and whether it must show
// a login screen at all — the local build answers with the local principal.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
		return
	}
	writeJSON(w, http.StatusOK, meResponse{
		Email: p.Email,
		Role:  p.Role,
		OrgID: p.OrgID.String(),
		Local: p.Local,
	})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.Local {
		writeErr(w, http.StatusNotFound, "auth_disabled", "authentication is not enabled")
		return
	}

	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	user, err := s.store.GetUser(r.Context(), p.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "password change failed")
		return
	}
	if !auth.VerifyPassword(user.PasswordHash, req.CurrentPassword) {
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "current password is incorrect")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), p.UserID, hash); err != nil {
		s.logger.Error("password update failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "password change failed")
		return
	}

	// Every existing session dies with the old credential, including this one —
	// a session stolen before the change must not outlive it.
	if err := s.store.DeleteUserSessions(r.Context(), p.UserID); err != nil {
		s.logger.Error("session revoke failed", zap.Error(err))
	}
	http.SetCookie(w, s.sessionCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// ---- user administration (admin only) ------------------------------------

type createUserRequest struct {
	Email    string     `json:"email"`
	Password string     `json:"password"`
	Role     store.Role `json:"role"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context(), principal(r).OrgID)
	if err != nil {
		s.logger.Error("list users failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to list users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": users})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	email := auth.NormalizeEmail(req.Email)
	if email == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "email is required")
		return
	}
	if !req.Role.Valid() {
		writeErr(w, http.StatusBadRequest, "validation_error", "role must be admin|analyst|viewer")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	// Scoped to the caller's organisation: an admin cannot mint users elsewhere.
	user, err := s.store.CreateUser(r.Context(), store.CreateUserParams{
		OrgID:        principal(r).OrgID,
		Email:        email,
		PasswordHash: hash,
		Role:         req.Role,
	})
	if errors.Is(err, store.ErrEmailTaken) {
		writeErr(w, http.StatusConflict, "email_taken", "that email is already registered")
		return
	}
	if err != nil {
		s.logger.Error("create user failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to create user")
		return
	}

	writeJSON(w, http.StatusCreated, user)
}

type resetPasswordRequest struct {
	Password string `json:"password"`
}

// handleResetUserPassword lets an admin set a new password for someone else in
// their organisation.
//
// The alternative — deleting and recreating the account — would mint a new user
// id, so a forgotten password would silently break any record of who did what.
// Identity has to survive a lost credential.
//
// Self-service is deliberately NOT routed here: changing your own password goes
// through /auth/password, which demands the current one. Letting an admin skip
// that for their own account would turn a hijacked admin session into a
// permanent takeover.
func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "invalid user id")
		return
	}

	p := principal(r)
	if id == p.UserID {
		writeErr(w, http.StatusBadRequest, "validation_error",
			"use /auth/password to change your own password")
		return
	}

	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	// Same tenant check as deletion: an admin must not reach across organisations
	// by guessing an id, and a miss reads as absent rather than forbidden.
	target, err := s.store.GetUser(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && target.OrgID != p.OrgID) {
		writeErr(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load user")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), id, hash); err != nil {
		s.logger.Error("reset password failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to reset password")
		return
	}

	// Their existing sessions die with the old credential — otherwise a reset
	// prompted by a suspected compromise would leave the intruder signed in.
	if err := s.store.DeleteUserSessions(r.Context(), id); err != nil {
		s.logger.Error("session revoke failed", zap.Error(err))
	}

	s.logger.Info("password reset by admin",
		zap.String("target_user_id", id.String()),
		zap.String("by_user_id", p.UserID.String()))

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "invalid user id")
		return
	}

	p := principal(r)
	if id == p.UserID {
		writeErr(w, http.StatusBadRequest, "validation_error",
			"you cannot delete your own account")
		return
	}

	// Confirm the target is in the caller's organisation before touching it,
	// so an admin cannot delete across tenants by guessing an id.
	target, err := s.store.GetUser(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && target.OrgID != p.OrgID) {
		writeErr(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load user")
		return
	}

	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		s.logger.Error("delete user failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
