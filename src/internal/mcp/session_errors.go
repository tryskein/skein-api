package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// mcpSessionErrorCode is the JSON-RPC error code jsonRPCSessionErrors uses
// for "the Mcp-Session-Id on this request does not identify a usable
// session" — unknown, malformed, or explicitly terminated. It sits in the
// JSON-RPC 2.0 spec's "server error" reserved range (-32000..-32099, §5.1)
// and is not one of mcp-go's own assigned codes — checked against
// github.com/mark3labs/mcp-go/mcp@v0.50.0 mcp/types.go: PARSE_ERROR -32700,
// INVALID_REQUEST -32600, METHOD_NOT_FOUND -32601, INVALID_PARAMS -32602,
// INTERNAL_ERROR -32603, RESOURCE_NOT_FOUND -32002,
// URL_ELICITATION_REQUIRED -32042, REQUEST_INTERRUPTED -32800 — so a client
// that wants to special-case a dead session can match on this without
// colliding with the library's own set.
const mcpSessionErrorCode = -32001

// jsonRPCSessionErrors rewrites the dead-session 404s mcp-go's POST handler
// produces — server/streamable_http.go handlePost, "Invalid session ID" /
// "Session terminated" (verified against v0.50.0), plus the same two
// messages and "No active session found for the given session ID" from
// handleSamplingResponse (handlePost dispatches into it for a POST body
// shaped like a sampling response) — from a bare text/plain http.Error body
// into a JSON-RPC 2.0 error object, so an MCP client parses the failure as
// an answer to its call instead of an opaque transport error (sk-303.10).
//
// The 404 STATUS is kept on purpose — it is the correct HTTP-level signal
// ("this session no longer exists on this server") and anything keying off
// status codes (a proxy, a metrics label) keeps working unchanged. Only the
// BODY shape changes. The original http.Error message text survives in
// error.data.detail so the "invalid id" vs "terminated" vs "no active
// session" distinction is not lost.
//
// Response id is always null: at the point mcp-go rejects the session it
// has not parsed a JSON-RPC id from the body either — mcp-go's own
// PARSE_ERROR path takes the identical shortcut (handlePost,
// s.writeJSONRPCError(w, nil, mcp.PARSE_ERROR, ...)) — and JSON-RPC 2.0
// §5.1 requires id = null precisely when "there was an error in detecting
// the id in the Request object".
//
// Scoped to POST only. mcp-go's GET and DELETE handlers don't produce this
// 404 at all — see Setup's doc comment ("Dead-session behaviour: POST vs
// GET") for the deliberate decision on GET.
//
// Scoped narrower than "any POST 404", too (sk-303.14 hardening): a 404 is
// only ever a REWRITE candidate when it is also text/plain (what
// http.Error always sets) — see sessionErrorRewriter.WriteHeader — and even
// then flush() only rewrites it if the body is verbatim one of
// knownDeadSessionMessages. Anything else — a JSON 404 from some future
// handler, a text/plain 404 that says something we don't recognize — is
// forwarded byte-for-byte, headers included, untouched. Without this a
// route that starts returning an unrelated 404 on POST would be silently
// mislabelled as a session error.
func jsonRPCSessionErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		rw := &sessionErrorRewriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		rw.flush()
	})
}

// knownDeadSessionMessages is the exact, closed set of text/plain
// http.Error bodies mcp-go v0.50.0's POST path (server/streamable_http.go)
// uses for "this Mcp-Session-Id does not identify a usable session":
// handlePost's own session-ID validation (lines 395 "Invalid session ID",
// 399 "Session terminated") and handleSamplingResponse's, which handlePost
// also dispatches into for a POST body shaped like a sampling response
// (lines 791/795, identical two messages, plus 848 "No active session
// found for the given session ID"). flush() only rewrites a buffered 404
// body that is verbatim one of these three — anything else that happens to
// 404 on POST (a future mcp-go 404, a route we haven't imagined yet) is
// forwarded untouched rather than mislabelled as a dead-session error.
var knownDeadSessionMessages = map[string]bool{
	"Invalid session ID": true,
	"Session terminated": true,
	"No active session found for the given session ID": true,
}

// isTextPlainContentType reports whether contentType is the (case-
// insensitive) "text/plain" media type http.Error always sets — the only
// content type any of knownDeadSessionMessages is ever served under, and
// the cheap first-pass filter WriteHeader uses to decide whether a 404 is
// even a rewrite CANDIDATE before buffering its body. A prefix check is
// enough here: this only ever needs to recognize http.Error's own fixed
// "text/plain; charset=utf-8", not parse an arbitrary caller-supplied media
// type per RFC 7231.
func isTextPlainContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/plain")
}

