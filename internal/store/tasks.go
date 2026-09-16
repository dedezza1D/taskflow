package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type CreateTaskParams struct {
	Type     string
	Payload  []byte // JSON
	Priority TaskPriority
	// OrgID is the tenant that owns the task. There is no fallback: the column
	// is NOT NULL with a foreign key, so a caller that forgets it fails loudly.
	OrgID uuid.UUID
}

func (s *Store) CreateTask(ctx context.Context, p CreateTaskParams) (*Task, error) {
	id := uuid.New()

	q := `
INSERT INTO tasks (id, type, payload, priority, status, org_id)
VALUES ($1, $2, $3, $4, 'queued', $5)
RETURNING id, type, payload, priority, status, created_at, updated_at, version;
`

	// The payload goes over as a string, not []byte. Postgres then resolves the
	// parameter against the jsonb column (a []byte would arrive as bytea and be
	// rejected), and SQLite stores it as TEXT — so one statement serves both
	// without a dialect-specific cast.
	var t Task
	err := s.db.QueryRow(ctx, q, id, p.Type, string(p.Payload), string(p.Priority), p.OrgID).Scan(
		&t.ID, &t.Type, &t.Payload, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.Version,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTask is the engine's read: workers and the reconciler act on any tenant's
// task. Request paths must use GetTaskForOrg.
func (s *Store) GetTask(ctx context.Context, id uuid.UUID) (*Task, error) {
	q := `
SELECT id, type, payload, priority, status, created_at, updated_at, version
FROM tasks
WHERE id = $1;
`
	return scanTask(s.db.QueryRow(ctx, q, id))
}

// GetTaskForOrg is the request-path read. Like GetDocumentForOrg, another
// tenant's task is ErrNotFound rather than forbidden, so an id probe cannot
// confirm that it exists elsewhere.
func (s *Store) GetTaskForOrg(ctx context.Context, id, orgID uuid.UUID) (*Task, error) {
	q := `
SELECT id, type, payload, priority, status, created_at, updated_at, version
FROM tasks
WHERE id = $1 AND org_id = $2;
`
	return scanTask(s.db.QueryRow(ctx, q, id, orgID))
}

func scanTask(row Row) (*Task, error) {
	var t Task
	err := row.Scan(
		&t.ID, &t.Type, &t.Payload, &t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.Version,
	)
	if errors.Is(err, ErrNoRows) {
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

// ListTasks lists across every tenant. It exists for the engine — the local
// queue claims queued work with it — and must never back a request path; use
// ListTasksForOrg there.
func (s *Store) ListTasks(ctx context.Context, p ListTasksParams) ([]Task, error) {
	return s.listTasks(ctx, p, nil)
}

// ListTasksForOrg is ListTasks scoped to one tenant.
func (s *Store) ListTasksForOrg(ctx context.Context, orgID uuid.UUID, p ListTasksParams) ([]Task, error) {
	return s.listTasks(ctx, p, &orgID)
}

func (s *Store) listTasks(ctx context.Context, p ListTasksParams, orgID *uuid.UUID) ([]Task, error) {
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	offset := p.Offset
	if offset < 0 {
		offset = 0
	}

	var status *string
	if p.Status != nil {
		sv := string(*p.Status)
		status = &sv
	}
	args := []any{status, p.Type, limit, offset}

	// The tenant clause is appended rather than made optional in SQL: org_id is
	// a UUID on PostgreSQL, so the CAST(... AS TEXT) IS NULL trick used for the
	// other filters would compare uuid to text and fail the statement.
	tenant := ""
	if orgID != nil {
		tenant = "\n  AND org_id = $5"
		args = append(args, *orgID)
	}

	q := `
SELECT id, type, payload, priority, status, created_at, updated_at, version
FROM tasks
-- CAST rather than $1::text: the same statement runs on SQLite, which has no
-- :: operator. A bare "$1 IS NULL" leaves PostgreSQL unable to infer the
-- parameter type and it fails the statement with 42P08.
WHERE (CAST($1 AS TEXT) IS NULL OR status = CAST($1 AS TEXT))
  AND (CAST($2 AS TEXT) IS NULL OR type = CAST($2 AS TEXT))` + tenant + `
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;
`

	rows, err := s.db.Query(ctx, q, args...)
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

// ListStaleTasks returns tasks stuck in a non-terminal state whose updated_at is
// older than cutoff — candidates for reconciliation: a 'queued' task whose enqueue
// was lost, or a 'processing' task left behind by a crashed worker. Ordered oldest
// first so the most-stuck are handled before the per-pass limit is reached.
func (s *Store) ListStaleTasks(ctx context.Context, cutoff time.Time, limit int) ([]Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	q := `
SELECT id, type, payload, priority, status, created_at, updated_at, version
FROM tasks
WHERE status IN ('queued', 'processing')
  AND updated_at < $1
ORDER BY updated_at ASC
LIMIT $2;
`
	rows, err := s.db.Query(ctx, q, cutoff, limit)
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
	if errors.Is(err, ErrNoRows) {
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

// MarkTaskEnqueued records that the task's message reached the queue.
//
// This is the transactional-outbox marker (see migration 008): NULL means the
// row is committed but no delivery was ever confirmed, which is the state a
// crash between COMMIT and publish leaves behind. Idempotent and not
// version-guarded — it races with nothing, since it only ever fills a NULL.
func (s *Store) MarkTaskEnqueued(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`UPDATE tasks SET enqueued_at = $2 WHERE id = $1 AND enqueued_at IS NULL;`, id, time.Now())
	return err
}

// ListUnpublishedTasks returns tasks that are committed, still queued, and have
// never been confirmed on the queue — a publish that was lost, or never made.
//
// Deliberately narrow, because that narrowness is what makes it fast. Nothing
// can be holding one of these: a worker only sees a task through a message, and
// there was no message. So the cutoff only has to clear the moment between
// INSERT and publish, not the processing ceiling the staleness sweep must wait
// out. A task that reached the queue and is merely waiting for a free worker
// carries a timestamp and is never selected here, however long the backlog.
func (s *Store) ListUnpublishedTasks(ctx context.Context, cutoff time.Time, limit int) ([]Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	q := `
SELECT id, type, payload, priority, status, created_at, updated_at, version
FROM tasks
WHERE status = 'queued'
  AND enqueued_at IS NULL
  AND created_at < $1
ORDER BY created_at ASC
LIMIT $2;
`
	rows, err := s.db.Query(ctx, q, cutoff, limit)
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
	return out, rows.Err()
}
