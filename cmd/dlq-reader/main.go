package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/nats-io/nats.go"
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

	q, err := queue.New(context.Background(), queue.Config{
		NATSURL:      cfg.NATSURL,
		StreamName:   cfg.NATSStreamName,
		ConsumerName: "dlq-reader",
		AckWait:      30 * time.Second,
		MaxDeliver:   5,
	})
	if err != nil {
		logger.Fatal("nats connection failed", zap.Error(err))
	}
	defer q.Close()

	js := q.JetStream()

	sub, err := js.PullSubscribe(queue.SubjectDLQ, "dlq-reader",
		nats.BindStream(cfg.NATSStreamName),
		nats.ManualAck(),
		nats.AckExplicit(),
	)
	if err != nil {
		logger.Fatal("pull subscribe failed", zap.Error(err))
	}

	logger.Info("listening for DLQ messages", zap.String("subject", queue.SubjectDLQ))

	for {
		msgs, err := sub.Fetch(10, nats.MaxWait(2*time.Second))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			logger.Fatal("fetch failed", zap.Error(err))
		}

		for _, m := range msgs {
			var dlq queue.DLQMessage
			if err := json.Unmarshal(m.Data, &dlq); err != nil {
				logger.Error("bad DLQ message JSON", zap.Error(err))
				_ = m.Ack()
				continue
			}

			pretty, _ := json.MarshalIndent(dlq, "", "  ")
			logger.Info("DLQ message", zap.String("json", string(pretty)))

			_ = m.Ack()
		}
	}
}
