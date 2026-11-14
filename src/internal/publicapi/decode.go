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
	"reflect"
	"slices"
	"strings"

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
		WriteParseError(w, err)
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
			if typeErr.Field == "" {
				return errors.New(rootTypeMessage(v))
			}
			field := jsonFieldPath(reflect.TypeOf(v), typeErr.Field)
			return &fieldError{field: field, msg: fmt.Sprintf("field %q has the wrong type", field)}
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
// a kind, a body) as 400 validation_error; a refusal of one member of the
// body names it in field, by its path in the request.
func WriteParseError(w http.ResponseWriter, err error) {
	var ext map[string]any
	var fe *fieldError
	if errors.As(err, &fe) {
		ext = map[string]any{"field": fe.field}
	}
	WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse, Detail: err.Error(), Extensions: ext})
}

// fieldError is a parsing refusal of one member of the body; field is its
// path as the request wrote it.
type fieldError struct {
	field string
	msg   string
}

func (e *fieldError) Error() string { return e.msg }

// rootTypeMessage names a body whose top-level JSON value has the wrong
// kind for target: an object is expected of every operation with a body.
func rootTypeMessage(target any) string {
	t := reflect.TypeOf(target)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && (t.Kind() == reflect.Struct || t.Kind() == reflect.Map) {
		return "the request body must be a JSON object"
	}
	return "the request body has the wrong type"
}

// jsonFieldPath turns the Field of a json.UnmarshalTypeError into the path
// of the member as the request wrote it. encoding/json joins the Go names
// of embedded structs into Field ("issueFields.priority" for priority of a
// body that embeds issueFields); those segments are dropped by walking
// target: a segment naming an anonymous struct field without a JSON name of
// its own is the embedding, not a member.
func jsonFieldPath(target reflect.Type, field string) string {
	segs := strings.Split(field, ".")
	out := make([]string, 0, len(segs))
	t := target
	for _, seg := range segs {
		t = elemType(t)
		if t == nil || t.Kind() != reflect.Struct {
			out = append(out, seg)
			t = nil
			continue
		}
		if f, ok := t.FieldByName(seg); ok && f.Anonymous && jsonName(f) == "" {
			t = f.Type
			continue
		}
		out = append(out, seg)
		t = memberType(t, seg)
	}
	return strings.Join(out, ".")
}

// elemType follows pointers, slices, arrays and maps to the type of their
// elements; nil stays nil.
func elemType(t reflect.Type) reflect.Type {
	for t != nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
		default:
			return t
		}
	}
	return nil
}

// memberType is the type of the member of struct t that JSON names name
// (promoted members of embedded structs included), nil when there is none.
func memberType(t reflect.Type, name string) reflect.Type {
	for _, f := range reflect.VisibleFields(t) {
		if f.Anonymous || !f.IsExported() {
			continue
		}
		n := jsonName(f)
		if n == "-" {
			continue
		}
		if n == "" {
			n = f.Name
		}
		if n == name {
			return f.Type
		}
	}
	return nil
}

// jsonName is the name the json tag of f gives, "" when it gives none.
func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}
