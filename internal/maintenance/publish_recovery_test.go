package maintenance

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// recordingBroker stands in for JetStream: the sweep only publishes, and what
// this test asserts is which tasks it published.
type recordingBroker struct {
	mu        sync.Mutex
	published []string
}

func (b *recordingBroker) PublishTask(_ context.Context, _ string, msg queue.TaskMessage, _ queue.Header) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, msg.TaskID)
	return nil
}

func (b *recordingBroker) PublishDLQ(context.Context, queue.DLQMessage, queue.Header) error {
	return nil
}

func (b *recordingBroker) Fetch(context.Context, int, time.Duration) ([]queue.Message, error) {
	return nil, nil
}

func (b *recordingBroker) Close() error { return nil }

func (b *recordingBroker) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.published...)
}

// Creating a task is two writes to two systems. When the second one never
// happens — the process dies between COMMIT and publish, or NATS is down — the
// row is queued with nothing to deliver it. This is the sweep that rescues it,
// and it must not wait out the staleness window the crashed-worker case needs.
func TestPublishRecoveryRepublishesWhatWasNeverDelivered(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(st.Close)

	org := uuid.New()
	if _, err := st.CreateOrganization(ctx, org, "local"); err != nil {
		t.Fatal(err)
	}
	mk := func() *store.Task {
		task, err := st.CreateTask(ctx, store.CreateTaskParams{
			Type: "demo", Payload: []byte(`{}`), Priority: store.PriorityNormal, OrgID: org,
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	lost, delivered := mk(), mk()
	if err := st.MarkTaskEnqueued(ctx, delivered.ID); err != nil {
		t.Fatal(err)
	}

	broker := &recordingBroker{}
	rc := &Reconciler{
		Logger: zap.NewNop(),
		Store:  st,
		Broker: broker,
		Config: &config.Config{
			WorkerReconcileInterval: time.Hour,
			// An hour of staleness: the other sweep cannot be what rescues this,
			// which is the whole point of separating them.
			WorkerReconcileStaleness: time.Hour,
			WorkerMaxAttempts:        5,
			// Negative, so the cutoff lands ahead of rows written a moment ago.
			// The delay itself is a scheduling choice; what is under test is
			// which rows the sweep selects.
			WorkerPublishRecoveryDelay: -time.Minute,
		},
		RecoverUnpublished: true,
	}

	if err := rc.recoverUnpublishedOnce(ctx); err != nil {
		t.Fatalf("recoverUnpublishedOnce: %v", err)
	}

	sent := broker.sent()
	if len(sent) != 1 || sent[0] != lost.ID.String() {
		t.Fatalf("published %v; want exactly the task that was never delivered (%s)", sent, lost.ID)
	}

	// Marked on the way out, so the next pass leaves it alone. Without this the
	// sweep republishes the same task on every tick, forever.
	if err := rc.recoverUnpublishedOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if again := broker.sent(); len(again) != 1 {
		t.Fatalf("the task was republished again on the next pass: %v", again)
	}

	got, err := st.GetTask(ctx, lost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusQueued {
		t.Errorf("status = %s; a republished task stays queued until a worker claims it", got.Status)
	}
}

// A task that exhausted its attempts and also never published must not be
// republished — that is a crash-pill, and the ledger says so.
func TestPublishRecoveryDeadLettersAnExhaustedTask(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	org := uuid.New()
	if _, err := st.CreateOrganization(ctx, org, "local"); err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, store.CreateTaskParams{
		Type: "demo", Payload: []byte(`{}`), Priority: store.PriorityNormal, OrgID: org,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		exec, err := st.CreateExecution(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		msg := "boom"
		if _, err := st.FinishExecution(ctx, exec.ID, store.ExecFailed, &msg); err != nil {
			t.Fatal(err)
		}
	}

	var deadLettered int
	broker := &recordingBroker{}
	rc := &Reconciler{
		Logger: zap.NewNop(),
		Store:  st,
		Broker: broker,
		Config: &config.Config{
			WorkerReconcileInterval:    time.Hour,
			WorkerReconcileStaleness:   time.Hour,
			WorkerMaxAttempts:          5,
			WorkerPublishRecoveryDelay: -time.Minute,
		},
		OnDeadLetter:       func(context.Context, *store.Task, error) { deadLettered++ },
		RecoverUnpublished: true,
	}

	if err := rc.recoverUnpublishedOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if sent := broker.sent(); len(sent) != 0 {
		t.Errorf("an exhausted task was republished: %v", sent)
	}
	if deadLettered != 1 {
		t.Errorf("dead-letter hook ran %d times, want 1", deadLettered)
	}
	got, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
}
