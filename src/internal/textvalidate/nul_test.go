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
		{"nil arguments", nil, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := FindNULInValue(c.v)
			var ne *NULError
			if found := errors.As(err, &ne); found != c.found {
				t.Fatalf("FindNULInValue = %v, want found=%v", err, c.found)
			}
			if c.found && (ne.Field != c.field || ne.InName != c.inName) {
				t.Errorf("FindNULInValue = field %q inName %v, want %q %v", ne.Field, ne.InName, c.field, c.inName)
			}
		})
	}
}

func TestFindNULInURL(t *testing.T) {
	cases := []struct {
		raw   string
		field string
		found bool
	}{
		{"/api/v1/issues?q=plain", "", false},
		{"/api/v1/issues?q=nul%00byte", "q", true},
		{"/api/v1/issues/nul%00byte", "path", true},
		{"/api/v1/issues?a=1&b=%00", "b", true},
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
		if c.found && ne.Field != c.field {
			t.Errorf("FindNULInURL(%q) field %q, want %q", c.raw, ne.Field, c.field)
		}
	}
}
