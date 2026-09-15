// Package pipeline is the compliance document pipeline: one document.process
// task type run as a checkpointed monolith — OCR → PII → report inline, each
// stage writing its artifact atomically and skipped on retry if that artifact
// already exists.
//
// Contract (the repo's one-sentence version): every document reaches a terminal
// outcome — report generated, or dead-lettered at stage X — with at-least-once
// idempotent execution, bounded ledger-derived retries, a full per-stage audit,
// and no document content or PII in the queue, DLQ, logs, traces, or audit
// error fields.
//
// A stage graduates into its own task/consumer only when it earns an
// independent lifecycle (slot-occupancy or a real priority/scaling need); OCR
// is the first candidate, and the checkpoint layout here (per-stage artifacts
// keyed by document) is what makes that split cheap later.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/worker"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TaskType is the single pipeline task type.
const TaskType = "document.process"

// Stage names in execution order — also the ledger the dead-letter wiring uses
// to derive "failed at stage X" (first stage without a checkpoint).
const (
	StageOCR    = "ocr"
	StagePII    = "pii"
	StageReport = "report"
)

// Artifact kinds per stage.
const (
	KindText     = "text"
	KindFindings = "findings"
	KindReport   = "report_json"
)

// Payload is the task payload: a REFERENCE, never bytes (C1). Document content
// lives in object storage; only this triple travels through the queue and DLQ.
type Payload struct {
	DocumentID  string `json:"document_id"`
	StorageURI  string `json:"storage_uri"`
	ContentType string `json:"content_type"`
}

// Config bounds the pipeline's slow stage.
type Config struct {
	// OCRTimeout is OCR's own sub-ceiling, distinct from (and required to be
	// smaller than) the whole-task lease ceiling: a hung tesseract fails THIS
	// stage cooperatively instead of burning the whole attempt budget's clock.
	OCRTimeout time.Duration
	// OCRLanguages is passed to tesseract -l (e.g. "eng" or "deu+eng").
	OCRLanguages string
	// TesseractBin overrides the tesseract binary path (tests); empty = "tesseract".
	TesseractBin string
}

type Pipeline struct {
	st      *store.Store
	objects objects.Store
	logger  *zap.Logger
	cfg     Config
}

func New(st *store.Store, obj objects.Store, logger *zap.Logger, cfg Config) *Pipeline {
	if cfg.OCRLanguages == "" {
		cfg.OCRLanguages = "eng"
	}
	if cfg.TesseractBin == "" {
		cfg.TesseractBin = "tesseract"
	}
	return &Pipeline{st: st, objects: obj, logger: logger, cfg: cfg}
}

// Register wires the handler into the worker registry.
func (p *Pipeline) Register(r *worker.Registry) {
	r.Register(TaskType, p.Handle)
}

// Handle is the document.process handler. It is safe under at-least-once
// delivery: every stage is checkpointed (skip if artifact exists), every write
// is idempotent, and terminal/erased documents ack without side effects.
func (p *Pipeline) Handle(ctx context.Context, task *store.Task) error {
	var pl Payload
	if err := json.Unmarshal(task.Payload, &pl); err != nil {
		return worker.Permanent(fmt.Errorf("document.process: bad payload: %w", err))
	}
	docID, err := uuid.Parse(pl.DocumentID)
	if err != nil {
		return worker.Permanent(fmt.Errorf("document.process: bad document_id: %w", err))
	}

	doc, err := p.st.GetDocument(ctx, docID)
	if errors.Is(err, store.ErrNotFound) {
		// Erased after enqueue (C4): the row is gone, so this message is a
		// no-op. Ack only once nothing is left under the prefix — an earlier
		// attempt may have raced the erasure and written behind it.
		p.logger.Info("document not found (erased); acking", zap.String("document_id", pl.DocumentID))
		return p.purgeErased(ctx, docID)
	}
	if err != nil {
		return fmt.Errorf("load document: %w", err)
	}

	switch doc.Status {
	case store.DocErased:
		// Tombstoned mid-erasure: never write its bytes again, and make sure
		// none survive.
		return p.purgeErased(ctx, docID)
	case store.DocCompleted:
		return nil // duplicate delivery after completion
	}

	// Visibility, not correctness: mark the document processing (best-effort,
	// optimistic). The checkpoints are what make retries safe.
	p.setStatus(ctx, docID, store.DocProcessing, nil)

	// Stage 1 — OCR (checkpointed; resumes here after a mid-OCR crash).
	text, err := p.runOCR(ctx, doc)
	if err != nil {
		return stageOutcome(StageOCR, err)
	}

	// Stage 2 — PII detection (findings carry category+location, never values).
	findings, err := p.runPII(ctx, doc, text)
	if err != nil {
		return stageOutcome(StagePII, err)
	}

	// Stage 3 — compliance report.
	if err := p.runReport(ctx, doc, findings); err != nil {
		return stageOutcome(StageReport, err)
	}

	p.setStatus(ctx, docID, store.DocCompleted, nil)

	// Data minimisation: the raw material has served its purpose, so destroy it
	// now rather than on a timer. Deliberately AFTER the completed transition —
	// see ShredRaw for why this cannot be done stage-by-stage.
	p.ShredRaw(ctx, doc)

	p.logger.Info("document pipeline completed",
		zap.String("document_id", docID.String()),
		zap.Int("findings", len(findings)),
	)
	return nil
}

