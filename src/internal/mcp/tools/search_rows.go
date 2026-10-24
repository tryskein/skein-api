package tools

// search_rows.go — the MCP projection of a search page (sk-282 T015, spec
// US-6).
//
// # What is removed and why it is not a field
//
// The snippet comes out of ts_headline wrapped in `<b>…</b>`: highlight
// markup for a browser that renders it. An agent renders nothing. It pays
// for the tags, and worse — a `<b>` in the middle of a word is noise in a
// string it may be matching on.
//
// So the markup is stripped in the MCP projection and NOWHERE ELSE. The
// search service and its SQL are shared with the REST search endpoint,
// which feeds the web UI's highlighted results; stripping at the source
// would silently un-highlight the interface without touching a single key,
// which is why row 62 of the test plan names "saved highlight markup"
// as its own leak vector.
//
// # What is NOT removed
//
// The matched text itself. Stripping the tags must leave the words: a
// snippet reduced to «…» would cost nothing and say nothing. Project
// identifier and the updated stamp stay too — US-6 declares them
// Unchanged, and cross-project search is unreadable without the project.

import (
	"strings"

	"skein-api/internal/search"
)

// SearchLimitDefault is the page size skein_search applies when the caller
// names none (sk-282 T015 / SC-004).
//
// Applied in the MCP handler, not in the search service: the service is
// shared with the REST search endpoint and INV-2 freezes its page size.
// The ceiling is the service's own (search.MaxPageLimit) and does not
// move — only the default does.
const SearchLimitDefault = 25

// highlightTags are the exact markers the search SQL wraps a match in
// (`StartSel=<b>,StopSel=</b>` in the ts_headline options). They are
// written here as literals rather than imported so this projection states
// what it removes; if the SQL ever changes them,
// TestSearchSnippet_NoHighlightMarkup fails on real output rather than
// silently stripping nothing.
var highlightTags = []string{"<b>", "</b>"}

// stripHighlight removes the highlight markup from a snippet, leaving the
// matched text in place.
//
// Both the raw tags and their HTML-escaped forms are removed. The escaped
// form is not hypothetical: anything that round-trips a snippet through an
// HTML-escaping layer turns `<b>` into `&lt;b&gt;`, and a caller reading
// "&lt;b&gt;" has markup on the wire just as surely — SC-004 says "neither in
// original nor in escaped form" for exactly that reason.
func stripHighlight(snippet string) string {
	out := snippet
	for _, tag := range highlightTags {
		out = strings.ReplaceAll(out, tag, "")
		out = strings.ReplaceAll(out, escapeHighlightTag(tag), "")
	}
	return out
}

// escapeHighlightTag renders a tag the way an HTML-escaping layer would.
func escapeHighlightTag(tag string) string {
	e := strings.ReplaceAll(tag, "&", "&amp;")
	e = strings.ReplaceAll(e, "<", "&lt;")
	return strings.ReplaceAll(e, ">", "&gt;")
}

// projectSearchResult returns a copy of the result with every snippet
// stripped.
//
// A copy, not an in-place edit: the value handed in belongs to the shared
// search service, and mutating it would be a change with an unbounded
// blast radius — the same instance backs the REST path.
func projectSearchResult(res *search.Result) *search.Result {
	if res == nil {
		return nil
	}
	out := *res
	out.Items = make([]search.Item, len(res.Items))
	copy(out.Items, res.Items)
	for i := range out.Items {
		out.Items[i].Snippet = stripHighlight(out.Items[i].Snippet)
	}
	return &out
}
