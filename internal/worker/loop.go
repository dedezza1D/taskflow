package worker

// The execution loop, extracted from the worker binary so the desktop build can
// run the same code in-process.
//
// Nothing here knows which broker it is talking to: fetch, settle, and the
// lease heartbeat all come through the queue.Broker seam. That is what lets one
// implementation serve both a JetStream deployment and a single-process
// application with no server at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/observability"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"
)

type msgAction int

const (
	actionAck msgAction = iota
	actionRetry
)

// Loop runs tasks off a broker until its context is cancelled.
type Loop struct {
	Logger   *zap.Logger
	Store    *store.Store
	Broker   queue.Broker
	Registry *Registry
	Config   *config.Config

	// OnDeadLetter, when set, is invoked at every point a task terminally fails
	// (permanent error, exhausted attempts, reconciler reap) so domain handlers
	// can project the outcome onto their own entities — the document pipeline
	// uses it to wire dead-letter → documents.status = failed(+stage).
	OnDeadLetter func(ctx context.Context, t *store.Task, reason error)
}

func (l *Loop) notifyDeadLetter(ctx context.Context, t *store.Task, reason error) {
	if l.OnDeadLetter != nil && t != nil {
		l.OnDeadLetter(ctx, t, reason)
	}
}

// Run blocks until ctx is cancelled, then waits for in-flight handlers to
// finish so a shutdown never abandons work mid-attempt.
func (l *Loop) Run(ctx context.Context) {
	wg := &sync.WaitGroup{}
	sem := make(chan struct{}, l.Config.WorkerConcurrency)

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			l.Logger.Info("worker loop stopped")
			return
		default:
		}

		msgs, err := l.Broker.Fetch(ctx, 1, l.Config.WorkerPollTimeout)
		if err != nil {
			l.Logger.Warn("fetch error", zap.Error(err))
			continue
		}

		for _, m := range msgs {
			sem <- struct{}{}
			wg.Add(1)

			go func(m queue.Message) {
				defer wg.Done()
				defer func() { <-sem }()

				action, attempt, err := l.handleMsg(ctx, m)
				if err != nil {
					l.Logger.Error("handle message failed", zap.Error(err))
					_ = m.Nak()
					return
				}

				switch action {
				case actionAck:
					_ = m.Ack()
				case actionRetry:
					delay := computeBackoff(l.Config.WorkerBackoffBase, l.Config.WorkerBackoffMax, attempt)
					time.Sleep(delay)
					_ = m.Nak()
				default:
					_ = m.Ack()
				}
			}(m)
		}
	}
}

