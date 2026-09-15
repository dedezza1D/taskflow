package main

import (
	"context"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Nothing used to call the expiry deletes, so both tables grew for the life of
// a deployment. The sweep has to remove what is dead and leave what is still
// meaningful: a live session, and a recently expired recovery link that should
// still answer "already used" rather than "invalid".
func TestSweepAuthOnce_Integration(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.DSN(t)

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	defer pool.Close()

	org := uuid.New()
	if _, err := st.CreateOrganization(ctx, org, "housekeeping"); err != nil {
		t.Fatal(err)
	}
	// Users, sessions and tokens all cascade from the organisation.
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, org) }()

	user, err := st.CreateUser(ctx, store.CreateUserParams{
		OrgID: org, Email: "housekeeping-" + uuid.NewString()[:8] + "@example.test",
		PasswordHash: "h", Role: store.RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	tag := uuid.NewString()[:8]
	sessions := map[string]struct {
		expires time.Time
		keep    bool
	}{
		"session-expired-" + tag: {now.Add(-time.Minute), false},
		"session-live-" + tag:    {now.Add(time.Hour), true},
	}
	for hash, s := range sessions {
		if err := st.CreateSession(ctx, hash, user.ID, s.expires); err != nil {
			t.Fatal(err)
		}
	}

	tokens := map[string]struct {
		expires time.Time
		keep    bool
	}{
		"token-long-expired-" + tag:     {now.Add(-resetTokenGrace - time.Hour), false},
		"token-recently-expired-" + tag: {now.Add(-time.Hour), true}, // inside the grace period
		"token-live-" + tag:             {now.Add(time.Hour), true},
	}
	for hash, tk := range tokens {
		if err := st.CreatePasswordResetToken(ctx, hash, user.ID, tk.expires); err != nil {
			t.Fatal(err)
		}
	}

	sweepAuthOnce(ctx, zap.NewNop(), st)

	count := func(table, hash string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE token_hash = $1`, hash).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	for hash, s := range sessions {
		if got := count("sessions", hash); (got == 1) != s.keep {
			t.Errorf("session %s: present=%v, want %v", hash, got == 1, s.keep)
		}
	}
	for hash, tk := range tokens {
		if got := count("password_reset_tokens", hash); (got == 1) != tk.keep {
			t.Errorf("reset token %s: present=%v, want %v", hash, got == 1, tk.keep)
		}
	}
}
