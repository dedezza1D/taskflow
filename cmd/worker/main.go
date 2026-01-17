// cmd/worker/main.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	workerpkg "github.com/dedezza1D/taskflow/internal/worker"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

type msgAction int

const (
	actionAck msgAction = iota
	actionRetry
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		panic(err)
	}

	logger, err := logging.New(logging.Config{Level: cfg.LogLevel})
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	st, err := store.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Fatal("db connection failed", zap.Error(err))
	}
	defer st.Close()

	q, err := queue.New(context.Background(), queue.Config{
		NATSURL:      cfg.NATSURL,
		StreamName:   cfg.NATSStreamName,
		ConsumerName: cfg.NATSConsumerName,
		AckWait:      30 * time.Second,
		MaxDeliver:   5,
	})
	if err != nil {
		logger.Fatal("nats connection failed", zap.Error(err))
	}
	defer q.Close()

	sub, err := ensurePullSub(q, cfg, logger)
	if err != nil {
		logger.Fatal("create pull consumer failed", zap.Error(err))
	}

	registry := workerpkg.DefaultHandlers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	wg := &sync.WaitGroup{}
	sem := make(chan struct{}, cfg.WorkerConcurrency)

	logger.Info("worker started",
		zap.Int("concurrency", cfg.WorkerConcurrency),
		zap.Duration("poll_timeout", cfg.WorkerPollTimeout),
		zap.Int("max_attempts", cfg.WorkerMaxAttempts),
		zap.Duration("backoff_base", cfg.WorkerBackoffBase),
		zap.Duration("backoff_max", cfg.WorkerBackoffMax),
	)

	go func() {
		<-stop
		logger.Info("shutdown signal received")
		cancel()
	}()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			logger.Info("worker stopped")
			return
		default:
		}

		msgs, err := sub.Fetch(1, nats.MaxWait(cfg.WorkerPollTimeout))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			logger.Warn("fetch error", zap.Error(err))
			continue
		}

		for _, m := range msgs {
			sem <- struct{}{}
			wg.Add(1)

			go func(m *nats.Msg) {
				defer wg.Done()
				defer func() { <-sem }()

				action, attempt, err := handleMsg(ctx, logger, st, q, registry, cfg, m)
				if err != nil {
					logger.Error("handle message failed", zap.Error(err))
					_ = m.Nak()
					return
				}

				switch action {
				case actionAck:
					_ = m.Ack()
				case actionRetry:
					delay := computeBackoff(cfg.WorkerBackoffBase, cfg.WorkerBackoffMax, attempt)
					time.Sleep(delay)
					_ = m.Nak()
				default:
					_ = m.Ack()
				}
			}(m)
		}
	}
}

func ensurePullSub(q *queue.Queue, cfg *config.Config, logger *zap.Logger) (*nats.Subscription, error) {
	js := q.JetStream()

	sub, err := js.PullSubscribe("tasks.*", cfg.NATSConsumerName,
		nats.BindStream(cfg.NATSStreamName),
		nats.ManualAck(),
		nats.AckExplicit(),
	)
	if err != nil {
		return nil, err
	}

	logger.Info("pull subscription ready",
		zap.String("stream", cfg.NATSStreamName),
		zap.String("consumer", cfg.NATSConsumerName),
	)
	return sub, nil
}

