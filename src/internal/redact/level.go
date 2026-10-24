// Package redact defines the shared redaction-level enum used by the
// war-room activity surfaces: the cross-project activity feed
// (issues.ListUserActivity) and the "now working" snapshot
// (dashboard.InProgress).
//
// It lives in its own leaf package so that both `issues` and `dashboard`
// can depend on it WITHOUT `dashboard` importing `issues` — the enum is
// the one type they must agree on to redact identically.
package redact

// Level selects how much of a cross-project activity row is stripped
// server-side before it is serialized to the caller.
//
// The three levels are ordered by increasing strictness (None < Soft <
// Hard). Two rules keep the enum fail-closed:
//
//  1. Callers ALWAYS pass a Level explicitly. There is no Go zero-value
//     default on any function parameter — the caller decides None / Soft
//     / Hard from the caller's role, never by omission.
//  2. The fail-closed default (an unknown or future Level ⇒ the strictest
//     strip) lives inside each redaction helper's `switch` — only Soft is
//     spared the hard strip, so a forgotten or newly-added Level
//     over-redacts rather than leaks.
type Level int

const (
	// None keeps the row verbatim. Used for admin callers and for the
	// caller's own visible rows (the /me/activity contract).
	None Level = iota

	// Soft strips human-readable CONTENT (title, summary, per-field diff)
	// but KEEPS every stable identifier (issue id, project prefix, both
	// projectId fields). This is the unchanged internal war-room contract:
	// a non-member sees that work happened and where, but not what it is.
	Soft

	// Hard strips content AND every stable project/task identifier: issue
	// id, project prefix, both projectId fields, the raw Mcp-Session-Id
	// lock holder, and any composite/encoded id (e.g. a dependency
	// entity_id "from::to::type"). This is the external war-room contract:
	// a non-member sees throughput + actor + timing as ambient context,
	// never the identity of inaccessible work.
	Hard
)
