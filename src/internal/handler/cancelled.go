package handler

import (
	"context"
	"errors"
	"net/http"
)

// AnswerCancelled answers a request its client cancelled with answer
// instead of the server error the handler wrote: when the request's context
// was cancelled (context.Canceled — the client went away, changed the filter,
// re-sent; not a deadline of our own) and the handler answers a 5xx, that 5xx
// is the consequence of the cancellation, not a fault of the service. The
// status the metrics middleware outside records is then answer's, so a
// cancelled request does not count as a server error. A timeout
// (context.DeadlineExceeded) and every other answer pass unchanged.
//
// The client is gone and never reads the answer; it exists for the metrics,
// the access log and any proxy still holding the connection.
func AnswerCancelled(answer func(http.ResponseWriter)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&cancelledWriter{ResponseWriter: w, ctx: r.Context(), answer: answer}, r)
		})
	}
}

// AnswerCancelledInternal is AnswerCancelled for the internal API: 408
// {"error":"request_cancelled"}, the answer /search gives a cancelled
// search.
func AnswerCancelledInternal(next http.Handler) http.Handler {
	return AnswerCancelled(func(w http.ResponseWriter) {
		writeProblem(w, http.StatusRequestTimeout, "request_cancelled", "", nil)
	})(next)
}

// cancelledWriter replaces a 5xx answer of a cancelled request by answer.
type cancelledWriter struct {
	http.ResponseWriter
	ctx    context.Context
	answer func(http.ResponseWriter)
	// wrote — the status is decided; replaced — answer was written and the
	// handler's own body is dropped.
	wrote, replaced bool
}

func (c *cancelledWriter) WriteHeader(status int) {
	if c.wrote {
		return
	}
	c.wrote = true
	if status >= http.StatusInternalServerError && errors.Is(c.ctx.Err(), context.Canceled) {
		c.replaced = true
		c.answer(c.ResponseWriter)
		return
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *cancelledWriter) Write(p []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	if c.replaced {
		return len(p), nil
	}
	return c.ResponseWriter.Write(p)
}

// Flush passes a flush on unless the answer was replaced.
func (c *cancelledWriter) Flush() {
	if c.replaced {
		return
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap is the writer underneath, for http.ResponseController.
func (c *cancelledWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
