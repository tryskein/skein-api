package main

// teardown_test.go — sk-353 regression coverage.
//
// Before sk-353, main() registered the NATS-drain wait and the three
// runner awaits as TWO separate `defer`s. None of those four waits reads
// another's done channel — they are genuinely independent — but Go's LIFO
// defer ordering made them run SEQUENTIALLY anyway: the NATS-drain defer
// ran to completion (its own ≤10s ceiling) before the three-runner-await
// defer even started (its own ≤10s ceiling, applied three times in a row).
// Worst case was their SUM, ≤40s, stacked on shutdownTimeout's own 25s for
// a theoretical total of ≤65s.
//
// This test pins the fix: runTeardown fans all four legs out on one
// sync.WaitGroup, so the combined wait is bounded by whichever ONE leg
// takes longest, not by their sum. It exercises the REAL awaitRunner and a
// REAL *nats.Conn (an embedded in-process NATS server, same pattern as
// internal/outbox/dispatcher_test.go's startEmbeddedNATS) — no fakes for
// the pieces that matter to the timing claim.

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATS launches a real, ephemeral in-process NATS server and
// returns a connected client. Mirrors internal/outbox/dispatcher_test.go's
// helper of the same name (unexported, package-local — no shared helper
// package exists between cmd/api and internal/outbox, and one file's worth
// of setup isn't worth introducing one for).
func startEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1 // pick a random free port
	opts.NoLog = true
	s := natstest.RunServer(&opts)
	t.Cleanup(s.Shutdown)

	nc, err := nats.Connect(s.ClientURL(), nats.Name("teardown-test"))
	if err != nil {
		t.Fatalf("connect embedded nats: %v", err)
	}
	t.Cleanup(func() {
		if !nc.IsClosed() {
			_ = nc.Drain()
		}
	})
	return nc
}

// TestRunTeardown_LegsRunConcurrentlyNotSequentially is the core sk-353
// regression: all four legs — push runner, cleanup runner, nudge
// scheduler, NATS drain — are made to "hang" (their done channels, and
// natsClosed, are never closed) so each one is guaranteed to ride its own
// ceiling out to the end. runnerShutdownTimeout and natsDrainTimeout are
// lowered for the duration of the test (same technique
// TestAwaitRunner_GivesUpAtTheCeilingAndSaysSo already uses — see that
// test's doc for why a package var, and why safe only absent
// t.Parallel() in this package).
//
// A SEQUENTIAL (pre-fix) shape would take ~4×ceiling. The fix takes
// ~1×ceiling. The assertion sits well below 4× and well above 1× so it
// fails hard on a sequential regression without being flaky about
// scheduler jitter on a healthy (parallel) run.
func TestRunTeardown_LegsRunConcurrentlyNotSequentially(t *testing.T) {
	prevRunner, prevNATS := runnerShutdownTimeout, natsDrainTimeout
	const ceiling = 150 * time.Millisecond
	runnerShutdownTimeout = ceiling
	natsDrainTimeout = ceiling
	t.Cleanup(func() {
		runnerShutdownTimeout = prevRunner
		natsDrainTimeout = prevNATS
	})

	nc := startEmbeddedNATS(t)

	var stopCalls int32
	stopWorker := func() { atomic.AddInt32(&stopCalls, 1) }

	// Every done channel below is deliberately NEVER closed: each leg must
	// ride its own ceiling out to prove the ceilings still apply
	// individually, not just that SOME wait eventually returns.
	pushDone := make(chan struct{})
	cleanupDone := make(chan struct{})
	nudgeDone := make(chan struct{})
	natsClosed := make(chan struct{})

	dispatcherDone := make(chan struct{})
	close(dispatcherDone) // no pending publishes — proceed straight to Drain

	start := time.Now()
	runTeardown(teardownDeps{
		stopWorker:     stopWorker,
		pushRunnerDone: pushDone,
		cleanupDone:    cleanupDone,
		nudgeDone:      nudgeDone,
		nc:             nc,
		dispatcherDone: dispatcherDone,
		natsClosed:     natsClosed,
	})
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&stopCalls); got != 1 {
		t.Errorf("stopWorker called %d times, want exactly 1", got)
	}

	// Sequential (pre-fix) worst case: ~4×150ms=600ms. Concurrent (fixed)
	// worst case: ~150ms. 2×ceiling (300ms) sits well clear of both,
	// giving real headroom for scheduler jitter in CI without hiding a
	// regression back to sequential waits.
	if max := 2 * ceiling; elapsed > max {
		t.Errorf("runTeardown took %v (ceiling=%v) — legs appear to be waiting "+
			"sequentially rather than concurrently: want well under %v (a "+
			"sequential wait over 4 legs would take close to %v)",
			elapsed, ceiling, max, 4*ceiling)
	}
}