func handleMsg(
	ctx context.Context,
	logger *zap.Logger,
	st *store.Store,
	q *queue.Queue,
	registry *workerpkg.Registry,
	cfg *config.Config,
	m *nats.Msg,
) (msgAction, int, error) {
	// Attempt number must come from JetStream delivery count (not DB state)
	attempt := 1
	if md, err := m.Metadata(); err == nil && md != nil && md.NumDelivered > 0 {
		attempt = int(md.NumDelivered)
	}

	var tm queue.TaskMessage
	if err := json.Unmarshal(m.Data, &tm); err != nil {
		// bad message → ack so it doesn't poison the stream
		return actionAck, attempt, err
	}

	taskID, err := uuid.Parse(tm.TaskID)
	if err != nil {
		return actionAck, attempt, err
	}

	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return actionAck, attempt, nil
		}
		return actionRetry, attempt, err
	}

	// terminal tasks should not be retried
	if task.Status == store.StatusCompleted || task.Status == store.StatusFailed || task.Status == store.StatusCancelled {
		return actionAck, attempt, nil
	}

	// if already processing, this is very likely a duplicate delivery; ack it
	if task.Status == store.StatusProcessing {
		return actionAck, attempt, nil
	}

	// Guard: if redelivery attempt already exceeded policy, fail permanently + DLQ
	if attempt > cfg.WorkerMaxAttempts {
		return failPermanently(ctx, logger, st, q, task, attempt, fmt.Errorf("max attempts exceeded (%d)", cfg.WorkerMaxAttempts), m)
	}

	// --- CLAIM FIRST (prevents duplicate side-effects) ---
	processing, err := tryUpdateStatus(ctx, st, taskID, store.StatusProcessing)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			// another worker likely took it
			return actionAck, attempt, nil
		}
		return actionRetry, attempt, err
	}

	// After claiming, re-check handler availability
	h, ok := registry.Get(task.Type)
	if !ok {
		// Mark failed + DLQ with the claim in place (prevents double-fail)
		return failPermanentlyClaimed(ctx, logger, st, q, task, processing.Version, attempt,
			fmt.Errorf("no handler registered for type %q", task.Type), m)
	}

	// Record execution start (idempotent per attempt)
	exec, err := st.CreateExecution(ctx, taskID, attempt)
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

	// Run handler
	runErr := h(ctx, task)
	if runErr == nil {
		if err := finishExecutionSucceeded(ctx, st, exec.ID); err != nil {
			_ = safeSetQueued(ctx, st, taskID)
			return actionRetry, attempt, err
		}

		_, err = st.UpdateTaskStatus(ctx, taskID, processing.Version, store.StatusCompleted)
		if err != nil && !errors.Is(err, store.ErrVersionConflict) {
			return actionRetry, attempt, err
		}

		logger.Info("task processed",
			zap.String("task_id", taskID.String()),
			zap.Int("attempt", attempt),
			zap.String("type", task.Type),
		)
		return actionAck, attempt, nil
	}

	// failure: record failed execution
	if err := finishExecutionFailed(ctx, st, exec.ID, runErr); err != nil {
		_ = safeSetQueued(ctx, st, taskID)
		return actionRetry, attempt, err
	}

	// permanent failure stops retries + publish DLQ
	if workerpkg.IsPermanent(runErr) {
		publishDLQBestEffort(ctx, logger, q, task, attempt, runErr, m)

		_, _ = st.UpdateTaskStatus(ctx, taskID, processing.Version, store.StatusFailed)

		logger.Error("task permanently failed",
			zap.String("task_id", taskID.String()),
			zap.Int("attempt", attempt),
			zap.String("type", task.Type),
			zap.String("error", runErr.Error()),
		)
		return actionAck, attempt, nil
	}

	// transient failure: if max attempts reached, fail task + publish DLQ
	if attempt >= cfg.WorkerMaxAttempts {
		publishDLQBestEffort(ctx, logger, q, task, attempt, runErr, m)

		_, _ = st.UpdateTaskStatus(ctx, taskID, processing.Version, store.StatusFailed)

		logger.Error("task failed (max attempts reached)",
			zap.String("task_id", taskID.String()),
			zap.Int("attempt", attempt),
			zap.String("type", task.Type),
			zap.String("error", runErr.Error()),
		)
		return actionAck, attempt, nil
	}

	// re-queue for visibility and retry later
	_ = safeSetQueued(ctx, st, taskID)

	logger.Warn("task failed, will retry",
		zap.String("task_id", taskID.String()),
		zap.Int("attempt", attempt),
		zap.String("type", task.Type),
		zap.String("error", runErr.Error()),
	)

	return actionRetry, attempt, nil
}

func publishDLQBestEffort(ctx context.Context, logger *zap.Logger, q *queue.Queue, task *store.Task, attempt int, reason error, m *nats.Msg) {
	if q == nil || task == nil || reason == nil || m == nil {
		return
	}
	dlq := queue.DLQMessage{
		TaskID:       task.ID.String(),
		TaskType:     task.Type,
		Attempt:      attempt,
		Error:        reason.Error(),
		OriginalSubj: m.Subject,
		OriginalData: m.Data,
		FailedAt:     time.Now(),
	}
	if err := q.PublishDLQ(ctx, dlq); err != nil {
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

// failPermanentlyClaimed assumes we already successfully transitioned the task to processing.
// This prevents duplicate side-effects on fast redelivery.
func failPermanentlyClaimed(
	ctx context.Context,
	logger *zap.Logger,
	st *store.Store,
	q *queue.Queue,
	task *store.Task,
	processingVersion int,
	attempt int,
	reason error,
	m *nats.Msg,
) (msgAction, int, error) {
	// Best-effort execution row for this delivery attempt
	exec, err := st.CreateExecution(ctx, task.ID, attempt)
	if err == nil {
		_ = finishExecutionFailed(ctx, st, exec.ID, reason)
	} else if errors.Is(err, store.ErrAlreadyExists) {
		// already recorded
	}

	// Publish DLQ (best effort)
	publishDLQBestEffort(ctx, logger, q, task, attempt, reason, m)

	// Mark task failed using the claimed version
	_, _ = st.UpdateTaskStatus(ctx, task.ID, processingVersion, store.StatusFailed)

	logger.Error("task permanently failed",
		zap.String("task_id", task.ID.String()),
		zap.Int("attempt", attempt),
		zap.String("type", task.Type),
		zap.String("error", reason.Error()),
	)

	return actionAck, attempt, nil
}

// Kept for other “fail without claim” usages if needed
func failPermanently(
	ctx context.Context,
	logger *zap.Logger,
	st *store.Store,
	q *queue.Queue,
	task *store.Task,
	attempt int,
	reason error,
	m *nats.Msg,
) (msgAction, int, error) {
	// Best-effort execution row for this delivery attempt
	exec, err := st.CreateExecution(ctx, task.ID, attempt)
	if err == nil {
		_ = finishExecutionFailed(ctx, st, exec.ID, reason)
	} else if errors.Is(err, store.ErrAlreadyExists) {
	}

	publishDLQBestEffort(ctx, logger, q, task, attempt, reason, m)
	_ = safeSetStatus(ctx, st, task.ID, store.StatusFailed)

	logger.Error("task permanently failed",
		zap.String("task_id", task.ID.String()),
		zap.Int("attempt", attempt),
		zap.String("type", task.Type),
		zap.String("error", reason.Error()),
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
	msg := cause.Error()
	_, err := st.FinishExecution(ctx, execID, store.ExecFailed, &msg)
	return err
}
