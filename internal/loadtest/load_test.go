//go:build load

// A load profile for the real engine: real PostgreSQL, real JetStream, real
// worker loops. Behind a build tag because it needs NATS — which nothing else
// in this suite does — and because a number nobody asked for is not worth a
// minute on every CI run.
//
//	docker compose up -d postgres nats
//	go test -tags load ./internal/loadtest -v -timeout 15m
//
// Knobs: LOAD_TASKS (default 1000), LOAD_WORKERS (default 10), LOAD_FAIL_RATE
// (default 20, meaning one task in 20 fails its first attempt), NATS_URL.
//
// What it measures is END-TO-END latency: from the moment a task's message is
// published to the moment its handler returns. That includes the queue wait, so
// under a backlog the number is a queue-depth measurement as much as a
// processing one — which is the honest thing to report for a worker pool.
package loadtest

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/testdb"
	"github.com/dedezza1D/taskflow/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

const taskType = "loadtest"

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// failsFirstAttempt picks a deterministic slice of the tasks to fail once, so a
// run exercises the retry path and reports a retry rate rather than claiming
// the happy path is the whole story.
func failsFirstAttempt(id uuid.UUID, rate int) bool {
	h := fnv.New32a()
	_, _ = h.Write(id[:])
	return rate > 0 && int(h.Sum32())%rate == 0
}

