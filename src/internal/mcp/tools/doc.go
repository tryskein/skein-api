// Package tools registers the Skein MCP tool surface on an mcp-go server.
//
// Tools are grouped by domain: issues (create/update/close/claim/…),
// project/member admin, search/inbox/notifications, attachments, themes,
// and the dependency graph tools:
//
//   - skein_dep_add / skein_dep_add_many — create edges (blocks, related,
//     parent-child add-only, …)
//   - skein_dep_remove — delete an edge by (from, to, type)
//   - skein_reparent — atomic parent-child move (delete + insert + version)
//   - skein_move — documented signpost for copy+supersedes across projects
//
// Error codes are the closed set in errors.go (ErrCode*). Handlers map
// store sentinels onto those codes; clients branch on the JSON `error`
// field, never on free-form message text.
package tools
