package tools

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
	"skein-api/internal/logging"
	skmcp "skein-api/internal/mcp"
)

// TestLogToolFailure_FailureLogsCodeAndReason is the core assertion of
// sk-303.38: a failed tool call must leave a WARN line naming the tool,
// the stable error code AND the actual message the caller received — the
// one thing skein_mcp_tool_errors_total cannot carry. Before this change
// a validation_error bucket of hundreds of calls a week was one number
// with (for skein_comment alone) at least six distinct causes hiding
// inside it.
func TestLogToolFailure_FailureLogsCodeAndReason(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const tool = "fake_logs_reason"
	h := instrumentTool(tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return skmcp.ToolError(ErrCodeValidationError, "issueId must be a full hierarchical id like \"demo-42\"", nil), nil
	})

	if _, err := h(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "mcp tool failed") {
		t.Fatalf("expected a \"mcp tool failed\" line, got:\n%s", got)
	}
	for _, want := range []string{
		"tool=" + tool,
		"code=" + ErrCodeValidationError,
		`reason="issueId must be a full hierarchical id like`,
		"outcome=" + outcomeBusinessError,
		"latencyMs=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log line missing %q:\n%s", want, got)
		}
	}
}

// TestLogToolFailure_SuccessDoesNotLog pins the other half: a clean
// success must never produce a "mcp tool failed" line, so the new line
// stays a pure failure signal an operator can grep without noise.
func TestLogToolFailure_SuccessDoesNotLog(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const tool = "fake_logs_reason_ok"
	h := instrumentTool(tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(`{"id":"sk-1"}`), nil
	})

	if _, err := h(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}

	if got := buf.String(); strings.Contains(got, "mcp tool failed") {
		t.Fatalf("a successful call produced a \"mcp tool failed\" line:\n%s", got)
	}
}

// TestLogToolFailure_KeysSurviveTheSanitizer is the production-handler
// pin for the pitfall the task called out twice already (sk-275,
// skein-web): logging.DualHandler blanks any attribute whose KEY
// contains password/token/secret/key/credential/authorization/apikey/
// api_key. "userId", "sessionId", "latencyMs" and friends must NOT trip
// that denylist, or the line would reach Loki as a wall of "***" and be
// worthless. Runs through the real DualHandler, not a plain text handler.
func TestLogToolFailure_KeysSurviveTheSanitizer(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const tool = "fake_sanitizer_pin"
	h := instrumentTool(tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return skmcp.ToolError(ErrCodeForbidden, "not a project member", nil), nil
	})

	ctx := auth.WithUserID(context.Background(), uuid.Must(uuid.NewRandom()))
	ctx = auth.WithSessionID(ctx, "sess-sanitizer-pin")

	if _, err := h(ctx, mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}

	got := buf.String()
	if strings.Contains(got, "***") {
		t.Fatalf("an attribute of the failure line was redacted by the secret-key denylist:\n%s", got)
	}
	for _, want := range []string{"tool=", "code=", "reason=", "outcome=", "latencyMs=", "userId=", "sessionId="} {
		if !strings.Contains(got, want) {
			t.Errorf("attribute %q missing from the production-handler line:\n%s", want, got)
		}
	}
}

// TestLogToolFailure_ReasonIsBounded is defence-in-depth, not a redaction:
// the "message" field of a ToolError envelope is built from static
// templates plus closed-set values in every call site reviewed for
// sk-303.38, but one path (an invalid commit hash echoed via %q) reflects
// caller-controlled text back into the message, and 700+ call sites is
// too many to prove the invariant holds forever. The reason attribute
// must never grow past maxLoggedToolFailureReason runes.
func TestLogToolFailure_ReasonIsBounded(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	huge := strings.Repeat("x", maxLoggedToolFailureReason*3)
	const tool = "fake_reason_bounded"
	h := instrumentTool(tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return skmcp.ToolError(ErrCodeValidationError, huge, nil), nil
	})

	if _, err := h(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}

	got := buf.String()
	if strings.Contains(got, huge) {
		t.Fatalf("the full %d-rune message reached the log unbounded:\n%s", len(huge), got)
	}
	// The bounded prefix (well under the full message) must still be
	// present — this is a length cap, not a content scrub.
	if !strings.Contains(got, strings.Repeat("x", 50)) {
		t.Errorf("expected the truncated prefix of the message in the log, got:\n%s", got)
	}
}

// TestLogToolFailure_OnlyMessageIsLogged is the sk-303.38 answer to "does
// a secret in the failure body reach the log in the open": a ToolError's
// structured `fields` (issueId, repoUrl, currentVersion, …) frequently
// carry a verbatim slice of caller input — see e.g. skein_comment's
// {"issueId": rawIssueID} on a malformed id — and DualHandler's sanitizer
// cannot help here because it redacts by attribute KEY, not by value (the
// same reasoning textvalidate's snippet-builder comment spells out for
// the sk-275 diagnostic). The fix applied here is structural rather than
// a content scan: logToolFailure only ever extracts the envelope's
// `message` string (see toolErrorMessage) and never the `fields` map, so
// a value placed in `fields` — however sensitive — has no path into this
// log line at all.
func TestLogToolFailure_OnlyMessageIsLogged(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secretLookingValue = "AKIA_FAKE_EXAMPLE_NOT_A_REAL_SECRET_1234567890"
	const tool = "fake_fields_not_logged"
	h := instrumentTool(tool, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return skmcp.ToolError(ErrCodeValidationError,
			"issueId must be a full hierarchical id like \"demo-42\"",
			map[string]any{"issueId": secretLookingValue}), nil
	})

	if _, err := h(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}

	if got := buf.String(); strings.Contains(got, secretLookingValue) {
		t.Fatalf("a value from the error envelope's structured fields reached the log:\n%s", got)
	}
}

// TestToolErrorMessage_ExtractsMessage is the unit-level pin for the
// helper the failure line is built from: it must read the same envelope
// toolErrorCode already reads, one field over, and return "" rather than
// panic or invent content on anything that isn't a clean envelope.
func TestToolErrorMessage_ExtractsMessage(t *testing.T) {
	tests := []struct {
		name string
		res  *mcpgo.CallToolResult
		want string
	}{
		{"envelope with message", skmcp.ToolError(ErrCodeValidationError, "id is required", nil), "id is required"},
		{"success has no message", mcpgo.NewToolResultText(`{"id":"sk-1"}`), ""},
		{"non-JSON text", mcpgo.NewToolResultText("plain text"), ""},
		{"envelope without message", mcpgo.NewToolResultText(`{"error":"forbidden"}`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolErrorMessage(tt.res); got != tt.want {
				t.Errorf("toolErrorMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
