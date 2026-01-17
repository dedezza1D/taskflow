package main

import (
	"context"
	"fmt"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/logging"
	"github.com/dedezza1D/taskflow/internal/queue"
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
		ConsumerName: "js-info",
		AckWait:      30 * time.Second,
		MaxDeliver:   5,
	})
	if err != nil {
		logger.Fatal("nats connection failed", zap.Error(err))
	}
	defer q.Close()

	js := q.JetStream()

	info, err := js.StreamInfo(cfg.NATSStreamName)
	if err != nil {
		logger.Fatal("StreamInfo failed", zap.Error(err))
	}

	fmt.Println("STREAM:", info.Config.Name)
	fmt.Println("SUBJECTS:")
	for _, s := range info.Config.Subjects {
		fmt.Println(" -", s)
	}
	fmt.Println("STATE:", "msgs=", info.State.Msgs, "bytes=", info.State.Bytes)
}
