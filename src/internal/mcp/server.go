package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/agents"
	"skein-api/internal/auth"
	"skein-api/internal/mcp/instructions"
)

// correlationIDKey carries X-Correlation-Id (or chi RequestID fallback)
// on the MCP per-RPC context so tool-handler logs join the REST trail.
type correlationIDKey struct{}

// WithCorrelationID returns a child context that carries the correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey{}, id)
}

// CorrelationIDFrom returns the correlation id copied by WithHTTPContextFunc,
// or "" when absent.
func CorrelationIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(correlationIDKey{}).(string); ok {
		return v
	}
	return ""
}

// Setup builds the MCP server, registers tools, wraps the Streamable-HTTP
// transport with our auth → agents-registry middleware, and returns an
// http.Handler ready to be mounted under /mcp plus a drainStreams func the
// caller MUST register with (*http.Server).RegisterOnShutdown — see
// "Shutdown and the standalone GET stream" below (sk-303.16).
//
// registerTools receives only the MCPServer — store/pool deps are closed
// over by the callback built in cmd/api (mcpRegisterToolsFn). The pool
// argument is used for the agent UPSERT in wrapAgentRegistry.
//
// heartbeatInterval > 0 turns on the transport keepalive (sk-274.1); see
// config.Config.MCPHeartbeatInterval for why and for the tuning contract.
//
// Wiring (outermost first, called per request):
//
//	RequireInternalSecret     — channel auth (BFF → API), main.go
//	auth.RequireAPIKey         — X-Skein-Api-Key, main.go
//	logMCPRequest              — trace of method + session presence + outcome
//	agentRegistryMiddleware    — UPSERT agents row when sessionID present
//	drainableGETStreams        — cancellable ctx for the standalone GET (sk-303.16)
//	jsonRPCSessionErrors       — rewrites dead-session 404 body on POST (sk-303.10)
//	StreamableHTTPServer       — JSON-RPC dispatch
//	WithHTTPContextFunc        — copy auth ctx values onto request ctx
//	tool handlers              — read ctx via auth.UserIDFrom etc.
//
// # Shutdown and the standalone GET stream (sk-303.16)
//
// mcp-go's GET handler (server/streamable_http.go, handleGet) keeps the
// standalone notification stream (SSE) open in a loop that exits only on
// <-r.Context().Done(). stdlib net/http.Server.Shutdown does NOT cancel
// in-flight requests' contexts — verified against go1.26.1
// src/net/http/server.go: Shutdown closes idle connections and polls
// closeIdleConns() until either every connection goes idle or ITS OWN ctx
// expires; a connection whose handler is still running is never idle. A
// client that opened the GET stream and never disconnects therefore blocks
// Shutdown until the deadline, which is exactly the
// `"HTTP server shutdown error","error":"context deadline exceeded"` line
// observed on every rollout (measured locally: with an open GET stream
// Shutdown returns context.DeadlineExceeded at the deadline; without one it
// returns nil in under 1ms — see shutdown_test.go).
//
// drainStreams cancels the context of every currently-open standalone GET
// request, which makes handleGet's loop return and the response finish
// cleanly (the client sees the stream close normally — chunked EOF — not a
// connection reset). The caller must wire it via RegisterOnShutdown, the
// stdlib-documented hook for exactly this "long-lived connection" case: it
// runs concurrently with Shutdown's own polling loop, so canceled streams
// go idle almost immediately instead of racing the deadline.
//
// # Dead-session behaviour: POST vs GET (sk-303.10)
//
// sk-274 shipped the keepalive that stops most connections from dying, but
// left one of its own acceptance criteria open: WHEN a connection dies
// anyway (network blip, session GC, a pod cycling mid-session — see below),
// what does the client see? Read against v0.50.0's transport
// (server/streamable_http.go):
//
//   - POST (a tool call) against a dead/unknown Mcp-Session-Id already got a
//     404 from mcp-go itself (handlePost, "Invalid session ID" / "Session
//     terminated") — but as a bare text/plain http.Error body, which an
//     MCP-JSON-RPC client parses as a transport failure, not as an answer to
//     its call. jsonRPCSessionErrors (session_errors.go) fixes exactly this:
//     same 404, but a JSON-RPC 2.0 error object for a body.
//
//   - GET (the standalone notification stream) does NOT validate the
//     session id at all — mcp-go's own comment says so (handleGet: "The MCP
//     specification doesn't require validating session ID for GET
//     requests"). An unknown or dead id silently gets a BRAND NEW, empty
//     session under that same id and a 200 OK. This is the literal
//     "the tools vanished" shape: no error anywhere, just a stream that
//     will never carry what the old session would have sent.
//
// Deliberate decision: GET is left as mcp-go implements it, NOT validated
// by a layer of our own, for two reasons — both load-bearing, so recorded
// here rather than left as silent scope:
//
//  1. We cannot validate correctly without duplicating mcp-go's own session
//     bookkeeping. The session registries this would need to check against
//     (MCPServer.sessions, StreamableHTTPServer.activeSessions) are
//     unexported; a parallel registry we maintain ourselves — mirrored off
//     POST-initialize/DELETE events we happen to see — would drift from the
//     library's own state the moment its idle-session sweeper reaps
//     something we didn't independently learn about, producing exactly the
//     kind of "two sources of truth quietly disagree" bug this task exists
//     to remove, not add.
//  2. It would close a narrow window that mostly doesn't matter in
//     practice. GET is the NOTIFICATION stream — supplementary per the MCP
//     spec, not the path an agent calls tools through. The consequence of a
//     stale GET reconnect is a silently-empty notification channel, not a
//     failed tool call; the tool-call path (POST) already gets a real error
//     from the jsonRPCSessionErrors fix above. And per the pod-restart
//     finding below, GET-side validation would only help for a session
//     that died WITHIN one pod's lifetime (e.g. hitting a configured
//     WithSessionIdleTTL) — sk-303's deployment does not currently set one
//     — since a session never survives a pod restart regardless of GET
//     validation.
//
// # Client/server boundary (sk-303.10 AC)
//
// What the server can fix ends at "the wire response is unambiguous":
// jsonRPCSessionErrors makes a dead-session POST a real, parseable JSON-RPC
// error instead of a bare 404 body. What a specific MCP client library then
// DOES with that — retry silently, surface "session expired, reconnect",
// or (as observed 2026-08-27 with a non-interactive grok run reporting
// "auth required" against a verified-valid key — see sk-304 investigation)
// misreport it as something else entirely — is the client's own
// error-handling code, which this service does not control and cannot fix
// from here. That specific "auth required" symptom was never explained by
// this session mechanism at all: it is a distinct client-side
// misclassification, not a server-side auth failure (see the sk-304
// journal check for what the server actually returned on that path).
//
// # Sessions do not survive a pod restart (sk-303.10 AC, sk-274 carry-over)
//
// By construction: streamableHttpSession state (StreamableHTTPServer's
// activeSessions and MCPServer's sessions, both server/streamable_http.go
// and server/session.go) is an in-process sync.Map with no external or
// shared backing store — no Redis, no DB row, nothing that outlives the
// process. A rolling deploy replaces the pod (see cmd/api's shutdownTimeout
// doc and the deployment manifest referenced there), so every session held
// by the terminated pod is gone the instant it exits, independent of
// anything in this file. An agent whose GET stream (or next POST) lands on
// the NEW pod after a rollout is, from the server's point of view, a
// brand-new, empty session under whatever id the client still holds —
// this is not a bug fixable by better session-id validation; it needs a
// shared session store to change, which is out of scope here. Not verified
// against a live rollout with a session actually in flight at the moment of
// restart (no such natural experiment was captured — sk-274's own note that
// one went unobserved during the 0.62.0 rollout still stands); the
// conclusion above is a direct reading of the code, not a log observation.
func Setup(pool *pgxpool.Pool, heartbeatInterval time.Duration, registerTools func(*server.MCPServer)) (h http.Handler, drainStreams func()) {
	s := server.NewMCPServer(
		"skein",
		"0.1.0",
		// listChanged=false — the tool surface is static at boot. When we
		// add per-user / per-project dynamic tools we'll flip this and
		// emit notifications/tools/list_changed; not in R2.1.
		server.WithToolCapabilities(false),
		// sk-275: one WARN line per tool_call_syntax_leaked rejection,
		// carrying what actually arrived (literal, bounded window around
		// it, field, number of text fields in the call). Registered here
		// rather than in each write tool because "how many text fields did
		// this call carry" only exists at the call boundary.
		server.WithToolHandlerMiddleware(ToolCallSyntaxDiagnostics),
		// Server `instructions` are returned in the initialize response
		// and read by every Claude Code session unconditionally. They
		// are the primary onboarding signal now that Tool Search is
		// default-on (research §2). Capped at ~2 KB by spec — see
		// instructions/embed.go for the budget assertion.
		server.WithInstructions(instructions.Initialize),
	)

	if registerTools != nil {
		registerTools(s)
	}

	opts := []server.StreamableHTTPOption{
		// Copy auth values from the http.Request context onto the per-RPC
		// context that mcp-go threads through to tool handlers. Without
		// this, the handlers see a fresh ctx with no user identity.
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if id, ok := auth.UserIDFrom(r.Context()); ok {
				ctx = auth.WithUserID(ctx, id)
			}
			if role, ok := auth.UserRoleFrom(r.Context()); ok {
				ctx = auth.WithUserRole(ctx, role)
			}
			// is_external (sk-163 S1): without this copy, MCP tool handlers
			// never see the flag — the surface-gate / ceiling derivation
			// would silently treat every api-key caller as internal.
			ctx = auth.WithIsExternal(ctx, auth.IsExternalFrom(r.Context()))
			if id, ok := auth.APIKeyIDFrom(r.Context()); ok {
				ctx = auth.WithAPIKeyID(ctx, id)
			}
			if sid := auth.SessionIDFrom(r.Context()); sid != "" {
				ctx = auth.WithSessionID(ctx, sid)
			}
			// Log correlation: prefer explicit X-Correlation-Id, else chi
			// RequestID from the outer middleware chain. Tool handlers that
			// log with CorrelationIDFrom can join MCP lines to the REST trail.
			if cid := strings.TrimSpace(r.Header.Get("X-Correlation-Id")); cid != "" {
				ctx = WithCorrelationID(ctx, cid)
			} else if rid := middleware.GetReqID(r.Context()); rid != "" {
				ctx = WithCorrelationID(ctx, rid)
			}
			return ctx
		}),
	}

	// sk-274.1 — keep the client's standalone GET (SSE) stream warm so the
	// gateway's idle timeout never closes it. mcp-go's ticker lives inside
	// its GET handler, so this is a no-op for clients that never open one.
	if heartbeatInterval > 0 {
		opts = append(opts, server.WithHeartbeatInterval(heartbeatInterval))
		// Announced at info, not debug: production runs at info, and an
		// operator asking "is the keepalive actually on?" after a connection
		// incident must not have to read env or code to find out.
		slog.Info("mcp streamable-http heartbeat enabled", "interval", heartbeatInterval.String())
		if heartbeatInterval > heartbeatSuspiciousInterval {
			// Not an error: the gateway's idle timeout is not knowable from
			// here, so an operator may legitimately want a long interval.
			// But an interval longer than most idle timeouts silently buys
			// nothing, which is exactly the failure we are trying to end.
			slog.Warn("mcp heartbeat interval is longer than most gateway idle timeouts; the keepalive may never fire before the stream is closed",
				"interval", heartbeatInterval.String())
		}
	} else {
		slog.Info("mcp streamable-http heartbeat disabled")
	}

	httpHandler := server.NewStreamableHTTPServer(s, opts...)

	tracker := newStreamTracker()

	return logMCPRequest(wrapAgentRegistry(pool, drainableGETStreams(tracker, jsonRPCSessionErrors(httpHandler)))), tracker.cancelAll
}

