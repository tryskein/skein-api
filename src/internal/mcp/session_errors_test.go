package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestJSONRPCSessionErrors_RewritesDeadSession404 is the sk-303.10 unit
// regression test for the middleware itself: mcp-go's POST handler
// (server/streamable_http.go handlePost) rejects a dead/unknown session
// with `http.Error(w, "<msg>", 404)` — a bare text/plain body an MCP-JSON-RPC
// client cannot parse as an answer to its call. jsonRPCSessionErrors must
// turn that into a JSON-RPC 2.0 error object while keeping the 404 status.
func TestJSONRPCSessionErrors_RewritesDeadSession404(t *testing.T) {
	cases := []struct {
		name        string
		upstreamMsg string
	}{
		{name: "invalid session id", upstreamMsg: "Invalid session ID"},
		{name: "terminated session", upstreamMsg: "Session terminated"},
		// handleSamplingResponse's own dead-session 404 (v0.50.0
		// streamable_http.go:848) — a different call site than handlePost's
		// but reached via the same POST /mcp path when the body looks like
		// a sampling response, so it must be recognized too.
		{name: "no active session (sampling response path)", upstreamMsg: "No active session found for the given session ID"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.upstreamMsg, http.StatusNotFound)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

			jsonRPCSessionErrors(next).ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d (404 must be preserved)", rec.Code, http.StatusNotFound)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			if rec.Header().Get("X-Content-Type-Options") != "" {
				t.Fatalf("X-Content-Type-Options leaked through from http.Error's text/plain response: %q",
					rec.Header().Get("X-Content-Type-Options"))
			}

			var body struct {
				JSONRPC string `json:"jsonrpc"`
				ID      any    `json:"id"`
				Error   struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Data    struct {
						Detail string `json:"detail"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, rec.Body.String())
			}
			if body.JSONRPC != "2.0" {
				t.Fatalf("jsonrpc = %q, want 2.0", body.JSONRPC)
			}
			if body.ID != nil {
				t.Fatalf("id = %v, want null (JSON-RPC 2.0 §5.1: null when no id could be detected)", body.ID)
			}
			if body.Error.Code != mcpSessionErrorCode {
				t.Fatalf("error.code = %d, want %d", body.Error.Code, mcpSessionErrorCode)
			}
			if body.Error.Data.Detail != tc.upstreamMsg {
				t.Fatalf("error.data.detail = %q, want %q (original mcp-go message must survive)",
					body.Error.Data.Detail, tc.upstreamMsg)
			}
		})
	}
}

// TestJSONRPCSessionErrors_OtherStatusesPassThrough proves the middleware
// only intervenes on 404 — any other response (success, or an unrelated
// error) reaches the client byte-for-byte unchanged.
func TestJSONRPCSessionErrors_OtherStatusesPassThrough(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "200 OK", status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{name: "400 bad content type", status: http.StatusBadRequest, body: "Invalid content type: must be 'application/json'"},
		{name: "202 accepted notification", status: http.StatusAccepted, body: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

			jsonRPCSessionErrors(next).ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q (untouched)", rec.Body.String(), tc.body)
			}
		})
	}
}

// TestJSONRPCSessionErrors_ScopedToPOST confirms the middleware never
// touches GET/DELETE responses, even a 404 — GET and DELETE don't produce
// this 404 shape in mcp-go, and the fix is deliberately POST-only (see
// Setup's doc comment, "Dead-session behaviour: POST vs GET").
func TestJSONRPCSessionErrors_ScopedToPOST(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "Invalid session ID", http.StatusNotFound)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/mcp", nil)

			jsonRPCSessionErrors(next).ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
			if ct := rec.Header().Get("Content-Type"); ct == "application/json" {
				t.Fatalf("Content-Type was rewritten to application/json for a %s request — middleware must be POST-only", method)
			}
		})
	}
}

// TestSetup_POSTDeadSession_EndToEnd drives an actual POST through the real
// Setup() handler chain (not just the middleware in isolation) with an
// Mcp-Session-Id the server has never seen, proving the wiring in Setup
// actually reaches jsonRPCSessionErrors.
func TestSetup_POSTDeadSession_EndToEnd(t *testing.T) {
	handler, _ := Setup(nil, 0, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", "session-that-was-never-registered")

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json\nbody: %s", ct, rec.Body.String())
	}

	var body struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	if body.Error.Code != mcpSessionErrorCode {
		t.Fatalf("error.code = %d, want %d", body.Error.Code, mcpSessionErrorCode)
	}
}

// TestSetup_GETUnknownSession_SilentlyOpensEmptySession is a characterization
// test, not an approval: it pins mcp-go v0.50.0's actual, documented-and-
// accepted behaviour (see Setup's "Dead-session behaviour: POST vs GET" doc
// comment for why we do NOT validate GET ourselves) so a future mcp-go
// bump that silently changes it — e.g. starts 404ing unknown GET session
// ids — is caught here instead of rediscovered the hard way.
func TestSetup_GETUnknownSession_SilentlyOpensEmptySession(t *testing.T) {
	handler, _ := Setup(nil, 0, nil)

	// handleGet blocks until the request context is done (that is exactly
	// sk-303.16's subject) — bound it so this test observes the response
	// mcp-go already sent (headers go out immediately, before the blocking
	// loop) and then returns instead of hanging forever.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	req.Header.Set("Mcp-Session-Id", "session-that-was-never-registered")

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d — if this changed, mcp-go now validates GET session ids "+
			"and the documented decision in Setup needs revisiting, not just this test",
			rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestSessionErrorRewriter_FlushDeliversPartialWrite is the sk-303.14
// regression test for the Flusher gap all three review engines found:
// sessionErrorRewriter embeds the bare http.ResponseWriter INTERFACE, which
// only promotes the three methods that interface itself declares
// (Header/Write/WriteHeader) — not Flush. mcp-go's handlePost
// (server/streamable_http.go v0.50.0, the notification-drain goroutine and
// the post-HandleMessage drain loop) type-asserts w.(http.Flusher) after
// every notification it pushes onto a POST response upgraded to SSE and
// flushes it immediately so the client sees it as it happens. With the
// assertion silently failing (no error, no log line — that's what makes
// this bug dangerous), the bytes sit in net/http's own internal buffer
// until the handler eventually returns instead of reaching the client when
// written.
//
// This drives a real *http.Server (not just an httptest.ResponseRecorder,
// which never round-trips Flush through an actual OS socket) through
// jsonRPCSessionErrors and proves the client observes the FIRST chunk
// before the handler unblocks to write the second — timing, not just
// eventual byte-for-byte equality, which the old wrapper would also
// satisfy once the handler finally returns and everything flushes at once.
func TestSessionErrorRewriter_FlushDeliversPartialWrite(t *testing.T) {
	unblock := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("chunk1\n")); err != nil {
			t.Errorf("writing chunk1: %v", err)
			return
		}
		f, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("http.ResponseWriter passed to the handler does not implement http.Flusher " +
				"(sessionErrorRewriter lost it)")
			return
		}
		f.Flush()
		<-unblock
		if _, err := w.Write([]byte("chunk2\n")); err != nil {
			t.Errorf("writing chunk2: %v", err)
		}
	})

	srv := httptest.NewServer(jsonRPCSessionErrors(next))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	type readResult struct {
		line string
		err  error
	}
	firstLine := make(chan readResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		firstLine <- readResult{line, err}
	}()

	select {
	case res := <-firstLine:
		if res.err != nil {
			t.Fatalf("reading flushed chunk1: %v", res.err)
		}
		if res.line != "chunk1\n" {
			t.Fatalf("first line = %q, want %q", res.line, "chunk1\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for chunk1 to arrive — Flush() did not reach the underlying " +
			"ResponseWriter, so the client never saw it until (at earliest) the handler returned")
	}

	close(unblock)

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading remainder: %v", err)
	}
	if string(rest) != "chunk2\n" {
		t.Fatalf("remainder = %q, want %q", rest, "chunk2\n")
	}
}

// TestSessionErrorRewriter_FlushIsNoopWhileIntercepting proves Flush does
// NOT leak the held-back 404 body to the underlying ResponseWriter early —
// flush() (lowercase, called once next.ServeHTTP has returned) is the only
// thing allowed to decide the final shape of an intercepted response. Uses
// an interface type assertion (not a direct method call on the concrete
// type) so this test fails as a normal assertion — not a package-wide
// compile error — on a wrapper that doesn't implement http.Flusher yet.
func TestSessionErrorRewriter_FlushIsNoopWhileIntercepting(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &sessionErrorRewriter{ResponseWriter: rec}

	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(http.StatusNotFound)
	if _, err := rw.Write([]byte("Session terminated\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	f, ok := interface{}(rw).(http.Flusher)
	if !ok {
		t.Fatal("sessionErrorRewriter does not implement http.Flusher")
	}
	f.Flush()

	if rec.Flushed {
		t.Fatal("Flush reached the underlying ResponseWriter while a 404 body was held back for rewriting")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("Flush leaked %d byte(s) of the held-back body before flush() rewrote it: %q",
			rec.Body.Len(), rec.Body.String())
	}
}

// TestJSONRPCSessionErrors_JSONNotFoundPassesThroughUnchanged is the
// sk-303.14 regression test for a review warning that the middleware
// rewrote ANY POST 404, not just mcp-go's known plain-text dead-session
// body. A hypothetical (or future) POST 404 with a JSON body must pass
// through byte-for-byte — rewriting it would embed that JSON as a string
// inside error.data.detail and mislabel it as a dead-session error.
func TestJSONRPCSessionErrors_JSONNotFoundPassesThroughUnchanged(t *testing.T) {
	const body = `{"error":"widget not found"}`
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	jsonRPCSessionErrors(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (must not be relabelled as the JSON-RPC error rewrite)", ct)
	}
	if rec.Body.String() != body {
		t.Fatalf("body = %q, want %q (untouched — not a known dead-session message)", rec.Body.String(), body)
	}
}

// TestJSONRPCSessionErrors_UnrelatedTextPlainNotFoundPassesThroughUnchanged
// proves a text/plain 404 whose body is NOT one of mcp-go's known
// dead-session messages also passes through untouched — the content-type
// check alone is not enough, the body has to actually match.
func TestJSONRPCSessionErrors_UnrelatedTextPlainNotFoundPassesThroughUnchanged(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "widget not found", http.StatusNotFound)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	jsonRPCSessionErrors(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8 (untouched)", ct)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), "widget not found"; got != want {
		t.Fatalf("body = %q, want %q (untouched — not a known dead-session message)", got, want)
	}
}
