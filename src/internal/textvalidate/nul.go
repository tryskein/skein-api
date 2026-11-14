package textvalidate

// nul.go — the guard against U+0000 in caller input.
//
// PostgreSQL cannot store U+0000 in text, varchar or jsonb: an INSERT or an
// UPDATE that carries one fails with SQLSTATE 22021, and a query parameter
// that carries one fails the same way. JSON may legitimately spell the
// character (`\u0000`), so a caller can always send it, and without a check
// it reaches the database and comes back as an internal error. The guard
// runs once per request at each transport boundary — the public REST API,
// the internal REST API and the MCP tool calls — over every string the
// request carries (body values and member names, query parameters, the
// path), so no write path has to remember the rule on its own.
//
// The character is refused, never stripped: the caller must learn that the
// value it sent is not the value that would be stored.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// NULCode is the wire-stable code of a refused U+0000 — the validation
// subcode next to validation_error on every surface.
const NULCode = "nul_character"

// nul is the character the guard refuses.
const nul = "\x00"

// NULError names where a U+0000 was found.
type NULError struct {
	// Field is the path of the offending value: "title",
	// "dependencies[1].reason", a query parameter name, or "path". An
	// empty Field is the request body itself.
	Field string
	// InName is true when the character is in a member name of the
	// object at Field rather than in a value.
	InName bool
}

func (e *NULError) Error() string {
	where := e.Field
	if where == "" {
		where = "the request body"
	}
	if e.InName {
		return "a field name in " + where + " must not contain U+0000"
	}
	return where + " must not contain U+0000"
}

// ContainsNUL reports whether s carries U+0000.
func ContainsNUL(s string) bool {
	return strings.Contains(s, nul)
}

// ScanJSONForNUL walks one JSON document and returns a *NULError for the
// first string — a value or a member name — that decodes to text carrying
// U+0000, in document order. A document that is not valid JSON is not this
// guard's to judge: it returns the decoding error and the caller lets the
// request reach the decoder that answers malformed input.
func ScanJSONForNUL(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []jsonFrame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if len(stack) > 0 {
				// The document ended inside a container.
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				beginValue(stack)
				stack = append(stack, jsonFrame{object: v == '{', expectKey: v == '{', index: -1})
			case '}', ']':
				stack = stack[:len(stack)-1]
				endValue(stack)
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				if ContainsNUL(v) {
					return &NULError{Field: framePath(stack), InName: true}
				}
				stack[n-1].key = v
				stack[n-1].expectKey = false
				continue
			}
			beginValue(stack)
			if ContainsNUL(v) {
				return &NULError{Field: framePath(stack)}
			}
			endValue(stack)
		default:
			beginValue(stack)
			endValue(stack)
		}
	}
}

// jsonFrame is one open container of the walk.
type jsonFrame struct {
	object bool
	// expectKey: the next string of an object is a member name.
	expectKey bool
	// key is the member being read; index the element of an array.
	key   string
	index int
}

// beginValue moves an array to its next element.
func beginValue(stack []jsonFrame) {
	if n := len(stack); n > 0 && !stack[n-1].object {
		stack[n-1].index++
	}
}

// endValue makes an object expect its next member name.
func endValue(stack []jsonFrame) {
	if n := len(stack); n > 0 && stack[n-1].object {
		stack[n-1].expectKey = true
	}
}

// framePath is the path of the current position. While a member name is
// being read the innermost object is named by its parent's path.
func framePath(stack []jsonFrame) string {
	var b strings.Builder
	for _, f := range stack {
		if f.object {
			if f.expectKey {
				// Reading a member name: the path stops at this object.
				break
			}
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(f.key)
			continue
		}
		if f.index < 0 {
			break
		}
		b.WriteString("[" + strconv.Itoa(f.index) + "]")
	}
	return b.String()
}

// FindNULInValue walks a decoded JSON value (maps, slices, strings — what
// encoding/json produces into any) and returns a *NULError for the first
// string carrying U+0000. Members are visited in sorted name order, so the
// same input always names the same field.
func FindNULInValue(v any) error {
	return findNUL(v, "")
}

func findNUL(v any, path string) error {
	switch t := v.(type) {
	case string:
		if ContainsNUL(t) {
			return &NULError{Field: path}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if ContainsNUL(k) {
				return &NULError{Field: path, InName: true}
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := findNUL(t[k], join(path, k)); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range t {
			if err := findNUL(item, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

// FindNULInURL returns a *NULError when the decoded path of u or one of its
// query parameters (name or value) carries U+0000. A query parameter is
// named by its own name; the path is named "path".
func FindNULInURL(u *url.URL) error {
	if ContainsNUL(u.Path) {
		return &NULError{Field: "path"}
	}
	// url.Query keeps the well-formed pairs of a malformed query, as the
	// handlers that read it do.
	query := u.Query()
	names := make([]string, 0, len(query))
	for name := range query {
		if ContainsNUL(name) {
			return &NULError{Field: "query", InName: true}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range query[name] {
			if ContainsNUL(v) {
				return &NULError{Field: name}
			}
		}
	}
	return nil
}
