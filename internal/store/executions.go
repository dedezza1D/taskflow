package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// CreateExecution records a new execution attempt for a task. The attempt number
// is assigned from the execution ledger (MAX(attempt)+1) in a single statement, so
// it is durable and independent of the broker's delivery count — a republished or
// redelivered message always advances the attempt rather than colliding with a
// prior one. The unique (task_id, attempt) index remains the concurrency guard: if
// two deliveries race, one wins and the other gets ErrAlreadyExists.
func (s *Store) CreateExecution(ctx context.Context, taskID uuid.UUID) (*TaskExecution, error) {
	id := uuid.New()
	q := `
INSERT INTO task_executions (id, task_id, attempt, status)
VALUES ($1, $2, (SELECT COALESCE(MAX(attempt), 0) + 1 FROM task_executions WHERE task_id = $2), 'started')
RETURNING id, task_id, attempt, status, error, started_at, finished_at;
`
	var e TaskExecution
	err := s.db.QueryRow(ctx, q, id, taskID).Scan(
		&e.ID, &e.TaskID, &e.Attempt, &e.Status, &e.Error, &e.StartedAt, &e.FinishedAt,
	)
	if err != nil {
		// Concurrency: if another worker just inserted the same (task_id, attempt), treat as "already exists".
		if errors.Is(err, ErrUniqueViolation) {
			return nil, ErrAlreadyExists
		}
		return nil, err
	}
	return &e, nil
}

// MaxAttempt returns the highest attempt number recorded for a task, or 0 if the
// task has no executions yet. It is the durable source of truth for both the
// worker's attempt cap and the reconciler's lost-enqueue vs crash-pill decision.
func (s *Store) MaxAttempt(ctx context.Context, taskID uuid.UUID) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT COALESCE(MAX(attempt), 0) FROM task_executions WHERE task_id = $1`,
		taskID,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) FinishExecution(ctx context.Context, execID uuid.UUID, status ExecutionStatus, errMsg *string) (*TaskExecution, error) {
	q := `
UPDATE task_executions
SET status = $2,
    error = $3,
    finished_at = $4
WHERE id = $1
RETURNING id, task_id, attempt, status, error, started_at, finished_at;
`
	now := time.Now()

	var e TaskExecution
	err := s.db.QueryRow(ctx, q, execID, string(status), errMsg, now).Scan(
		&e.ID, &e.TaskID, &e.Attempt, &e.Status, &e.Error, &e.StartedAt, &e.FinishedAt,
	)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) ListExecutions(ctx context.Context, taskID uuid.UUID, limit int) ([]TaskExecution, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	q := `
SELECT id, task_id, attempt, status, error, started_at, finished_at
FROM task_executions
WHERE task_id = $1
ORDER BY attempt DESC
LIMIT $2;
`
	rows, err := s.db.Query(ctx, q, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TaskExecution, 0, limit)
	for rows.Next() {
		var e TaskExecution
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Attempt, &e.Status, &e.Error, &e.StartedAt, &e.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
