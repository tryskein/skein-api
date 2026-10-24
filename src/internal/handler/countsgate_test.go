package handler

// countsgate_test.go — sk-622.21, T024, matrix row No. 49 (INV-11, US-16 «the
// gate», edge 15). Unit: the computation is substituted, the clock is the
// testing/synctest bubble's — no database, no live waiting.
//
// The rules, from K8: requests of one user arriving together or within the
// collection window share one computation and all get its result; two users
// get two computations, each its own; a request arriving while a computation
// is already running does not join it and gets a new one; after a batch
// ends nothing is kept — the next request computes again. The window is
// taken from the package (countsGateWindow), not as a literal: changing it
// does not break the test, a zero window does (the third request of
// OneUserOneComputation would then open a second batch).
//
// The limit of the row: the real computation, the user key on the route and
// the concurrent freshness are proved by No. 50.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
)

// gateCall is one request's outcome.
type gateCall struct {
	val      int
	computed bool
	err      error
}

// fakeCompute counts its runs; run n (1-based) returns base + n after
// taking `took` of bubble time.
type fakeCompute struct {
	runs atomic.Int32
	base int
	took time.Duration
}

func (f *fakeCompute) fn(context.Context) (int, error) {
	n := f.runs.Add(1)
	time.Sleep(f.took)
	return f.base + int(n), nil
}

// ask runs one request in its own goroutine and delivers its outcome.
func ask(g *countsGate[int], user uuid.UUID, f *fakeCompute) <-chan gateCall {
	out := make(chan gateCall, 1)
	go func() {
		v, c, err := g.Do(context.Background(), user, f.fn)
		out <- gateCall{v, c, err}
	}()
	return out
}

// Simultaneous and close (inside the window) requests of one user → one
// computation, everybody gets its result.
func TestCountsGate_OneUserOneComputation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		f := &fakeCompute{base: 100, took: time.Millisecond}
		u := uuid.New()

		first := ask(g, u, f)
		second := ask(g, u, f)
		synctest.Wait()
		time.Sleep(countsGateWindow / 2) // still inside the window
		third := ask(g, u, f)

		var got []gateCall
		for _, ch := range []<-chan gateCall{first, second, third} {
			got = append(got, <-ch)
		}
		if n := f.runs.Load(); n != 1 {
			t.Fatalf("computations = %d, want 1 for three requests inside the window", n)
		}
		computed := 0
		for i, c := range got {
			if c.err != nil || c.val != 101 {
				t.Errorf("request %d = %+v, want the batch's result 101", i+1, c)
			}
			if c.computed {
				computed++
			}
		}
		if computed != 1 {
			t.Errorf("%d requests report having opened the batch, want exactly 1", computed)
		}
	})
}

// Two users in the same moment → two computations, each gets its own.
func TestCountsGate_TwoUsersTwoComputations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		fu := &fakeCompute{base: 100}
		fv := &fakeCompute{base: 200}
		u, v := uuid.New(), uuid.New()

		cu := ask(g, u, fu)
		cv := ask(g, v, fv)
		ru, rv := <-cu, <-cv
		if ru.val != 101 || rv.val != 201 {
			t.Errorf("U got %d, V got %d; want 101 and 201 — each user its own computation", ru.val, rv.val)
		}
		if fu.runs.Load() != 1 || fv.runs.Load() != 1 {
			t.Errorf("computations U %d, V %d; want 1 and 1", fu.runs.Load(), fv.runs.Load())
		}
	})
}

// A request arriving while the computation is running does not join it;
// it gets a new computation.
func TestCountsGate_LateRequestDoesNotJoinARunningComputation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		f := &fakeCompute{base: 100, took: time.Second}
		u := uuid.New()

		early := ask(g, u, f)
		time.Sleep(countsGateWindow + time.Millisecond) // the window closed, the computation runs
		synctest.Wait()
		if n := f.runs.Load(); n != 1 {
			t.Fatalf("precondition: computations started = %d, want 1", n)
		}
		late := ask(g, u, f)

		re, rl := <-early, <-late
		if re.val != 101 {
			t.Errorf("early request = %d, want 101", re.val)
		}
		if rl.val != 102 || !rl.computed {
			t.Errorf("late request = %+v, want a new computation (102, computed)", rl)
		}
		if n := f.runs.Load(); n != 2 {
			t.Errorf("computations = %d, want 2", n)
		}
	})
}

// After a batch ends its result is not kept: the next request computes.
func TestCountsGate_NothingKeptAfterTheBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		f := &fakeCompute{base: 100}
		u := uuid.New()

		if r := <-ask(g, u, f); r.val != 101 {
			t.Fatalf("first = %d, want 101", r.val)
		}
		if r := <-ask(g, u, f); r.val != 102 || !r.computed {
			t.Errorf("next request after the batch = %+v, want a fresh computation (102)", r)
		}
	})
}

