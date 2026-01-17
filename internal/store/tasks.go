package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type CreateTaskParams struct {
	Type     string
	Payload  []byte // JSON
	Priority TaskPriority
}

func (s *Store) CreateTask(ctx context.Context, p CreateTaskParams) (*Task, error) {
	id := uuid.New()

	q := `
INSERT INTO tasks (id, type, payload, priority, status)
VALUES ($1, $2, $3::jsonb, $4, 'queued')
RETURNING id, type, payload, priority, status, created_at, updated_at, version;
`

	var t Task
	err := s.db.QueryRow(ctx, q, id, p.Type, p.Payload, string(p.Priority)).Scan(
		&t.ID, &t.Type, &t.Payload, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.Version,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) GetTask(ctx context.Context, id uuid.UUID) (*Task, error) {
	q := `
SELECT id, type, payload, priority, status, created_at, updated_at, version
FROM tasks
WHERE id = $1;
`
	var t Task
	err := s.db.QueryRow(ctx, q, id).Scan(
		&t.ID, &t.Type, &t.Payload, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

type ListTasksParams struct {
	Status *TaskStatus
	Type   *string
	Limit  int
	Offset int
}

func (s *Store) ListTasks(ctx context.Context, p ListTasksParams) ([]Task, error) {
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	offset := p.Offset
	if offset < 0 {
		offset = 0
	}

	// simple filter building (safe parameterization)
	q := `
SELECT id, type, payload, priority, status, created_at, updated_at, version
FROM tasks
WHERE ($1::text IS NULL OR status = $1)
  AND ($2::text IS NULL OR type = $2)
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;
`

	var status *string
	if p.Status != nil {
		sv := string(*p.Status)
		status = &sv
	}

	rows, err := s.db.Query(ctx, q, status, p.Type, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Task, 0, limit)
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Type, &t.Payload, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.Version); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Update status with optimistic locking
func (s *Store) UpdateTaskStatus(ctx context.Context, id uuid.UUID, expectedVersion int, newStatus TaskStatus) (*Task, error) {
	q := `
UPDATE tasks
SET status = $3,
    version = version + 1
WHERE id = $1 AND version = $2
RETURNING id, type, payload, priority, status, created_at, updated_at, version;
`

	var t Task
	err := s.db.QueryRow(ctx, q, id, expectedVersion, string(newStatus)).Scan(
		&t.ID, &t.Type, &t.Payload, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// either not found OR version mismatch; check existence
		_, getErr := s.GetTask(ctx, id)
		if getErr == ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, ErrVersionConflict
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
