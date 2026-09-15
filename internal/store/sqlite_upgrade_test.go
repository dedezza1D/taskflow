package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// A desktop database file outlives the build that created it. The consolidated
// schema only describes fresh files, so a column added later has to be brought
// in by upgradeSQLite — and its backfill has to leave no task without a tenant,
// or the scoped reads would hide that user's own history from them.
func TestSQLiteUpgradeAddsTaskTenant(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	org := uuid.New()
	linkedTask, strayTask, doc := uuid.New(), uuid.New(), uuid.New()

	// The tables as a build from before tasks.org_id left them.
	old, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (
		   id TEXT PRIMARY KEY, name TEXT NOT NULL,
		   created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')));`,
		`CREATE TABLE tasks (
		   id TEXT PRIMARY KEY, type TEXT NOT NULL, payload TEXT NOT NULL,
		   priority TEXT NOT NULL DEFAULT 'normal', status TEXT NOT NULL DEFAULT 'queued',
		   created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
		   updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
		   version INTEGER NOT NULL DEFAULT 1);`,
		`CREATE TABLE documents (
		   id TEXT PRIMARY KEY, filename TEXT NOT NULL, content_type TEXT NOT NULL,
		   storage_uri TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'uploaded',
		   failed_stage TEXT NULL, task_id TEXT NULL REFERENCES tasks(id) ON DELETE SET NULL,
		   org_id TEXT NOT NULL REFERENCES organizations(id),
		   created_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
		   updated_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
		   version INTEGER NOT NULL DEFAULT 1, raw_shredded_at TIMESTAMP);`,
	} {
		if _, err := old.ExecContext(ctx, q); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name) VALUES (?, 'local');`, []any{org}},
		{`INSERT INTO tasks (id, type, payload) VALUES (?, 'document.process', '{}');`, []any{linkedTask}},
		{`INSERT INTO tasks (id, type, payload) VALUES (?, 'demo', '{}');`, []any{strayTask}},
		{`INSERT INTO documents (id, filename, content_type, storage_uri, task_id, org_id)
		  VALUES (?, 'a.txt', 'text/plain', 'fs://a', ?, ?);`, []any{doc, linkedTask, org}},
	} {
		if _, err := old.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
	}
	_ = old.Close()

	st, err := NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("opening a legacy file must upgrade it, got %v", err)
	}

	for _, id := range []uuid.UUID{linkedTask, strayTask} {
		if _, err := st.GetTaskForOrg(ctx, id, org); err != nil {
			t.Fatalf("task %s was not backfilled into the file's organisation: %v", id, err)
		}
	}
	if _, err := st.GetTaskForOrg(ctx, linkedTask, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backfilled task leaked to another tenant: %v", err)
	}

	// New work on the upgraded file names its tenant like anywhere else.
	if _, err := st.CreateTask(ctx, CreateTaskParams{
		Type: "demo", Payload: []byte(`{}`), Priority: PriorityNormal, OrgID: org,
	}); err != nil {
		t.Fatalf("CreateTask on upgraded file: %v", err)
	}

	// And opening it again is a no-op, not a duplicate-column failure.
	st.Close()
	again, err := NewSQLite(ctx, path)
	if err != nil {
		t.Fatalf("reopening an upgraded file: %v", err)
	}
	again.Close()
}
