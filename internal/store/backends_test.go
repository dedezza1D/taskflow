package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/testdb"
	"github.com/google/uuid"
)

// Every store method is written once, in PostgreSQL idiom, and runs unchanged
// on both backends. That only holds if both are tested, and for a long time
// they were not: 25 methods had SQLite coverage and 11 had PostgreSQL coverage,
// which is how `($1 IS NULL OR status = $1)` shipped. PostgreSQL cannot infer a
// parameter's type from `IS NULL` alone and rejected the statement outright,
// while SQLite — which infers nothing, and so cannot fail to — passed happily.
// The document list and the task list were 500s on every served deployment with
// the suite green.
//
// The reverse has happened too: expired sessions once authenticated on SQLite
// only, because timestamps were compared as text and the longer string won.
//
// So the rule here is that a case runs on both unless there is a reason it
// cannot. Write new store tests with eachBackend, not against one driver.

type backend struct {
	name string
	open func(t *testing.T) *Store
}

// postgresDSN is the dedicated test database (see internal/testdb), never the
// development one a running worker is attached to.
func postgresDSN(t *testing.T) string {
	return testdb.DSN(t)
}

// backends returns the drivers to run a conformance case against. Both of them,
// always.
//
// An earlier version skipped PostgreSQL when no server answered. That is the
// failure mode this file exists to prevent, wearing a friendlier face: a skip
// reports "ok" for a package that verified half of what it claims, and nobody
// reads a skip. If the database is not there, these tests have not run, and
// saying so is the only honest outcome.
func backends(t *testing.T) []backend {
	t.Helper()

	list := []backend{{
		name: "sqlite",
		open: func(t *testing.T) *Store {
			t.Helper()
			st, err := NewSQLite(context.Background(), filepath.Join(t.TempDir(), "taskflow.db"))
			if err != nil {
				t.Fatalf("NewSQLite: %v", err)
			}
			t.Cleanup(st.Close)
			return st
		},
	}}

	// The probe asks whether the schema is there, not merely whether something
	// answers. Those are different questions, and the difference cost an hour:
	// a stale container from an older revision of this project grabbed port
	// 5432 before the compose one could, accepted the connection, and served a
	// database with no organizations table. Every case then failed with 42P01
	// instead of saying the one useful thing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var reason error
	probe, err := New(ctx, postgresDSN(t))
	if err != nil {
		reason = fmt.Errorf("cannot connect: %w", err)
	} else {
		var table *string
		if err := probe.db.QueryRow(ctx, `SELECT to_regclass('public.organizations')::text;`).Scan(&table); err != nil {
			reason = fmt.Errorf("cannot query: %w", err)
		} else if table == nil {
			reason = errors.New("connected, but this database has no taskflow schema -- " +
				"something else is probably listening on that port")
		}
		probe.Close()
	}
	if reason != nil {
		// Instruction first: the pgx error below is several lines of dial
		// failures, and whatever comes after them does not get read.
		t.Fatalf("PostgreSQL is required by this suite and is not usable.\n"+
			"  start it with: docker compose up -d postgres\n"+
			"  dsn: %s\n"+
			"  cause: %v", postgresDSN(t), reason)
	}

	return append(list, backend{
		name: "postgres",
		open: func(t *testing.T) *Store {
			t.Helper()
			st, err := New(context.Background(), postgresDSN(t))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(st.Close)
			// PostgreSQL is a shared, long-lived database rather than a fresh
			// file per test. Without this, every run leaves rows behind — which
			// is not hypothetical: an afternoon was spent chasing a UI that was
			// faithfully reporting 36 orphaned documents from earlier runs.
			cleanupPostgres(t, st)
			return st
		},
	})
}

// eachBackend runs one case against every available driver.
func eachBackend(t *testing.T, fn func(t *testing.T, st *Store)) {
	t.Helper()
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Registered before the store opens, so it runs last: t.Cleanup is
			// LIFO, and the postgres cleanup below still needs this registry.
			t.Cleanup(func() { forgetOrgs(t) })
			fn(t, b.open(t))
		})
	}
}

// cleanupPostgres removes what the test wrote, once it finishes.
//
// Organisation-owned rows go by organisation: users, sessions and reset tokens
// cascade from it, and documents have to be deleted first because their foreign
// key does not cascade.
//
// Tasks cascade from their organisation too, but the type marker below is kept
// as a second net: every task this suite creates uses conformanceTaskType, and
// cleanup deletes exactly that.
//
// Two cheaper ideas were wrong, both for the same reason -- they deleted rows
// by asking "did this appear recently?" rather than "did I write this?".
// Matching on created_at breaks because one case deliberately backdates rows to
// test chronological ordering. Diffing the task table before and after breaks
// worse: `go test ./...` runs package binaries in parallel, so rows that appear
// during this test may belong to internal/pipeline, and deleting them made two
// of its integration tests fail with "not found" — a test suite corrupting
// another package's run, which is a considerably worse bug than the leak it was
// cleaning up.
func cleanupPostgres(t *testing.T, st *Store) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		for _, org := range createdOrgs(t) {
			if _, err := st.db.Exec(ctx, `DELETE FROM documents WHERE org_id = $1;`, org); err != nil {
				t.Logf("cleanup documents for %s: %v", org, err)
			}
			if _, err := st.db.Exec(ctx, `DELETE FROM organizations WHERE id = $1;`, org); err != nil {
				t.Logf("cleanup organisation %s: %v", org, err)
			}
		}

		if _, err := st.db.Exec(ctx, `DELETE FROM tasks WHERE type = $1;`, conformanceTaskType); err != nil {
			t.Logf("cleanup tasks: %v", err)
		}
	})
}

// conformanceTaskType marks the tasks this suite creates, so cleanup can remove
// them without guessing. Unique per test binary, so two runs cannot delete each
// other's rows either.
var conformanceTaskType = "conformance-" + uuid.NewString()

// orgRegistry records the organisations a test created, so cleanup knows what
// to remove. Keyed by test, because subtests get their own store. The mutex is
// not needed today — no case calls t.Parallel — and is here so that adding one
// later does not introduce a data race nobody was looking for.
var (
	orgMu       sync.Mutex
	orgRegistry = map[*testing.T][]uuid.UUID{}
)

func createdOrgs(t *testing.T) []uuid.UUID {
	orgMu.Lock()
	defer orgMu.Unlock()
	return append([]uuid.UUID(nil), orgRegistry[t]...)
}

func forgetOrgs(t *testing.T) {
	orgMu.Lock()
	defer orgMu.Unlock()
	delete(orgRegistry, t)
}

// seedOrg gives the tenant foreign key something to point at, and registers the
// organisation for cleanup on the backends that need it.
func seedOrg(t *testing.T, st *Store) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := st.CreateOrganization(context.Background(), id, "test"); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	orgMu.Lock()
	orgRegistry[t] = append(orgRegistry[t], id)
	orgMu.Unlock()
	return id
}