// sessionErrorRewriter holds back a 404 response body instead of forwarding
// it immediately, so jsonRPCSessionErrors can replace it with a JSON-RPC
// error object once the wrapped handler is done writing AND the body turns
// out to actually be one of knownDeadSessionMessages. Every other
// status/content-type/body combination passes straight through untouched.
//
// It forwards http.Flusher and http.Hijacker to the underlying
// ResponseWriter and implements Unwrap (sk-303.14): embedding the bare
// http.ResponseWriter INTERFACE only promotes the three methods that
// interface itself declares (Header/Write/WriteHeader) — Go does not
// promote Flush/Hijack through an embedded interface field even though the
// concrete value stored in it supports them. Without the explicit methods
// below, `w.(http.Flusher)` on a *sessionErrorRewriter always fails, and
// mcp-go's handlePost silently stops flushing mid-call SSE notifications on
// every POST — not just the ones that end up 404ing.
type sessionErrorRewriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	intercept   bool
	buf         bytes.Buffer
}

// Compile-time guarantee that the interface-promotion gap above can't
// silently come back: if a future edit again embeds a bare interface
// instead of forwarding these methods, the package fails to build.
var (
	_ http.Flusher  = (*sessionErrorRewriter)(nil)
	_ http.Hijacker = (*sessionErrorRewriter)(nil)
)

func (w *sessionErrorRewriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if status == http.StatusNotFound && isTextPlainContentType(w.Header().Get("Content-Type")) {
		// Held back — flush() rewrites it once next.ServeHTTP has returned,
		// the full body is known, AND that body actually matches
		// knownDeadSessionMessages; otherwise flush() forwards it
		// untouched. Gating on content type here means a 404 with any
		// other content type (e.g. a hypothetical JSON 404) skips
		// buffering entirely and is written straight through below.
		w.intercept = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *sessionErrorRewriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercept {
		return w.buf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Flush implements http.Flusher by delegating to the underlying
// ResponseWriter — see the type doc comment for why this has to be spelled
// out explicitly. mcp-go's handlePost (server/streamable_http.go v0.50.0,
// the notification-drain goroutine around line 457/465 and the
// post-HandleMessage drain loop around line 507/516/536) flushes after
// every notification it writes onto a POST response upgraded to SSE; a
// no-op Flush doesn't error, it just silently defeats timely delivery.
//
// While a 404 body is still held back for the knownDeadSessionMessages
// decision (w.intercept), Flush is a deliberate no-op: nothing has reached
// the real ResponseWriter yet, and flushing here would either panic
// (WriteHeader was never actually called on it) or leak the held-back
// buffer to the client before flush() has decided whether to rewrite it —
// breaking the interception this type exists to do.
func (w *sessionErrorRewriter) Flush() {
	if w.intercept {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker by delegating to the underlying
// ResponseWriter, for the same interface-promotion reason as Flush.
// mcp-go's POST path does not hijack the connection today, but a wrapper
// that silently downgrades an optional capability of the writer it wraps
// is exactly the class of bug this file exists to eliminate.
func (w *sessionErrorRewriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Unwrap exposes the underlying ResponseWriter per net/http's documented
// wrapping convention — http.ResponseController's doc comment: "The
// ResponseWriter should be the original value passed to the
// Handler.ServeHTTP method, or have an Unwrap method returning the
// original ResponseWriter." Flush/Hijack above already satisfy a direct
// type assertion on their own (which is what mcp-go actually uses), but
// Unwrap keeps this type well-behaved for anything that walks the wrapper
// chain via http.NewResponseController instead.
func (w *sessionErrorRewriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// flush emits the rewritten body once the wrapped handler has returned. A
// no-op unless intercept was set, i.e. unless the wrapped handler actually
// wrote a 404 with a text/plain content type — every other status/content
// type was already forwarded live by Write.
func (w *sessionErrorRewriter) flush() {
	if !w.intercept {
		return
	}

	detail := strings.TrimSpace(w.buf.String())

	if !knownDeadSessionMessages[detail] {
		// WriteHeader's content-type check made this a CANDIDATE, but the
		// body isn't one of mcp-go's known dead-session messages — forward
		// the original 404 exactly as the wrapped handler wrote it
		// (headers included: WriteHeader never touched them, so whatever
		// the handler set on w.Header() is still there untouched) rather
		// than mislabel an unrelated 404 as a session error.
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		return
	}

	errResp := mcp.NewJSONRPCError(
		mcp.NewRequestId(nil),
		mcpSessionErrorCode,
		"mcp session error: "+detail,
		map[string]string{"detail": detail},
	)
	body, err := json.Marshal(errResp)
	if err != nil {
		// mcp.JSONRPCError's fields are all plain strings/ints/maps —
		// Marshal cannot fail on it in practice. Fall back to the original
		// plain-text body rather than lose the response entirely if it
		// somehow does.
		w.ResponseWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		return
	}

	// http.Error() already set Content-Type: text/plain and
	// X-Content-Type-Options: nosniff on this same (still-open, since we
	// never called the real WriteHeader) header map — overwrite both so the
	// client's JSON-RPC decoder is not told to distrust its own content
	// type.
	h := w.ResponseWriter.Header()
	h.Set("Content-Type", "application/json")
	h.Del("X-Content-Type-Options")
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}
