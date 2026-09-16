package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
	"github.com/dedezza1D/taskflow/internal/maintenance"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/observability"
	"github.com/dedezza1D/taskflow/internal/pipeline"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	workerpkg "github.com/dedezza1D/taskflow/internal/worker"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// onDeadLetter, when set, is invoked at every point a task terminally fails
// (permanent error, exhausted attempts, reconciler reap) so domain handlers can
// project the outcome onto their own entities — the document pipeline uses it
// to wire dead-letter → documents.status = failed(+stage).
var onDeadLetter func(ctx context.Context, t *store.Task, reason error)

func notifyDeadLetter(ctx context.Context, t *store.Task, reason error) {
	if onDeadLetter != nil && t != nil {
		onDeadLetter(ctx, t, reason)
	}
}

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
		// NOTE: AckWait/MaxDeliver here are NOT applied to the pull consumer — the
		// consumer's ack deadline is set from cfg.WorkerAckWait by NewNATSBroker.
		// These fields only affect queue.New's internal defaults.
		AckWait:    30 * time.Second,
		MaxDeliver: 5,
	})
	if err != nil {
		logger.Fatal("nats connection failed", zap.Error(err))
	}
	defer q.Close()

	broker, err := queue.NewNATSBroker(q, queue.ConsumerConfig{
		StreamName:   cfg.NATSStreamName,
		ConsumerName: cfg.NATSConsumerName,
		AckWait:      cfg.WorkerAckWait,
	}, logger)
	if err != nil {
		logger.Fatal("create pull consumer failed", zap.Error(err))
	}
	defer func() { _ = broker.Close() }()

	registry := workerpkg.DefaultHandlers()

	// Compliance document pipeline (OCR → PII → report) on the same engine.
	obj, err := objects.NewFS(cfg.ObjectsDir)
	if err != nil {
		logger.Fatal("object store init failed", zap.Error(err))
	}
	pl := pipeline.New(st, obj, logger, pipeline.Config{
		OCRTimeout:   cfg.WorkerOCRTimeout,
		OCRLanguages: cfg.OCRLanguages,
	})
	pl.Register(registry)

	loop := &workerpkg.Loop{
		Logger:       logger,
		Store:        st,
		Broker:       broker,
		Registry:     registry,
		Config:       cfg,
		OnDeadLetter: pl.MarkDeadLettered,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

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

	// Background reconciler: rescues stuck tasks (lost enqueue / crashed worker).
	reconciler := &maintenance.Reconciler{
		Logger:       logger,
		Store:        st,
		Broker:       broker,
		Config:       cfg,
		OnDeadLetter: pl.MarkDeadLettered,
		// Two systems, one crash window: the row commits here and the message
		// goes to JetStream next.
		RecoverUnpublished: true,
	}
	go reconciler.Run(ctx)

	// Background retention sweep: destroys raw material left by documents that
	// dead-lettered, which have no completion event to shred against.
	go maintenance.RunRawRetention(ctx, logger, st, pl, cfg)

	// Background auth housekeeping: expired sessions and recovery links.
	go runAuthHousekeeping(ctx, logger, st)

	loop.Run(ctx)
	logger.Info("worker stopped")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
