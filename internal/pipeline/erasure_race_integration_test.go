package pipeline

// C4 against a worker that is already mid-stage: the erasure request arrives
// after Handle has checked for a tombstone but before a stage writes its
// artifact. Without the fence in saveCheckpoint, that artifact outlives the
// erasure in a directory nothing enumerates any more.
//
// Runs on both backends, like the store conformance suite: the desktop build
// erases through SQLite, and the race is the same there.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/testdb"
	"go.uber.org/zap"
)

// hookedObjects runs a callback just before the first Put of a named object,
// which is exactly where an erasure has to land to race a stage.
type hookedObjects struct {
	objects.Store
	name  string
	hook  func()
	fired bool
}

func (h *hookedObjects) Put(ctx context.Context, key string, r io.Reader) (string, error) {
	if !h.fired && strings.HasSuffix(key, "/"+h.name) {
		h.fired = true
		h.hook()
	}
	return h.Store.Put(ctx, key, r)
}

func TestErasureDuringStageLeavesNoBytes_Integration(t *testing.T) {
	stores := []struct {
		name string
		open func(t *testing.T) *store.Store
	}{
		{"sqlite", func(t *testing.T) *store.Store {
			ctx := context.Background()
			st, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "taskflow.db"))
			if err != nil {
				t.Fatalf("NewSQLite: %v", err)
			}
			t.Cleanup(st.Close)
			if _, err := st.CreateOrganization(ctx, auth.LocalOrgID, "local"); err != nil {
				t.Fatalf("create local org: %v", err)
			}
			return st
		}},
		{"postgres", func(t *testing.T) *store.Store {
			st, err := store.New(context.Background(), testdb.DSN(t))
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			t.Cleanup(st.Close)
			return st
		}},
	}

	cases := []struct {
		name string
		// The stage write the erasure races.
		object string
		// fullErasure runs all four C4 steps; otherwise it stops after removing
		// the bytes, so the row is still there (tombstoned) when the stage writes.
		fullErasure bool
	}{
		{"row deleted before OCR text is written", "ocr.txt", true},
		{"tombstoned before findings are written", "findings.json", false},
		{"row deleted before report is written", "report.json", true},
	}

	for _, b := range stores {
		t.Run(b.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					ctx := context.Background()
					st := b.open(t)

					root := t.TempDir()
					fsStore, err := objects.NewFS(root)
					if err != nil {
						t.Fatal(err)
					}
					doc, task := createDoc(t, st, fsStore, piiContent)

					// The erasure handler's sequence, run from inside the stage.
					erase := func() {
						cur, err := st.GetDocument(ctx, doc.ID)
						if err != nil {
							t.Fatalf("erase: load: %v", err)
						}
						if _, err := st.UpdateDocumentStatus(ctx, doc.ID, cur.Version, store.DocErased, nil); err != nil {
							t.Fatalf("erase: tombstone: %v", err)
						}
						if err := fsStore.RemovePrefix(ctx, "documents/"+doc.ID.String()); err != nil {
							t.Fatalf("erase: bytes: %v", err)
						}
						if !tc.fullErasure {
							return
						}
						if _, err := st.DeleteTask(ctx, task.ID); err != nil {
							t.Fatalf("erase: task: %v", err)
						}
						if _, err := st.DeleteDocument(ctx, doc.ID); err != nil {
							t.Fatalf("erase: row: %v", err)
						}
					}

					hooked := &hookedObjects{Store: fsStore, name: tc.object, hook: erase}
					pl := New(st, hooked, zap.NewNop(), Config{OCRTimeout: 5 * time.Second})

					if err := pl.Handle(ctx, task); err != nil {
						t.Fatalf("a document erased mid-stage must ack cleanly, got %v", err)
					}
					if !hooked.fired {
						t.Fatalf("the pipeline never wrote %s; the race was not exercised", tc.object)
					}

					if left := filesUnder(t, filepath.Join(root, "documents", doc.ID.String())); len(left) > 0 {
						t.Fatalf("erasure outrun by the stage: %v still on disk", left)
					}

					if !tc.fullErasure {
						got, err := st.GetDocument(ctx, doc.ID)
						if err != nil {
							t.Fatal(err)
						}
						if got.Status != store.DocErased {
							t.Fatalf("status = %s; the tombstone must survive the stage", got.Status)
						}
					}
				})
			}
		})
	}
}

// filesUnder lists every regular file below dir; a missing dir is empty.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, filepath.Base(path))
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}
