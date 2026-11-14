package publicapi

// fields.go — fields= on reads (US-8): the response keeps the named fields
// plus id; on a list it projects the items and keeps the envelope. A write
// does not accept fields=.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"skein-api/internal/errcode"
)

const fieldsParam = "fields"

// fieldsKey carries the fields= of a read.
type fieldsKey struct{}

// isRead reports whether the method of a route reads.
func isRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// parseFields reads fields=: a comma-separated list of non-empty names, one
// parameter. nil when absent.
func parseFields(r *http.Request) (fields []string, present bool, ok bool) {
	vals, present := r.URL.Query()[fieldsParam]
	if !present {
		return nil, false, true
	}
	if len(vals) != 1 {
		return nil, true, false
	}
	for _, f := range strings.Split(vals[0], ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			return nil, true, false
		}
		fields = append(fields, f)
	}
	return fields, true, true
}

// checkFields applies the fields= rule of rt: on a write it is refused; on a
// read it is parsed into the context for WriteJSON.
func checkFields(w http.ResponseWriter, r *http.Request, rt Route) (*http.Request, bool) {
	fields, present, ok := parseFields(r)
	if !present {
		return r, true
	}
	if !isRead(rt.Method) {
		WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse,
			Detail: "fields is accepted only by reads"})
		return nil, false
	}
	if !ok {
		WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse,
			Detail: "fields must be one comma-separated list of field names"})
		return nil, false
	}
	return r.WithContext(context.WithValue(r.Context(), fieldsKey{}, fields)), true
}

// projectable is a list envelope: WriteJSON projects its items.
type projectable interface {
	itemType() reflect.Type
}

// WriteJSON writes v as the JSON response of the operation with status:
//
//   - a read with fields= keeps the named fields plus id — of each item of a
//     List, whose nextCursor and hasMore stay; a field the resource does
//     not have is 400 validation_error;
//   - a write whose route honours Prefer and whose request asks for
//     return=minimal gets {id, version} and Preference-Applied;
//   - otherwise v whole.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		rt, _ := RouteFromContext(r.Context())
		writeInternal(w, r, rt, "encoding the response", err)
		return
	}
	if fields, ok := r.Context().Value(fieldsKey{}).([]string); ok {
		body, err = project(v, body, fields)
		if err != nil {
			WriteProblem(w, Problem{Code: errcode.ValidationError, Origin: OriginParse, Detail: err.Error()})
			return
		}
	} else if preferMinimal(r.Context()) {
		body, err = minimal(body)
		if err != nil {
			rt, _ := RouteFromContext(r.Context())
			writeInternal(w, r, rt, "shaping the minimal response", err)
			return
		}
		w.Header().Set("Preference-Applied", "return=minimal")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		slog.DebugContext(r.Context(), "public API: writing the response failed", "error", err.Error())
	}
}

type unknownFieldError struct{ name string }

func (e unknownFieldError) Error() string { return "unknown field in fields: " + e.name }

// project keeps fields plus id of the object body, or of each item of a
// List.
func project(v any, body []byte, fields []string) ([]byte, error) {
	if l, ok := v.(projectable); ok {
		if err := knownFields(l.itemType(), fields); err != nil {
			return nil, err
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(env["items"], &items); err != nil {
			return nil, err
		}
		for i := range items {
			items[i] = keep(items[i], fields)
		}
		raw, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		env["items"] = raw
		return json.Marshal(env)
	}
	if err := knownFields(reflect.TypeOf(v), fields); err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	return json.Marshal(keep(obj, fields))
}

// keep is obj restricted to fields and id.
func keep(obj map[string]json.RawMessage, fields []string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(fields)+1)
	if id, ok := obj["id"]; ok {
		out["id"] = id
	}
	for _, f := range fields {
		if v, ok := obj[f]; ok {
			out[f] = v
		}
	}
	return out
}

// knownFields refuses a field that the JSON form of t does not have. The
// set is the type's, not the response's: a field omitted from this one
// response because it is empty is still a field of the resource.
func knownFields(t reflect.Type, fields []string) error {
	names := jsonFieldNames(t)
	for _, f := range fields {
		if _, ok := names[f]; !ok {
			return unknownFieldError{name: f}
		}
	}
	return nil
}

// jsonFieldNames are the names the JSON encoding of struct type t has,
// promoted embedded fields included.
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]struct{}{}
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" && !strings.Contains(tag, ",") {
			continue
		}
		if f.Anonymous && name == "" {
			for n := range jsonFieldNames(f.Type) {
				out[n] = struct{}{}
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = struct{}{}
	}
	return out
}
