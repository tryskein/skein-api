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

// onPublicAPI reports whether ctx is a request of the public API.
func onPublicAPI(ctx context.Context) bool {
	on, _ := ctx.Value(publicAPIKey{}).(bool)
	return on
}

// logMessage is the message of a log line of an operation: outcome is
// "error", "ok", "warning" or "rejected". Every line of this package that
// reports the outcome of an operation takes its message from here, so a
// public API request never logs as an MCP call.
func logMessage(ctx context.Context, outcome string) string {
	if onPublicAPI(ctx) {
		return "public API operation " + outcome
	}
	return "mcp tool " + outcome
}

// transportMessage is the message of another line that names its
// transport: mcp, as the MCP tools wrote it, or public for a request of the
// public API.
func transportMessage(ctx context.Context, mcp, public string) string {
	if onPublicAPI(ctx) {
		return public
	}
	return mcp
}
