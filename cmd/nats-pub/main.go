package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/nats-io/nats.go"
)

func main() {
	var (
		taskID   = flag.String("task-id", "", "Task UUID to publish")
		subject  = flag.String("subject", queue.SubjectNormal, "NATS subject (tasks.normal|tasks.high|tasks.low)")
		count    = flag.Int("count", 2, "How many times to publish the same message")
		interval = flag.Duration("interval", 50*time.Millisecond, "Delay between publishes")
	)
	flag.Parse()

	if *taskID == "" {
		panic("missing --task-id")
	}
	if *count <= 0 {
		panic("--count must be > 0")
	}

	cfg := config.Load()

	q, err := queue.New(context.Background(), queue.Config{
		NATSURL:      cfg.NATSURL,
		StreamName:   cfg.NATSStreamName,
		ConsumerName: cfg.NATSConsumerName,
		AckWait:      30 * time.Second,
		MaxDeliver:   5,
	})
	if err != nil {
		panic(err)
	}
	defer q.Close()

	msg := queue.TaskMessage{
		TaskID:   *taskID,
		Priority: "normal",
	}

	b, _ := json.Marshal(msg)
	fmt.Printf("publishing %d time(s) to %s: %s\n", *count, *subject, string(b))

	for i := 0; i < *count; i++ {
		hdr := nats.Header{} // optional; empty is fine
		if err := q.PublishTask(context.Background(), *subject, msg, hdr); err != nil {
			panic(err)
		}
		time.Sleep(*interval)
	}

	fmt.Println("done")
}
