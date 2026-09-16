package queue

import (
	"context"
	"net/textproto"
	"time"
)

// The broker seam.
//
// The worker's lifecycle logic — claim, attempt ledger, lease, retry, DLQ — is
// already written against the DATABASE, not against NATS. The broker only does
// two things: hand over work, and let the worker settle it. So the interface is
// small, and a second implementation does not have to reproduce any of the
// reliability machinery.
//
// This is what lets the desktop build drop NATS entirely: one process, one
// file, nothing to connect to.

// Header carries trace context alongside a message. Its own type rather than
// nats.Header so the local build does not link a broker client it never uses.
type Header map[string][]string

func NewHeader() Header { return Header{} }

func (h Header) Get(key string) string {
	if h == nil {
		return ""
	}
	v := h[textproto.CanonicalMIMEHeaderKey(key)]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (h Header) Set(key, value string) {
	h[textproto.CanonicalMIMEHeaderKey(key)] = []string{value}
}

func (h Header) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}

// Message is one unit of work as the worker sees it.
//
// Ack, Nak and InProgress are the whole settlement vocabulary: done with it,
// give it back, still working. A broker that cannot express those cannot give
// the at-least-once guarantee the pipeline is built on.
type Message interface {
	Data() []byte
	Subject() string
	Header() Header

	// Ack settles the message as handled. The durable outcome is already in the
	// database by this point; acking only stops redelivery.
	Ack() error
	// Nak returns the message for redelivery.
	Nak() error
	// NakWithDelay returns the message for redelivery no sooner than delay from
	// now. It is how a retry backs off without the worker holding a
	// concurrency slot for the whole wait.
	NakWithDelay(delay time.Duration) error
	// InProgress is the liveness heartbeat that keeps a long-running handler's
	// claim alive — see worker.RunWithLease.
	InProgress() error
}

// Consumer is the pull side.
type Consumer interface {
	// Fetch blocks for at most wait and returns up to max messages. Returning
	// zero messages with a nil error means "nothing right now" — the ordinary
	// idle case, not a failure.
	Fetch(ctx context.Context, max int, wait time.Duration) ([]Message, error)
	Close() error
}

// Publisher is the enqueue side.
type Publisher interface {
	PublishTask(ctx context.Context, subject string, msg TaskMessage, hdr Header) error
	PublishDLQ(ctx context.Context, msg DLQMessage, hdr Header) error
}

// Broker is both halves, which is what the worker binary needs.
type Broker interface {
	Publisher
	Consumer
}
