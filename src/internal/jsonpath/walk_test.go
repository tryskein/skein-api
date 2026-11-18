package jsonpath

import (
	"encoding/json"
	"errors"
	"fmt"
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

// WalkStrings names each string the way Walk does (member names by their
// object's path, values by their own), building it only when asked.
func TestWalkStrings_PathsMatchWalk(t *testing.T) {
	doc := []byte(`{"title":"a","labels":["x","y"],"deps":[{"reason":"r","to":["k"]}],"n":1,"o":{"p":{"q":"z"}}}`)
	type hit struct {
		s, path string
		name    bool
	}
	var want, got []hit
	if err := Walk(doc, func(tok Token) bool {
		if s, ok := tok.Value.(string); ok {
			want = append(want, hit{s, tok.Path, tok.Name})
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := WalkStrings(doc, func(s string, name bool, pathOf func() string) bool {
		got = append(got, hit{s, pathOf(), name})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("WalkStrings\n%v\nWalk\n%v", got, want)
	}
}

// A scan that never asks for a path costs allocations proportional to the
// document, not to its size times its depth: doubling a deeply nested
// document about doubles the work (Walk, which builds every path, grows
// far faster).
func TestWalkStrings_CostGrowsWithSize(t *testing.T) {
	nested := func(depth int) []byte {
		return []byte(strings.Repeat(`{"k":[`, depth) + `"v"` + strings.Repeat(`]}`, depth))
	}
	allocs := func(doc []byte) float64 {
		return testing.AllocsPerRun(3, func() {
			_ = WalkStrings(doc, func(string, bool, func() string) bool { return true })
		})
	}
	small, large := allocs(nested(500)), allocs(nested(1000))
	if large > 2.6*small {
		t.Fatalf("allocations %v at depth 500, %v at depth 1000: more than linear", small, large)
	}
}
