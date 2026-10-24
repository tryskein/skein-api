package tools

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// attachmentActions is the action table of skein_attachments — the enum
// of the schema, the runtime allowlist and the `allowedActions` an agent
// gets back on a miss, all from one slice (see actionSet).
//
// Order is the lifecycle an agent walks, not alphabetical: mint a URL,
// upload out of band, confirm, then read and eventually delete. The enum
// is rendered in this order in tools/list, so it doubles as the shortest
// possible statement of the upload protocol.
var attachmentActions = actionSet{order: []string{
	"upload_url", "confirm", "list", "metadata", "get_url", "delete",
}}

// RegisterAttachments wires skein_attachments — the six R4.2 attachment
// tools folded into one tool with an `action` discriminator (sk-255.3).
//
// The per-action handlers are untouched and still live in their own
// files (handleAttachmentUploadURL and friends); this file only supplies
// the schema and the dispatch table. That is what keeps the ~350 lines
// of attachment_test.go, which call those handlers directly, green
// without an edit — and it keeps per-action validation, sentinel-error
// mapping and visibility checks out of one 600-line switch.
//
// Schema shape (sk-255.3, forced by the platform): `action` is the only
// required parameter and every other field is optional at the JSON Schema
// level, with conditional requirements stated in prose in each parameter
// description. Anthropic's API rejects a top-level oneOf/allOf/anyOf in
// input_schema with HTTP 400, so per-action required sets cannot be
// expressed structurally at all.
//
// Annotations are the coarsening this consolidation pays for: three of
// the six actions are pure reads, but MCP annotations are per tool, so
// the tool as a whole must declare the strongest hint any action needs —
// readOnly=false, destructive=true (delete is a hard delete). A client
// that filters by readOnlyHint therefore loses access to list / metadata
// / get_url. The "Read-only actions:" line in the description is the
// compensation, and it is indexed by Tool Search on top of that.
func RegisterAttachments(s *server.MCPServer, deps attachmentDeps) {
	tool := mcpgo.NewTool(
		"skein_attachments",
		mcpgo.WithDescription(
			"Files attached to an issue or to one of its comments. Uploading "+
				"takes two calls — action=\"upload_url\", then action=\"confirm\" "+
				"once the file has landed in storage; an unconfirmed attachment "+
				"is invisible to list. Presigned URLs expire after 10 minutes. "+
				"Read-only actions: list, metadata, get_url.",
		),
		mcpgo.WithString(actionParam,
			mcpgo.Required(),
			mcpgo.Enum(attachmentActions.enum()...),
			mcpgo.Description(
				"What to do:\n"+
					"  - \"upload_url\" → mint a presigned upload URL and create a pending attachment row.\n"+
					"  - \"confirm\" → mark an uploaded file ready (idempotent; re-emits no event on a second call).\n"+
					"  - \"list\" → list every confirmed attachment on an issue.\n"+
					"  - \"metadata\" → filename, contentType, sizeBytes, status and uploader of one attachment.\n"+
					"  - \"get_url\" → mint a presigned download URL for a ready attachment.\n"+
					"  - \"delete\" → hard-delete the row and remove the stored object.",
			),
		),
		mcpgo.WithString("issueId",
			mcpgo.Description("Full hierarchical issue id (e.g. \"demo-42\"). Required for upload_url and list."),
		),
		mcpgo.WithString("attachmentId",
			mcpgo.Description("Attachment UUID, as returned by upload_url or list. Required for confirm, metadata, get_url and delete."),
		),
		mcpgo.WithString("filename",
			mcpgo.Description("Original filename, kept as Content-Disposition on download. Required for upload_url."),
		),
		mcpgo.WithString("contentType",
			mcpgo.Description("MIME type of the upload (e.g. \"image/png\"). Re-checked at confirm time. Required for upload_url."),
		),
		mcpgo.WithNumber("sizeBytes",
			mcpgo.Description("Exact file size in bytes. The presigned policy binds this value — uploads of a different size are rejected by S3. Required for upload_url."),
			mcpgo.Min(1),
		),
		mcpgo.WithString("commentId",
			mcpgo.Description("Optional UUID of a comment under the issue. When set on upload_url, the attachment is scoped to that comment instead of the issue itself."),
		),
		// Coarsened on purpose — see the doc comment above.
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(false),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
	// Carried over from the folded skein_attachment_list: a busy issue's
	// attachment list is the only response in this group that can get
	// large, and the cap now has to sit on the tool that serves it.
	tool.Meta = mcpgo.NewMetaFromMap(map[string]any{
		"anthropic/maxResultSizeChars": 200000,
	})

	addActionTool(s, tool, attachmentActions.with(map[string]server.ToolHandlerFunc{
		"upload_url": handleAttachmentUploadURL(deps),
		"confirm":    handleAttachmentConfirm(deps),
		"list":       handleAttachmentList(deps),
		"metadata":   handleAttachmentMetadata(deps),
		"get_url":    handleAttachmentGetURL(deps),
		"delete":     handleAttachmentDelete(deps),
	}))
}
