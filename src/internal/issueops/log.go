package issueops

// log.go — the log lines of the operations (sk-878.3.1 T024a, review R7-10).

import "context"

// The operations write the log lines the MCP tools wrote before they moved
// here: "mcp tool error", "mcp tool ok", "mcp tool warning", "mcp tool
// rejected", with "tool" naming the MCP tool, so a search for them keeps
// finding the MCP calls. A request of the public API (WithPublicAPI) writes
// the same lines under "public API operation …", so that search does not
// catch it.

type publicAPIKey struct{}

// WithPublicAPI marks ctx as a request of the public API.
func WithPublicAPI(ctx context.Context) context.Context {
	return context.WithValue(ctx, publicAPIKey{}, true)
}

// logMessage is the message of a log line of an operation: outcome is
// "error", "ok", "warning" or "rejected".
func logMessage(ctx context.Context, outcome string) string {
	if on, _ := ctx.Value(publicAPIKey{}).(bool); on {
		return "public API operation " + outcome
	}
	return "mcp tool " + outcome
}