// ShredRaw destroys a document's raw material — the uploaded original and the
// OCR text — leaving the findings and report, which by construction hold
// category and location but never values. Idempotent: safe to call twice, and
// safe on a document that has already been shredded or erased.
//
// Why this runs at pipeline COMPLETION and not after each stage: loadCheckpoint
// treats a missing artifact object as "recompute this stage". Deleting ocr.txt
// as soon as findings.json lands would mean a redelivery arriving before the
// report committed would try to recompute OCR — against an original that is
// also gone — and dead-letter a document that was one stage from done. Shredding
// only at the terminal state is safe because Handle short-circuits on
// DocCompleted and never reaches the checkpoint logic again.
//
// The artifact ROWS survive; only the objects are removed. The rows are the
// stage ledger that failedStageFromLedger and the UI timeline read, and they
// carry no content.
func (p *Pipeline) ShredRaw(ctx context.Context, doc *store.Document) {
	uris := []string{doc.StorageURI}

	// The OCR text's location comes from its artifact row rather than a
	// reconstructed key: the URI scheme belongs to the storage backend.
	if art, err := p.st.GetArtifact(ctx, doc.ID, StageOCR, KindText); err == nil {
		uris = append(uris, art.StorageURI)
	} else if !errors.Is(err, store.ErrNotFound) {
		p.logger.Warn("shred: ocr artifact lookup failed",
			zap.String("document_id", doc.ID.String()), zap.Error(err))
		return
	}

	for _, uri := range uris {
		if err := p.objects.Remove(ctx, uri); err != nil && !errors.Is(err, objects.ErrNotFound) {
			// Leave raw_shredded_at NULL so the retention sweep retries. Marking
			// it now would strand bytes that nothing would ever revisit.
			p.logger.Error("shred: object removal failed",
				zap.String("document_id", doc.ID.String()), zap.Error(err))
			return
		}
	}

	if err := p.st.MarkRawShredded(ctx, doc.ID); err != nil {
		p.logger.Warn("shred: mark failed",
			zap.String("document_id", doc.ID.String()), zap.Error(err))
		return
	}

	p.logger.Info("raw material shredded",
		zap.String("document_id", doc.ID.String()),
		zap.Int("objects", len(uris)),
	)
}

// stageErr tags an error with its stage for logs/audit (values are scrubbed at
// the chokepoint; the stage name itself is never sensitive). Wrapping with %w
// preserves the chain, so worker.IsPermanent still sees a PermanentError inside.
func stageErr(stage string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("stage %s: %w", stage, err)
}

// stageOutcome is stageErr, except that an erasure detected by the fence is a
// clean ack rather than a failure: the bytes are already gone, and retrying
// would only record a failed execution for a document that no longer exists.
func stageOutcome(stage string, err error) error {
	if errors.Is(err, errDocumentErased) {
		return nil
	}
	return stageErr(stage, err)
}

// errDocumentErased reports that the document was erased while a stage was
// computing, and that whatever the stage wrote has been purged.
var errDocumentErased = errors.New("document erased during processing")

// purgeErased removes every object under an erased document's prefix. The
// erasure handler already did this once; running it again is what catches a
// write that landed after it (see saveCheckpoint). Idempotent. A failure is
// returned, not swallowed, so the message is retried rather than acked while
// bytes remain.
func (p *Pipeline) purgeErased(ctx context.Context, docID uuid.UUID) error {
	if err := p.objects.RemovePrefix(ctx, "documents/"+docID.String()); err != nil {
		p.logger.Error("erasure fence: purge failed",
			zap.String("document_id", docID.String()), zap.Error(err))
		return fmt.Errorf("purge erased document: %w", err)
	}
	return nil
}

// ---- checkpoint plumbing -----------------------------------------------

