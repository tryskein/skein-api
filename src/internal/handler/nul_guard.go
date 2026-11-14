package handler

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"skein-api/internal/textvalidate"
)

// RefuseNUL is the internal API's guard against U+0000, which the database
// cannot store (textvalidate/nul.go): a request whose path, query parameter
// or JSON body string (value or member name) carries the character is
// answered 422 {"error":"validation_error","code":"nul_character",
// "field":…,"message":…} before any handler runs, so no write or lookup of
// the internal API reaches the store with one.
//
// The body is read up to requestBodyMaxBytes, the largest body any handler
// of the internal API accepts; a longer body, a body that is not valid JSON
// and a body whose read fails are handed on as they arrived, for the
// handler to answer as it always has.
func RefuseNUL(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := textvalidate.FindNULInURL(r.URL); err != nil {
			writeNULProblem(w, err)
			return
		}
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, requestBodyMaxBytes+1))
		if err != nil || len(raw) > requestBodyMaxBytes {
			r.Body = replayedBody{Reader: io.MultiReader(bytes.NewReader(raw), r.Body), Closer: r.Body}
			next.ServeHTTP(w, r)
			return
		}
		r.Body = replayedBody{Reader: bytes.NewReader(raw), Closer: r.Body}
		var nulErr *textvalidate.NULError
		if err := textvalidate.ScanJSONForNUL(raw); errors.As(err, &nulErr) {
			writeNULProblem(w, nulErr)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// replayedBody serves the bytes the guard read, then the rest of the
// original body, and closes the original.
type replayedBody struct {
	io.Reader
	io.Closer
}

// writeNULProblem answers a refused U+0000.
func writeNULProblem(w http.ResponseWriter, err error) {
	extra := map[string]any{"code": textvalidate.NULCode}
	var nulErr *textvalidate.NULError
	if errors.As(err, &nulErr) && nulErr.Field != "" {
		extra["field"] = nulErr.Field
	}
	writeProblem(w, http.StatusUnprocessableEntity, "validation_error", err.Error(), extra)
}
