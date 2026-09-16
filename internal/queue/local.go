package queue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
	"go.uber.org/zap"
)

// LocalQueue is the in-process broker for the desktop build.
//
// It carries no durable state of its own, and that is the point: the tasks table
// already IS the queue. A row with status='queued' is pending work; the worker's
// optimistic claim is the dequeue; the reconciler already rescues anything left
// 'processing' by a crash. Reimplementing durability here would duplicate
// machinery that exists and is tested.
//
// So this type only supplies what a broker adds on top of a table: a wake-up
// signal so a freshly enqueued task starts immediately instead of at the next
// poll, and in-memory tracking of what is currently in flight so the same row is
// not handed out twice while someone is working on it.
//
// Crash semantics are unchanged: in-flight bookkeeping dies with the process,
// and on restart the reconciler finds the stale 'processing' rows and requeues
// them — the same path a NATS redelivery takes.
type LocalQueue struct {
	store  *store.Store
	logger *zap.Logger

	// wake carries no payload; it only says "look at the table now". Buffered
	// and written non-blockingly, because a missed wake-up costs latency (the
	// next poll catches it), never correctness.
	wake chan struct{}

	mu       sync.Mutex
	inFlight map[string]struct{}
}

func NewLocal(st *store.Store, logger *zap.Logger) *LocalQueue {
	return &LocalQueue{
		store:    st,
		logger:   logger,
		wake:     make(chan struct{}, 1),
		inFlight: make(map[string]struct{}),
	}
}

// PublishTask is a notification, not a write: the caller has already committed
// the task row, so there is nothing to persist and nothing to lose.
func (q *LocalQueue) PublishTask(_ context.Context, _ string, _ TaskMessage, _ Header) error {
	select {
	case q.wake <- struct{}{}:
	default: // a wake-up is already pending; one is as good as many
	}
	return nil
}

// PublishDLQ records a terminal failure.
//
// There is no dead-letter stream to inspect on a single machine, and there does
// not need to be: the task row is 'failed' and task_executions holds the
// scrubbed error. The durable record already exists — this only makes the event
// visible in the log.
func (q *LocalQueue) PublishDLQ(_ context.Context, msg DLQMessage, _ Header) error {
	q.logger.Error("task dead-lettered",
		zap.String("task_id", msg.TaskID),
		zap.String("task_type", msg.TaskType),
		zap.Int("attempt", msg.Attempt),
		zap.String("error", msg.Error),
	)
	return nil
}

// Fetch waits for a wake-up (or the timeout) and then claims from the table.
//
// The wake-up is only a hint. The table is consulted either way, so a lost
// signal degrades latency rather than losing work — and a task enqueued by some
// other means (a reconciler republish, a row written directly) is picked up just
// the same.
func (q *LocalQueue) Fetch(ctx context.Context, max int, wait time.Duration) ([]Message, error) {
	if max <= 0 {
		max = 1
	}

	// Shutdown is not a failure. Checking here — and translating a cancellation
	// out of the claim below — keeps the worker's loop from logging an error
	// every time it stops.
	if ctx.Err() != nil {
		return nil, nil
	}

	if msgs, err := q.claim(ctx, max); err != nil || len(msgs) > 0 {
		return msgs, ignoreCancellation(err)
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, nil
	case <-timer.C:
		// Idle timeout: poll anyway. This is the path that recovers work whose
		// wake-up was dropped while the buffer was full.
		msgs, err := q.claim(ctx, max)
		return msgs, ignoreCancellation(err)
	case <-q.wake:
		msgs, err := q.claim(ctx, max)
		return msgs, ignoreCancellation(err)
	}
}

func ignoreCancellation(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (q *LocalQueue) claim(ctx context.Context, max int) ([]Message, error) {
	queued := store.StatusQueued
	tasks, err := q.store.ListTasks(ctx, store.ListTasksParams{
		Status: &queued,
		Limit:  max + len(q.inFlightSnapshot()),
	})
	if err != nil {
		return nil, err
	}

	out := make([]Message, 0, max)
	for i := range tasks {
		if len(out) == max {
			break
		}
		t := tasks[i]
		if !q.take(t.ID.String()) {
			continue // already handed out and not yet settled
		}

		data, err := json.Marshal(TaskMessage{
			TaskID:   t.ID.String(),
			Priority: string(t.Priority),
		})
		if err != nil {
			q.release(t.ID.String())
			return nil, err
		}

		out = append(out, &localMessage{
			queue:   q,
			taskID:  t.ID.String(),
			subject: SubjectForPriority(string(t.Priority)),
			data:    data,
			header:  NewHeader(),
		})
	}
	return out, nil
}

func (q *LocalQueue) take(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, busy := q.inFlight[id]; busy {
		return false
	}
	q.inFlight[id] = struct{}{}
	return true
}

func (q *LocalQueue) release(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inFlight, id)
}

func (q *LocalQueue) inFlightSnapshot() map[string]struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]struct{}, len(q.inFlight))
	for k := range q.inFlight {
		out[k] = struct{}{}
	}
	return out
}

func (q *LocalQueue) Close() error { return nil }

type localMessage struct {
	queue   *LocalQueue
	taskID  string
	subject string
	data    []byte
	header  Header

	once sync.Once
}

func (m *localMessage) Data() []byte    { return m.data }
func (m *localMessage) Subject() string { return m.subject }
func (m *localMessage) Header() Header  { return m.header }

// Ack and Nak both just stop tracking the row as in-flight. What separates them
// is the task's own status, which the worker has already written: an acked task
// is terminal (or was a duplicate), a naked one is back to 'queued' and will be
// picked up by the next claim.
func (m *localMessage) Ack() error {
	m.once.Do(func() { m.queue.release(m.taskID) })
	return nil
}

func (m *localMessage) Nak() error {
	m.once.Do(func() {
		m.queue.release(m.taskID)
		// Wake the fetch loop so a retry does not wait for the idle timeout.
		select {
		case m.queue.wake <- struct{}{}:
		default:
		}
	})
	return nil
}

// NakWithDelay keeps the task marked in flight until the delay has passed, so
// claim skips it, then releases it exactly as Nak does.
//
// The hold lives only in memory, and that is enough: the row is already
// 'queued', so if the process exits during the wait the task is simply picked
// up on the next start — sooner than asked, never lost.
func (m *localMessage) NakWithDelay(delay time.Duration) error {
	if delay <= 0 {
		return m.Nak()
	}
	m.once.Do(func() {
		time.AfterFunc(delay, func() {
			m.queue.release(m.taskID)
			select {
			case m.queue.wake <- struct{}{}:
			default:
			}
		})
	})
	return nil
}

// InProgress is a no-op: there is no remote ack deadline to extend. The claim is
// the in-memory entry, which lives exactly as long as this process does — and a
// process that dies releases everything at once, which is precisely the case the
// reconciler already handles.
func (m *localMessage) InProgress() error { return nil }