// loadCheckpoint returns the artifact bytes if the stage already completed.
// The invariant "artifact row exists ⇒ object is complete" holds because the
// object is written atomically BEFORE the row. If the object is missing anyway
// (out-of-band deletion), we recompute rather than fail: Put/CreateArtifact
// are both idempotent, so recomputing converges.
func (p *Pipeline) loadCheckpoint(ctx context.Context, docID uuid.UUID, stage, kind string) ([]byte, bool, error) {
	art, err := p.st.GetArtifact(ctx, docID, stage, kind)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rc, err := p.objects.Get(ctx, art.StorageURI)
	if errors.Is(err, objects.ErrNotFound) {
		p.logger.Warn("checkpoint object missing; recomputing stage",
			zap.String("document_id", docID.String()), zap.String("stage", stage))
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// saveCheckpoint writes the stage artifact atomically, then records the row.
// Order matters: object first (atomic visibility), row second, so the row's
// existence is the completion signal.
//
// It is also the ERASURE FENCE. Handle checks for a tombstone only when it
// starts, and a stage can run for minutes (the OCR ceiling) after that. If C4
// erases the document in that window, its RemovePrefix runs before this Put,
// and the artifact — for OCR, the document's full text — would land in a
// directory no row points to any more, where neither erasure nor the retention
// sweep (both of which enumerate rows) would ever find it.
//
// So after writing, the document is re-read. The two orders cannot both miss:
// the erasure tombstones BEFORE it removes bytes, and this reads AFTER it
// writes them. Either the read sees the tombstone (or the missing row) and
// purges here, or the tombstone came later — and then so did the erasure's
// RemovePrefix, which takes this object with it.
func (p *Pipeline) saveCheckpoint(ctx context.Context, docID uuid.UUID, stage, kind, key string, data []byte) error {
	uri, err := p.objects.Put(ctx, key, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("write %s artifact: %w", stage, err)
	}
	// Not returned yet: once the row is gone this fails on its foreign key, and
	// the fence below has to run first to tell that apart from a real failure.
	_, recordErr := p.st.CreateArtifact(ctx, docID, stage, kind, uri)

	doc, err := p.st.GetDocument(ctx, docID)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && doc.Status == store.DocErased):
		if err := p.purgeErased(ctx, docID); err != nil {
			return err
		}
		p.logger.Info("erasure fence: document erased mid-stage; artifact purged",
			zap.String("document_id", docID.String()), zap.String("stage", stage))
		return errDocumentErased
	case err != nil:
		// Cannot tell whether an erasure happened. Keep nothing we cannot account
		// for: drop the object and retry. A row left pointing at it is harmless —
		// loadCheckpoint recomputes a stage whose object is missing.
		if rmErr := p.objects.Remove(ctx, uri); rmErr != nil {
			p.logger.Error("erasure fence: could not drop unverified artifact",
				zap.String("document_id", docID.String()), zap.String("stage", stage), zap.Error(rmErr))
		}
		return fmt.Errorf("verify document after %s write: %w", stage, err)
	}

	if recordErr != nil {
		return fmt.Errorf("record %s artifact: %w", stage, recordErr)
	}
	return nil
}

func objectKey(docID uuid.UUID, name string) string {
	return "documents/" + docID.String() + "/" + name
}

// setStatus is a best-effort optimistic transition (retry a few times, then
// give up quietly): document status is a projection for readers; the task
// ledger remains the source of truth for the engine.
func (p *Pipeline) setStatus(ctx context.Context, docID uuid.UUID, status store.DocumentStatus, failedStage *string) {
	for i := 0; i < 3; i++ {
		doc, err := p.st.GetDocument(ctx, docID)
		if err != nil {
			return
		}
		if doc.Status == store.DocErased {
			return // never overwrite the erasure tombstone
		}
		_, err = p.st.UpdateDocumentStatus(ctx, docID, doc.Version, status, failedStage)
		if err == nil || !errors.Is(err, store.ErrVersionConflict) {
			return
		}
	}
}

// ---- dead-letter → document status wiring --------------------------------

// MarkDeadLettered is called by the engine (worker DLQ points and the
// reconciler's reap path) when a task terminally fails. For document tasks it
// projects that outcome onto the document: status=failed plus failed_stage,
// derived from the checkpoint ledger (first stage without an artifact) — the
// same durable source the attempt counter uses, so it is correct for both
// "handler returned permanent error" and "crash-pill reaped with no error at
// hand".
func (p *Pipeline) MarkDeadLettered(ctx context.Context, task *store.Task, reason error) {
	if task == nil || task.Type != TaskType {
		return
	}
	var pl Payload
	if err := json.Unmarshal(task.Payload, &pl); err != nil {
		return
	}
	docID, err := uuid.Parse(pl.DocumentID)
	if err != nil {
		return
	}

	stage := p.failedStageFromLedger(ctx, docID)
	p.setStatus(ctx, docID, store.DocFailed, &stage)

	p.logger.Error("document dead-lettered",
		zap.String("document_id", docID.String()),
		zap.String("failed_stage", stage),
		zap.String("error", worker.ScrubError(reason)),
	)
}

func (p *Pipeline) failedStageFromLedger(ctx context.Context, docID uuid.UUID) string {
	type sk struct{ stage, kind string }
	for _, s := range []sk{{StageOCR, KindText}, {StagePII, KindFindings}, {StageReport, KindReport}} {
		if _, err := p.st.GetArtifact(ctx, docID, s.stage, s.kind); errors.Is(err, store.ErrNotFound) {
			return s.stage
		}
	}
	return StageReport
}
