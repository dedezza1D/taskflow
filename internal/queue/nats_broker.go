package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// natsBroker adapts the JetStream Queue to the Broker seam: the served
// deployment's implementation, where durability and redelivery come from the
// stream rather than from a single process's memory.
type natsBroker struct {
	q   *Queue
	sub *nats.Subscription
}

// ConsumerConfig is what a durable pull consumer needs beyond the connection.
type ConsumerConfig struct {
	StreamName   string
	ConsumerName string
	AckWait      time.Duration
}

// NewNATSBroker binds a durable pull subscription and returns it as a Broker.
//
// The AckWait reconcile is here rather than at the call site because it is a
// property of this broker: nats.go rejects a subscribe whose AckWait disagrees
// with an existing durable consumer, so a changed setting would otherwise stop
// the worker from starting at all.
func NewNATSBroker(q *Queue, cfg ConsumerConfig, logger *zap.Logger) (Broker, error) {
	js := q.JetStream()

	if ci, err := js.ConsumerInfo(cfg.StreamName, cfg.ConsumerName); err == nil && ci != nil {
		if ci.Config.AckWait != cfg.AckWait {
			updated := ci.Config // preserve the other (some immutable) fields
			updated.AckWait = cfg.AckWait
			if _, err := js.UpdateConsumer(cfg.StreamName, &updated); err != nil {
				return nil, fmt.Errorf("reconcile consumer ack wait: %w", err)
			}
			logger.Info("reconciled consumer AckWait",
				zap.Duration("from", ci.Config.AckWait),
				zap.Duration("to", cfg.AckWait),
			)
		}
	} else if err != nil && !errors.Is(err, nats.ErrConsumerNotFound) {
		return nil, fmt.Errorf("consumer info: %w", err)
	}

	sub, err := js.PullSubscribe("tasks.*", cfg.ConsumerName,
		nats.BindStream(cfg.StreamName),
		nats.ManualAck(),
		nats.AckExplicit(),
		// Modest deadline, kept alive by InProgress heartbeats while a handler
		// runs (see worker.RunWithLease): the slow-but-healthy case keeps its
		// claim, the crashed case is released quickly.
		nats.AckWait(cfg.AckWait),
	)
	if err != nil {
		return nil, err
	}

	logger.Info("pull subscription ready",
		zap.String("stream", cfg.StreamName),
		zap.String("consumer", cfg.ConsumerName),
	)
	return &natsBroker{q: q, sub: sub}, nil
}

func (b *natsBroker) PublishTask(ctx context.Context, subject string, msg TaskMessage, hdr Header) error {
	return b.q.PublishTask(ctx, subject, msg, nats.Header(hdr))
}

func (b *natsBroker) PublishDLQ(ctx context.Context, msg DLQMessage, hdr Header) error {
	return b.q.PublishDLQ(ctx, msg, nats.Header(hdr))
}

func (b *natsBroker) Fetch(_ context.Context, max int, wait time.Duration) ([]Message, error) {
	msgs, err := b.sub.Fetch(max, nats.MaxWait(wait))
	if err != nil {
		// An idle fetch times out; that is "nothing right now", not a failure.
		if errors.Is(err, nats.ErrTimeout) {
			return nil, nil
		}
		return nil, err
	}

	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, &natsMessage{msg: m})
	}
	return out, nil
}

func (b *natsBroker) Close() error {
	if b.sub != nil {
		return b.sub.Drain()
	}
	return nil
}

type natsMessage struct{ msg *nats.Msg }

func (m *natsMessage) Data() []byte      { return m.msg.Data }
func (m *natsMessage) Subject() string   { return m.msg.Subject }
func (m *natsMessage) Header() Header    { return Header(m.msg.Header) }
func (m *natsMessage) Ack() error        { return m.msg.Ack() }
func (m *natsMessage) Nak() error        { return m.msg.Nak() }
func (m *natsMessage) InProgress() error { return m.msg.InProgress() }