func TestLoadProfile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	total := envInt("LOAD_TASKS", 1000)
	workers := envInt("LOAD_WORKERS", 10)
	failRate := envInt("LOAD_FAIL_RATE", 20)
	natsURL := envStr("NATS_URL", "nats://localhost:4222")
	stream := envStr("NATS_STREAM_NAME", "TASKFLOW")

	dsn := testdb.DSN(t)
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)

	org := uuid.New()
	if _, err := st.CreateOrganization(ctx, org, "loadtest"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := pool.Exec(c, `DELETE FROM tasks WHERE org_id = $1`, org); err != nil {
			t.Logf("cleanup tasks: %v", err)
		}
		if _, err := pool.Exec(c, `DELETE FROM organizations WHERE id = $1`, org); err != nil {
			t.Logf("cleanup org: %v", err)
		}
	})

	// A durable consumer of its own, so a dev worker on the same stream does not
	// eat this run's messages. Whatever is left in the stream afterwards is
	// harmless: the rows are deleted, and a worker that cannot find a task acks.
	consumer := "loadtest-" + uuid.NewString()[:8]
	q, err := queue.New(ctx, queue.Config{
		NATSURL:      natsURL,
		StreamName:   stream,
		ConsumerName: consumer,
		AckWait:      30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NATS is required by this test.\n"+
			"  start it with: docker compose up -d nats\n"+
			"  url: %s\n  cause: %v", natsURL, err)
	}
	t.Cleanup(q.Close)
	t.Cleanup(func() { _ = q.JetStream().DeleteConsumer(stream, consumer) })

	cfg := &config.Config{
		WorkerConcurrency:       1, // one in flight per worker: workers is the pool size
		WorkerPollTimeout:       500 * time.Millisecond,
		WorkerMaxAttempts:       5,
		WorkerBackoffBase:       200 * time.Millisecond,
		WorkerBackoffMax:        time.Second,
		WorkerHeartbeatInterval: 5 * time.Second,
		WorkerMaxProcessing:     60 * time.Second,
	}

	var (
		mu        sync.Mutex
		published = make(map[uuid.UUID]time.Time, total)
		latencies = make([]time.Duration, 0, total)
		failedIDs = make(map[uuid.UUID]bool)
	)
	done := make(chan uuid.UUID, total)

	registry := worker.NewRegistry()
	registry.Register(taskType, func(ctx context.Context, task *store.Task) error {
		mu.Lock()
		alreadyFailed := failedIDs[task.ID]
		if failsFirstAttempt(task.ID, failRate) && !alreadyFailed {
			failedIDs[task.ID] = true
			mu.Unlock()
			return fmt.Errorf("injected failure for the retry path")
		}
		start := published[task.ID]
		mu.Unlock()

		// A few milliseconds of "work": enough that the numbers are not pure
		// bookkeeping, small enough that the engine is what is being measured.
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}

		mu.Lock()
		latencies = append(latencies, time.Since(start))
		mu.Unlock()
		done <- task.ID
		return nil
	})

	// Each worker gets its own pull subscription on the shared durable, which is
	// what N worker processes look like to the stream.
	for i := 0; i < workers; i++ {
		broker, err := queue.NewNATSBroker(q, queue.ConsumerConfig{
			StreamName:   stream,
			ConsumerName: consumer,
			AckWait:      30 * time.Second,
		}, zap.NewNop())
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		loop := &worker.Loop{
			Logger:   zap.NewNop(),
			Store:    st,
			Broker:   broker,
			Registry: registry,
			Config:   cfg,
		}
		go loop.Run(ctx)
	}

	t.Logf("creating %d tasks for %d workers (1 in %d fails its first attempt)", total, workers, failRate)
	createStart := time.Now()

	// Eight producers: a single one makes this a measurement of sequential
	// INSERT latency rather than of the engine.
	var created int
	var createdMu sync.Mutex
	var producers sync.WaitGroup
	for p := 0; p < 8; p++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for i := 0; i < total/8; i++ {
				task, err := st.CreateTask(ctx, store.CreateTaskParams{
					Type: taskType, Payload: []byte(`{}`), Priority: store.PriorityNormal, OrgID: org,
				})
				if err != nil {
					t.Errorf("create task: %v", err)
					return
				}
				mu.Lock()
				published[task.ID] = time.Now()
				mu.Unlock()

				if err := q.PublishTask(ctx, queue.SubjectForPriority("normal"),
					queue.TaskMessage{TaskID: task.ID.String(), Priority: "normal"}, nil); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
				if err := st.MarkTaskEnqueued(ctx, task.ID); err != nil {
					t.Errorf("mark enqueued: %v", err)
					return
				}
				createdMu.Lock()
				created++
				createdMu.Unlock()
			}
		}()
	}
	producers.Wait()
	t.Logf("created and published %d tasks in %s", created, time.Since(createStart).Round(time.Millisecond))

	deadline := time.After(10 * time.Minute)
	for completed := 0; completed < created; {
		select {
		case <-done:
			completed++
		case <-deadline:
			t.Fatalf("timed out with %d/%d completed", completed, created)
		}
	}
	// From the first publish, not from the end of production: the workers were
	// draining the whole time, and timing only the tail would report a
	// throughput the system never sustained.
	elapsed := time.Since(createStart)

	// A handler signals completion before the loop writes the terminal status,
	// so the last few rows are still settling. Let them, then stop the workers —
	// cancelling first would count a task as stuck that was mid-update.
	settleTerminalStates(t, ctx, pool, org, created)
	cancel()

	// Correctness first: the numbers mean nothing if the run did not finish
	// cleanly.
	var queued, processing, completedRows, failedRows int
	if err := pool.QueryRow(context.Background(), `
		SELECT
			count(*) FILTER (WHERE status = 'queued'),
			count(*) FILTER (WHERE status = 'processing'),
			count(*) FILTER (WHERE status = 'completed'),
			count(*) FILTER (WHERE status = 'failed')
		FROM tasks WHERE org_id = $1`, org).
		Scan(&queued, &processing, &completedRows, &failedRows); err != nil {
		t.Fatalf("status counts: %v", err)
	}
	var executions int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM task_executions e JOIN tasks t ON t.id = e.task_id
		WHERE t.org_id = $1`, org).Scan(&executions); err != nil {
		t.Fatalf("execution count: %v", err)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*p)].Round(time.Millisecond)
	}

	t.Log("")
	t.Logf("tasks            %d across %d workers", created, workers)
	t.Logf("wall time        %s", elapsed.Round(time.Millisecond))
	t.Logf("throughput       %.1f tasks/s", float64(created)/elapsed.Seconds())
	t.Logf("latency p50      %s", pct(0.50))
	t.Logf("latency p95      %s", pct(0.95))
	t.Logf("latency p99      %s", pct(0.99))
	t.Logf("latency max      %s", latencies[len(latencies)-1].Round(time.Millisecond))
	t.Logf("success rate     %.1f%% (%d completed, %d failed)",
		100*float64(completedRows)/float64(created), completedRows, failedRows)
	t.Logf("retry rate       %.1f%% (%d executions for %d tasks)",
		100*float64(executions-created)/float64(created), executions, created)
	t.Log("")

	if completedRows != created {
		t.Errorf("%d/%d tasks completed; queued=%d processing=%d failed=%d",
			completedRows, created, queued, processing, failedRows)
	}
	if queued != 0 || processing != 0 {
		t.Errorf("tasks left in a non-terminal state: queued=%d processing=%d", queued, processing)
	}
}

// settleTerminalStates waits for the bookkeeping behind the last completions.
// The handler reports done from inside the attempt; the loop then finishes the
// execution row and writes the task's terminal status. Counting before that
// settles reports a phantom stuck task.
func settleTerminalStates(t *testing.T, ctx context.Context, pool *pgxpool.Pool, org uuid.UUID, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var completed int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM tasks WHERE org_id = $1 AND status = 'completed'`, org).Scan(&completed); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if completed >= want || time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
