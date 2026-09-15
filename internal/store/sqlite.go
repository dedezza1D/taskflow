package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed schema_sqlite.sql
var sqliteSchema string

// sqliteDB adapts database/sql + modernc.org/sqlite to the DB seam.
//
// The driver is pure Go on purpose: a cgo dependency would make the desktop
// installer a cross-compilation problem on every platform it ships to.
type sqliteDB struct {
	db *sql.DB
}

// NewSQLite opens (creating if needed) a local database file and applies the
// schema. This is the desktop build's entry point — no server, no network, and
// the file never leaves the machine.
func NewSQLite(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite allows exactly one writer. The pipeline runs its stages
	// concurrently, so without this the second writer gets SQLITE_BUSY instead
	// of waiting its turn. Serialising here is cheaper than teaching every
	// caller to retry, and a single-user desktop app is not throughput-bound.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite schema: %w", err)
	}
	if err := upgradeSQLite(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("upgrade sqlite schema: %w", err)
	}

	return &Store{db: &sqliteDB{db: db}}, nil
}

// upgradeSQLite brings a database file created by an older build up to the
// current schema. The consolidated schema only covers fresh files: CREATE TABLE
// IF NOT EXISTS leaves an existing table exactly as it was.
func upgradeSQLite(ctx context.Context, db *sql.DB) error {
	has, err := sqliteHasColumn(ctx, db, "tasks", "org_id")
	if err != nil {
		return err
	}
	if !has {
		// SQLite cannot add a NOT NULL column without a default, and a default
		// tenant is what 005 removed on purpose. Nullable here; every insert
		// still names its tenant, and the backfill leaves no NULLs behind.
		stmts := []string{
			`ALTER TABLE tasks ADD COLUMN org_id TEXT REFERENCES organizations(id) ON DELETE CASCADE;`,
			`UPDATE tasks SET org_id = (SELECT d.org_id FROM documents d WHERE d.task_id = tasks.id)
			 WHERE org_id IS NULL;`,
			// A desktop file has exactly one organisation, so anything unlinked
			// belongs to it.
			`UPDATE tasks SET org_id = (SELECT id FROM organizations ORDER BY created_at LIMIT 1)
			 WHERE org_id IS NULL;`,
		}
		for _, q := range stmts {
			if _, err := db.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_tasks_org_created ON tasks(org_id, created_at DESC);`)
	return err
}

func sqliteHasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?;`, table, column).Scan(&n)
	return n > 0, err
}

// sqliteDSN carries the pragmas that have to be set per connection.
//
// WAL lets a reader proceed while a write is in flight; busy_timeout turns a
// lock collision into a wait rather than an immediate error; foreign_keys is OFF
// by default in SQLite, and the erasure cascade depends on it.
func sqliteDSN(path string) string {
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + strings.Join(pragmas, "&")
}

// sqliteTimeLayout is the single canonical on-disk timestamp format.
//
// SQLite has no date type: a timestamp is text, and every comparison is a STRING
// comparison. So the format has to be fixed-width, UTC, and lexicographically
// ordered — otherwise "greater string" stops meaning "later time".
//
// That is not hypothetical. Left to itself the driver writes Go's
// time.Time.String() ("2026-09-11 00:19:52.4143168 +0000 UTC") while SQLite's
// own CURRENT_TIMESTAMP writes "2026-09-11 00:19:52". The first is a prefix of
// the second plus more characters, so it compares GREATER no matter what the
// actual instants are — which made an expired session authenticate.
//
// Milliseconds, because that is the finest granularity SQLite's strftime('%f')
// emits and the schema defaults have to produce the identical shape.
const sqliteTimeLayout = "2006-01-02 15:04:05.000"

// bindArgs converts time.Time arguments to the canonical format. Every other
// type (uuid.UUID, strings, ints, nil) the driver handles natively.
func bindArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			out[i] = v.UTC().Format(sqliteTimeLayout)
		case *time.Time:
			if v == nil {
				out[i] = nil
			} else {
				out[i] = v.UTC().Format(sqliteTimeLayout)
			}
		default:
			out[i] = a
		}
	}
	return out
}

