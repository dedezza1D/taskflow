package maintenance

import (
	"context"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

// DocumentJanitor is the half of the pipeline this sweep needs. An interface
// rather than *pipeline.Pipeline so the sweep stays a scheduling concern and
// can be tested without an object store.
type DocumentJanitor interface {
	// ShredRaw destroys a document's raw material (original + OCR text).
	ShredRaw(ctx context.Context, doc *store.Document)
	// DiscardOrphanedUpload removes an upload that never got a task: its
	// objects, then its row.
	DiscardOrphanedUpload(ctx context.Context, doc *store.Document) error
}

// RunRawRetention is the safety net behind the pipeline's inline shred.
//
// A document that reaches the report is shredded immediately, the moment the
// raw bytes stop being useful — that is the primary mechanism and it needs no
// timer. This sweep exists for the documents that mechanism cannot reach: ones
// that DEAD-LETTERED (no completion event to shred against), ones whose inline
// shred failed and left the row unmarked on purpose so it would be retried
// here, and uploads that never got a task at all.
//
// It sweeps once at startup before the first tick, because a process that runs
// for less than the interval — the desktop build, opened to scan a document
// and closed again — would otherwise never sweep at all.
//
// Safe in every replica: MarkRawShredded is a no-op on an already-shredded row,
// and object removal is idempotent, so concurrent sweeps converge instead of
// racing — including against a pipeline shredding the same document inline.
func RunRawRetention(ctx context.Context, logger *zap.Logger, st *store.Store, docs DocumentJanitor, cfg *config.Config) {
	ticker := time.NewTicker(cfg.WorkerReconcileInterval)
	defer ticker.Stop()

	logger.Info("raw retention sweep started",
		zap.Duration("interval", cfg.WorkerReconcileInterval),
		zap.Duration("raw_retention", cfg.WorkerRawRetention),
	)

	if err := sweepRawOnce(ctx, logger, st, docs, cfg); err != nil {
		logger.Warn("raw retention sweep failed", zap.Error(err))
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("raw retention sweep stopped")
			return
		case <-ticker.C:
			if err := sweepRawOnce(ctx, logger, st, docs, cfg); err != nil {
				logger.Warn("raw retention sweep failed", zap.Error(err))
			}
		}
	}
}

func sweepRawOnce(ctx context.Context, logger *zap.Logger, st *store.Store, docs DocumentJanitor, cfg *config.Config) error {
	cutoff := time.Now().Add(-cfg.WorkerRawRetention)

	pending, err := st.ListRawShreddablePending(ctx, cutoff, 100)
	if err != nil {
		return err
	}
	for i := range pending {
		docs.ShredRaw(ctx, &pending[i])
	}

	// Uploads that never got a task: no pipeline run will ever reach them, so
	// the same retention window is all the raw material gets.
	orphans, err := st.ListOrphanedUploads(ctx, cutoff, 100)
	if err != nil {
		return err
	}
	discarded := 0
	for i := range orphans {
		if err := docs.DiscardOrphanedUpload(ctx, &orphans[i]); err != nil {
			logger.Warn("orphaned upload not discarded; will retry", zap.Error(err))
			continue
		}
		discarded++
	}

	if len(pending) > 0 || len(orphans) > 0 {
		logger.Info("raw retention sweep pass",
			zap.Int("documents", len(pending)),
			zap.Int("orphaned_uploads", discarded),
			zap.Time("cutoff", cutoff),
		)
	}
	return nil
}