// heartbeatSuspiciousInterval is the point past which a configured heartbeat
// is more likely a mistake than a choice — the cluster gateway's idle timeout
// has historically been 300–900s, and an interval above this buys nothing.
const heartbeatSuspiciousInterval = 5 * time.Minute

// streamTracker holds the cancel funcs of every currently in-flight
// standalone GET (SSE) request to /mcp, so they can all be cancelled in one
// shot at shutdown — see the "Shutdown and the standalone GET stream" note
// on Setup (sk-303.16). Safe for concurrent use; cancelAll is idempotent and
// may run concurrently with track/release (a request finishing normally
// mid-drain just no-ops its own already-fired cancel).
//
// draining remembers that cancelAll has run (sk-303.14): the original
// implementation snapshotted t.cancels under the mutex, released it, and
// only then invoked the snapshot — so a GET admitted by track() in the
// window between the snapshot and the (or after the) cancel loop was never
// cancelled at all, reproducing the exact sk-303.16 bug one level up
// (Shutdown blocks on that stream until its own deadline). draining turns
// "cancel every currently-tracked stream" into a permanent state change:
// once set, every later track() call is handed an already-cancelled
// context immediately instead of being added to the map, with no window
// where a new registration can slip past the one-and-only cancelAll call.
type streamTracker struct {
	mu       sync.Mutex
	cancels  map[uint64]context.CancelFunc
	nextID   uint64
	draining bool
}

