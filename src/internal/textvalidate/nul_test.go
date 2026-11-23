package textvalidate

import (
	"errors"
	"net/url"
	"testing"
)

func TestScanJSONForNUL(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		found  bool
		field  string
		inName bool
	}{
		{"clean object", `{"title":"a","n":1,"labels":["x","y"],"o":{"k":null}}`, false, "", false},
		{"top-level value", `{"title":"nul\u0000byte"}`, true, "title", false},
		{"escaped backslash is not NUL", `{"title":"a\\u0000b"}`, false, "", false},
		{"array element", `{"labels":["ok","x\u0000"]}`, true, "labels[1]", false},
		{"nested member", `{"dependencies":[{"toId":"a"},{"toId":"b","reason":"\u0000"}]}`, true, "dependencies[1].reason", false},
		{"after a nested object", `{"o":{"a":1},"title":"\u0000"}`, true, "title", false},
		{"member name at the root", `{"ti\u0000tle":"x"}`, true, "", true},
		{"member name in a nested object", `{"o":{"k\u0000":1}}`, true, "o", true},
		{"root string", `"\u0000"`, true, "", false},
		{"root array", `[{"title":"ok"},{"title":"\u0000"}]`, true, "[1].title", false},
		{"nested arrays", `{"m":[[1],["a","\u0000"]]}`, true, "m[1][1]", false},
		// Several offending strings: the first in document order is named,
		// whatever the order of the names.
		{"two values, the later name first", `{"b":"x\u0000","a":"y\u0000"}`, true, "b", false},
		{"a member name before a value", `{"a\u0000":"v","b":"x\u0000"}`, true, "", true},
		{"a value before a member name", `{"title":"\u0000","a\u0000":1}`, true, "title", false},
		{"an array element before a later value", `{"list":["ok","x\u0000"],"title":"\u0000"}`, true, "list[1]", false},
		{"a nested value before a root value", `{"o":{"k":"\u0000"},"a":"\u0000"}`, true, "o.k", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ScanJSONForNUL([]byte(c.raw))
			var ne *NULError
			found := errors.As(err, &ne)
			if found != c.found {
				t.Fatalf("ScanJSONForNUL(%s) = %v, want found=%v", c.raw, err, c.found)
			}
			if !found {
				if err != nil {
					t.Fatalf("ScanJSONForNUL(%s) = %v, want nil", c.raw, err)
				}
				return
			}
			if ne.Field != c.field || ne.InName != c.inName {
				t.Errorf("ScanJSONForNUL(%s) = field %q inName %v, want %q %v", c.raw, ne.Field, ne.InName, c.field, c.inName)
			}
		})
	}
}

func TestScanJSONForNUL_MalformedIsNotJudged(t *testing.T) {
	err := ScanJSONForNUL([]byte(`{"title":`))
	var ne *NULError
	if err == nil || errors.As(err, &ne) {
		t.Fatalf("ScanJSONForNUL(malformed) = %v, want a decoding error that is not *NULError", err)
	}
}

func TestFindNULInValue(t *testing.T) {
	cases := []struct {
		name   string
		v      any
		field  string
		inName bool
		found  bool
	}{
		{"clean", map[string]any{"title": "a", "n": 1.0, "deps": []any{map[string]any{"reason": "r"}}}, "", false, false},
		{"value", map[string]any{"body": "x\x00"}, "body", false, true},
		{"nested", map[string]any{"deps": []any{map[string]any{"reason": "ok"}, map[string]any{"reason": "\x00"}}}, "deps[1].reason", false, true},
		{"member name", map[string]any{"a": map[string]any{"k\x00": "v"}}, "a", true, true},
		// Several offending strings: members are visited in sorted name
		// order, so the same input always names the same field.
		{"two values, sorted first wins", map[string]any{"b": "\x00", "a": "\x00", "c": "\x00"}, "a", false, true},
		{"a clean member before an offending one", map[string]any{"a": "ok", "b": "\x00"}, "b", false, true},
		{"nested in the first member", map[string]any{"b": []any{"ok", "\x00"}, "a": map[string]any{"c": "\x00"}}, "a.c", false, true},
		{"array elements in index order", []any{"ok", map[string]any{"x": "\x00"}, "\x00"}, "[1].x", false, true},
		{"root string", "\x00", "", false, true},
		{"nil arguments", nil, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := FindNULInValue(c.v)
			var ne *NULError
			if found := errors.As(err, &ne); found != c.found {
				t.Fatalf("FindNULInValue = %v, want found=%v", err, c.found)
			}
			if !c.found && err != nil {
				t.Fatalf("FindNULInValue = %v, want nil", err)
			}
			if c.found && (ne.Field != c.field || ne.InName != c.inName) {
				t.Errorf("FindNULInValue = field %q inName %v, want %q %v", ne.Field, ne.InName, c.field, c.inName)
			}
		})
	}
}

func TestFindNULInURL(t *testing.T) {
	cases := []struct {
		raw    string
		field  string
		inName bool
		found  bool
	}{
		{"/api/v1/issues?q=plain", "", false, false},
		{"/api/v1/issues?q=nul%00byte", "q", false, true},
		{"/api/v1/issues/nul%00byte", "path", false, true},
		{"/api/v1/issues?a=1&b=%00", "b", false, true},
		// Several offending parameters: named in sorted order.
		{"/api/v1/issues?b=%00&a=%00", "a", false, true},
		// Every value of a repeated parameter is checked.
		{"/api/v1/issues?a=ok&a=x%00", "a", false, true},
		{"/api/v1/issues?a=ok&a=fine", "", false, false},
		// The path is checked before the query.
		{"/api/v1/issues/x%00?a=%00", "path", false, true},
		// A NUL in a parameter name names the query.
		{"/api/v1/issues?a%00b=1", "query", true, true},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		err = FindNULInURL(u)
		var ne *NULError
		if found := errors.As(err, &ne); found != c.found {
			t.Fatalf("FindNULInURL(%q) = %v, want found=%v", c.raw, err, c.found)
		}
		if !c.found && err != nil {
			t.Fatalf("FindNULInURL(%q) = %v, want nil", c.raw, err)
		}
		if c.found && (ne.Field != c.field || ne.InName != c.inName) {
			t.Errorf("FindNULInURL(%q) = field %q inName %v, want %q %v", c.raw, ne.Field, ne.InName, c.field, c.inName)
		}
	}
}

// TestFirstNUL_SortedChoiceIsStable — with many competing offending members
// or parameters, built in different orders, the named field is always the
// first in sorted order. Go randomises map iteration, so a choice that
// followed it would name different fields across these inputs and calls.
func TestFirstNUL_SortedChoiceIsStable(t *testing.T) {
	names := []string{"q", "d", "m", "b", "x", "k", "f", "z", "h"}
	for rot := range names {
		order := append(append([]string{}, names[rot:]...), names[:rot]...)
		value := map[string]any{}
		query := url.Values{}
		for _, n := range order {
			value[n] = "bad\x00"
			query.Add(n, "bad\x00")
		}
		for call := 0; call < 5; call++ {
			var ne *NULError
			if err := FindNULInValue(value); !errors.As(err, &ne) || ne.Field != "b" {
				t.Fatalf("FindNULInValue (order %v) = %v, want field b", order, err)
			}
			u := &url.URL{Path: "/api/v1/issues", RawQuery: query.Encode()}
			if err := FindNULInURL(u); !errors.As(err, &ne) || ne.Field != "b" {
				t.Fatalf("FindNULInURL (order %v) = %v, want field b", order, err)
			}
		}
	}
}
