package operator

// server.go — the operator's router and server (contract.go).

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter is the operator's router: the key check in front of everything,
// then the three operations of ops.
//
// @title Skein operator API
// @version 1.0
// @description The SaaS operator's API of skein-api (US-11): block, unblock and delete accounts. Served on its own port (SKEIN_OPERATOR_API_PORT) only with SKEIN_OPERATOR_API_ENABLED=true; every request needs X-Skein-Operator-Key.
// @securityDefinitions.apikey OperatorKeyAuth
// @in header
// @name X-Skein-Operator-Key
// @description The operator's key, SKEIN_OPERATOR_API_KEY. Missing or wrong: 401 operator_unauthorized on every path.
func NewRouter(key string, ops Operations) http.Handler {
	h := &handlers{ops: ops}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer, requireKey(key))
	r.Post(PathBlock, h.Block)
	r.Post(PathUnblock, h.Unblock)
	r.Post(PathDelete, h.Delete)
	return r
}

// The timeouts of the operator's server. ReadTimeout bounds reading a
// request, its body included, so a client without the key cannot hold the
// connection with a slow body; net/http lifts the read deadline once the body
// is consumed, so it does not limit how long an operation runs. IdleTimeout
// bounds a kept-alive connection waiting for its next request.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

// NewServer is the operator's http.Server on addr.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// requireKey answers 401 to a request without the key, whatever its path:
// it runs before routing (router-level middleware), so an unknown path is
// not told apart from a known one without the key.
func requireKey(key string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(key))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := sha256.Sum256([]byte(r.Header.Get(KeyHeader)))
			if r.Header.Get(KeyHeader) == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				OperatorRequestsTotal.WithLabelValues(operationNone, resultUnauthorized).Inc()
				writeError(w, http.StatusUnauthorized, CodeUnauthorized, "operator key required", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
