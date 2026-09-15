package pipeline

// Integration test (needs Postgres with migrations applied, same convention as
// the store tests). Uses text/plain originals so it runs without tesseract —
// the OCR stage's passthrough path — while still exercising every checkpoint,
// the idempotent re-run, resume-from-checkpoint, and the erasure tombstone.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/pii"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/testdb"
	"github.com/dedezza1D/taskflow/internal/worker"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func setup(t *testing.T) (*store.Store, *objects.FS, *Pipeline) {
	t.Helper()
	st, err := store.New(context.Background(), testdb.DSN(t))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	obj, err := objects.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	pl := New(st, obj, zap.NewNop(), Config{OCRTimeout: 5 * time.Second})
	return st, obj, pl
}

func createDoc(t *testing.T, st *store.Store, obj *objects.FS, content string) (*store.Document, *store.Task) {
	t.Helper()
	ctx := context.Background()

	docID := uuid.New()
	uri, err := obj.Put(ctx, "documents/"+docID.String()+"/original", strings.NewReader(content))
	if err != nil {
		t.Fatalf("put original: %v", err)
	}
	doc, err := st.CreateDocumentWithID(ctx, docID, store.CreateDocumentParams{
		Filename:    "test.txt",
		ContentType: "text/plain",
		StorageURI:  uri,
		OrgID:       auth.LocalOrgID,
	})
	if err != nil {
		t.Fatalf("create document: %v", err)
	}

	payload, _ := json.Marshal(Payload{DocumentID: docID.String(), StorageURI: uri, ContentType: "text/plain"})
	task, err := st.CreateTask(ctx, store.CreateTaskParams{Type: TaskType, Payload: payload, Priority: store.PriorityNormal, OrgID: auth.LocalOrgID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := st.SetDocumentTask(ctx, docID, task.ID); err != nil {
		t.Fatalf("link task: %v", err)
	}
	return doc, task
}

const piiContent = "customer: john.doe@example.com\n" +
	"card: 4111 1111 1111 1111\n" +
	"iban: DE89 3704 0044 0532 0130 00\n"

func TestPipelineEndToEnd_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)

	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, err := st.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.DocCompleted {
		t.Fatalf("document status = %s, want completed", got.Status)
	}

	arts, err := st.ListArtifacts(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 3 {
		t.Fatalf("artifacts = %d, want 3 (ocr, pii, report)", len(arts))
	}

	// C-guarantee on the pipeline's OWN artifacts: findings and report carry
	// category+location, never the raw values.
	findingsArt, err := st.GetArtifact(ctx, doc.ID, StagePII, KindFindings)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := obj.Get(ctx, findingsArt.StorageURI)
	if err != nil {
		t.Fatal(err)
	}
	findingsJSON, _ := io.ReadAll(rc)
	rc.Close()

	var fa FindingsArtifact
	if err := json.Unmarshal(findingsJSON, &fa); err != nil {
		t.Fatalf("findings artifact not JSON: %v", err)
	}
	if fa.Counts[pii.CategoryEmail] == 0 || fa.Counts[pii.CategoryCreditCard] == 0 || fa.Counts[pii.CategoryIBAN] == 0 {
		t.Fatalf("expected email+card+iban findings, got %v", fa.Counts)
	}
	for _, leaked := range []string{"john.doe@example.com", "4111", "DE89"} {
		if strings.Contains(string(findingsJSON), leaked) {
			t.Fatalf("findings artifact leaks raw value %q", leaked)
		}
	}

	repArt, err := st.GetArtifact(ctx, doc.ID, StageReport, KindReport)
	if err != nil {
		t.Fatal(err)
	}
	rc, err = obj.Get(ctx, repArt.StorageURI)
	if err != nil {
		t.Fatal(err)
	}
	reportJSON, _ := io.ReadAll(rc)
	rc.Close()
	if strings.Contains(string(reportJSON), "4111") {
		t.Fatal("report artifact leaks raw card digits")
	}

	// Idempotent redelivery: run the whole handler again — same terminal state,
	// same artifact set, no error.
	task2, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pl.Handle(ctx, task2); err != nil {
		t.Fatalf("second Handle must be a no-op, got %v", err)
	}
	arts2, _ := st.ListArtifacts(ctx, doc.ID)
	if len(arts2) != 3 {
		t.Fatalf("re-run changed artifact count: %d", len(arts2))
	}
}

func TestPipelineResumesFromCheckpoint_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	// Simulate "worker crashed after OCR": the OCR checkpoint exists with known
	// content that DIFFERS from the original. The retry must resume from the
	// checkpoint (findings reflect the checkpoint text), not redo OCR.
	doc, task := createDoc(t, st, obj, "original says alice@original.example only\n")

	checkpointText := "checkpoint says bob@checkpoint.example and card 4111 1111 1111 1111\n"
	uri, err := obj.Put(ctx, "documents/"+doc.ID.String()+"/ocr.txt", strings.NewReader(checkpointText))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArtifact(ctx, doc.ID, StageOCR, KindText, uri); err != nil {
		t.Fatal(err)
	}

	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	art, err := st.GetArtifact(ctx, doc.ID, StagePII, KindFindings)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := obj.Get(ctx, art.StorageURI)
	data, _ := io.ReadAll(rc)
	rc.Close()

	var fa FindingsArtifact
	_ = json.Unmarshal(data, &fa)
	if fa.Counts[pii.CategoryCreditCard] == 0 {
		t.Fatalf("PII stage should have consumed the OCR checkpoint (card present there): %v", fa.Counts)
	}
}

