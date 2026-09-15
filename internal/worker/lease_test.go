package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunWithLease_ReturnsHandlerResult(t *testing.T) {
	want := errors.New("boom")
	got := RunWithLease(context.Background(),
		func(ctx context.Context) error { return want },
		func() error { return nil },
		10*time.Millisecond, time.Second)
	if !errors.Is(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestRunWithLease_HeartbeatsWhileRunning(t *testing.T) {
	var beats int32
	err := RunWithLease(context.Background(),
		func(ctx context.Context) error {
			time.Sleep(55 * time.Millisecond)
			return nil
		},
		func() error { atomic.AddInt32(&beats, 1); return nil },
		10*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n := atomic.LoadInt32(&beats); n < 3 {
		t.Fatalf("expected several heartbeats, got %d", n)
	}
}

func TestRunWithLease_NoHeartbeatForFastHandler(t *testing.T) {
	var beats int32
	_ = RunWithLease(context.Background(),
		func(ctx context.Context) error { return nil },
		func() error { atomic.AddInt32(&beats, 1); return nil },
		50*time.Millisecond, time.Second)
	// Give any stray heartbeat a chance to (incorrectly) fire.
	time.Sleep(70 * time.Millisecond)
	if n := atomic.LoadInt32(&beats); n != 0 {
		t.Fatalf("expected no heartbeats for fast handler, got %d", n)
	}
}

func TestRunWithLease_CeilingCancelsHandler(t *testing.T) {
	start := time.Now()
	err := RunWithLease(context.Background(),
		func(ctx context.Context) error {
			<-ctx.Done() // well-behaved: respects cancellation
			return ctx.Err()
		},
		func() error { return nil },
		10*time.Millisecond, 40*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("ceiling did not fire promptly: %v", elapsed)
	}
}

func TestRunWithLease_ReturnsAtCeilingForHungHandler(t *testing.T) {
	// A handler that ignores ctx entirely. With the old blocking implementation
	// this wedges the caller forever; the ceiling must free us regardless.
	blocked := make(chan struct{})
	defer close(blocked) // let the detached goroutine exit cleanly after the test

	done := make(chan error, 1)
	go func() {
		done <- RunWithLease(context.Background(),
			func(ctx context.Context) error {
				<-blocked // never polls ctx
				return nil
			},
			func() error { return nil },
			10*time.Millisecond, 40*time.Millisecond)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline exceeded at ceiling, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithLease never returned after its ceiling: caller is wedged")
	}
}

func TestRunWithLease_HeartbeatStopsAfterCeiling(t *testing.T) {
	var beats int32
	_ = RunWithLease(context.Background(),
		func(ctx context.Context) error {
			<-ctx.Done()
			// Keep "running" past the ceiling; heartbeats must already have stopped.
			time.Sleep(60 * time.Millisecond)
			return ctx.Err()
		},
		func() error { atomic.AddInt32(&beats, 1); return nil },
		10*time.Millisecond, 30*time.Millisecond)
	before := atomic.LoadInt32(&beats)
	time.Sleep(60 * time.Millisecond)
	after := atomic.LoadInt32(&beats)
	if after != before {
		t.Fatalf("heartbeats continued after ceiling: before=%d after=%d", before, after)
	}
}
