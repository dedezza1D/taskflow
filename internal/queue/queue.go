package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	SubjectHigh   = "tasks.high"
	SubjectNormal = "tasks.normal"
	SubjectLow    = "tasks.low"
	SubjectDLQ    = "tasks.dlq"
)

type Config struct {
	NATSURL      string
	StreamName   string
	ConsumerName string
	AckWait      time.Duration
	MaxDeliver   int
}

type Queue struct {
	nc  *nats.Conn
	js  nats.JetStreamContext
	cfg Config
}

type TaskMessage struct {
	TaskID   string `json:"task_id"`
	Priority string `json:"priority"`
}

type DLQMessage struct {
	TaskID       string    `json:"task_id"`
	TaskType     string    `json:"task_type"`
	Attempt      int       `json:"attempt"`
	Error        string    `json:"error"`
	OriginalSubj string    `json:"original_subject"`
	OriginalData []byte    `json:"original_data"`
	FailedAt     time.Time `json:"failed_at"`
}

func New(ctx context.Context, cfg Config) (*Queue, error) {
	if cfg.AckWait == 0 {
		cfg.AckWait = 30 * time.Second
	}
	if cfg.MaxDeliver == 0 {
		cfg.MaxDeliver = 5
	}

	nc, err := nats.Connect(cfg.NATSURL,
		nats.Timeout(5*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, err
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, err
	}

	q := &Queue{nc: nc, js: js, cfg: cfg}
	if err := q.ensureStream(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	return q, nil
}

func (q *Queue) Close() {
	if q.nc != nil {
		q.nc.Close()
	}
}

func (q *Queue) ensureStream(ctx context.Context) error {
	// Desired subjects for our application stream.
	desired := []string{SubjectHigh, SubjectNormal, SubjectLow, SubjectDLQ}

	// If stream exists: merge subjects safely and update only if needed.
	if info, err := q.js.StreamInfo(q.cfg.StreamName); err == nil && info != nil {
		existing := info.Config.Subjects
		merged, changed := mergeSubjects(existing, desired)
		if !changed {
			return nil
		}

		sc := info.Config          // copy existing config
		sc.Subjects = merged       // only change subjects
		sc.Name = q.cfg.StreamName // ensure name stays correct

		if _, err := q.js.UpdateStream(&sc); err != nil {
			return fmt.Errorf("update stream: %w", err)
		}
		return nil
	}

	// Otherwise create stream.
	sc := &nats.StreamConfig{
		Name:      q.cfg.StreamName,
		Subjects:  desired,
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
	}
	if _, err := q.js.AddStream(sc); err != nil {
		return fmt.Errorf("add stream: %w", err)
	}
	return nil
}

func mergeSubjects(existing, desired []string) ([]string, bool) {
	set := make(map[string]struct{}, len(existing)+len(desired))
	out := make([]string, 0, len(existing)+len(desired))

	// keep existing order
	for _, s := range existing {
		if _, ok := set[s]; ok {
			continue
		}
		set[s] = struct{}{}
		out = append(out, s)
	}

	changed := false
	for _, s := range desired {
		if _, ok := set[s]; ok {
			continue
		}
		set[s] = struct{}{}
		out = append(out, s)
		changed = true
	}

	return out, changed
}

func (q *Queue) PublishTask(ctx context.Context, subject string, msg TaskMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = q.js.Publish(subject, b)
	return err
}

func (q *Queue) JetStream() nats.JetStreamContext {
	return q.js
}

func (q *Queue) PublishDLQ(ctx context.Context, msg DLQMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = q.js.Publish(SubjectDLQ, b)
	return err
}
