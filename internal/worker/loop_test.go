package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A task waiting out its retry backoff must not occupy a concurrency slot, and
// must not hold up shutdown. Both used to happen: the loop slept through the
// backoff inside the handler goroutine, so with one slot a single failing task
// stopped all other work for the whole delay, and Run waited for every sleep
// to finish before returning.
//
// Runs on the desktop's in-process stack (SQLite + LocalQueue), which is the
// real thing rather than a fake, and needs no server.
func TestRetryBackoffDoesNotHoldASlotOrShutdown(t *testing.T) {
	ctx := context.Background()

	st, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "loop.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(st.Close)
	org := uuid.New()
	if _, err := st.CreateOrganization(ctx, org, "local"); err != nil {
		t.Fatal(err)
	}

	enqueue := func(typ string) *store.Task {
		task, err := st.CreateTask(ctx, store.CreateTaskParams{
			Type: typ, Payload: json.RawMessage(`{}`), Priority: store.PriorityNormal, OrgID: org,
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	registry := NewRegistry()
	registry.Register("flaky", func(context.Context, *store.Task) error {
		return errors.New("transient failure")
	})
	registry.Register("ok", func(context.Context, *store.Task) error { return nil })

	const backoff = 5 * time.Second
	broker := queue.NewLocal(st, zap.NewNop())
	loop := &Loop{
		Logger:   zap.NewNop(),
		Store:    st,
		Broker:   broker,
		Registry: registry,
		Config: &config.Config{
			WorkerConcurrency:       1, // the one slot a sleeping retry would take
			WorkerPollTimeout:       20 * time.Millisecond,
			WorkerMaxAttempts:       5,
			WorkerBackoffBase:       backoff,
			WorkerBackoffMax:        backoff,
			WorkerHeartbeatInterval: time.Second,
			WorkerMaxProcessing:     10 * time.Second,
		},
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		loop.Run(runCtx)
		close(stopped)
	}()

	flaky := enqueue("flaky")
	waitFor(t, 2*time.Second, "flaky task's first attempt", func() bool {
		n, err := st.MaxAttempt(ctx, flaky.ID)
		return err == nil && n >= 1
	})

	ok := enqueue("ok")
	waitFor(t, 2*time.Second, "healthy task to complete while the flaky one backs off", func() bool {
		got, err := st.GetTask(ctx, ok.ID)
		return err == nil && got.Status == store.StatusCompleted
	})

	if n, _ := st.MaxAttempt(ctx, flaky.ID); n != 1 {
		t.Errorf("flaky task ran %d times inside its %s backoff; the delay was not honoured", n, backoff)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatalf("Run did not return promptly on shutdown; it is waiting out a %s backoff", backoff)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}
