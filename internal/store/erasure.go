package store

import (
	"context"

	"github.com/google/uuid"
)

// DeleteDocument removes the documents row; document_artifacts cascade with it.
// Returns whether a row was deleted (erasure is idempotent — deleting an
// already-erased document is not an error).
func (s *Store) DeleteDocument(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM documents WHERE id = $1;`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteTask removes a task row; task_executions cascade with it. Part of the
// C4 erasure sequence: after this, a worker holding an in-flight message for
// the task loads ErrNotFound and acks — the queue-side tombstone falls out of
// the engine's existing "task not found → ack" rule.
func (s *Store) DeleteTask(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM tasks WHERE id = $1;`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
