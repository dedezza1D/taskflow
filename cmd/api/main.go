package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dedezza1D/taskflow/api/httpapi"
	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
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
		ConsumerName: cfg.NATSConsumerName, // not used by API, but fine to keep consistent
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