// TestRunTeardown_NoNATS_WaitsOnlyOnRunners pins the nc==nil path: no NATS
// leg is started at all (NATS_URL unset / connectNATS failed), so the
// combined wait is bounded by the three (still-hanging) runner legs alone
// — proving the NATS leg's absence doesn't change the runner legs'
// fan-out, and that runTeardown doesn't block forever on a nil nc.
func TestRunTeardown_NoNATS_WaitsOnlyOnRunners(t *testing.T) {
	prevRunner := runnerShutdownTimeout
	const ceiling = 100 * time.Millisecond
	runnerShutdownTimeout = ceiling
	t.Cleanup(func() { runnerShutdownTimeout = prevRunner })

	pushDone := make(chan struct{})
	cleanupDone := make(chan struct{})
	nudgeDone := make(chan struct{})

	start := time.Now()
	runTeardown(teardownDeps{
		stopWorker:     func() {},
		pushRunnerDone: pushDone,
		cleanupDone:    cleanupDone,
		nudgeDone:      nudgeDone,
		nc:             nil, // NATS disabled entirely
	})
	elapsed := time.Since(start)

	if max := 2 * ceiling; elapsed > max {
		t.Errorf("runTeardown (nc=nil) took %v, want well under %v", elapsed, max)
	}
}

// TestRunTeardown_AllLegsFinishEarly_ReturnsPromptly is the happy-path
// control: when every leg finishes well before its ceiling, runTeardown
// must return promptly (not wait out the ceiling regardless of outcome).
// This is what the real measurement in shutdownTimeout's doc comment
// (~8ms end-to-end against a live Postgres+NATS) exercises in production;
// here it's pinned deterministically without any external process.
func TestRunTeardown_AllLegsFinishEarly_ReturnsPromptly(t *testing.T) {
	prevRunner, prevNATS := runnerShutdownTimeout, natsDrainTimeout
	runnerShutdownTimeout = 5 * time.Second
	natsDrainTimeout = 5 * time.Second
	t.Cleanup(func() {
		runnerShutdownTimeout = prevRunner
		natsDrainTimeout = prevNATS
	})

	nc := startEmbeddedNATS(t)

	pushDone := make(chan struct{})
	cleanupDone := make(chan struct{})
	nudgeDone := make(chan struct{})
	natsClosed := make(chan struct{})
	dispatcherDone := make(chan struct{})
	close(dispatcherDone)

	// Every runner "exits" almost immediately, same as a healthy shutdown.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(pushDone)
		close(cleanupDone)
		close(nudgeDone)
		close(natsClosed)
	}()
	wg.Wait()

	start := time.Now()
	runTeardown(teardownDeps{
		stopWorker:     func() {},
		pushRunnerDone: pushDone,
		cleanupDone:    cleanupDone,
		nudgeDone:      nudgeDone,
		nc:             nc,
		dispatcherDone: dispatcherDone,
		natsClosed:     natsClosed,
	})
	elapsed := time.Since(start)

	// Generous bound (both ceilings are 5s above): a healthy return should
	// take low milliseconds, not seconds. 1s catches a regression back to
	// "always wait out the ceiling" without being flaky about CI jitter.
	if elapsed > time.Second {
		t.Errorf("runTeardown with every leg already done took %v, want well under 1s "+
			"(ceilings are 5s — a return anywhere near that means the ceiling always "+
			"fires instead of returning as soon as every leg finishes)", elapsed)
	}
}

