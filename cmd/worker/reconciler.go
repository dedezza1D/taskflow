package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	workerpkg "github.com/dedezza1D/taskflow/internal/worker"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
)

type reconcileAction int

const (
	reconcileRepublish reconcileAction = iota
	reconcileDeadLetter
)

// decideReconcile is the two-case split for a stale task, keyed off the durable
// execution ledger:
//
//   - priorAttempts >= maxAttempts -> the task has used its whole budget (a
//     crash-pill, or attempts that all failed): dead-letter it, do NOT republish
//     (republishing would just churn).
//   - otherwise -> a lost enqueue or a worker that died mid-flight: republish so
//     it runs again as attempt priorAttempts+1 (the ledger guarantees no
//     collision with the unique (task_id, attempt) index).
func decideReconcile(priorAttempts, maxAttempts int) reconcileAction {
	if priorAttempts >= maxAttempts {
		return reconcileDeadLetter
	}
	return reconcileRepublish
}

// runReconciler runs reconcileOnce on a ticker until ctx is cancelled. It is safe
// to run in every worker replica: each rescue is claimed with an optimistic-locked
// status write before any publish, so concurrent sweeps single-flight down to one
// republish/DLQ per task rather than amplifying with replica count.
func runReconciler(ctx context.Context, logger *zap.Logger, st *store.Store, q queue.Broker, cfg *config.Config) {
	ticker := time.NewTicker(cfg.WorkerReconcileInterval)
	defer ticker.Stop()

	logger.Info("reconciler started",
		zap.Duration("interval", cfg.WorkerReconcileInterval),
		zap.Duration("staleness", cfg.WorkerReconcileStaleness),
	)

	for {
		select {
		case <-ctx.Done():
			logger.Info("reconciler stopped")
			return
		case <-ticker.C:
			if err := reconcileOnce(ctx, logger, st, q, cfg); err != nil {
				logger.Warn("reconcile pass failed", zap.Error(err))
			}
		}
	}
}

// reconcileOnce sweeps one batch of stale tasks and applies the two-case split.
func reconcileOnce(ctx context.Context, logger *zap.Logger, st *store.Store, q queue.Broker, cfg *config.Config) error {
	cutoff := time.Now().Add(-cfg.WorkerReconcileStaleness)

	stale, err := st.ListStaleTasks(ctx, cutoff, 100)
	if err != nil {
		return err
	}

	for i := range stale {
		t := &stale[i]

		prior, err := st.MaxAttempt(ctx, t.ID)
		if err != nil {
			logger.Warn("reconcile: max attempt lookup failed", zap.String("task_id", t.ID.String()), zap.Error(err))
			continue
		}

		switch decideReconcile(prior, cfg.WorkerMaxAttempts) {
		case reconcileDeadLetter:
			reconcileDeadLetterTask(ctx, logger, st, q, t, prior)
		case reconcileRepublish:
			reconcileRepublishTask(ctx, logger, st, q, t, prior)
		}
	}
	return nil
}

// reconcileRepublishTask rescues a stuck task by re-publishing it. The
// optimistic-locked write to 'queued' before publishing does double duty: it
// single-flights across replicas (only the version-guard winner publishes) and
// bumps updated_at so the task is debounced for a full staleness window instead of
// being re-selected — and re-published — on every sweep. It also genuinely resets a
// 'processing' task (whose message is gone) so the worker won't ack it as a dup.
func reconcileRepublishTask(ctx context.Context, logger *zap.Logger, st *store.Store, q queue.Broker, t *store.Task, prior int) {
	if _, err := st.UpdateTaskStatus(ctx, t.ID, t.Version, store.StatusQueued); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return // another reconciler or worker owns it
		}
		logger.Warn("reconcile: requeue failed", zap.String("task_id", t.ID.String()), zap.Error(err))
		return
	}

	hdr := queue.NewHeader()
	otel.GetTextMapPropagator().Inject(ctx, hdr)

	subject := queue.SubjectForPriority(string(t.Priority))
	if err := q.PublishTask(ctx, subject, queue.TaskMessage{
		TaskID:   t.ID.String(),
		Priority: string(t.Priority),
	}, hdr); err != nil {
		logger.Error("reconcile: republish failed", zap.String("task_id", t.ID.String()), zap.Error(err))
		return
	}

	logger.Info("reconcile: republished stale task",
		zap.String("task_id", t.ID.String()),
		zap.String("from_status", string(t.Status)),
		zap.Int("prior_attempts", prior),
	)
}

// reconcileDeadLetterTask terminally fails a stale task that has exhausted its
// attempt budget, publishing a DLQ entry (references only — no document content)
// and marking the task failed.
func reconcileDeadLetterTask(ctx context.Context, logger *zap.Logger, st *store.Store, q queue.Broker, t *store.Task, prior int) {
	reason := fmt.Errorf("reconciler: stale %s task exceeded max attempts (%d)", t.Status, prior)

	// Claim the terminal transition first (optimistic lock) so only one replica
	// flips the status and publishes the DLQ entry. The terminal status is the
	// guarantee; the DLQ is best-effort inspection.
	if _, err := st.UpdateTaskStatus(ctx, t.ID, t.Version, store.StatusFailed); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return // another reconciler or worker owns it
		}
		logger.Warn("reconcile: mark failed failed", zap.String("task_id", t.ID.String()), zap.Error(err))
		return
	}

	dlq := queue.DLQMessage{
		TaskID:       t.ID.String(),
		TaskType:     t.Type,
		Attempt:      prior,
		Error:        workerpkg.ScrubError(reason),
		OriginalSubj: queue.SubjectForPriority(string(t.Priority)),
		OriginalData: nil, // reconciler has no original message; payload lives in the DB
		FailedAt:     time.Now(),
	}

	hdr := queue.NewHeader()
	otel.GetTextMapPropagator().Inject(ctx, hdr)
	hdr.Set("task_id", t.ID.String())

	if err := q.PublishDLQ(ctx, dlq, hdr); err != nil {
		logger.Error("reconcile: DLQ publish failed", zap.String("task_id", t.ID.String()), zap.Error(err))
	}

	// Dead-letter → document status wiring (same hook the worker's terminal
	// paths use): a crash-pill document ends visibly failed at its stage.
	notifyDeadLetter(ctx, t, reason)

	logger.Error("reconcile: dead-lettered stale task",
		zap.String("task_id", t.ID.String()),
		zap.String("from_status", string(t.Status)),
		zap.Int("prior_attempts", prior),
	)
}