func (l *Loop) handleMsg(ctx context.Context, m queue.Message) (msgAction, int, error) {
	logger, st, q, registry, cfg := l.Logger, l.Store, l.Broker, l.Registry, l.Config
	// Extract trace context from NATS headers (if present)
	if m.Header() != nil {
		ctx = otel.GetTextMapPropagator().Extract(ctx, m.Header())
	}
	tr := otel.Tracer("taskflow/worker")
	ctx, span := tr.Start(ctx, "taskflow.handle_msg")
	defer span.End()

	// Default for early returns (bad message / bad id) before we know the task.
	// The real attempt number is derived from the execution ledger below.
	attempt := 1

	var tm queue.TaskMessage
	if err := json.Unmarshal(m.Data(), &tm); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "bad_message")
		return actionAck, attempt, err
	}

	taskID, err := uuid.Parse(tm.TaskID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "bad_task_id")
		return actionAck, attempt, err
	}

	// Attempt number from the execution ledger (MAX(attempt)+1): durable and
	// independent of the broker delivery count, so it stays correct across
	// same-message redelivery and reconciler republish (where NumDelivered resets).
	priorAttempts, err := st.MaxAttempt(ctx, taskID)
	if err != nil {
		return actionRetry, attempt, err
	}
	attempt = priorAttempts + 1

	span.SetAttributes(
		attribute.String("messaging.subject", m.Subject()),
		attribute.String("task.id", taskID.String()),
		attribute.String("task.priority", tm.Priority),
		attribute.Int("task.attempt", attempt),
	)

	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return actionAck, attempt, nil
		}
		return actionRetry, attempt, err
	}

	// Terminal tasks should not be retried
	if task.Status == store.StatusCompleted || task.Status == store.StatusFailed || task.Status == store.StatusCancelled {
		return actionAck, attempt, nil
	}

	// If already processing, likely duplicate delivery
	if task.Status == store.StatusProcessing {
		return actionAck, attempt, nil
	}

	// Policy: attempts exceeded -> permanent fail + DLQ
	if attempt > cfg.WorkerMaxAttempts {
		return l.failPermanently(ctx, logger, st, q, task, attempt, fmt.Errorf("max attempts exceeded (%d)", cfg.WorkerMaxAttempts), m)
	}

	// Claim first (prevents duplicate side-effects)
	processing, err := tryUpdateStatus(ctx, st, taskID, store.StatusProcessing)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return actionAck, attempt, nil
		}
		return actionRetry, attempt, err
	}

	h, ok := registry.Get(task.Type)
	if !ok {
		return l.failPermanentlyClaimed(ctx, logger, st, q, task, processing.Version, attempt,
			fmt.Errorf("no handler registered for type %q", task.Type), m)
	}

	// Record execution start. Attempt number is assigned by the ledger (MAX+1);
	// the unique (task_id, attempt) index still guards concurrent double-deliveries.
	exec, err := st.CreateExecution(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			logger.Info("execution already exists; acking",
				zap.String("task_id", taskID.String()),
				zap.Int("attempt", attempt),
				zap.String("type", task.Type),
			)
			return actionAck, attempt, nil
		}

		_ = safeSetQueued(ctx, st, taskID)
		return actionRetry, attempt, err
	}
	attempt = exec.Attempt // authoritative attempt number assigned by the ledger

	// Run handler under a lease: heartbeats keep the ack deadline alive while the
	// stage runs; the ceiling bounds a single attempt so a hung handler releases
	// the message instead of holding it for the full AckWait.
	observability.TasksStartedTotal.WithLabelValues(task.Type, tm.Priority).Inc()
	start := time.Now()
	runErr := RunWithLease(ctx,
		func(c context.Context) error { return h(c, task) },
		func() error { return m.InProgress() },
		cfg.WorkerHeartbeatInterval,
		cfg.WorkerMaxProcessing,
	)
	observability.TaskDuration.WithLabelValues(task.Type).Observe(time.Since(start).Seconds())

	if runErr == nil {
		if err := finishExecutionSucceeded(ctx, st, exec.ID); err != nil {
			_ = safeSetQueued(ctx, st, taskID)
			return actionRetry, attempt, err
		}

		_, err = st.UpdateTaskStatus(ctx, taskID, processing.Version, store.StatusCompleted)
		if err != nil && !errors.Is(err, store.ErrVersionConflict) {
			return actionRetry, attempt, err
		}

		observability.TasksCompletedTotal.WithLabelValues(task.Type).Inc()

		logger.Info("task processed",
			zap.String("task_id", taskID.String()),
			zap.Int("attempt", attempt),
			zap.String("type", task.Type),
		)
		return actionAck, attempt, nil
	}

	// C2/C3: scrub before the error enters the span (exported to Tempo).
	scrubbed := ScrubError(runErr)
	span.RecordError(errors.New(scrubbed))
	span.SetStatus(codes.Error, scrubbed)

	// Failure: record failed execution
	reason := "retryable"
	if IsPermanent(runErr) {
		reason = "permanent"
	}
	observability.TasksFailedTotal.WithLabelValues(task.Type, reason).Inc()

	if err := finishExecutionFailed(ctx, st, exec.ID, runErr); err != nil {
		_ = safeSetQueued(ctx, st, taskID)
		return actionRetry, attempt, err
	}

	// Permanent failure -> DLQ + mark failed
	if IsPermanent(runErr) {
		l.publishDLQBestEffort(ctx, logger, q, task, attempt, runErr, m)
		_, _ = st.UpdateTaskStatus(ctx, taskID, processing.Version, store.StatusFailed)
		l.notifyDeadLetter(ctx, task, runErr)

		logger.Error("task permanently failed",
			zap.String("task_id", taskID.String()),
			zap.Int("attempt", attempt),
			zap.String("type", task.Type),
			zap.String("error", ScrubError(runErr)),
		)
		return actionAck, attempt, nil
	}

	// Transient failure: if max attempts reached -> DLQ + mark failed
	if attempt >= cfg.WorkerMaxAttempts {
		l.publishDLQBestEffort(ctx, logger, q, task, attempt, runErr, m)
		_, _ = st.UpdateTaskStatus(ctx, taskID, processing.Version, store.StatusFailed)
		l.notifyDeadLetter(ctx, task, runErr)

		logger.Error("task failed (max attempts reached)",
			zap.String("task_id", taskID.String()),
			zap.Int("attempt", attempt),
			zap.String("type", task.Type),
			zap.String("error", ScrubError(runErr)),
		)
		return actionAck, attempt, nil
	}

	// Re-queue and retry later
	_ = safeSetQueued(ctx, st, taskID)

	logger.Warn("task failed, will retry",
		zap.String("task_id", taskID.String()),
		zap.Int("attempt", attempt),
		zap.String("type", task.Type),
		zap.String("error", ScrubError(runErr)),
	)

	return actionRetry, attempt, nil
}

