package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/mail"
	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

// resetThrottle bounds how often one account can trigger a recovery email.
// Without it the endpoint is a free mail cannon pointed at any address the
// attacker knows — the victim's inbox is the target, not the account.
const resetThrottle = 60 * time.Second

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

type resetPasswordWithTokenRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// authConfigResponse is public and carries no secrets — just what the login
// screen needs to decide which controls to render.
type authConfigResponse struct {
	AuthEnabled      bool `json:"auth_enabled"`
	PasswordRecovery bool `json:"password_recovery"`
}

func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, authConfigResponse{
		AuthEnabled:      s.authEnabled,
		PasswordRecovery: s.authEnabled && s.mailer != nil,
	})
}

// handleForgotPassword issues a recovery link.
//
// It answers 204 no matter what — unknown address, throttled, or send failure.
// Any other behaviour turns the endpoint into an oracle for which addresses
// have accounts, which in a tool whose users are named people is disclosure in
// its own right. The cost is that a genuine misconfiguration looks like success
// to the caller, so failures are logged loudly on the server.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled || s.mailer == nil {
		writeErr(w, http.StatusNotFound, "recovery_disabled",
			"password recovery is not configured on this server")
		return
	}

	var req forgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	email := auth.NormalizeEmail(req.Email)
	// Detach from the request: the response is sent immediately and identically
	// either way, so the work must not die with the connection — and its timing
	// must not be observable.
	go s.issueRecoveryEmail(context.WithoutCancel(r.Context()), email)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) issueRecoveryEmail(ctx context.Context, email string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	user, err := s.store.GetUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		// The address is not logged: it is personal data, and here it is
		// whatever the caller chose to type.
		s.logger.Info("recovery requested for unknown address")
		return
	}
	if err != nil {
		s.logger.Error("recovery lookup failed", zap.Error(err))
		return
	}

	last, err := s.store.LastPasswordResetRequest(ctx, user.ID)
	if err != nil {
		s.logger.Error("recovery throttle lookup failed", zap.Error(err))
		return
	}
	if !last.IsZero() && time.Since(last) < resetThrottle {
		s.logger.Info("recovery throttled", zap.String("user_id", user.ID.String()))
		return
	}

	token, tokenHash, err := auth.NewSessionToken()
	if err != nil {
		s.logger.Error("recovery token generation failed", zap.Error(err))
		return
	}
	if err := s.store.CreatePasswordResetToken(ctx, tokenHash, user.ID, time.Now().Add(s.resetTTL)); err != nil {
		s.logger.Error("recovery token store failed", zap.Error(err))
		return
	}

	link := fmt.Sprintf("%s/reset-password?token=%s",
		strings.TrimRight(s.baseURL, "/"), url.QueryEscape(token))

	if err := s.mailer.Send(ctx, mail.Message{
		To:      user.Email,
		Subject: "Redefinir sua senha do TaskFlow Compliance",
		Body:    recoveryBody(link, s.resetTTL),
	}); err != nil {
		s.logger.Error("recovery email send failed",
			zap.Error(err), zap.String("user_id", user.ID.String()))
		return
	}

	s.logger.Info("recovery email sent", zap.String("user_id", user.ID.String()))
}

func recoveryBody(link string, ttl time.Duration) string {
	return fmt.Sprintf(`Alguém pediu a redefinição da senha desta conta no TaskFlow Compliance.

Para escolher uma senha nova, abra:

%s

O link vale por %s e só pode ser usado uma vez.

Se não foi você, ignore esta mensagem: a senha atual continua valendo e nada
muda até que o link seja aberto.
`, link, humanDuration(ttl))
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d hora(s)", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutos", int(d.Minutes()))
	}
}

// handleResetPassword redeems a recovery link.
//
// Unlike the request side, this one answers honestly: the caller already holds
// the token, so telling them it expired or was already used reveals nothing
// they could not learn by trying, and leaving them guessing helps nobody.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled || s.mailer == nil {
		writeErr(w, http.StatusNotFound, "recovery_disabled",
			"password recovery is not configured on this server")
		return
	}

	var req resetPasswordWithTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Token == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "token is required")
		return
	}

	// Validate the new password BEFORE spending the token: a too-short password
	// should not burn the user's one-shot link and force another email.
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	user, err := s.store.ConsumePasswordResetToken(r.Context(), auth.HashToken(req.Token))
	switch {
	case errors.Is(err, store.ErrTokenUsed):
		writeErr(w, http.StatusGone, "token_used",
			"this link has already been used; request a new one")
		return
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusGone, "token_invalid",
			"this link is invalid or has expired; request a new one")
		return
	case err != nil:
		s.logger.Error("consume reset token failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "could not reset the password")
		return
	}

	if err := s.store.UpdateUserPassword(r.Context(), user.ID, hash); err != nil {
		s.logger.Error("reset password write failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "could not reset the password")
		return
	}

	// Everything tied to the old credential goes: live sessions, and any other
	// recovery link still outstanding.
	if err := s.store.DeleteUserSessions(r.Context(), user.ID); err != nil {
		s.logger.Error("session revoke failed", zap.Error(err))
	}
	if err := s.store.InvalidatePasswordResetTokens(r.Context(), user.ID); err != nil {
		s.logger.Error("reset token invalidation failed", zap.Error(err))
	}

	s.logger.Info("password reset via recovery link", zap.String("user_id", user.ID.String()))
	http.SetCookie(w, s.sessionCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}