func newStreamTracker() *streamTracker {
	return &streamTracker{cancels: make(map[uint64]context.CancelFunc)}
}

// track derives a cancellable context from r's, registers the cancel func,
// and returns the request rebound to it plus a release func. The caller
// MUST defer release() so a request that ends on its own (client hangs up,
// handler returns) doesn't leak an entry — and, since release also calls
// cancel, doesn't leave a stale CancelFunc for cancelAll to invoke on a
// context nobody is selecting on anymore.
//
// If draining is already set — cancelAll has run, or is running, on the
// SAME lock this method takes — the derived context is cancelled before
// track even returns, and nothing is added to the map: cancelAll will
// never run again to catch it later, so the only correct move is to hand
// the caller a context that is already Done(). drainableGETStreams rebinds
// r to it either way, so mcp-go's handleGet loop (which selects on
// <-r.Context().Done()) returns immediately instead of serving a stream
// Shutdown has already started draining.
func (t *streamTracker) track(r *http.Request) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(r.Context())

	t.mu.Lock()
	if t.draining {
		t.mu.Unlock()
		cancel()
		return r.WithContext(ctx), func() {}
	}
	id := t.nextID
	t.nextID++
	t.cancels[id] = cancel
	t.mu.Unlock()

	release := func() {
		t.mu.Lock()
		delete(t.cancels, id)
		t.mu.Unlock()
		cancel()
	}
	return r.WithContext(ctx), release
}

