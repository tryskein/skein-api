package tools

import "testing"

// TestStripPythonCommentLines — the comment stripping of the client loader
// and prepare: full-line comments go (LF and CRLF), code stays, and sources
// it cannot strip safely are refused instead of stripped wrongly.
func TestStripPythonCommentLines(t *testing.T) {
	got := stripPythonCommentLines("# head\nx = 1  # tail\n    # indented\r\ny = \"#\"\r\n")
	if want := "x = 1  # tail\ny = \"#\"\r\n"; got != want {
		t.Fatalf("stripped = %q, want %q", got, want)
	}
	for name, src := range map[string]string{
		"bare CR between a comment and code": "# comment\rx = 1\n",
		"triple-quoted string":               "x = \"\"\"\n# not a comment\n\"\"\"\n",
		"line continued by a backslash":      "x = \"a\\\n# b\"\n",
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: the source was stripped instead of refused", name)
				}
			}()
			stripPythonCommentLines(src)
		}()
	}
}
