package maintenance

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// fakeJanitor records what the sweep handed it.
type fakeJanitor struct {
	mu       sync.Mutex
	shredded []uuid.UUID
	discards []uuid.UUID
	seen     chan struct{}
}

func (f *fakeJanitor) ShredRaw(_ context.Context, doc *store.Document) {
	f.mu.Lock()
	f.shredded = append(f.shredded, doc.ID)
	f.mu.Unlock()
	f.signal()
}

func (f *fakeJanitor) DiscardOrphanedUpload(_ context.Context, doc *store.Document) error {
	f.mu.Lock()
	f.discards = append(f.discards, doc.ID)
	f.mu.Unlock()
	f.signal()
	return nil
}

func (f *fakeJanitor) signal() {
	select {
	case f.seen <- struct{}{}:
	default:
	}
}

// The desktop build is opened to scan a document and closed again, often in
// less than one tick. A sweep that only ran on the ticker would never run at
// all there, which is how dead-lettered documents kept their original forever.
func TestRawRetentionSweepsAtStartup(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "maintenance.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(st.Close)

	org := uuid.New()
	if _, err := st.CreateOrganization(ctx, org, "local"); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) *store.Document {
		d, err := st.CreateDocument(ctx, store.CreateDocumentParams{
			Filename: name, ContentType: "text/plain", StorageURI: "fs://" + name, OrgID: org,
		})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	deadLettered := mk("failed.txt")
	stage := "ocr"
	if _, err := st.UpdateDocumentStatus(ctx, deadLettered.ID, deadLettered.Version, store.DocFailed, &stage); err != nil {
		t.Fatal(err)
	}
	orphan := mk("never-linked.txt")

	janitor := &fakeJanitor{seen: make(chan struct{}, 1)}
	cfg := &config.Config{
		// An hour between ticks: anything the sweep does here it did at startup.
		WorkerReconcileInterval: time.Hour,
		// Negative, so the cutoff lands a minute ahead and rows written a moment
		// ago already count as old. Retention itself is covered by the store
		// conformance tests; what is under test here is WHEN the sweep runs.
		WorkerRawRetention: -time.Minute,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go RunRawRetention(runCtx, zap.NewNop(), st, janitor, cfg)

	deadline := time.After(3 * time.Second)
	for {
		janitor.mu.Lock()
		done := len(janitor.shredded) > 0 && len(janitor.discards) > 0
		janitor.mu.Unlock()
		if done {
			break
		}
		select {
		case <-janitor.seen:
		case <-deadline:
			janitor.mu.Lock()
			t.Fatalf("startup sweep did not run: shredded=%v discarded=%v", janitor.shredded, janitor.discards)
		}
	}

	janitor.mu.Lock()
	defer janitor.mu.Unlock()
	if len(janitor.shredded) != 1 || janitor.shredded[0] != deadLettered.ID {
		t.Errorf("shredded %v, want just the dead-lettered document", janitor.shredded)
	}
	if len(janitor.discards) != 1 || janitor.discards[0] != orphan.ID {
		t.Errorf("discarded %v, want just the orphaned upload", janitor.discards)
	}
}