// cancelAll marks the tracker draining and cancels every currently-tracked
// stream's context, all inside one locked section. Called from
// (*http.Server).RegisterOnShutdown, so it runs in its own goroutine
// concurrently with Shutdown's closeIdleConns polling loop — the cancelled
// GET handlers return, their connections go idle, and Shutdown observes
// that instead of racing its own deadline.
//
// Setting draining and taking the snapshot under the SAME lock acquisition
// (not two separate ones, as the original one-shot-snapshot version did) is
// what closes the sk-303.14 drain race: since t.mu serializes every
// track()/cancelAll() call, any track() call's critical section either
// completes entirely before this one — in which case its entry is already
// in the snapshot below and gets cancelled by this same call — or entirely
// after it, in which case it observes draining=true and self-cancels
// instead of registering. There is no interleaving where a stream is
// registered without either happening.
func (t *streamTracker) cancelAll() {
	t.mu.Lock()
	t.draining = true
	cancels := make([]context.CancelFunc, 0, len(t.cancels))
	for _, c := range t.cancels {
		cancels = append(cancels, c)
	}
	t.mu.Unlock()

	for _, c := range cancels {
		c()
	}
}

// drainableGETStreams rebinds every GET request's context to one tracked by
// t, so t.cancelAll can unblock mcp-go's handleGet loop (which selects on
// <-r.Context().Done()) during shutdown. Scoped to GET only: POST (tool
// calls) and DELETE (session termination) are short-lived and already
// finish well within Shutdown's own polling loop — force-cancelling one
// mid-flight would abort a tool call for no benefit.
func drainableGETStreams(t *streamTracker, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		r, release := t.track(r)
		defer release()
		next.ServeHTTP(w, r)
	})
}

