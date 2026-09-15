package worker

import (
	"context"
	"time"
)

// RunWithLease runs h under a maximum-processing ceiling while emitting periodic
// liveness heartbeats.
//
// heartbeat is the broker "still working" signal (e.g. nats.Msg.InProgress). It
// is called every interval while h runs, extending the message's ack deadline so
// the broker does not redeliver work that is still in flight. When h returns — or
// when the ceiling elapses — heartbeats stop, so a crashed or hung worker releases
// the message after at most one short ack-wait instead of holding it for a long
// static timeout. This is why a modest AckWait + heartbeats beats a giant AckWait:
// the slow-but-healthy case keeps its lease, the crashed case is released quickly.
//
// The ceiling is enforced two ways: the context passed to h is cancelled, AND
// RunWithLease returns when the ceiling elapses even if h has not. h runs in its
// own goroutine, so a handler that ignores ctx cannot pin the caller (or its
// worker slot) — it keeps running detached until it finishes, but the caller is
// freed at the ceiling and the message is released. A well-behaved stage (one that
// passes ctx to exec.CommandContext, or chunks its work and polls ctx) is also
// cancelled promptly. The only residue of an uncooperative handler is a detached
// goroutine plus a possible duplicate delivery — which the at-least-once contract
// already requires handlers to tolerate.
//
// interval <= 0 disables heartbeats; ceiling <= 0 disables the timeout (h runs under
// the parent ctx). RunWithLease blocks until h returns or the ceiling fires,
// whichever comes first, and returns h's result or the context error respectively.
func RunWithLease(
	ctx context.Context,
	h func(context.Context) error,
	heartbeat func() error,
	interval, ceiling time.Duration,
) error {
	runCtx := ctx
	if ceiling > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, ceiling)
		defer cancel()
	}

	if interval > 0 && heartbeat != nil {
		stop := make(chan struct{})
		stopped := make(chan struct{})
		// Stop heartbeats and wait for the ticker goroutine to fully exit before
		// returning, so no heartbeat can fire once RunWithLease has returned (and
		// no goroutine is left running against the message we are about to settle).
		defer func() { close(stop); <-stopped }()

		go func() {
			defer close(stopped)
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done(): // ceiling fired or caller returned
					return
				case <-stop: // h returned
					return
				case <-t.C:
					_ = heartbeat()
				}
			}
		}()
	}

	// Run h detached so the ceiling can free the caller even when h ignores ctx.
	// The channel is buffered so the detached goroutine never blocks on send after
	// we have already returned via runCtx.Done().
	result := make(chan error, 1)
	go func() { result <- h(runCtx) }()

	select {
	case err := <-result:
		return err
	case <-runCtx.Done():
		return runCtx.Err()
	}
}
