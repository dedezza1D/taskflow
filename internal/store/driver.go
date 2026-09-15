package store

import (
	"context"
	"errors"
)

// The driver seam.
//
// The store has 42 methods but touches its database through exactly four
// operations. So the abstraction goes HERE, at the driver, rather than as an
// interface over the repository: a 42-method interface would have to be
// implemented twice, kept in sync by hand, and would push the change out to
// every call site. This way the SQL and the scanning — the parts that carry the
// domain rules — are written once, and a second backend only has to answer four
// questions.
//
// That matters because the desktop build runs the same pipeline against SQLite:
// one process, one file, no server to connect to.

// ErrNoRows is the driver-neutral "query matched nothing". Each backend
// translates its own sentinel into this one, so the methods above never mention
// pgx or database/sql.
var ErrNoRows = errors.New("no rows in result set")

// ErrUniqueViolation is the driver-neutral unique-constraint breach. The store
// relies on it for concurrency control — two workers racing the same
// (task_id, attempt) must produce one winner — so it cannot stay behind a
// backend-specific SQLSTATE.
var ErrUniqueViolation = errors.New("unique constraint violation")

type Row interface {
	Scan(dest ...any) error
}

type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// Result is the subset of a write's outcome the store actually reads.
// RowsAffected is how optimistic locking tells "someone else moved it" from
// "it isn't there", so it is not optional.
type Result interface {
	RowsAffected() int64
}

type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (Result, error)
	Close()
}
