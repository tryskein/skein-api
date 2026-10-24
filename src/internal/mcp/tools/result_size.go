package tools

// result_size.go — what `_meta["anthropic/maxResultSizeChars"]` actually
// buys, and the one number a Skein tool cannot declare its way past.
//
// The annotation is a SERVER-SIDE DECLARATION read out of the tools/list
// entry: the client raises that tool's own result threshold to the
// declared value. Claude Code's MCP reference states the rule and its
// limit verbatim (https://code.claude.com/docs/en/mcp-servers,
// «Raise the limit for a specific tool», read 2026-08-20):
//
//	«Claude Code raises that tool's threshold to the annotated value, up
//	to a hard ceiling of 500,000 characters.»
//
// and, for what happens to a result that does not fit:
//
//	«Without the annotation, results that exceed the default threshold
//	are persisted to disk and replaced with a file reference in the
//	conversation.»
//
// So under-declaring is not a corrupted payload — it is a legitimate
// answer taken OUT of the conversation and handed back as a path, which
// an agent then has to go and read. That is the cost INV-5 is protecting
// against on this field, and it is why a tool's declaration is sized on
// the heaviest response it can produce rather than on its typical one.
//
// Two more properties of the field, from the same page, that decide how
// these numbers are chosen elsewhere in this package:
//
//   - the default threshold for a tool that declares NOTHING is
//     MAX_MCP_OUTPUT_TOKENS, 25 000 tokens by default — which is why the
//     large read-tools declare at all;
//   - a declaration applies to TEXT content only; image payloads stay
//     under the token limit regardless. Every Skein tool answers with one
//     JSON text block (internal/mcp.ToolJSON), so the text rule is the
//     only one that applies here.

// MCPMaxDeclarableResultChars is that hard ceiling.
//
// It is a CLIENT-side limit, not something this server enforces: declaring
// more is not an error, it is simply ignored, and a tool that can emit
// more than this cannot be fully covered by any declaration. Naming it
// keeps that fact checkable — the epic_create guard asserts against this
// constant instead of re-deriving a number nobody can look up.
const MCPMaxDeclarableResultChars = 500000
