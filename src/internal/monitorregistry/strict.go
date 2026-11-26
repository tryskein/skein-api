package monitorregistry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// checkStrictJSON walks doc against the Go type it will be decoded into and
// refuses what encoding/json would silently accept but a strict reader (the
// runner's Python json) reads differently:
//
//   - a key of a schema object that differs from its field name only in
//     case: encoding/json matches field names case-insensitively;
//   - a key repeated in one object: encoding/json keeps merging into the
//     same field, a dict keeps the last value.
//
// Keys of free-form maps (platforms, engine ids, JSON paths of a sign) are
// taken as written; only their repetition is refused.
func checkStrictJSON(doc []byte, t reflect.Type) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	if err := walkStrict(dec, t, "$"); err != nil {
		return err
	}
	return nil
}

// walkStrict consumes one JSON value from dec, checking it against t.
func walkStrict(dec *json.Decoder, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(json.RawMessage(nil)) {
		var skip json.RawMessage
		return dec.Decode(&skip)
	}
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return fmt.Errorf("%s: unexpected end of the document", path)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	delim, isDelim := tok.(json.Delim)
	switch {
	case isDelim && delim == '{':
		return walkObject(dec, t, path)
	case isDelim && delim == '[':
		elem := t
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			elem = t.Elem()
		}
		for i := 0; dec.More(); i++ {
			if err := walkStrict(dec, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ']'
		return err
	}
	return nil // a scalar; its type is checked by the decode that follows
}

func walkObject(dec *json.Decoder, t reflect.Type, path string) error {
	fields := map[string]reflect.Type{}
	isStruct := t.Kind() == reflect.Struct
	if isStruct {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			fields[name] = f.Type
		}
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		key, _ := tok.(string)
		kpath := path + "." + key
		if seen[key] {
			return fmt.Errorf("%s: the key %q appears twice in one object", kpath, key)
		}
		seen[key] = true
		var vt reflect.Type
		switch {
		case isStruct:
			ft, ok := fields[key]
			if !ok {
				return fmt.Errorf("%s: unknown key %q (keys are case-sensitive)", kpath, key)
			}
			vt = ft
		case t.Kind() == reflect.Map:
			vt = t.Elem()
		default:
			vt = reflect.TypeOf((*any)(nil)).Elem()
		}
		if err := walkStrict(dec, vt, kpath); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}
