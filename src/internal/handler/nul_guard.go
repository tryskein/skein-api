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
		// The JSON walk (which builds the field path of a hit) runs only on
		// a body that can hold U+0000 at all: a raw 0x00 byte or the escape
		// \u0000. Any other body is NUL-free, and skipping the walk keeps a
		// deeply nested body from costing more than its bytes (sk-977.7.10).
		if !mayContainNUL(raw) {
			next.ServeHTTP(w, r)
			return
		}
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

// mayContainNUL reports whether a JSON body can encode U+0000: as a raw
// 0x00 byte or as the escape \u0000 (the only escape of that character; its
// hex digits have no case). A false positive (an escaped backslash before
// "u0000") only costs the full scan.
func mayContainNUL(raw []byte) bool {
	return bytes.IndexByte(raw, 0) >= 0 || bytes.Contains(raw, []byte(`\u0000`))
}