// TestRunTeardown_DispatcherNeverStops_ReturnsWithinCeiling is the sk-353
// round-2 regression (codex + grok review, 2026-09-05): before this fix,
// the NATS leg's wait on dispatcherDone — `<-d.dispatcherDone` — had NO
// ceiling at all. A dispatcher that somehow missed workerCtx's
// cancellation (future regression, a stuck DB call, whatever) hung that
// one goroutine, and therefore wg.Wait(), forever: on the happy path the
// process never exits; on the srv.Shutdown-error branch main() never
// reaches os.Exit(1) either, since runTeardownOnce() is called
// synchronously before it.
//
// dispatcherDone is deliberately NEVER closed here, so this test would
// hang (and fail via its own outer timeout) if the bare-channel-receive
// regression ever came back. It also pins the timeout-branch POLICY this
// fix chose: nc.Drain() must NOT be called once the ceiling fires — the
// dispatcher might still be mid-Publish, and every outbox event is
// durably recorded in Postgres before the dispatcher ever calls Publish
// (see outbox.New's doc comment), so skipping Drain here costs nothing
// durability cares about, and keeps this leg's own worst case at exactly
// natsDrainTimeout instead of natsDrainTimeout again on top.
func TestRunTeardown_DispatcherNeverStops_ReturnsWithinCeiling(t *testing.T) {
	logs := captureLogs(t)

	prevRunner, prevNATS := runnerShutdownTimeout, natsDrainTimeout
	const ceiling = 150 * time.Millisecond
	runnerShutdownTimeout = ceiling
	natsDrainTimeout = ceiling
	t.Cleanup(func() {
		runnerShutdownTimeout = prevRunner
		natsDrainTimeout = prevNATS
	})

	nc := startEmbeddedNATS(t)

	// The three runner legs finish immediately — only the NATS leg's
	// dispatcherDone wait is under test here.
	pushDone := make(chan struct{})
	cleanupDone := make(chan struct{})
	nudgeDone := make(chan struct{})
	close(pushDone)
	close(cleanupDone)
	close(nudgeDone)

	natsClosed := make(chan struct{})     // never closed either — must not matter
	dispatcherDone := make(chan struct{}) // deliberately NEVER closed

	returned := make(chan struct{})
	var elapsed time.Duration
	go func() {
		defer close(returned)
		start := time.Now()
		runTeardown(teardownDeps{
			stopWorker:     func() {},
			pushRunnerDone: pushDone,
			cleanupDone:    cleanupDone,
			nudgeDone:      nudgeDone,
			nc:             nc,
			dispatcherDone: dispatcherDone,
			natsClosed:     natsClosed,
		})
		elapsed = time.Since(start)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("runTeardown did not return within five seconds while dispatcherDone was " +
			"never closed (ceiling=150ms): the wait on it has no ceiling. A dispatcher " +
			"that misses workerCtx's cancellation would hang the whole shutdown teardown " +
			"forever, and on the srv.Shutdown-error path main() would never reach " +
			"os.Exit(1) either")
	}

	// Bounded at ~1×ceiling, NOT 2×ceiling: a regression back to two
	// independent fresh ceilings (dispatcherDone-wait, then a fresh
	// natsClosed-wait) would take close to 2×ceiling here, since
	// natsClosed is also never closed.
	if max := 2 * ceiling; elapsed > max {
		t.Errorf("runTeardown took %v with an un-closing dispatcherDone (ceiling=%v), "+
			"want well under %v (a value near 2×ceiling means dispatcherDone and "+
			"natsClosed are being waited on with two INDEPENDENT ceilings instead of "+
			"one shared deadline)", elapsed, ceiling, max)
	}

	if nc.IsDraining() {
		t.Error("nc started draining even though the outbox dispatcher never reported " +
			"stopping — Drain must not be attempted while the dispatcher might still be " +
			"mid-Publish")
	}

	if s := logs(); !strings.Contains(s, "outbox dispatcher did not stop within the shutdown ceiling") {
		t.Errorf("timeout on dispatcherDone was not logged:\n%s\nthat is the only "+
			"external evidence an incident review would have that the dispatcher missed "+
			"cancellation", s)
	}
}
