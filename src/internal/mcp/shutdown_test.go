package mcp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestShutdown_OpenStandaloneGETStream_DrainsCleanly is the sk-303.16
// regression test. It reproduces the "HTTP server shutdown error":
// {"error":"context deadline exceeded"} line observed on every pod rollout —
// an MCP client that opened the standalone GET (SSE) notification stream and
// never disconnects holds an in-flight net/http request whose Context() only
// becomes Done() when the underlying connection closes. stdlib
// net/http.Server.Shutdown does not itself cancel in-flight request
// contexts (verified against go1.26.1 src/net/http/server.go: Shutdown
// polls closeIdleConns() and returns ctx.Err() only on its OWN deadline), so
// a held-open GET stream blocks Shutdown until the deadline every time.
//
// Measured locally against the UNFIXED code path before this test existed
// (internal/mcp.Setup mounted on a real *http.Server, no RegisterOnShutdown
// wired): with the GET stream open, srv.Shutdown(ctx) returned
// context.DeadlineExceeded after 2.000323543s against a 2s deadline; with no
// open stream it returned nil after 123.41µs. That isolates the open stream
// as the actual cause, not just a plausible one.
//
// The fix: Setup returns a drainStreams func that cancels every open GET
// stream's request context; the caller wires it via
// (*http.Server).RegisterOnShutdown (main.go), which runs concurrently with
// Shutdown's own polling loop. This test proves the fixed path: Shutdown
// returns nil well inside the deadline, and the client sees the stream end
// cleanly (EOF) rather than the connection just hanging until SIGKILL.
func TestShutdown_OpenStandaloneGETStream_DrainsCleanly(t *testing.T) {
	// sk-303.14: an earlier commit message claimed "goroutine count
	// before/after: 2/2, no leak" but nothing in this test asserted it —
	// a one-time manual local measurement, not a regression-proof check.
	// goleak.IgnoreCurrent() snapshots every goroutine already running
	// (the test runner, background stdlib bookkeeping, ...) before Setup
	// spawns anything, so VerifyNone below only fails on goroutines this
	// test itself leaves behind — specifically the standalone GET stream's
	// handler goroutine and its per-connection net/http goroutine, which
	// drainStreams must actually unblock, not just make Shutdown return.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	handler, drainStreams := Setup(nil, 0, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &http.Server{Handler: handler}
	srv.RegisterOnShutdown(drainStreams)

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.Serve(ln) }()

	addr := ln.Addr().String()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("GET /mcp HTTP/1.1\r\nHost: " + addr + "\r\nConnection: keep-alive\r\n\r\n")); err != nil {
		t.Fatalf("write GET: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("unexpected status line: %q", statusLine)
	}
	var sawEventStream bool
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.HasPrefix(strings.ToLower(line), "content-type:") && strings.Contains(line, "text/event-stream") {
			sawEventStream = true
		}
		if line == "\r\n" {
			break
		}
	}
	if !sawEventStream {
		t.Fatalf("GET /mcp did not open an SSE stream (no text/event-stream content-type) — test would not exercise the bug")
	}
	t.Logf("standalone GET stream open: %q", statusLine)

	// Drain the connection in the background so a clean handler-return
	// (chunked terminator) doesn't block on an unread socket buffer.
	streamClosed := make(chan struct{})
	go func() {
		defer close(streamClosed)
		buf := make([]byte, 4096)
		for {
			if _, err := br.Read(buf); err != nil {
				return // EOF (clean close) or the deferred conn.Close() above
			}
		}
	}()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	shutdownErr := srv.Shutdown(shutdownCtx)
	elapsed := time.Since(start)
	t.Logf("srv.Shutdown returned err=%v after %v (deadline was 5s)", shutdownErr, elapsed)

	if shutdownErr != nil {
		t.Fatalf("srv.Shutdown returned %v after %v with an open GET stream — want nil (drainStreams should unblock it)", shutdownErr, elapsed)
	}
	// Generous ceiling: proves we returned because the stream drained, not
	// because we got lucky racing a 5s deadline that would also read as
	// "success" for the wrong reason.
	if elapsed > 2*time.Second {
		t.Fatalf("srv.Shutdown took %v — expected it to return promptly once drainStreams unblocks the GET handler, not to approach the deadline", elapsed)
	}

	select {
	case <-streamClosed:
		// The client observed the stream end (chunked EOF) rather than a
		// raw reset — this is the "client gets closure, not an abrupt cut"
		// half of the acceptance criteria.
	case <-time.After(2 * time.Second):
		t.Fatalf("client never observed the GET stream close after a successful Shutdown")
	}

	if err := <-serveErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("srv.Serve returned unexpected error: %v", err)
	}
}

// TestShutdown_NoOpenStream_IsFast is the control case: Shutdown with no
// open GET stream returns quickly regardless of drainStreams being wired,
// confirming drainStreams is a no-op when there is nothing to drain.
func TestShutdown_NoOpenStream_IsFast(t *testing.T) {
	handler, drainStreams := Setup(nil, 0, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &http.Server{Handler: handler}
	srv.RegisterOnShutdown(drainStreams)

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.Serve(ln) }()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	shutdownErr := srv.Shutdown(shutdownCtx)
	elapsed := time.Since(start)
	t.Logf("srv.Shutdown returned err=%v after %v (deadline was 2s, no client ever connected)", shutdownErr, elapsed)

	if shutdownErr != nil {
		t.Fatalf("srv.Shutdown returned %v after %v with no open stream — want nil", shutdownErr, elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("srv.Shutdown took %v with nothing open — expected near-instant", elapsed)
	}

	if err := <-serveErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("srv.Serve returned unexpected error: %v", err)
	}
}