func (l *Loop) publishDLQBestEffort(ctx context.Context, logger *zap.Logger, q queue.Broker, task *store.Task, attempt int, reason error, m queue.Message) {
	if q == nil || task == nil || reason == nil || m == nil {
		return
	}

	dlq := queue.DLQMessage{
		TaskID:   task.ID.String(),
		TaskType: task.Type,
		Attempt:  attempt,
		// C2: scrub before the error enters the DLQ stream (MaxAge 7d) and the
		// dlq-reader's logs, so document content / PII never lands there.
		Error:        ScrubError(reason),
		OriginalSubj: m.Subject(),
		OriginalData: m.Data(),
		FailedAt:     time.Now(),
	}

	hdr := queue.NewHeader()
	otel.GetTextMapPropagator().Inject(ctx, hdr)
	hdr.Set("task_id", task.ID.String())

	if err := q.PublishDLQ(ctx, dlq, hdr); err != nil {
		logger.Error("failed to publish DLQ message", zap.Error(err), zap.String("task_id", task.ID.String()))
	}
}

func computeBackoff(base, max time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

func (l *Loop) failPermanentlyClaimed(
	ctx context.Context,
	logger *zap.Logger,
	st *store.Store,
	q queue.Broker,
	task *store.Task,
	processingVersion int,
	attempt int,
	reason error,
	m queue.Message,
) (msgAction, int, error) {
	exec, err := st.CreateExecution(ctx, task.ID)
	if err == nil {
		_ = finishExecutionFailed(ctx, st, exec.ID, reason)
	} else if errors.Is(err, store.ErrAlreadyExists) {
		// already recorded
	}

	l.publishDLQBestEffort(ctx, logger, q, task, attempt, reason, m)
	_, _ = st.UpdateTaskStatus(ctx, task.ID, processingVersion, store.StatusFailed)
	l.notifyDeadLetter(ctx, task, reason)

	logger.Error("task permanently failed",
		zap.String("task_id", task.ID.String()),
		zap.Int("attempt", attempt),
		zap.String("type", task.Type),
		zap.String("error", ScrubError(reason)),
	)

	return actionAck, attempt, nil
}

func (l *Loop) failPermanently(
	ctx context.Context,
	logger *zap.Logger,
	st *store.Store,
	q queue.Broker,
	task *store.Task,
	attempt int,
	reason error,
	m queue.Message,
) (msgAction, int, error) {
	exec, err := st.CreateExecution(ctx, task.ID)
	if err == nil {
		_ = finishExecutionFailed(ctx, st, exec.ID, reason)
	} else if errors.Is(err, store.ErrAlreadyExists) {
	}

	l.publishDLQBestEffort(ctx, logger, q, task, attempt, reason, m)
	_ = safeSetStatus(ctx, st, task.ID, store.StatusFailed)
	l.notifyDeadLetter(ctx, task, reason)

	logger.Error("task permanently failed",
		zap.String("task_id", task.ID.String()),
		zap.Int("attempt", attempt),
		zap.String("type", task.Type),
		zap.String("error", ScrubError(reason)),
	)

	return actionAck, attempt, nil
}

func safeSetQueued(ctx context.Context, st *store.Store, id uuid.UUID) error {
	return safeSetStatus(ctx, st, id, store.StatusQueued)
}

func safeSetStatus(ctx context.Context, st *store.Store, id uuid.UUID, status store.TaskStatus) error {
	t, err := st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	_, err = st.UpdateTaskStatus(ctx, id, t.Version, status)
	if errors.Is(err, store.ErrVersionConflict) {
		return nil
	}
	return err
}

func tryUpdateStatus(ctx context.Context, st *store.Store, id uuid.UUID, status store.TaskStatus) (*store.Task, error) {
	for i := 0; i < 3; i++ {
		t, err := st.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		updated, err := st.UpdateTaskStatus(ctx, id, t.Version, status)
		if err == nil {
			return updated, nil
		}
		if errors.Is(err, store.ErrVersionConflict) {
			continue
		}
		return nil, err
	}
	return nil, store.ErrVersionConflict
}

func finishExecutionSucceeded(ctx context.Context, st *store.Store, execID uuid.UUID) error {
	_, err := st.FinishExecution(ctx, execID, store.ExecSucceeded, nil)
	return err
}

func finishExecutionFailed(ctx context.Context, st *store.Store, execID uuid.UUID, cause error) error {
	// C2: scrub before persisting so document content / PII never lands in the audit table.
	msg := ScrubError(cause)
	_, err := st.FinishExecution(ctx, execID, store.ExecFailed, &msg)
	return err
}
