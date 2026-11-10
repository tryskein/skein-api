package publicapi

// router.go — the router of the public API, built from a route table passed
// as a parameter (plan D-1): the product passes ProductRoutes, a test passes a
// table of its own.
//
// The router serves only the paths of its table, all under /api/v1/ (INV-10):
// nothing of the internal API, /mcp, /agent/v1, /metrics or the health
// probes. It answers no CORS headers (browsers of other origins are not
// supported) and does not take the client address from forwarding headers
// (no middleware.RealIP).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/errcode"
	"skein-api/internal/issueops"
	"skein-api/internal/handler"
)

// MaxBodyBytes is the largest request body the public API accepts (1 MiB,
// spec "Limits"); a larger one gets 413 payload_too_large.
const MaxBodyBytes = 1 << 20

// jsonMediaType is the only media type of a request body.
const jsonMediaType = "application/json"

// RouterOptions configures a router.
type RouterOptions struct {
	// OperationTimeout is the deadline of one operation
	// (SKEIN_PUBLIC_API_WRITE_TIMEOUT): the context the handler runs with
	// ends after it, so a transaction still open by then is rolled back.
	// Must be > 0.
	OperationTimeout time.Duration
	// Chain is put in front of the handler of every route (the request
	// chain of the public entry, Chain.Wrap); nil leaves the handlers bare.
	Chain RouteWrapper
	// Versions reads the current version of the resource of a route that
	// checks If-Match. Required when the table has such a route.
	Versions VersionProvider
	// Idempotency wraps the operation of every route that accepts
	// Idempotency-Key (idempotency.Middleware in production): it runs after
	// the request chain and the body checks, before If-Match and Prefer, so
	// a repeat answered from the stored outcome is not held to the business
	// preconditions again (US-7). Required when the table has such a route.
	Idempotency RouteWrapper
}

// IdempotencyKeyHeader names the key of a request that may be repeated
// (US-7).
const IdempotencyKeyHeader = "Idempotency-Key"

// RouteWrapper wraps the handler of a route.
type RouteWrapper interface {
	Wrap(rt Route, next http.Handler) http.Handler
}

// routeContextKey carries the route of the request.
type routeContextKey struct{}

// RouteFromContext returns the route serving the request, set by the router
// before the route's handler runs.
func RouteFromContext(ctx context.Context) (Route, bool) {
	rt, ok := ctx.Value(routeContextKey{}).(Route)
	return rt, ok
}

// candidateMethods are the methods a 405 answer may list in Allow.
var candidateMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// NewRouter builds the router of routes. It panics on a defective table (a
// row without operationId, method, handler or authentication mode, a path
// outside /api/v1/, the same method and path twice) or on a non-positive
// operation timeout: both are programmer errors that must stop the binary at
// start.
func NewRouter(opts RouterOptions, routes []Route) http.Handler {
	if opts.OperationTimeout <= 0 {
		panic("publicapi: operation timeout must be > 0")
	}
	mux := chi.NewRouter()
	// Outside the panic recovery, as on the internal router: a panicking
	// operation is counted as the 500 the client received.
	mux.Use(handler.PublicHTTPMetrics, recoverProblem)
	mux.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(w, Problem{Code: errcode.RouteNotFound, Origin: OriginPath})
	})
	mux.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		if allow := allowedMethods(mux, r.URL.Path); allow != "" {
			w.Header().Set("Allow", allow)
		}
		WriteProblem(w, Problem{Code: errcode.MethodNotAllowed, Origin: OriginParse})
	})

	seen := map[string]string{}
	for _, rt := range routes {
		if defect := validRoute(rt); defect != "" {
			panic("publicapi: " + defect)
		}
		if rt.IfMatch != IfMatchNone && opts.Versions == nil {
			panic("publicapi: " + rt.OperationID + " checks If-Match but the router has no version provider")
		}
		if rt.Idempotent && opts.Idempotency == nil {
			panic("publicapi: " + rt.OperationID + " accepts Idempotency-Key but the router has no Idempotency middleware")
		}
		key := rt.Method + " " + rt.Path
		if other, dup := seen[key]; dup {
			panic(fmt.Sprintf("publicapi: %s and %s both route %s", other, rt.OperationID, key))
		}
		seen[key] = rt.OperationID
		mux.Method(rt.Method, rt.Path, routeHandler(opts, rt))
	}
	return mux
}

// routeHandler wraps the handler of rt: the operation runs under its
// deadline with the route in its context; the request chain comes first,
// then Idempotency-Key is refused on a route that does not accept it, then
// the body is checked (media type, size), then the request rules of the
// route — fields=, Idempotency, If-Match, Prefer — all before the
// operation.
func routeHandler(opts RouterOptions, rt Route) http.Handler {
	var op http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, ok := checkIfMatch(w, r, rt, opts.Versions)
		if !ok {
			return
		}
		rt.Handler.ServeHTTP(w, applyPrefer(r, rt))
	})
	if rt.Idempotent {
		op = opts.Idempotency.Wrap(rt, op)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, sent := r.Header[IdempotencyKeyHeader]; sent && !rt.Idempotent {
			WriteProblem(w, Problem{Code: errcode.IdempotencyNotSupported, Origin: OriginParse,
				Detail: rt.OperationID + " does not accept " + IdempotencyKeyHeader})
			return
		}
		if hasBody(r) {
			if !isJSON(r.Header.Get("Content-Type")) {
				WriteProblem(w, Problem{Code: errcode.UnsupportedMediaType, Origin: OriginParse})
				return
			}
			if r.ContentLength > MaxBodyBytes {
				WriteProblem(w, Problem{Code: errcode.PayloadTooLarge, Origin: OriginParse})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		}
		r, ok := checkFields(w, r, rt)
		if !ok {
			return
		}
		op.ServeHTTP(w, r)
	})
	var chained http.Handler = inner
	if opts.Chain != nil {
		chained = opts.Chain.Wrap(rt, inner)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), opts.OperationTimeout)
		defer cancel()
		ctx = context.WithValue(ctx, routeContextKey{}, rt)
		// The issue operations log their lines as public API ones.
		ctx = issueops.WithPublicAPI(ctx)
		chained.ServeHTTP(w, r.WithContext(ctx))
	})
}

// IsBodyTooLarge reports whether err is the failure of reading a body past
// MaxBodyBytes; a decoder answers it with 413 payload_too_large.
func IsBodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

// hasBody reports whether the request carries a body: a positive declared
// length or a body of unknown length (chunked).
func hasBody(r *http.Request) bool {
	return r.ContentLength > 0 || (r.ContentLength < 0 && r.Body != nil && r.Body != http.NoBody)
}

// isJSON reports whether contentType is application/json (parameters such as
// charset allowed).
func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, jsonMediaType)
}

// allowedMethods lists the methods the router serves on path, for the Allow
// header of a 405 answer (RFC 9110, section 15.5.6).
func allowedMethods(mux *chi.Mux, path string) string {
	var allow []string
	for _, m := range candidateMethods {
		if mux.Match(chi.NewRouteContext(), m, path) {
			allow = append(allow, m)
		}
	}
	return strings.Join(allow, ", ")
}

// recoverProblem answers a panicking handler with 500 internal_error in the
// problem format, after logging the panic with its stack.
func recoverProblem(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "public API: handler panicked",
				"method", r.Method, "path", r.URL.Path,
				"error", fmt.Sprint(rec), "stackTrace", string(debug.Stack()))
			WriteProblem(w, Problem{Code: errcode.InternalError, Origin: OriginDomain})
		}()
		next.ServeHTTP(w, r)
	})
}
