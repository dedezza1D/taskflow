package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dedezza1D/taskflow/api/httpapi"
	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
	"github.com/dedezza1D/taskflow/internal/observability"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
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
		ServiceName: firstNonEmpty(cfg.OTELServiceName, "taskflow-api"),
		Endpoint:    cfg.OTELExporterOTLPEndpoint,
		Env:         cfg.Env,
	})
	if err != nil {
		logger.Fatal("otel init failed", zap.Error(err))
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// Postgres store
	st, err := store.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Fatal("db connection failed", zap.Error(err))
	}
	defer st.Close()

	// NATS JetStream queue (publisher)
	q, err := queue.New(context.Background(), queue.Config{
		NATSURL:      cfg.NATSURL,
		StreamName:   cfg.NATSStreamName,
		ConsumerName: cfg.NATSConsumerName, // not used by API, but ok to keep consistent
		AckWait:      30 * time.Second,
		MaxDeliver:   5,
	})
	if err != nil {
		logger.Fatal("nats connection failed", zap.Error(err))
	}
	defer q.Close()

	// HTTP server
	server := httpapi.NewServer(httpapi.Config{Port: cfg.HTTPPort}, logger, st, q)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := server.Start(); err != nil && err.Error() != "http: Server closed" {
			logger.Fatal("http server error", zap.Error(err))
		}
	}()

	<-stop
	logger.Info("shutdown signal received")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", zap.Error(err))
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