func (d *sqliteDB) QueryRow(ctx context.Context, query string, args ...any) Row {
	return sqliteRow{row: d.db.QueryRowContext(ctx, query, bindArgs(args)...)}
}

func (d *sqliteDB) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := d.db.QueryContext(ctx, query, bindArgs(args)...)
	if err != nil {
		return nil, translateSQLite(err)
	}
	return &sqliteRows{rows: rows}, nil
}

func (d *sqliteDB) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	res, err := d.db.ExecContext(ctx, query, bindArgs(args)...)
	if err != nil {
		return nil, translateSQLite(err)
	}
	return sqliteResult{res: res}, nil
}

func (d *sqliteDB) Close() {
	if d.db != nil {
		_ = d.db.Close()
	}
}

type sqliteRow struct{ row *sql.Row }

func (r sqliteRow) Scan(dest ...any) error {
	shims, finish := scanShims(dest)
	if err := translateSQLite(r.row.Scan(shims...)); err != nil {
		return err
	}
	return finish()
}

type sqliteRows struct{ rows *sql.Rows }

func (r *sqliteRows) Next() bool { return r.rows.Next() }
func (r *sqliteRows) Err() error { return translateSQLite(r.rows.Err()) }
func (r *sqliteRows) Close()     { _ = r.rows.Close() }

func (r *sqliteRows) Scan(dest ...any) error {
	shims, finish := scanShims(dest)
	if err := translateSQLite(r.rows.Scan(shims...)); err != nil {
		return err
	}
	return finish()
}

// sqliteResult adapts sql.Result, whose RowsAffected can fail; SQLite always
// knows the count, so an error here means the driver is broken and reporting 0
// is the honest answer — the store reads this only to distinguish "nothing
// matched" from "row updated", and 0 correctly says nothing matched.
type sqliteResult struct{ res sql.Result }

func (r sqliteResult) RowsAffected() int64 {
	n, err := r.res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}

// scanShims substitutes destinations database/sql cannot fill directly.
//
// json.RawMessage is the only one: it is a []byte underneath, but a named type,
// so the standard converter refuses it. Scanning through a plain []byte and
// copying afterwards keeps the 42 store methods free of backend-specific
// scanning. Everything else (uuid.UUID, time.Time and their pointer forms)
// round-trips natively.
func scanShims(dest []any) (shims []any, finish func() error) {
	var fixups []func()
	shims = make([]any, len(dest))

	var failure error

	for i, d := range dest {
		switch target := d.(type) {
		case *json.RawMessage:
			var raw []byte
			holder := &raw
			shims[i] = holder
			fixups = append(fixups, func() { *target = json.RawMessage(*holder) })

		// Timestamps come back as the canonical string bindArgs wrote, so they
		// are parsed with the same layout rather than left to the driver.
		case *time.Time:
			var raw string
			holder := &raw
			shims[i] = holder
			fixups = append(fixups, func() {
				t, err := parseSQLiteTime(*holder)
				if err != nil {
					failure = err
					return
				}
				*target = t
			})

		case **time.Time:
			var raw *string
			holder := &raw
			shims[i] = holder
			fixups = append(fixups, func() {
				if *holder == nil {
					*target = nil
					return
				}
				t, err := parseSQLiteTime(**holder)
				if err != nil {
					failure = err
					return
				}
				*target = &t
			})

		default:
			shims[i] = d
		}
	}

	return shims, func() error {
		for _, fix := range fixups {
			fix()
		}
		return failure
	}
}

// parseSQLiteTime accepts the canonical layout plus the shapes SQLite itself
// can produce, so a database touched by an external tool (or an older schema
// default) still reads back rather than failing at the scan.
func parseSQLiteTime(s string) (time.Time, error) {
	for _, layout := range []string{
		sqliteTimeLayout,
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

// translateSQLite maps the driver's vocabulary onto the neutral sentinels, the
// mirror of what the Postgres adapter does.
func translateSQLite(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoRows
	}
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %w", ErrUniqueViolation, err)
		}
	}
	return err
}
