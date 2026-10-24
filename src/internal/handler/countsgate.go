package handler

// countsgate.go — sk-622.21, T024 (US-16, INV-11, decision K8): the «gate
// with a batch» in front of the header counts.
//
// The tabs of one person ask for /me/notification-counts together — a
// personal signal reaches every tab at once, and each re-reads. The gate
// lets the requests of ONE user that arrive close together share ONE
// computation on this replica, without a cache:
//
//   - the first request of a user opens a batch; the batch collects for
//     countsGateWindow; every request of the same user arriving meanwhile
//     joins it;
//   - when the window closes the batch is sealed — removed from the gate, so
//     a request arriving from now on opens a NEW batch — and only then is
//     the computation started. Every request of the batch therefore arrived
//     BEFORE the computation began, so the computation sees every change
//     committed before any of them was sent — in particular every change a
//     personal signal has already told the client about (INV-11), on any
//     replica, with no state shared between replicas;
//   - the result goes to the batch's requests and is then dropped: nothing
//     is stored, so a late computation cannot overwrite a fresher one, and
//     the next request after the batch computes again.
//
// A request never joins a computation that has already started — that is
// the difference from singleflight, which would hand a caller a result read
// before the caller's request existed.
//
// Bounds of a computation (found in review).
// The computation is detached from the request that opened the batch (a tab
// that goes away must not fail the others), so nothing on the request side
// bounds it. It is bounded on its own:
//
//   - by countsComputeTimeout — a computation that has not finished by then
//     is cancelled, so a pool outage or a lock wait cannot accumulate orphan
//     computations, each holding a pool waiter or a connection;
//   - by the service lifetime — bindLifetime ties the gate to the context of
//     the service's workers: when it is cancelled at shutdown every running
//     computation is cancelled, no new batch is opened, and the returned
//     channel closes once every computation has returned (main awaits it in
//     runTeardown with the other workers);
//   - a panic in the computation is recovered in the batch's goroutine —
//     outside the request goroutine chi's Recoverer does not reach it and it
//     would kill the process — logged with its stack, and handed to every
//     waiter as an error, so no waiter hangs on an unclosed batch.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
)

// countsGateWindow is how long a batch collects requests before it computes.
// Tens of milliseconds: long enough for the re-reads that one personal
// signal triggers in several tabs to land in one batch, short enough not to
// be felt in a header refresh.
const countsGateWindow = 30 * time.Millisecond

// countsComputeTimeout is the deadline of one batch's computation, counted
// from its start (after the collection window). The computation is one SQL
// statement over indexed, caller-scoped rows (computeCounts); a healthy one
// takes milliseconds, so 10 s is reached only when the database or the pool
// is not answering. It is also well inside the server's shutdown budget, so a
// request waiting on a stuck computation does not hold srv.Shutdown up to its
// own ceiling.
const countsComputeTimeout = 10 * time.Second

var (
	// errCountsGateStopped answers a request that arrives after the
	// service lifetime the gate is bound to has ended.
	errCountsGateStopped = errors.New("counts gate: service is shutting down")
	// errCountsComputePanicked is what the waiters of a batch whose
	// computation panicked receive.
	errCountsComputePanicked = errors.New("counts gate: computation panicked")
)

// countsGate groups the computations of one value per user. T is the value
// computed; compute is given per request but runs once per batch (the
// requests of one user ask for the same thing).
type countsGate[T any] struct {
	window  time.Duration
	timeout time.Duration

	// life is cancelled when the bound service lifetime ends; every
	// computation's context is cancelled with it.
	life     context.Context
	stopLife context.CancelFunc

	mu      sync.Mutex
	open    map[uuid.UUID]*countsBatch[T]
	stopped bool
	// running counts the batches between opening and return of their
	// goroutine. Add happens under mu and only while !stopped, Wait only
	// after stopped is set under mu — so no Add races the Wait.
	running sync.WaitGroup
}

// countsBatch is one batch: closed `done` publishes res/err to its waiters.
type countsBatch[T any] struct {
	done chan struct{}
	res  T
	err  error
}

func newCountsGate[T any](window time.Duration) *countsGate[T] {
	life, stop := context.WithCancel(context.Background())
	return &countsGate[T]{
		window:   window,
		timeout:  countsComputeTimeout,
		life:     life,
		stopLife: stop,
		open:     map[uuid.UUID]*countsBatch[T]{},
	}
}

// bindLifetime ties the gate to the service lifetime ctx: once ctx ends, no
// batch is opened any more, every running computation is cancelled, and the
// returned channel closes after every batch goroutine has returned. Call it
// once; until it is called the gate lives as long as the process.
func (g *countsGate[T]) bindLifetime(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		g.mu.Lock()
		g.stopped = true
		g.mu.Unlock()
		g.stopLife()
		g.running.Wait()
		close(done)
	}()
	return done
}

// Do returns the value for userID computed by the batch the request joins
// (or opens), and whether the request opened it (computed=true) or joined
// one already collecting (computed=false).
//
// The batch's computation runs on a context detached from the cancellation
// of the request that opened it (its values are kept), so a tab that goes
// away does not fail the other tabs of the batch; it is bounded by
// countsComputeTimeout and the service lifetime instead (see the file
// header). A request whose own context ends while waiting returns that
// context's error. After the service lifetime has ended Do returns
// errCountsGateStopped.
func (g *countsGate[T]) Do(ctx context.Context, userID uuid.UUID, compute func(context.Context) (T, error)) (res T, computed bool, err error) {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		var zero T
		return zero, false, errCountsGateStopped
	}
	b, joined := g.open[userID]
	if !joined {
		b = &countsBatch[T]{done: make(chan struct{})}
		g.open[userID] = b
		g.running.Add(1)
		go g.run(context.WithoutCancel(ctx), userID, b, compute)
	}
	g.mu.Unlock()

	select {
	case <-b.done:
		return b.res, !joined, b.err
	case <-ctx.Done():
		var zero T
		return zero, !joined, ctx.Err()
	}
}

// run collects for the window, seals the batch, then computes under the
// computation deadline and the service lifetime.
func (g *countsGate[T]) run(detached context.Context, userID uuid.UUID, b *countsBatch[T], compute func(context.Context) (T, error)) {
	// Deferred in reverse: the recovered panic becomes b.err, THEN the
	// waiters are released, THEN the batch stops counting as running.
	defer g.running.Done()
	defer close(b.done)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("notification counts: batch computation panicked",
				"userId", userID.String(),
				"panic", fmt.Sprint(r),
				"stackTrace", string(debug.Stack()))
			var zero T
			b.res, b.err = zero, fmt.Errorf("%w: %v", errCountsComputePanicked, r)
		}
	}()

	collect := time.NewTimer(g.window)
	defer collect.Stop()
	select {
	case <-collect.C:
	case <-g.life.Done():
	}
	g.mu.Lock()
	delete(g.open, userID)
	g.mu.Unlock()
	if err := g.life.Err(); err != nil {
		b.err = fmt.Errorf("%w: %w", errCountsGateStopped, err)
		return
	}

	ctx, cancel := context.WithTimeout(detached, g.timeout)
	defer cancel()
	stopOnShutdown := context.AfterFunc(g.life, cancel)
	defer stopOnShutdown()
	b.res, b.err = compute(ctx)
}
