package httpapi

// A failed upload must leave nothing behind. Before, a failure creating or
// linking the processing task answered 500 but kept the document row — status
// "uploaded", no task to ever process it — and its original on disk, where no
// sweep would look.
//
// The failures are forced with SQLite triggers, which is why these run on a
// private SQLite file rather than the shared PostgreSQL test database: a trigger
// there would break every other package's tests running in parallel.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

func TestFailedUploadLeavesNothingBehind(t *testing.T) {
	cases := []struct {
		name    string
		trigger string
	}{
		{
			name: "task creation fails",
			trigger: `CREATE TRIGGER fail_task BEFORE INSERT ON tasks
			          BEGIN SELECT RAISE(ABORT, 'injected: task insert'); END;`,
		},
		{
			name: "linking the task fails",
			trigger: `CREATE TRIGGER fail_link BEFORE UPDATE OF task_id ON documents
			          BEGIN SELECT RAISE(ABORT, 'injected: task link'); END;`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "taskflow.db")

			st, err := store.NewSQLite(ctx, dbPath)
			if err != nil {
				t.Fatalf("NewSQLite: %v", err)
			}
			t.Cleanup(st.Close)
			if _, err := st.CreateOrganization(ctx, auth.LocalOrgID, "local"); err != nil {
				t.Fatalf("create local org: %v", err)
			}

			raw, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			if _, err := raw.ExecContext(ctx, tc.trigger); err != nil {
				t.Fatalf("install trigger: %v", err)
			}

			objectsRoot := t.TempDir()
			fs, err := objects.NewFS(objectsRoot)
			if err != nil {
				t.Fatal(err)
			}
			// Auth off: the local principal, same as the desktop build.
			srv := NewServer(Config{Port: "0", Objects: fs, MaxUploadBytes: 1 << 20}, zap.NewNop(), st, nil)
			ts := httptest.NewServer(srv.httpServer.Handler)
			t.Cleanup(ts.Close)

			ct := "text/plain"
			body, formCT := uploadBody(t, "file", "cpf.txt", &ct, []byte("CPF 529.982.247-25\n"))
			resp := postUpload(t, ts.Client(), ts.URL, body, formCT)
			resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", resp.StatusCode)
			}

			docs, err := st.ListDocuments(ctx, store.ListDocumentsParams{Limit: 50, OrgID: auth.LocalOrgID})
			if err != nil {
				t.Fatal(err)
			}
			if len(docs) != 0 {
				t.Errorf("%d document row(s) survived a failed upload", len(docs))
			}
			tasks, err := st.ListTasks(ctx, store.ListTasksParams{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 0 {
				t.Errorf("%d task row(s) survived a failed upload", len(tasks))
			}
			if left := countObjects(t, objectsRoot); left != 0 {
				t.Errorf("%d stored object(s) survived a failed upload", left)
			}
		})
	}
}