func TestPipelineTombstoneAndErasure_Integration(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)

	// Tombstone (step 1 of C4): an in-flight delivery must ack without touching
	// bytes or producing artifacts.
	if _, err := st.UpdateDocumentStatus(ctx, doc.ID, doc.Version, store.DocErased, nil); err != nil {
		t.Fatal(err)
	}
	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("tombstoned document must ack cleanly, got %v", err)
	}
	if arts, _ := st.ListArtifacts(ctx, doc.ID); len(arts) != 0 {
		t.Fatalf("tombstoned document must produce no artifacts, got %d", len(arts))
	}

	// Steps 2–4: bytes, task+executions, row. Then prove full erasure.
	if err := obj.RemovePrefix(ctx, "documents/"+doc.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetDocument(ctx, doc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("document row must be gone, got %v", err)
	}
	if _, err := st.GetTask(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("task row must be gone, got %v", err)
	}
	if _, err := obj.Get(ctx, doc.StorageURI); !errors.Is(err, objects.ErrNotFound) {
		t.Fatalf("original bytes must be gone, got %v", err)
	}

	// A straggler redelivery after full erasure also acks (row not found).
	if err := pl.Handle(ctx, task); err != nil {
		t.Fatalf("post-erasure redelivery must ack, got %v", err)
	}
}

func TestPipelineBadPayloadIsPermanent(t *testing.T) {
	st, _, pl := setup(t)
	ctx := context.Background()

	task, err := st.CreateTask(ctx, store.CreateTaskParams{
		Type:     TaskType,
		Payload:  []byte(`{"document_id":"not-a-uuid"}`),
		Priority: store.PriorityNormal,
		OrgID:    auth.LocalOrgID,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pl.Handle(ctx, task)
	if err == nil || !worker.IsPermanent(err) {
		t.Fatalf("bad payload must be permanent (dead-letter, not crash-loop), got %v", err)
	}
}

func TestMarkDeadLetteredDerivesStageFromLedger(t *testing.T) {
	st, obj, pl := setup(t)
	ctx := context.Background()

	doc, task := createDoc(t, st, obj, piiContent)

	// OCR checkpoint exists, PII doesn't → failed_stage must be "pii".
	uri, _ := obj.Put(ctx, "documents/"+doc.ID.String()+"/ocr.txt", strings.NewReader("text"))
	if _, err := st.CreateArtifact(ctx, doc.ID, StageOCR, KindText, uri); err != nil {
		t.Fatal(err)
	}

	pl.MarkDeadLettered(ctx, task, errors.New("boom"))

	got, err := st.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.DocFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if got.FailedStage == nil || *got.FailedStage != StagePII {
		t.Fatalf("failed_stage = %v, want pii", got.FailedStage)
	}
}
