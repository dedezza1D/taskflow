package pipeline

// Data-minimisation tests: the raw material (original + OCR text) must not
// survive the pipeline, and destroying it must not disturb the checkpoint
// ledger or the at-least-once redelivery contract.
//
// Needs Postgres with the migrations applied, same convention as the other
// integration tests here.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
)

func objectExists(t *testing.T, obj *objects.FS, uri string) bool {
	t.Helper()
	rc, err := obj.Get(context.Background(), uri)
	if errors.Is(err, objects.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("get %s: %v", uri, err)
	}
	rc.Close()
	return true
}

func TestShredRawDestroysOriginalAndOCR_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)

	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The raw material is gone...
	if objectExists(t, obj, doc.StorageURI) {
		t.Error("original survived the pipeline — raw content still on disk")
	}
	ocrArt, err := st.GetArtifact(ctx, doc.ID, StageOCR, KindText)
	if err != nil {
		t.Fatalf("ocr artifact row: %v", err)
	}
	if objectExists(t, obj, ocrArt.StorageURI) {
		t.Error("ocr.txt survived the pipeline — the searchable rendering is the worse leak")
	}

	// ...but the PII-free outputs remain, because they are the product.
	findingsArt, err := st.GetArtifact(ctx, doc.ID, StagePII, KindFindings)
	if err != nil {
		t.Fatal(err)
	}
	if !objectExists(t, obj, findingsArt.StorageURI) {
		t.Error("findings.json was destroyed; only raw material should be")
	}
	repArt, err := st.GetArtifact(ctx, doc.ID, StageReport, KindReport)
	if err != nil {
		t.Fatal(err)
	}
	if !objectExists(t, obj, repArt.StorageURI) {
		t.Error("report.json was destroyed; only raw material should be")
	}

	got, err := st.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RawShreddedAt == nil {
		t.Error("raw_shredded_at not stamped — nothing proves minimisation happened")
	}
	if got.Status != store.DocCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
}

// The stage ledger is what failedStageFromLedger and the UI timeline read.
// Shredding removes objects, never the rows.
func TestShredRawPreservesStageLedger_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)
	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	arts, err := st.ListArtifacts(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 3 {
		t.Fatalf("artifact rows = %d, want 3 — shredding must not erase the ledger", len(arts))
	}
	if stage := pl.failedStageFromLedger(ctx, doc.ID); stage != StageReport {
		t.Errorf("failedStageFromLedger = %q after a full run, want %q", stage, StageReport)
	}
}

// The safety property that dictated shredding at completion rather than per
// stage: a redelivery of an already-shredded document must ack, not try to
// recompute OCR against an original that no longer exists.
func TestRedeliveryAfterShredIsNoOp_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)
	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	task2, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pl.Handle(ctx, task2); err != nil {
		t.Fatalf("redelivery after shred must be a no-op, got: %v", err)
	}

	got, err := st.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.DocCompleted {
		t.Errorf("status = %s after redelivery, want completed", got.Status)
	}
}

func TestShredRawIsIdempotent_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)
	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	before, err := st.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Handle already shredded once; do it twice more.
	pl.ShredRaw(ctx, before)
	pl.ShredRaw(ctx, before)

	after, err := st.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RawShreddedAt == nil {
		t.Fatal("raw_shredded_at cleared by a repeat shred")
	}
	if !after.RawShreddedAt.Equal(*before.RawShreddedAt) {
		t.Errorf("repeat shred moved the audit timestamp: %v -> %v",
			before.RawShreddedAt, after.RawShreddedAt)
	}
}

// A document that never completes has no shred hook, so it is the sweep's job.
func TestListRawShreddablePending_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	// A failed document, aged past the cutoff by the status write.
	failedDoc, _ := createDoc(t, st, obj, piiContent)
	stage := StageOCR
	if _, err := st.UpdateDocumentStatus(ctx, failedDoc.ID, failedDoc.Version, store.DocFailed, &stage); err != nil {
		t.Fatal(err)
	}

	// A completed one, shredded inline — must never appear in the sweep.
	doneDoc, doneTask := createDoc(t, st, obj, piiContent)
	if err := pl.Handle(ctx, doneTask); err != nil {
		t.Fatal(err)
	}

	pending, err := st.ListRawShreddablePending(ctx, time.Now().Add(time.Minute), 500)
	if err != nil {
		t.Fatal(err)
	}

	ids := make(map[uuid.UUID]bool, len(pending))
	for _, d := range pending {
		ids[d.ID] = true
	}
	if !ids[failedDoc.ID] {
		t.Error("failed document missing from the sweep — its original would live forever")
	}
	if ids[doneDoc.ID] {
		t.Error("completed document queued for sweep despite being shredded inline")
	}

	// A completed document that still holds raw bytes must be picked up too:
	// either its inline shred failed (the pipeline leaves raw_shredded_at NULL
	// on purpose so this retry exists) or the row predates the feature. Without
	// this branch, that design would silently strand bytes forever.
	strandedDoc, _ := createDoc(t, st, obj, piiContent)
	if _, err := st.UpdateDocumentStatus(ctx, strandedDoc.ID, strandedDoc.Version, store.DocCompleted, nil); err != nil {
		t.Fatal(err)
	}
	withStranded, err := st.ListRawShreddablePending(ctx, time.Now().Add(time.Minute), 500)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range withStranded {
		if d.ID == strandedDoc.ID {
			found = true
		}
	}
	if !found {
		t.Error("completed document with a failed inline shred is never retried — bytes stranded forever")
	}

	// Sweeping it destroys the original and takes it out of the queue.
	pl.ShredRaw(ctx, failedDoc)
	if objectExists(t, obj, failedDoc.StorageURI) {
		t.Error("swept document kept its original")
	}
	again, err := st.ListRawShreddablePending(ctx, time.Now().Add(time.Minute), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range again {
		if d.ID == failedDoc.ID {
			t.Fatal("document still pending after being shredded — the sweep would loop on it forever")
		}
	}
}

// The TTL is what keeps the sweep from racing a document that is still retrying.
func TestRawSweepRespectsTTL_Integration(t *testing.T) {
	st, obj, _ := setup(t)
	ctx := context.Background()

	doc, _ := createDoc(t, st, obj, piiContent)
	stage := StageOCR
	if _, err := st.UpdateDocumentStatus(ctx, doc.ID, doc.Version, store.DocFailed, &stage); err != nil {
		t.Fatal(err)
	}

	// Cutoff in the past: nothing has been terminal long enough yet.
	pending, err := st.ListRawShreddablePending(ctx, time.Now().Add(-time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range pending {
		if d.ID == doc.ID {
			t.Fatal("document swept before its TTL elapsed")
		}
	}
}

// Guards the claim the whole feature rests on: what survives holds no values.
func TestSurvivingArtifactsHoldNoRawValues_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)
	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	arts, err := st.ListArtifacts(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, art := range arts {
		rc, err := obj.Get(ctx, art.StorageURI)
		if errors.Is(err, objects.ErrNotFound) {
			continue // shredded — that is the point
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}

		for _, leaked := range []string{"john.doe@example.com", "4111", "DE89"} {
			if strings.Contains(string(data), leaked) {
				t.Errorf("surviving %s artifact leaks raw value %q", art.Stage, leaked)
			}
		}
	}
}
