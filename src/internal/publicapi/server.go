package publicapi

// server.go — the http.Server of the public API on its own port (plan D-1).

import (
	"net/http"
	"time"

	"skein-api/internal/issues"
)

// The timeouts of the public server (spec, "Non-functional constraints": the
// server bounds reading the headers, reading the body, writing and idling).
const (
	// ReadHeaderTimeout bounds reading the request headers.
	ReadHeaderTimeout = 10 * time.Second
	// ReadTimeout bounds reading the whole request, headers and the body of
	// at most MaxBodyBytes.
	ReadTimeout = 30 * time.Second
	// IdleTimeout bounds a kept-alive connection waiting for its next
	// request.
	IdleTimeout = 120 * time.Second
	// ResponseWriteMargin is the time left, after the operation's deadline
	// and the read that follows a committed write, to write the response
	// body.
	ResponseWriteMargin = 5 * time.Second
)

// WriteTimeout is the write timeout of the public server for an operation
// deadline of operationTimeout. It exceeds the deadline by the read that
// follows a committed write (issues.ReadBackTimeout, which runs detached
// from the operation's context) and by the time to write the body, so an
// operation that committed just before its deadline still gets its response
// out. It stays below the life of an Idempotency "in progress" mark
// (operationTimeout + 30 s, spec "Data: Idempotency"): by the time the mark
// expires the original request has answered or been cut off.
func WriteTimeout(operationTimeout time.Duration) time.Duration {
	return operationTimeout + issues.ReadBackTimeout + ResponseWriteMargin
}

// NewServer is the public API server on addr serving handler, for an
// operation deadline of operationTimeout (SKEIN_PUBLIC_API_WRITE_TIMEOUT).
func NewServer(addr string, handler http.Handler, operationTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout(operationTimeout),
		IdleTimeout:       IdleTimeout,
	}
}
