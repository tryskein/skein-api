package mcp

// toolsyntax_diag.go — sk-275: one diagnostic line per
// `tool_call_syntax_leaked` rejection.
//
// # Why a middleware and not a log line inside the validator
//
// The record has to carry four things: the literal, a window of the text
// around it, the field, and HOW MANY text fields the call carried. The
// validator sees one field at a time and never sees the call, so the last
// of the four does not exist at that layer — and it is the axis the
// original report named ("long multiline values and two or more
// text fields in one call"). The tool-call boundary is the only
// place where all four are in scope at once, and wrapping it once covers
// every write tool instead of thirty validate sites.
//
// # Why it re-scans the arguments instead of parsing the rejection
//
// The arguments are the evidence. The whole reason sk-275 stayed open was
// that nobody had looked at what physically arrived; a diagnosis derived
// from the server's own error message would be circular, and one derived
// from the arguments answers the question directly — if the literal is
// there, the client glued it; if it is not, the guard fired on nothing.
//
// Scope is MCP only, deliberately. The REST surface runs the same guard,
// but a browser form cannot mis-close an XML tool call, and "how many text
// fields did the call have" is not a question a form submission answers.

import (
	"context"
	"encoding/json"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/textvalidate"
)

// ToolCallSyntaxDiagnostics wraps a tool handler and, when the call was
// rejected for tool-call syntax leakage, emits ONE WARN line describing
// what arrived.
//
// WARN, not ERROR: nothing is broken server-side — the guard did its job
// and the caller was told precisely what to fix. It is above INFO because
// production runs at INFO and this is the line an operator needs when an
// agent fleet starts burning turns on rejected calls.
//
// The handler's result is returned untouched: this is an observer, and a
// diagnostic that could change a rejection into something else would be a
// worse bug than the one it documents.
//
// None of the attribute keys below may contain password / token / secret /
// key / credential / authorization / apikey / api_key: logging.DualHandler
// blanks any attribute whose key contains one of those, so a key named too
// close to the list would reach Loki as `***` and the record would be
// silently worthless. TestToolCallSyntaxDiagnostics_KeysSurviveTheSanitizer
// pins that against the real handler.
func ToolCallSyntaxDiagnostics(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		res, err := next(ctx, req)
		if err != nil || res == nil || !res.IsError {
			return res, err
		}
		if !isToolCallSyntaxRejection(res) {
			return res, err
		}

		d := textvalidate.DiagnoseArguments(req.GetArguments())
		attrs := []any{
			"tool", req.Params.Name,
			"correlationId", CorrelationIDFrom(ctx),
			// The axis the report named: how many text fields shared the
			// call. A leak needs at least two — one to bleed out of and
			// one to bleed into.
			"textFields", d.TextFields,
			"leakedFields", d.FindingsTotal,
		}
		if len(d.Findings) > 0 {
			f := d.Findings[0]
			attrs = append(attrs,
				"field", f.Field,
				"needle", f.Needle,
				"offset", f.Offset,
				"valueLen", f.ValueLen,
				// Bounded AND redacted window, never the whole value —
				// prose fields run to tens of kilobytes, and the words in
				// them are the caller's, not ours. textvalidate.snippet
				// keeps the punctuation and the matched literal and
				// replaces every letter and digit with a placeholder, so
				// this attribute cannot carry a credential the caller
				// pasted into a description. It must be safe HERE, at the
				// point it is built: logging.DualHandler redacts by
				// attribute KEY (password/token/secret/…), so a window
				// travelling under the key `snippet` is invisible to it.
				"snippet", f.Snippet,
			)
		}
		slog.WarnContext(ctx,
			"mcp tool call rejected: tool-call wrapper literal present in a text field",
			attrs...)
		return res, err
	}
}

// isToolCallSyntaxRejection reports whether an error result carries the
// tool_call_syntax_leaked code.
//
// The code travels in one of two positions: as the envelope's `error` (the
// REST-shaped mapping) or under `code` beside `error: "validation_error"`
// (what the MCP write tools emit via validationToolError). Both are
// checked, and anything that does not parse as our JSON envelope is simply
// not a match — a diagnostic must never turn a malformed payload into a
// second failure.
func isToolCallSyntaxRejection(res *mcpgo.CallToolResult) bool {
	for _, c := range res.Content {
		text, ok := c.(mcpgo.TextContent)
		if !ok {
			continue
		}
		var body struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal([]byte(text.Text), &body) != nil {
			continue
		}
		if body.Error == textvalidate.ToolCallSyntaxCode || body.Code == textvalidate.ToolCallSyntaxCode {
			return true
		}
	}
	return false
}
