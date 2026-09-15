// Package testdb gives the integration tests a PostgreSQL database of their own.
//
// They used to run against the development database — the one the
// docker-compose worker is attached to. That worker's reconciler found every
// task a test had left queued, republished it, and processed it against
// objects that lived in a test's temp directory: the development UI filled up
// with dead-lettered documents nobody had uploaded, next to orphaned
// organisations and queued rows that never finished. Tests need rows that
// nothing else acts on, so they get a database nothing else is attached to.
//
// Only test files import this package.
package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Name is the database created when TEST_DATABASE_URL is not set.
const Name = "taskflow_test"

const defaultBaseURL = "postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable"

// migrationLock serialises migration across test binaries: `go test ./...` runs
// packages in parallel, and concurrent CREATE TRIGGER / ALTER TABLE on the same
// schema deadlock or fail on catalog races. The value is arbitrary but fixed.
const migrationLock = 7_246_014_202

// createLock serialises creating the database itself; see ensureDatabase.
const createLock = 7_246_014_201

var (
	once   sync.Once
	dsn    string
	setErr error
)

// DSN returns the connection string of a migrated test database, creating the
// database on first use in this process.
//
// TEST_DATABASE_URL names it explicitly. Otherwise the server comes from
// DATABASE_URL (or the docker-compose default) and the database is Name — never
// the one DATABASE_URL points at.
//
// A missing server fails the test rather than skipping it: a skipped
// integration test reports "ok" for coverage that never ran.
func DSN(t testing.TB) string {
	t.Helper()
	once.Do(func() { dsn, setErr = prepare() })
	if setErr != nil {
		t.Fatalf("PostgreSQL is required by these integration tests.\n"+
			"  start it with: docker compose up -d postgres\n"+
			"  cause: %v", setErr)
	}
	return dsn
}

func prepare() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	target := os.Getenv("TEST_DATABASE_URL")
	if target == "" {
		base := os.Getenv("DATABASE_URL")
		if base == "" {
			base = defaultBaseURL
		}
		var err error
		if target, err = withDatabase(base, Name); err != nil {
			return "", err
		}
		if err := ensureDatabase(ctx, base, Name); err != nil {
			return "", err
		}
	}

	if err := migrate(ctx, target); err != nil {
		return "", err
	}
	return target, nil
}

func withDatabase(rawURL, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// ensureDatabase creates the test database through the server's existing one.
//
// Serialised with an advisory lock on that database, which every test binary
// connects to. Without it, the first `go test ./...` on a fresh server had
// several packages see "no such database" at once and race CREATE DATABASE;
// the losers failed with a unique violation on pg_database and took every test
// in their package down with them.
func ensureDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect %s: %w", redact(adminURL), err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, createLock); err != nil {
		return fmt.Errorf("create lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, createLock) }()

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return fmt.Errorf("look up database %s: %w", name, err)
	}
	if exists {
		return nil
	}

	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	var pgErr *pgconn.PgError
	// Still tolerated: something outside this package may create it too.
	if errors.As(err, &pgErr) && (pgErr.Code == "42P04" || pgErr.Code == "23505") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// migrate applies every migration in order. They are idempotent by contract,
// so running them on each test process is safe and keeps the test schema from
// drifting behind a newly added file.
func migrate(ctx context.Context, target string) error {
	dir, err := migrationsDir()
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	conn, err := pgx.Connect(ctx, target)
	if err != nil {
		return fmt.Errorf("connect %s: %w", redact(target), err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLock) }()

	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		// Through pgconn: a migration is several statements, which only the
		// simple protocol accepts in one round trip.
		if _, err := conn.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// migrationsDir finds api/deployments/migrations from wherever `go test` runs a
// package: its working directory is that package's source directory.
func migrationsDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "api", "deployments", "migrations"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

// Exec runs a statement against the test database — for cleanup of rows the
// store has no method to delete, such as organisations.
func Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, DSN(t))
	if err != nil {
		t.Errorf("testdb exec connect: %v", err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Errorf("testdb exec %q: %v", sql, err)
	}
}

func redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "(unparseable url)"
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return strings.TrimSpace(u.String())
}