// logMCPRequest emits one debug line on arrival and one on completion for
// every inbound /mcp request. Until now the transport logged nothing, so a
// connection post-mortem (sk-274) had no way to tell whether a client ever
// opened the standalone GET stream — the whole premise of the heartbeat —
// or how often it reconnected.
//
// The arrival line stays at Debug and keeps its original timing (logged
// BEFORE next.ServeHTTP, not after): for a GET stream, next.ServeHTTP does
// not return until the stream closes, so this is the only line an operator
// watching Debug output sees the moment a client actually opens one — the
// exact thing this line was added for (sk-274 postmortem). At Info it would
// double the log volume of a busy agent fleet for data that only matters
// while diagnosing transport problems.
//
// sk-303.10: a bare arrival line never told anyone how the request ENDED — a
// dead-session 404 on POST left no trace anywhere, in skein-api, at any log
// level, because nothing captured the response status. The completion line
// below fixes that: it fires after next.ServeHTTP returns (for a GET stream,
// when it finally closes) and always carries the status. Its LEVEL is what
// makes failures visible in prod without a log-level flip: Debug on success
// keeps it exactly as invisible-by-default as the arrival line (so normal
// traffic still costs prod nothing), but a non-2xx/3xx status is exactly the
// class of thing a connection post-mortem needs and is promoted to Warn —
// the transport-error signal this task exists to surface reaches prod logs
// even though DEBUG is off there.
//
// We log the *presence* of Mcp-Session-Id, never its value: it is the session
// bearer for this transport, and DualHandler's sanitiser keys off field names
// it would not recognise here. Accept is logged because it is what separates a
// GET keepalive stream ("text/event-stream") from an ordinary POST. The
// configured heartbeat interval is deliberately NOT repeated here — it is
// process-level state, logged once at startup by Setup.
func logMCPRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		path := r.URL.Path
		hasSessionID := r.Header.Get("Mcp-Session-Id") != ""
		accept := r.Header.Get("Accept")

		slog.DebugContext(r.Context(), "mcp request",
			"method", method,
			"path", path,
			"hasSessionId", hasSessionID,
			"accept", accept,
		)

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		elapsed := time.Since(start)

		// WrapResponseWriter.Status() reports 0 when the handler never
		// called WriteHeader/Write at all (e.g. the client disconnected
		// before the transport wrote anything) — record it as such rather
		// than mislabelling it 200, which net/http would have sent nobody.
		status := ww.Status()
		attrs := []any{
			"method", method,
			"path", path,
			"hasSessionId", hasSessionID,
			"accept", accept,
			"status", status,
			"elapsedMs", elapsed.Milliseconds(),
		}
		if status == 0 || status >= 400 {
			slog.WarnContext(r.Context(), "mcp request failed", attrs...)
			return
		}
		slog.DebugContext(r.Context(), "mcp request completed", attrs...)
	})
}

// wrapAgentRegistry runs the agents UPSERT before delegating to the MCP
// transport. We do this synchronously (not in a goroutine) so any DB
// pressure naturally backpressures MCP throughput — which is the right
// behaviour. Failures are best-effort: log + continue.
//
// The UPSERT is skipped on the very first request of a session (the
// initialize call) because mcp-go generates the session id server-side
// and only puts it in the response header — the inbound r.Header
// has nothing yet. After the client has the id, every subsequent
// request carries it in Mcp-Session-Id and we register the row.
func wrapAgentRegistry(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		userID, hasUser := auth.UserIDFrom(ctx)
		sessionID := auth.SessionIDFrom(ctx)

		if hasUser && sessionID != "" {
			if err := agents.Upsert(ctx, pool, userID, sessionID, agents.LastSeenGranularity); err != nil {
				slog.WarnContext(ctx, "agents upsert failed",
					"userId", userID.String(),
					"sessionId", sessionID,
					"error", err.Error(),
				)
			}
		}

		next.ServeHTTP(w, r)
	})
}
