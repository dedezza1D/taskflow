package main

import (
	"context"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/pipeline"
	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

// runRawRetention is the safety net behind the pipeline's inline shred.
//
// A document that reaches the report is shredded immediately, the moment the
// raw bytes stop being useful — that is the primary mechanism and it needs no
// timer. This sweep exists for the documents that mechanism cannot reach: ones
// that DEAD-LETTERED (no completion event to shred against), and ones whose
// inline shred failed and left the row unmarked on purpose so it would be
// retried here.
//
// Safe in every replica: MarkRawShredded is a no-op on an already-shredded row,
// and object removal is idempotent, so concurrent sweeps converge instead of
// racing — including against a pipeline shredding the same document inline.
func runRawRetention(ctx context.Context, logger *zap.Logger, st *store.Store, pl *pipeline.Pipeline, cfg *config.Config) {
	ticker := time.NewTicker(cfg.WorkerReconcileInterval)
	defer ticker.Stop()

	logger.Info("raw retention sweep started",
		zap.Duration("interval", cfg.WorkerReconcileInterval),
		zap.Duration("raw_retention", cfg.WorkerRawRetention),
	)

	for {
		select {
		case <-ctx.Done():
			logger.Info("raw retention sweep stopped")
			return
		case <-ticker.C:
			if err := sweepRawOnce(ctx, logger, st, pl, cfg); err != nil {
				logger.Warn("raw retention sweep failed", zap.Error(err))
			}
		}
	}
}

func sweepRawOnce(ctx context.Context, logger *zap.Logger, st *store.Store, pl *pipeline.Pipeline, cfg *config.Config) error {
	cutoff := time.Now().Add(-cfg.WorkerRawRetention)

	docs, err := st.ListRawShreddablePending(ctx, cutoff, 100)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}

	for i := range docs {
		pl.ShredRaw(ctx, &docs[i])
	}

	logger.Info("raw retention sweep pass",
		zap.Int("documents", len(docs)),
		zap.Time("cutoff", cutoff),
	)
	return nil
}
