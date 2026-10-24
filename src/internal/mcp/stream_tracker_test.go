package mcp

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestStreamTracker_TrackAfterCancelAllReturnsAlreadyCancelledContext is the
// sk-303.14 regression test for the drain race codex found in cancelAll:
// the original implementation took a one-time snapshot of the tracked
// cancel funcs under the mutex, released the mutex, and only then invoked
// them. A GET stream that reaches track() after that snapshot was taken —
// but the tracker never remembers that draining started — is registered
// like any other in-flight stream and is never cancelled by this (the only)
// call to cancelAll. That reproduces the exact bug sk-303.16 fixed one
// level up: http.Server.Shutdown blocks on that stream until its own
// deadline.
//
// This test collapses the timing window to its simplest, fully
// deterministic form: call cancelAll() first (nothing tracked — exactly the
// "empty snapshot" case in codex's report), THEN track() a new stream. On
// the buggy tracker this reproduces a live, un-cancelled context (the bug);
// the fix must make any track() call that lands after draining has started
// return an already-cancelled context immediately, no timing race needed to
// observe it.
func TestStreamTracker_TrackAfterCancelAllReturnsAlreadyCancelledContext(t *testing.T) {
	tracker := newStreamTracker()

	// Simulate Shutdown's drainStreams already having run — the moment
	// after which no new stream should ever be able to outlive it.
	tracker.cancelAll()

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	tracked, release := tracker.track(req)
	defer release()

	select {
	case <-tracked.Context().Done():
		// Correct: registered after draining started, so it must never
		// have been handed a live context in the first place.
	default:
		t.Fatal("streamTracker.track() after cancelAll() returned a LIVE context — " +
			"a GET stream admitted after Shutdown started draining would never be " +
			"cancelled by this (the only) cancelAll call, and would block " +
			"http.Server.Shutdown until its own deadline (the exact sk-303.16 bug, " +
			"reintroduced by the drain race)")
	}
}

// TestStreamTracker_CancelAllRaceWithConcurrentTrack stresses the same
// invariant under real concurrency and -race: a burst of track() calls
// racing a single cancelAll() call must ALWAYS end with every returned
// context Done() once everything settles — regardless of which side of
// cancelAll's critical section each track() call happened to land on.
// go test -race must also find no data race on the tracker's internal
// map/flag.
func TestStreamTracker_CancelAllRaceWithConcurrentTrack(t *testing.T) {
	tracker := newStreamTracker()

	const n = 50
	results := make([]*http.Request, n)
	releases := make([]func(), n)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			tracked, release := tracker.track(req)
			results[i] = tracked
			releases[i] = release
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		tracker.cancelAll()
	}()

	close(start)
	wg.Wait()

	for i, r := range results {
		select {
		case <-r.Context().Done():
		default:
			t.Errorf("track() call #%d returned a context still live after cancelAll settled — "+
				"drain race not closed", i)
		}
		releases[i]()
	}
}
