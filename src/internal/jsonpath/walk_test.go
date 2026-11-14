package jsonpath

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestAtOffset — the value ending at the offset json.UnmarshalTypeError
// reports is named by its path, array indexes included. The offsets are
// those encoding/json reports for these inputs: the end of a scalar, the end
// of the opening delimiter of an object or an array.
func TestAtOffset(t *testing.T) {
	type dep struct {
		Reason string `json:"reason"`
	}
	type body struct {
		Priority *int           `json:"priority"`
		Labels   []string       `json:"labels"`
		Deps     []dep          `json:"dependencies"`
		M        map[string]int `json:"m"`
	}
	cases := []struct {
		raw, want string
	}{
		{`{"priority":"high"}`, "priority"},
		{`{"labels":["a", 5]}`, "labels[1]"},
		{`{"labels": [ "a" ,  true ] }`, "labels[1]"},
		{`{"dependencies":[{"reason":"x"},{"reason":7}]}`, "dependencies[1].reason"},
		{`{"labels":{}}`, "labels"},
		{`{"dependencies":[{"reason":"x"}, [1]]}`, "dependencies[1]"},
		{`{"m":{"a":"x"}}`, "m.a"},
		{`[1]`, ""},
	}
	for _, c := range cases {
		var b body
		err := json.NewDecoder(strings.NewReader(c.raw)).Decode(&b)
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			t.Fatalf("%s: decoding gave %v, want a type error", c.raw, err)
		}
		got, ok := AtOffset([]byte(c.raw), te.Offset)
		if !ok || got != c.want {
			t.Errorf("AtOffset(%s, %d) = %q, %v; want %q", c.raw, te.Offset, got, ok, c.want)
		}
	}
}

func TestWalk_TruncatedDocument(t *testing.T) {
	if err := Walk([]byte(`{"title":`), func(Token) bool { return true }); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("Walk(truncated) = %v, want io.ErrUnexpectedEOF", err)
	}
}
