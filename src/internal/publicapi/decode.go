package publicapi

// decode.go — the strict parsing of a request (spec "General rules of the
// surface"): an unknown body field, an unknown query parameter, a body that
// is not one JSON value — 400 validation_error from the parsing layer; a
// body over 1 MiB — 413.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"skein-api/internal/errcode"
)

// DecodeBody decodes the JSON body of r into v strictly: an unknown field,
// trailing data or malformed JSON is 400 validation_error, a body over
// MaxBodyBytes 413 payload_too_large; the problem is written and false
// returned. An empty body decodes as {}.
func DecodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if IsBodyTooLarge(err) {
			WriteProblem(w, Problem{Code: errcode.PayloadTooLarge, Origin: OriginParse})
			return false
		}
		WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse, Detail: "the request body could not be read"})
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	if err := decodeStrict(raw, v); err != nil {
		WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse, Detail: err.Error()})
		return false
	}
	return true
}

// decodeStrict decodes exactly one JSON value without unknown fields.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return fmt.Errorf("the request body is not valid JSON (offset %d)", syntax.Offset)
		case errors.As(err, &typeErr):
			return fmt.Errorf("field %q has the wrong type", typeErr.Field)
		}
		return fmt.Errorf("invalid request body: %v", err)
	}
	if dec.More() {
		return errors.New("the request body has data after its JSON value")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the request body has data after its JSON value")
	}
	return nil
}

// CheckQuery refuses a query parameter outside allowed and the parameters
// the router itself reads for this route (fields on a read, force on a
// guarded operation): 400 validation_error is written and false returned.
func CheckQuery(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	rt, _ := RouteFromContext(r.Context())
	for name := range r.URL.Query() {
		if slices.Contains(allowed, name) ||
			(name == fieldsParam && isRead(rt.Method)) ||
			(name == forceParam && rt.Guarded) {
			continue
		}
		WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse,
			Detail: "unknown query parameter: " + name})
		return false
	}
	return true
}

// WriteParseError answers an error of the parsing layer (a limit, a cursor,
// a kind) as 400 validation_error.
func WriteParseError(w http.ResponseWriter, err error) {
	WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse, Detail: err.Error()})
}
