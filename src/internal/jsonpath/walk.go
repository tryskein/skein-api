// Package jsonpath walks a JSON document token by token and names the
// position of each token by its path in the document ("title", "labels[1]",
// "dependencies[0].reason"), the form the API answers a refused member in.
package jsonpath

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Token is one token of the document as Walk meets it.
type Token struct {
	// Path names the token's position. A value is named by its own path; a
	// member name is named by the path of the object it belongs to ("" at
	// the top of the document).
	Path string
	// Name is true for a member name, false for a value.
	Name bool
	// Value is the token: a string, json.Number, bool, nil, or the
	// json.Delim that opens or closes an object or an array.
	Value json.Token
	// End is the offset in the document just after the token.
	End int64
}

// Walk calls visit for every token of the one JSON document raw, in
// document order, until visit returns false. A document that is not valid
// JSON is reported with the decoder's error once the walk reaches the defect;
// one that ends inside an object or an array, with io.ErrUnexpectedEOF.
func Walk(raw []byte, visit func(Token) bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if len(stack) > 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		t := Token{Value: tok, End: dec.InputOffset()}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				beginValue(stack)
				t.Path = path(stack)
				stack = append(stack, frame{object: v == '{', expectKey: v == '{', index: -1})
			case '}', ']':
				stack = stack[:len(stack)-1]
				t.Path = path(stack)
				endValue(stack)
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				t.Path, t.Name = path(stack), true
				if !visit(t) {
					return nil
				}
				stack[n-1].key = v
				stack[n-1].expectKey = false
				continue
			}
			beginValue(stack)
			t.Path = path(stack)
			endValue(stack)
		default:
			beginValue(stack)
			t.Path = path(stack)
			endValue(stack)
		}
		if !visit(t) {
			return nil
		}
	}
}

// WalkStrings calls visit for every string token of raw — member names and
// string values — in document order, until visit returns false. Unlike Walk
// it does not build the path of every token: visit gets a function that
// builds the path of the current string on request, so a scan that needs
// the path only for a rare hit costs time proportional to the document, not
// to its size times its depth. Errors as Walk.
func WalkStrings(raw []byte, visit func(s string, name bool, pathOf func() string) bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if len(stack) > 0 {
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
				stack = append(stack, frame{object: v == '{', expectKey: v == '{', index: -1})
			case '}', ']':
				stack = stack[:len(stack)-1]
				endValue(stack)
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				if !visit(v, true, func() string { return path(stack) }) {
					return nil
				}
				stack[n-1].key = v
				stack[n-1].expectKey = false
				continue
			}
			beginValue(stack)
			if !visit(v, false, func() string { return path(stack) }) {
				return nil
			}
			endValue(stack)
		default:
			beginValue(stack)
			endValue(stack)
		}
	}
}

// AtOffset is the path of the value of raw that ends at offset — for an
// object or an array, whose opening delimiter ends there — the position
// json.UnmarshalTypeError.Offset reports. ok is false when no value ends
// there.
func AtOffset(raw []byte, offset int64) (p string, ok bool) {
	_ = Walk(raw, func(t Token) bool {
		if t.Name || t.End != offset {
			return true
		}
		if d, isDelim := t.Value.(json.Delim); isDelim && d != '{' && d != '[' {
			return true
		}
		p, ok = t.Path, true
		return false
	})
	return p, ok
}

// frame is one open container of the walk.
type frame struct {
	object bool
	// expectKey: the next string of an object is a member name.
	expectKey bool
	// key is the member being read; index the element of an array.
	key   string
	index int
}

// beginValue moves an array to its next element.
func beginValue(stack []frame) {
	if n := len(stack); n > 0 && !stack[n-1].object {
		stack[n-1].index++
	}
}

// endValue makes an object expect its next member name.
func endValue(stack []frame) {
	if n := len(stack); n > 0 && stack[n-1].object {
		stack[n-1].expectKey = true
	}
}

// path is the path of the current position. While a member name is
// expected the innermost object is named by its parent's path.
func path(stack []frame) string {
	var b strings.Builder
	for _, f := range stack {
		if f.object {
			if f.expectKey {
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
