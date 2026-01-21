package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
	"github.com/dedezza1D/taskflow/internal/observability"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	workerpkg "github.com/dedezza1D/taskflow/internal/worker"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	observability.RegisterMetrics()

	shutdownTracing, err := observability.InitTracing(context.Background(), observability.OTelConfig{
		ServiceName: firstNonEmpty(cfg.OTELServiceName, "taskflow-worker"),
		Endpoint:    cfg.OTELExporterOTLPEndpoint,
		Env:         cfg.Env,
	})
	if err != nil {
		logger.Fatal("otel init failed", zap.Error(err))
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// Metrics endpoint
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		addr := fmt.Sprintf(":%d", cfg.WorkerMetricsPort)
		logger.Info("worker metrics server starting", zap.String("addr", addr))
		_ = http.ListenAndServe(addr, mux)
	}()

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
	// Extract trace context from NATS headers (if present)
	if m.Header != nil {
		ctx = otel.GetTextMapPropagator().Extract(ctx, observability.NATSHeaderCarrier{H: m.Header})
	}
	tr := otel.Tracer("taskflow/worker")
	ctx, span := tr.Start(ctx, "taskflow.handle_msg")
	defer span.End()

	// Attempt number from JetStream delivery count
	attempt := 1
	if md, err := m.Metadata(); err == nil && md != nil && md.NumDelivered > 0 {
		attempt = int(md.NumDelivered)
	}

	var tm queue.TaskMessage
	if err := json.Unmarshal(m.Data, &tm); err != nil {
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

	span.SetAttributes(
		attribute.String("messaging.subject", m.Subject),
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
		return failPermanently(ctx, logger, st, q, task, attempt, fmt.Errorf("max attempts exceeded (%d)", cfg.WorkerMaxAttempts), m)
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
	observability.TasksStartedTotal.WithLabelValues(task.Type, tm.Priority).Inc()
	start := time.Now()
	runErr := h(ctx, task)
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

	span.RecordError(runErr)
	span.SetStatus(codes.Error, runErr.Error())

	// Failure: record failed execution
	reason := "retryable"
	if workerpkg.IsPermanent(runErr) {
		reason = "permanent"
	}
	observability.TasksFailedTotal.WithLabelValues(task.Type, reason).Inc()

	if err := finishExecutionFailed(ctx, st, exec.ID, runErr); err != nil {
		_ = safeSetQueued(ctx, st, taskID)
		return actionRetry, attempt, err
	}

	// Permanent failure -> DLQ + mark failed
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

	// Transient failure: if max attempts reached -> DLQ + mark failed
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

	// Re-queue and retry later
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

	hdr := nats.Header{}
	otel.GetTextMapPropagator().Inject(ctx, observability.NATSHeaderCarrier{H: hdr})
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
	exec, err := st.CreateExecution(ctx, task.ID, attempt)
	if err == nil {
		_ = finishExecutionFailed(ctx, st, exec.ID, reason)
	} else if errors.Is(err, store.ErrAlreadyExists) {
		// already recorded
	}

	publishDLQBestEffort(ctx, logger, q, task, attempt, reason, m)
	_, _ = st.UpdateTaskStatus(ctx, task.ID, processingVersion, store.StatusFailed)

	logger.Error("task permanently failed",
		zap.String("task_id", task.ID.String()),
		zap.Int("attempt", attempt),
		zap.String("type", task.Type),
		zap.String("error", reason.Error()),
	)

	return actionAck, attempt, nil
}

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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