// A request whose context ends while waiting returns its error and does not
// fail the batch for the others.
func TestCountsGate_CancelledWaiterLeavesTheBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		f := &fakeCompute{base: 100}
		u := uuid.New()

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		var cancelled gateCall
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, c, err := g.Do(ctx, u, f.fn)
			cancelled = gateCall{v, c, err}
		}()
		other := ask(g, u, f)
		synctest.Wait()
		cancel()
		wg.Wait()
		if cancelled.err == nil {
			t.Errorf("cancelled request returned %+v, want its context error", cancelled)
		}
		if r := <-other; r.err != nil || r.val != 101 {
			t.Errorf("the other request of the batch = %+v, want 101", r)
		}
	})
}

// blockingCompute blocks until its context ends and reports how it ended —
// a computation stuck on the database (pool outage, lock wait).
type blockingCompute struct {
	ended chan error
}

func newBlockingCompute() *blockingCompute { return &blockingCompute{ended: make(chan error, 1)} }

func (b *blockingCompute) fn(ctx context.Context) (int, error) {
	<-ctx.Done()
	b.ended <- ctx.Err()
	return 0, ctx.Err()
}

// endedWith reports how the computation ended, or ok=false if it still runs.
func (b *blockingCompute) endedWith() (err error, ok bool) {
	select {
	case err := <-b.ended:
		return err, true
	default:
		return nil, false
	}
}

// The computation is detached from its
// waiters, so when every waiter has gone it must still end by itself — at
// its own deadline (countsComputeTimeout, taken from the package), not
// never.
func TestCountsGate_AbandonedComputationReachesItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		c := newBlockingCompute()
		u := uuid.New()

		ctx, cancel := context.WithCancel(context.Background())
		res := make(chan gateCall, 1)
		go func() {
			v, comp, err := g.Do(ctx, u, c.fn)
			res <- gateCall{v, comp, err}
		}()
		time.Sleep(countsGateWindow + time.Millisecond) // sealed, computing
		synctest.Wait()
		cancel()
		if r := <-res; !errors.Is(r.err, context.Canceled) {
			t.Fatalf("the only waiter = %+v, want its own cancellation", r)
		}
		synctest.Wait()
		if err, ok := c.endedWith(); ok {
			t.Fatalf("computation ended (%v) when its waiter left, want it detached from waiters", err)
		}

		time.Sleep(countsComputeTimeout)
		synctest.Wait()
		err, ok := c.endedWith()
		if !ok {
			t.Fatal("computation still running past countsComputeTimeout with no waiter left")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("computation ended with %v, want its deadline", err)
		}
	})
}

// Stopping the service (the lifetime the
// gate is bound to) cancels a running computation at once, the gate's done
// channel closes after it returned, its waiters are released with an error,
// and no new batch opens afterwards.
func TestCountsGate_ServiceStopCancelsRunningComputation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		life, stop := context.WithCancel(context.Background())
		done := g.bindLifetime(life)
		c := newBlockingCompute()
		u := uuid.New()

		waiter := make(chan gateCall, 1)
		go func() {
			v, comp, err := g.Do(context.Background(), u, c.fn)
			waiter <- gateCall{v, comp, err}
		}()
		time.Sleep(countsGateWindow + time.Millisecond) // sealed, computing
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("gate reported stopped while the service is running")
		default:
		}

		stop() // no bubble time passes: far before countsComputeTimeout
		synctest.Wait()
		err, ok := c.endedWith()
		if !ok {
			t.Fatal("computation still running after the service stopped")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("computation ended with %v, want cancellation by the service stop", err)
		}
		if r := <-waiter; r.err == nil {
			t.Errorf("waiter = %+v, want an error after its computation was cancelled", r)
		}
		select {
		case <-done:
		default:
			t.Fatal("gate done channel not closed after its computation returned")
		}
		if _, _, err := g.Do(context.Background(), u, c.fn); !errors.Is(err, errCountsGateStopped) {
			t.Errorf("request after the stop: err = %v, want errCountsGateStopped", err)
		}
	})
}

// A panic in the batch's computation
// does not kill the process and does not leave the waiters hanging — every
// waiter of the batch gets an error — and the gate keeps working.
func TestCountsGate_PanickingComputationReleasesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCountsGate[int](countsGateWindow)
		u := uuid.New()
		panicking := &fakeCompute{}
		boom := func(ctx context.Context) (int, error) {
			panicking.runs.Add(1)
			panic("boom")
		}

		results := make(chan gateCall, 2)
		for range 2 {
			go func() {
				v, comp, err := g.Do(context.Background(), u, boom)
				results <- gateCall{v, comp, err}
			}()
		}
		for i := range 2 {
			if r := <-results; !errors.Is(r.err, errCountsComputePanicked) {
				t.Errorf("waiter %d = %+v, want errCountsComputePanicked", i+1, r)
			}
		}
		if n := panicking.runs.Load(); n != 1 {
			t.Errorf("computations = %d, want 1 for one batch", n)
		}

		f := &fakeCompute{base: 100}
		if r := <-ask(g, u, f); r.err != nil || r.val != 101 {
			t.Errorf("request after the panicked batch = %+v, want a fresh computation (101)", r)
		}
	})
}
