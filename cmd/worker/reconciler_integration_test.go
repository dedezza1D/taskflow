package main

import (
	"context"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// TestReconcileOnce_Integration exercises the reconciler's real DB/queue effects
// (the decision logic itself is covered by TestDecideReconcile). Requires Postgres.
func TestReconcileOnce_Integration(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	defer pool.Close()

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	// The reconciler only PUBLISHES, and this test asserts on database state, so
	// the in-process broker serves as well as JetStream here — and keeps the test
	// free of a NATS dependency it never actually exercises.
	q := queue.NewLocal(st, zap.NewNop())

	cfg := &config.Config{
		WorkerMaxAttempts:        5,
		WorkerReconcileStaleness: 30 * time.Second, // the rows below are 1h old -> stale
	}

	// republish case: stale 'queued', no executions.
	id1 := uuid.New()
	// The legacy organisation is seeded by migrations 003 and 007.
	mustExec(t, ctx, pool, `INSERT INTO tasks (id,type,payload,priority,status,created_at,updated_at,version,org_id)
		VALUES ($1,'demo','{}','normal','queued', now()-interval '1 hour', now()-interval '1 hour', 1, '00000000-0000-0000-0000-000000000001')`, id1)

	// dead-letter case: stale 'processing' with executions at the cap.
	id2 := uuid.New()
	mustExec(t, ctx, pool, `INSERT INTO tasks (id,type,payload,priority,status,created_at,updated_at,version,org_id)
		VALUES ($1,'demo','{}','normal','processing', now()-interval '1 hour', now()-interval '1 hour', 1, '00000000-0000-0000-0000-000000000001')`, id2)
	mustExec(t, ctx, pool, `INSERT INTO task_executions (id,task_id,attempt,status,started_at,finished_at)
		SELECT gen_random_uuid(), $1, g, 'failed', now()-interval '1 hour', now()-interval '1 hour' FROM generate_series(1,5) g`, id2)

	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM tasks WHERE id = ANY($1)`, []uuid.UUID{id1, id2}) }()

	if err := reconcileOnce(ctx, zap.NewNop(), st, q, cfg); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}

	// republish: claimed (version bumped, updated_at refreshed) but still queued.
	s1, v1 := taskState(t, ctx, pool, id1)
	if s1 != "queued" {
		t.Fatalf("id1 status = %s, want queued", s1)
	}
	if v1 <= 1 {
		t.Fatalf("id1 version not bumped (claim/debounce did not fire): %d", v1)
	}

	// dead-letter: terminal failed, with NO extra execution (no rerun).
	s2, _ := taskState(t, ctx, pool, id2)
	if s2 != "failed" {
		t.Fatalf("id2 status = %s, want failed", s2)
	}
	if n := execCount(t, ctx, pool, id2); n != 5 {
		t.Fatalf("id2 executions = %d, want 5 (no rerun)", n)
	}

	// Second sweep: id1 was debounced (updated_at refreshed within the staleness
	// window), so it must NOT be re-selected/re-published — guards the amplification fix.
	if err := reconcileOnce(ctx, zap.NewNop(), st, q, cfg); err != nil {
		t.Fatalf("reconcileOnce (2nd): %v", err)
	}
	_, v1b := taskState(t, ctx, pool, id1)
	if v1b != v1 {
		t.Fatalf("id1 re-touched on second sweep (amplification not debounced): version %d -> %d", v1, v1b)
	}
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec failed: %v", err)
	}
}

func taskState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (status string, version int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT status, version FROM tasks WHERE id=$1`, id).Scan(&status, &version); err != nil {
		t.Fatalf("taskState: %v", err)
	}
	return status, version
}

func execCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_executions WHERE task_id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("execCount: %v", err)
	}
	return n
}
