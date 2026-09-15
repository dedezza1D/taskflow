package main

import (
	"context"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

// authHousekeepingInterval paces the sweep. Correctness never depends on it:
// an expired session or recovery link is refused by the lookup itself, in SQL.
// The sweep only keeps those tables from growing for as long as the deployment
// lives — which, before it ran, they did: nothing called the delete methods.
const authHousekeepingInterval = 15 * time.Minute

// resetTokenGrace keeps an expired recovery link around for a day. While the row
// exists, redeeming a spent link answers "already used" rather than "invalid",
// which is the more useful thing to tell someone clicking yesterday's email.
const resetTokenGrace = 24 * time.Hour

// runAuthHousekeeping deletes expired sessions and long-expired recovery
// tokens. Safe in every replica: both deletes are idempotent, so concurrent
// sweeps converge.
func runAuthHousekeeping(ctx context.Context, logger *zap.Logger, st *store.Store) {
	logger.Info("auth housekeeping started",
		zap.Duration("interval", authHousekeepingInterval),
		zap.Duration("reset_token_grace", resetTokenGrace),
	)

	// Once at startup too: a deployment that restarts more often than the
	// interval would otherwise never reach the first pass.
	sweepAuthOnce(ctx, logger, st)

	ticker := time.NewTicker(authHousekeepingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("auth housekeeping stopped")
			return
		case <-ticker.C:
			sweepAuthOnce(ctx, logger, st)
		}
	}
}

// sweepAuthOnce runs one pass. The two deletes are independent: a failure in
// one is logged and does not stop the other.
func sweepAuthOnce(ctx context.Context, logger *zap.Logger, st *store.Store) {
	sessions, err := st.DeleteExpiredSessions(ctx)
	if err != nil {
		logger.Warn("expired session sweep failed", zap.Error(err))
	}

	tokens, err := st.DeleteExpiredPasswordResetTokens(ctx, resetTokenGrace)
	if err != nil {
		logger.Warn("expired reset token sweep failed", zap.Error(err))
	}

	if sessions > 0 || tokens > 0 {
		logger.Info("auth housekeeping pass",
			zap.Int64("sessions_deleted", sessions),
			zap.Int64("reset_tokens_deleted", tokens),
		)
	}
}
