package queue

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// These run against a real SQLite store, because the whole design of the local
// queue is "the tasks table IS the queue" — testing it against a fake table
// would test nothing.

func newLocalQueue(t *testing.T) (*LocalQueue, *store.Store) {
	t.Helper()
	st, err := store.NewSQLite(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(st.Close)
	// Every task names its tenant, so the fresh file needs one to point at.
	if _, err := st.CreateOrganization(context.Background(), testOrg, "local"); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return NewLocal(st, zap.NewNop()), st
}

var testOrg = uuid.New()

func enqueue(t *testing.T, st *store.Store, typ string) *store.Task {
	t.Helper()
	task, err := st.CreateTask(context.Background(), store.CreateTaskParams{
		Type: typ, Payload: json.RawMessage(`{}`), Priority: store.PriorityNormal, OrgID: testOrg,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

func TestLocalQueueDeliversQueuedWork(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()

	task := enqueue(t, st, "demo")

	msgs, err := q.Fetch(ctx, 1, time.Second)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	var tm TaskMessage
	if err := json.Unmarshal(msgs[0].Data(), &tm); err != nil {
		t.Fatalf("payload is not a TaskMessage: %v", err)
	}
	if tm.TaskID != task.ID.String() {
		t.Fatalf("delivered %s, want %s", tm.TaskID, task.ID)
	}
	if msgs[0].Subject() != SubjectForPriority("normal") {
		t.Errorf("subject = %q", msgs[0].Subject())
	}
}

// A delayed nak keeps the task out of reach until the delay passes, then gives
// it back like a plain nak.
func TestLocalQueueNakWithDelayHoldsTheTaskBack(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()
	task := enqueue(t, st, "demo")

	msgs, err := q.Fetch(ctx, 1, 50*time.Millisecond)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Fetch: %d messages, err %v", len(msgs), err)
	}
	const delay = 300 * time.Millisecond
	if err := msgs[0].NakWithDelay(delay); err != nil {
		t.Fatal(err)
	}

	early, err := q.Fetch(ctx, 1, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatal("task redelivered before its delay elapsed")
	}

	// The release wakes the fetch, so this returns shortly after the delay
	// rather than at the end of the wait.
	later, err := q.Fetch(ctx, 1, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 {
		t.Fatal("task not redelivered after its delay")
	}
	var tm TaskMessage
	_ = json.Unmarshal(later[0].Data(), &tm)
	if tm.TaskID != task.ID.String() {
		t.Fatalf("redelivered %s, want %s", tm.TaskID, task.ID)
	}
}

// A task already handed out must not be handed out again until it is settled,
// or two workers would do the same job concurrently.
func TestLocalQueueDoesNotRedeliverInFlightWork(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()
	enqueue(t, st, "demo")

	first, err := q.Fetch(ctx, 10, 50*time.Millisecond)
	if err != nil || len(first) != 1 {
		t.Fatalf("first fetch: %d msgs, %v", len(first), err)
	}

	second, err := q.Fetch(ctx, 10, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("in-flight task was delivered again (%d messages)", len(second))
	}
}

// Nak returns the work. The task row is still 'queued' — the worker resets it —
// so the next fetch must pick it up.
func TestLocalQueueNakMakesWorkAvailableAgain(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()
	task := enqueue(t, st, "demo")

	first, _ := q.Fetch(ctx, 1, 50*time.Millisecond)
	if len(first) != 1 {
		t.Fatal("expected one message")
	}
	if err := first[0].Nak(); err != nil {
		t.Fatalf("Nak: %v", err)
	}

	again, err := q.Fetch(ctx, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("naked work was not redelivered (%d messages)", len(again))
	}
	var tm TaskMessage
	_ = json.Unmarshal(again[0].Data(), &tm)
	if tm.TaskID != task.ID.String() {
		t.Fatalf("redelivered the wrong task")
	}
}

// After Ack the row is terminal, so it must not come back.
func TestLocalQueueAckedWorkIsNotRedelivered(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()
	task := enqueue(t, st, "demo")

	msgs, _ := q.Fetch(ctx, 1, 50*time.Millisecond)
	if len(msgs) != 1 {
		t.Fatal("expected one message")
	}

	// The worker marks the task terminal before acking; mirror that.
	if _, err := st.UpdateTaskStatus(ctx, task.ID, task.Version, store.StatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := msgs[0].Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	after, err := q.Fetch(ctx, 1, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("a completed task was delivered again (%d messages)", len(after))
	}
}

// Publishing is only a wake-up: it must make a waiting Fetch return promptly
// rather than sit out the full idle timeout.
func TestLocalQueuePublishWakesAWaitingFetch(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()

	done := make(chan []Message, 1)
	go func() {
		msgs, err := q.Fetch(ctx, 1, 10*time.Second)
		if err != nil {
			t.Errorf("Fetch: %v", err)
		}
		done <- msgs
	}()

	// Give the fetch time to block on the wake channel.
	time.Sleep(100 * time.Millisecond)
	enqueue(t, st, "demo")
	if err := q.PublishTask(ctx, "tasks.normal", TaskMessage{}, NewHeader()); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	select {
	case msgs := <-done:
		if len(msgs) != 1 {
			t.Fatalf("woke up with %d messages, want 1", len(msgs))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Fetch did not wake on publish — it waited for the idle timeout")
	}
}

// A dropped wake-up must cost latency, never work: the idle poll is the
// recovery path, and it is what also picks up a reconciler republish.
func TestLocalQueueFindsWorkWithoutAWakeUp(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()

	// Enqueue directly, with no PublishTask at all.
	enqueue(t, st, "demo")

	msgs, err := q.Fetch(ctx, 1, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("idle poll did not find queued work (%d messages)", len(msgs))
	}
}

func TestLocalQueueIdleFetchReturnsNothing(t *testing.T) {
	q, _ := newLocalQueue(t)

	start := time.Now()
	msgs, err := q.Fetch(context.Background(), 1, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("an idle fetch must not be an error: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("got %d messages from an empty queue", len(msgs))
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v — it did not wait for the timeout", elapsed)
	}
}

// Settling twice must not corrupt the in-flight bookkeeping; the worker's error
// paths can reach both Nak and Ack for one message.
func TestLocalQueueSettlingIsIdempotent(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()
	enqueue(t, st, "demo")

	msgs, _ := q.Fetch(ctx, 1, 50*time.Millisecond)
	if len(msgs) != 1 {
		t.Fatal("expected one message")
	}

	for i := 0; i < 3; i++ {
		if err := msgs[0].Ack(); err != nil {
			t.Fatalf("Ack %d: %v", i, err)
		}
	}
	if err := msgs[0].Nak(); err != nil {
		t.Fatalf("Nak after Ack: %v", err)
	}

	// One release, so the row is available exactly once more.
	again, err := q.Fetch(ctx, 10, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("got %d messages after repeated settling, want 1", len(again))
	}
}

// InProgress exists for the lease heartbeat. There is no remote deadline here,
// but it must stay callable — worker.RunWithLease invokes it on a ticker.
func TestLocalQueueInProgressIsHarmless(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()
	enqueue(t, st, "demo")

	msgs, _ := q.Fetch(ctx, 1, 50*time.Millisecond)
	if len(msgs) != 1 {
		t.Fatal("expected one message")
	}
	for i := 0; i < 5; i++ {
		if err := msgs[0].InProgress(); err != nil {
			t.Fatalf("InProgress: %v", err)
		}
	}
}

func TestLocalQueueRespectsMaxAndCancellation(t *testing.T) {
	q, st := newLocalQueue(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		enqueue(t, st, "demo")
	}

	msgs, err := q.Fetch(ctx, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the requested 2", len(msgs))
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Fetch(cancelled, 1, time.Second); err != nil {
		t.Fatalf("a cancelled fetch should return cleanly, got %v", err)
	}
}
