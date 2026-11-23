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

// walkDoc is the document of the token-contract tests; the offsets below are
// counted on it by hand:
//
//	{"a":[1,{"b":2.50}],"c":"x"}
//	0    5 7 9   13   18  23  27
const walkDoc = `{"a":[1,{"b":2.50}],"c":"x"}`

// TestWalk_TokenSequence — every token of a document with a nested array and
// object, in order: its path (a member name by its object's path, a closing
// delimiter by the path of the container it closes), whether it is a name,
// its value — a number as json.Number with its source text — and the offset
// just after it.
func TestWalk_TokenSequence(t *testing.T) {
	want := []Token{
		{Path: "", Value: json.Delim('{'), End: 1},
		{Path: "", Name: true, Value: "a", End: 4},
		{Path: "a", Value: json.Delim('['), End: 6},
		{Path: "a[0]", Value: json.Number("1"), End: 7},
		{Path: "a[1]", Value: json.Delim('{'), End: 9},
		{Path: "a[1]", Name: true, Value: "b", End: 12},
		{Path: "a[1].b", Value: json.Number("2.50"), End: 17},
		{Path: "a[1]", Value: json.Delim('}'), End: 18},
		{Path: "a", Value: json.Delim(']'), End: 19},
		{Path: "", Name: true, Value: "c", End: 23},
		{Path: "c", Value: "x", End: 27},
		{Path: "", Value: json.Delim('}'), End: 28},
	}
	var got []Token
	if err := Walk([]byte(walkDoc), func(tok Token) bool {
		got = append(got, tok)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("Walk gave %d tokens %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestAtOffset_OnlyAValueEndsThere — an offset that is the end of a member
// name, the end of a closing delimiter, a point between tokens or the start
// of the document names nothing (ok=false); a value at the top of the
// document is named by the empty path with ok=true.
func TestAtOffset_OnlyAValueEndsThere(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		offset int64
		want   string
		ok     bool
	}{
		{"the end of a member name", walkDoc, 4, "", false},
		{"the end of a nested member name", walkDoc, 12, "", false},
		{"the end of a closing brace", walkDoc, 18, "", false},
		{"the end of a closing bracket", walkDoc, 19, "", false},
		{"the end of the document's closing brace", walkDoc, 28, "", false},
		{"between a colon and a value", walkDoc, 5, "", false},
		{"the start of the document", walkDoc, 0, "", false},
		{"past the document", walkDoc, 40, "", false},
		{"a nested number", walkDoc, 17, "a[1].b", true},
		{"an opening bracket", walkDoc, 6, "a", true},
		{"the document's opening brace", walkDoc, 1, "", true},
		{"a scalar document", `42`, 2, "", true},
		{"a string document", `"x"`, 3, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := AtOffset([]byte(c.raw), c.offset)
			if got != c.want || ok != c.ok {
				t.Errorf("AtOffset(%s, %d) = %q, %v; want %q, %v", c.raw, c.offset, got, ok, c.want, c.ok)
			}
		})
	}
}
