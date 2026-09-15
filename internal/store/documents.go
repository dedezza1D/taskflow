package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type DocumentStatus string

const (
	DocUploaded   DocumentStatus = "uploaded"
	DocProcessing DocumentStatus = "processing"
	DocCompleted  DocumentStatus = "completed"
	DocFailed     DocumentStatus = "failed"
	// DocErased is the erasure tombstone: while the row still exists during the
	// C4 sequence, any worker holding an in-flight message sees a terminal
	// status and acks without processing.
	DocErased DocumentStatus = "erased"
)

type Document struct {
	ID          uuid.UUID      `json:"id"`
	Filename    string         `json:"filename"`
	ContentType string         `json:"content_type"`
	StorageURI  string         `json:"storage_uri"`
	Status      DocumentStatus `json:"status"`
	FailedStage *string        `json:"failed_stage,omitempty"`
	TaskID      *uuid.UUID     `json:"task_id,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Version     int            `json:"version"`
	// RawShreddedAt is when the original and OCR text were destroyed. Nil means
	// the document still holds raw content. Exposed so a reader can verify data
	// minimisation happened rather than take it on trust.
	RawShreddedAt *time.Time `json:"raw_shredded_at,omitempty"`
}

type DocumentArtifact struct {
	ID         uuid.UUID `json:"id"`
	DocumentID uuid.UUID `json:"document_id"`
	Stage      string    `json:"stage"`
	Kind       string    `json:"kind"`
	StorageURI string    `json:"storage_uri"`
	CreatedAt  time.Time `json:"created_at"`
}

const documentColumns = `id, filename, content_type, storage_uri, status, failed_stage, task_id, created_at, updated_at, version, raw_shredded_at`

func scanDocument(row Row) (*Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.Filename, &d.ContentType, &d.StorageURI, &d.Status,
		&d.FailedStage, &d.TaskID, &d.CreatedAt, &d.UpdatedAt, &d.Version, &d.RawShreddedAt)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

type CreateDocumentParams struct {
	Filename    string
	ContentType string
	StorageURI  string
	// OrgID is the owning tenant. Required: it is what every read filters on,
	// so a zero value here would create a document nobody can reach.
	OrgID uuid.UUID
}

func (s *Store) CreateDocument(ctx context.Context, p CreateDocumentParams) (*Document, error) {
	return s.CreateDocumentWithID(ctx, uuid.New(), p)
}

// CreateDocumentWithID lets the caller mint the ID first — the upload path
// names the object key (documents/{id}/original) BEFORE inserting the row, so
// the row can carry the final storage URI from birth.
func (s *Store) CreateDocumentWithID(ctx context.Context, id uuid.UUID, p CreateDocumentParams) (*Document, error) {
	q := `
INSERT INTO documents (id, filename, content_type, storage_uri, status, org_id)
VALUES ($1, $2, $3, $4, 'uploaded', $5)
RETURNING ` + documentColumns + `;`
	return scanDocument(s.db.QueryRow(ctx, q, id, p.Filename, p.ContentType, p.StorageURI, p.OrgID))
}

// GetDocument loads a document regardless of tenant. Used by the pipeline, which
// acts on behalf of the system rather than a signed-in user; request paths must
// use GetDocumentForOrg so one tenant cannot read another's documents by id.
func (s *Store) GetDocument(ctx context.Context, id uuid.UUID) (*Document, error) {
	q := `SELECT ` + documentColumns + ` FROM documents WHERE id = $1;`
	return scanDocument(s.db.QueryRow(ctx, q, id))
}

// GetDocumentForOrg is the request-path read: a document belonging to another
// tenant is ErrNotFound, not a 403, so an id probe cannot confirm that a
// document exists elsewhere.
func (s *Store) GetDocumentForOrg(ctx context.Context, id, orgID uuid.UUID) (*Document, error) {
	q := `SELECT ` + documentColumns + ` FROM documents WHERE id = $1 AND org_id = $2;`
	return scanDocument(s.db.QueryRow(ctx, q, id, orgID))
}

// SetDocumentTask links the driving document.process task to the document. Done
// once at upload; not version-guarded because it races with nothing.
func (s *Store) SetDocumentTask(ctx context.Context, id, taskID uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `UPDATE documents SET task_id = $2 WHERE id = $1;`, id, taskID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateDocumentStatus transitions status (and failed_stage) with optimistic
// locking — same semantics as UpdateTaskStatus: ErrNotFound if the row is gone,
// ErrVersionConflict if someone else moved it first.
func (s *Store) UpdateDocumentStatus(ctx context.Context, id uuid.UUID, expectedVersion int, status DocumentStatus, failedStage *string) (*Document, error) {
	q := `
UPDATE documents
SET status = $3,
    failed_stage = $4,
    version = version + 1
WHERE id = $1 AND version = $2
RETURNING ` + documentColumns + `;`
	d, err := scanDocument(s.db.QueryRow(ctx, q, id, expectedVersion, string(status), failedStage))
	if errors.Is(err, ErrNotFound) {
		if _, getErr := s.GetDocument(ctx, id); getErr == ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, ErrVersionConflict
	}
	return d, err
}

type ListDocumentsParams struct {
	Status *DocumentStatus
	Limit  int
	Offset int
	// OrgID scopes the listing to one tenant. Required on request paths.
	OrgID uuid.UUID
}

func (s *Store) ListDocuments(ctx context.Context, p ListDocumentsParams) ([]Document, error) {
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}

	q := `
SELECT ` + documentColumns + `
FROM documents
WHERE org_id = $4
  -- CAST rather than $1::text because this same statement also runs on
  -- SQLite, which has no :: operator. And the cast is not cosmetic: a bare
  -- "$1 IS NULL" gives PostgreSQL nothing to infer the parameter type
  -- from, and it rejects the whole statement with 42P08 before it ever reaches
  -- the comparison that would have told it.
  AND (CAST($1 AS TEXT) IS NULL OR status = CAST($1 AS TEXT))
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;`

	var status *string
	if p.Status != nil {
		sv := string(*p.Status)
		status = &sv
	}

	rows, err := s.db.Query(ctx, q, status, limit, offset, p.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Document, 0, limit)
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Filename, &d.ContentType, &d.StorageURI, &d.Status,
			&d.FailedStage, &d.TaskID, &d.CreatedAt, &d.UpdatedAt, &d.Version, &d.RawShreddedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkRawShredded records that a document's raw material (the original and the
// OCR text) has been destroyed. Idempotent and NOT version-guarded: it races
// with nothing — the raw bytes are already gone by the time it runs, and a
// second call on an already-shredded row is a no-op rather than a conflict.
// The timestamp is the audit answer to "when was this minimised?".
func (s *Store) MarkRawShredded(ctx context.Context, id uuid.UUID) error {
	q := `UPDATE documents SET raw_shredded_at = $2
	      WHERE id = $1 AND raw_shredded_at IS NULL;`
	_, err := s.db.Exec(ctx, q, id, time.Now())
	return err
}

// ListRawShreddablePending returns documents that still hold raw bytes and have
// been in a terminal state since before cutoff.
//
// Two populations land here, and both matter:
//
//   - DEAD-LETTERED documents, which have no completion event to hang a shred
//     on and would otherwise keep their original forever — the worst case,
//     since nobody looks at a failed document again.
//   - COMPLETED documents whose inline shred did not finish (object store
//     briefly unavailable, or a row predating this feature). The pipeline
//     deliberately leaves raw_shredded_at NULL when removal fails, and this is
//     the retry that makes that safe. Without it, "the sweep will get it" would
//     be a comment that lies.
//
// Erased documents are excluded: C4 already removed the whole prefix.
func (s *Store) ListRawShreddablePending(ctx context.Context, cutoff time.Time, limit int) ([]Document, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `
SELECT ` + documentColumns + `
FROM documents
WHERE raw_shredded_at IS NULL
  AND status IN ('failed', 'completed')
  AND updated_at < $1
ORDER BY updated_at
LIMIT $2;`

	rows, err := s.db.Query(ctx, q, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Document, 0, limit)
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Filename, &d.ContentType, &d.StorageURI, &d.Status,
			&d.FailedStage, &d.TaskID, &d.CreatedAt, &d.UpdatedAt, &d.Version, &d.RawShreddedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateArtifact records a stage checkpoint idempotently: the unique
// (document_id, stage, kind) index absorbs the at-least-once double-write, and
// the caller always gets the surviving row back.
func (s *Store) CreateArtifact(ctx context.Context, docID uuid.UUID, stage, kind, storageURI string) (*DocumentArtifact, error) {
	id := uuid.New()
	q := `
INSERT INTO document_artifacts (id, document_id, stage, kind, storage_uri)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (document_id, stage, kind) DO NOTHING;`
	if _, err := s.db.Exec(ctx, q, id, docID, stage, kind, storageURI); err != nil {
		return nil, err
	}
	return s.GetArtifact(ctx, docID, stage, kind)
}

func (s *Store) GetArtifact(ctx context.Context, docID uuid.UUID, stage, kind string) (*DocumentArtifact, error) {
	q := `
SELECT id, document_id, stage, kind, storage_uri, created_at
FROM document_artifacts
WHERE document_id = $1 AND stage = $2 AND kind = $3;`
	var a DocumentArtifact
	err := s.db.QueryRow(ctx, q, docID, stage, kind).Scan(
		&a.ID, &a.DocumentID, &a.Stage, &a.Kind, &a.StorageURI, &a.CreatedAt)
	if errors.Is(err, ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListArtifacts enumerates every artifact of a document — the enumerability
// half of C4: erasure can prove it visited everything.
func (s *Store) ListArtifacts(ctx context.Context, docID uuid.UUID) ([]DocumentArtifact, error) {
	q := `
SELECT id, document_id, stage, kind, storage_uri, created_at
FROM document_artifacts
WHERE document_id = $1
ORDER BY created_at ASC;`
	rows, err := s.db.Query(ctx, q, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DocumentArtifact
	for rows.Next() {
		var a DocumentArtifact
		if err := rows.Scan(&a.ID, &a.DocumentID, &a.Stage, &a.Kind, &a.StorageURI, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
