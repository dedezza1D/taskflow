// Package maintenance holds the background sweeps that keep a deployment
// honest: rescuing tasks whose message was lost, and destroying raw material
// the pipeline no longer needs.
//
// They used to live in cmd/worker, which meant the desktop build — the same
// engine in one process - ran none of them. A document that dead-lettered
// there kept its original forever, and a task left 'processing' by a crash was
// never rescued, even though the local queue's design says the reconciler is
// what rescues it.
package maintenance

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

// DeadLetterFunc is called when a task terminally fails, so a domain can
// project the outcome onto its own entities — the document pipeline uses it to
// mark the document failed at its stage.
type DeadLetterFunc func(ctx context.Context, t *store.Task, reason error)

// Reconciler rescues tasks the happy path lost track of. Two different failures,
// swept separately because they need different clocks:
//
//   - A PUBLISH that never happened. Creating a task writes a row and then
//     publishes a message, two systems with a crash window between them. Such a
//     task is queued with enqueued_at NULL, and nothing can be holding it -
//     there is no message to hold. It is republished within seconds.
//   - A WORKER that died mid-flight, leaving the task 'processing' with its
//     message gone. This one must wait out the processing ceiling, or the sweep
//     would rescue work that is still legitimately running.
//
// Folding them into one sweep is what used to make a lost publish wait ten
// minutes for a constraint that belongs to the other case.
//
// Safe in every replica: each rescue is claimed with an optimistic-locked status
// write before any publish, so concurrent sweeps single-flight down to one
// republish/DLQ per task instead of amplifying with replica count.
type Reconciler struct {
	Logger       *zap.Logger
	Store        *store.Store
	Broker       queue.Broker
	Config       *config.Config
	OnDeadLetter DeadLetterFunc

	// RecoverUnpublished turns on the publish sweep. The desktop build leaves it
	// off: there the tasks table IS the queue, publishing is a wake-up rather
	// than a delivery, and every task would look unpublished forever.
	RecoverUnpublished bool
}

// Run sweeps on a ticker until ctx is cancelled, starting with one pass
// immediately: a process that restarts more often than the interval would
// otherwise never rescue anything, which is every desktop run.
func (rc *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(rc.Config.WorkerReconcileInterval)
	defer ticker.Stop()

	rc.Logger.Info("reconciler started",
		zap.Duration("interval", rc.Config.WorkerReconcileInterval),
		zap.Duration("staleness", rc.Config.WorkerReconcileStaleness),
		zap.Bool("publish_recovery", rc.RecoverUnpublished),
	)

	rc.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			rc.Logger.Info("reconciler stopped")
			return
		case <-ticker.C:
			rc.sweep(ctx)
		}
	}
}

func (rc *Reconciler) sweep(ctx context.Context) {
	if rc.RecoverUnpublished {
		if err := rc.recoverUnpublishedOnce(ctx); err != nil {
			rc.Logger.Warn("publish recovery pass failed", zap.Error(err))
		}
	}
	if err := reconcileOnce(ctx, rc.Logger, rc.Store, rc.Broker, rc.Config, rc.OnDeadLetter); err != nil {
		rc.Logger.Warn("reconcile pass failed", zap.Error(err))
	}
}

// recoverUnpublishedOnce republishes tasks that were committed but never
// confirmed on the queue.
//
// The cutoff only has to clear the gap between INSERT and publish in a healthy
// request, so it is seconds rather than minutes. Republishing one that was in
// fact delivered is harmless: the worker claims optimistically and acks a task
// it finds already terminal or in flight.
func (rc *Reconciler) recoverUnpublishedOnce(ctx context.Context) error {
	cutoff := time.Now().Add(-rc.Config.WorkerPublishRecoveryDelay)

	pending, err := rc.Store.ListUnpublishedTasks(ctx, cutoff, 100)
	if err != nil {
		return err
	}
	for i := range pending {
		t := &pending[i]
		prior, err := rc.Store.MaxAttempt(ctx, t.ID)
		if err != nil {
			rc.Logger.Warn("publish recovery: max attempt lookup failed",
				zap.String("task_id", t.ID.String()), zap.Error(err))
			continue
		}
		// A task that never reached the queue cannot have exhausted its
		// attempts, but the ledger decides either way — a crash-pill that was
		// also never confirmed must dead-letter, not churn.
		switch decideReconcile(prior, rc.Config.WorkerMaxAttempts) {
		case reconcileDeadLetter:
			reconcileDeadLetterTask(ctx, rc.Logger, rc.Store, rc.Broker, t, prior, rc.OnDeadLetter)
		case reconcileRepublish:
			reconcileRepublishTask(ctx, rc.Logger, rc.Store, rc.Broker, t, prior)
		}
	}
	return nil
}

// reconcileOnce sweeps one batch of stale tasks and applies the two-case split.
func reconcileOnce(ctx context.Context, logger *zap.Logger, st *store.Store, q queue.Broker, cfg *config.Config, onDeadLetter DeadLetterFunc) error {
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
			reconcileDeadLetterTask(ctx, logger, st, q, t, prior, onDeadLetter)
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

	// The message is on the queue again, so the publish marker is true again —
	// and the publish sweep stops considering this task.
	if err := st.MarkTaskEnqueued(ctx, t.ID); err != nil {
		logger.Warn("reconcile: enqueue marker failed", zap.String("task_id", t.ID.String()), zap.Error(err))
	}

	logger.Info("reconcile: republished task",
		zap.String("task_id", t.ID.String()),
		zap.String("from_status", string(t.Status)),
		zap.Int("prior_attempts", prior),
	)
}

// reconcileDeadLetterTask terminally fails a stale task that has exhausted its
// attempt budget, publishing a DLQ entry (references only — no document content)
// and marking the task failed.
func reconcileDeadLetterTask(ctx context.Context, logger *zap.Logger, st *store.Store, q queue.Broker, t *store.Task, prior int, onDeadLetter DeadLetterFunc) {
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
	if onDeadLetter != nil {
		onDeadLetter(ctx, t, reason)
	}

	logger.Error("reconcile: dead-lettered stale task",
		zap.String("task_id", t.ID.String()),
		zap.String("from_status", string(t.Status)),
		zap.Int("prior_attempts", prior),
	)
}
