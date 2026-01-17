package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) CreateExecution(ctx context.Context, taskID uuid.UUID, attempt int) (*TaskExecution, error) {
	id := uuid.New()
	q := `
INSERT INTO task_executions (id, task_id, attempt, status)
VALUES ($1, $2, $3, 'started')
RETURNING id, task_id, attempt, status, error, started_at, finished_at;
`
	var e TaskExecution
	err := s.db.QueryRow(ctx, q, id, taskID, attempt).Scan(
		&e.ID, &e.TaskID, &e.Attempt, &e.Status, &e.Error, &e.StartedAt, &e.FinishedAt,
	)
	if err != nil {
		// Idempotency: if another worker already inserted (task_id, attempt), treat as "already exists".
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrAlreadyExists
		}
		return nil, err
	}
	return &e, nil
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
	if errors.Is(err, pgx.ErrNoRows) {
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
