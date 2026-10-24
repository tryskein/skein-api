package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// requestBodyMaxBytes is the cap on JSON request bodies for projects /
// components / aliases endpoints. Anything bigger is either an attack or
// a misuse — handlers reject before json.Decode allocates the full buffer.
//
// 1 MiB is the usual request-validation limit — keeps headroom
// for description fields with embedded markdown without exposing us to
// memory-pressure DoS.
const requestBodyMaxBytes = 1 << 20 // 1 MiB

// problemBody is the shape of every business-error JSON response. Matches
// the loose ProblemDetails-style format used elsewhere in the codebase
// ({"error":"<code>","message":"<msg>",...extra}).
//
// Extra fields are spliced in at write time from the optional `extra` map
// so we don't need a struct per error variant.
type problemBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// writeProblem encodes a uniform error response. status carries the HTTP
// code, code is a stable machine-readable error string (e.g.
// "id_prefix_taken"), msg is human-readable, and extra holds any
// structured context (offending ids, conflicting names, etc).
func writeProblem(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if extra == nil {
		_ = json.NewEncoder(w).Encode(problemBody{Error: code, Message: msg})
		return
	}

	// Merge into a flat map so callers see one combined object.
	merged := make(map[string]any, len(extra)+2)
	for k, v := range extra {
		merged[k] = v
	}
	merged["error"] = code
	if msg != "" {
		merged["message"] = msg
	}
	_ = json.NewEncoder(w).Encode(merged)
}

// writeJSON encodes body as the response payload under the supplied HTTP
// status. Package-wide success-path counterpart to writeProblem — every
// handler in this package funnels its 2xx bodies through it so the
// Content-Type header and the encode-failure log line stay uniform.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("failed to encode JSON response", "error", err.Error())
	}
}

// errBodyTooLarge is the local sentinel for "request body exceeded
// requestBodyMaxBytes". We unwrap MaxBytesError into this so callers can
// branch on a known type.
var errBodyTooLarge = errors.New("request body too large")

// decodeJSON installs a body-size limit, requires a non-empty Content-Type
// of application/json (or no Content-Type at all — some shells omit it),
// and DisallowUnknownFields so typos in field names surface as errors
// instead of silent ignore.
func decodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, requestBodyMaxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	switch {
	case errors.Is(err, io.EOF):
		return errors.New("request body is empty")
	case err == nil:
		return nil
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return errBodyTooLarge
	}
	return fmt.Errorf("invalid JSON: %w", err)
}

// parseUUIDParam reads a path param via chi and parses it. The error is
// always "<key>: invalid UUID" so handlers can wrap straight into a 400.
func parseUUIDParam(r *http.Request, key string) (uuid.UUID, error) {
	raw := chi.URLParam(r, key)
	if raw == "" {
		return uuid.Nil, fmt.Errorf("%s: missing", key)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s: invalid UUID", key)
	}
	return id, nil
}

// Method405 is the canonical "method not allowed" handler used for
// endpoints we deliberately do NOT support (e.g. DELETE /projects/{id} —
// soft-archive only, never hard-delete). The Allow header is mandatory in
// HTTP/1.1 so clients can discover the allowed verbs.
func Method405(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		writeProblem(w, http.StatusMethodNotAllowed,
			"method_not_allowed",
			"this endpoint cannot be deleted; use POST /archive instead", nil)
	}
}
