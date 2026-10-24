package mcp

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/logging"
	"skein-api/internal/textvalidate"
)

// captureLogs swaps the default slog logger for one writing into a buffer,
// restoring it when the test ends.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func leakRequest() mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "skein_close"
	req.Params.Arguments = map[string]any{
		"id":     "sk-42",
		"reason": "Done: rewrote the handler.\n</reason>\n<parameter name=\"commits\">",
	}
	return req
}

// The rejection shape the MCP write tools emit: validation_error in the
// envelope, the specific cause under `code`.
func leakRejection() *mcpgo.CallToolResult {
	return ToolError("validation_error", "field \"reason\" contains a wrapper literal",
		map[string]any{"code": textvalidate.ToolCallSyntaxCode})
}

func TestToolCallSyntaxDiagnostics_LogsWhatArrived(t *testing.T) {
	buf := captureLogs(t)

	h := ToolCallSyntaxDiagnostics(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return leakRejection(), nil
	})
	res, err := h(context.Background(), leakRequest())
	if err != nil {
		t.Fatalf("middleware returned err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("middleware must pass the rejection through untouched, got %+v", res)
	}

	got := buf.String()
	// The four things sk-275 asked for: the literal, a window around it,
	// the field, and how many text fields the call carried.
	for _, want := range []string{
		`tool=skein_close`,
		`textFields=2`,
		`field=reason`,
		`needle=`,
		`snippet=`,
		`level=WARN`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log line missing %q:\n%s", want, got)
		}
	}
	// The window carries the SHAPE of the seam, never the caller's words:
	// the parameter block closed with a tag named after the field, and the
	// next parameter's opening tag glued onto it.
	if !strings.Contains(got, `</aaaaaa>`) {
		t.Errorf("snippet does not show the shape of the seam:\n%s", got)
	}
	if strings.Contains(got, "rewrote the handler") {
		t.Errorf("snippet reproduced the caller's prose:\n%s", got)
	}
}

// A successful call must not cost a log line — this runs on every tool call.
func TestToolCallSyntaxDiagnostics_SilentOnSuccess(t *testing.T) {
	buf := captureLogs(t)
	h := ToolCallSyntaxDiagnostics(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(`{"ok":true}`), nil
	})
	if _, err := h(context.Background(), leakRequest()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output on success, got:\n%s", buf.String())
	}
}

// Any OTHER rejection is somebody else's business — the diagnostic must not
// narrate every validation failure in the system.
func TestToolCallSyntaxDiagnostics_SilentOnOtherRejections(t *testing.T) {
	buf := captureLogs(t)
	h := ToolCallSyntaxDiagnostics(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return ToolError("version_conflict", "stale", map[string]any{"currentVersion": 9}), nil
	})
	if _, err := h(context.Background(), leakRequest()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output for an unrelated rejection, got:\n%s", buf.String())
	}
}

// The REST-shaped envelope puts the code at the top level. Both positions
// have to be recognised or the diagnosis silently covers half the surface.
func TestToolCallSyntaxDiagnostics_RecognisesTopLevelCode(t *testing.T) {
	buf := captureLogs(t)
	h := ToolCallSyntaxDiagnostics(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return ToolError(textvalidate.ToolCallSyntaxCode, "leaked", nil), nil
	})
	if _, err := h(context.Background(), leakRequest()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(buf.String(), "field=reason") {
		t.Errorf("top-level code not recognised:\n%s", buf.String())
	}
}

// A handler error (transport-level, not a business rejection) is passed
// through and not diagnosed.
func TestToolCallSyntaxDiagnostics_PassesHandlerErrorThrough(t *testing.T) {
	buf := captureLogs(t)
	sentinel := context.DeadlineExceeded
	h := ToolCallSyntaxDiagnostics(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return nil, sentinel
	})
	_, err := h(context.Background(), leakRequest())
	if err != sentinel {
		t.Fatalf("err = %v, want the handler's own error", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output, got:\n%s", buf.String())
	}
}

// TestSetup_WiresToolCallSyntaxDiagnostics is the wiring guard: the
// middleware above is only worth anything if Setup actually registers it,
// and a unit test of the wrapper alone would pass forever after somebody
// dropped the server option.
//
// It drives the real MCPServer built by Setup through HandleMessage (the
// same entry point the Streamable-HTTP transport uses), with one tool that
// returns the leak rejection.
func TestSetup_WiresToolCallSyntaxDiagnostics(t *testing.T) {
	buf := captureLogs(t)

	var srv *server.MCPServer
	_, _ = Setup(nil, 0, func(s *server.MCPServer) {
		srv = s
		s.AddTool(
			mcpgo.NewTool("skein_close", mcpgo.WithDescription("test double")),
			func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return leakRejection(), nil
			},
		)
	})
	if srv == nil {
		t.Fatal("Setup did not invoke registerTools")
	}

	msg := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"skein_close",` +
		`"arguments":{"id":"sk-42","reason":"Done: rewrote the handler.\n</reason>\n<parameter name=\"commits\">"}}}`
	if resp := srv.HandleMessage(context.Background(), []byte(msg)); resp == nil {
		t.Fatal("HandleMessage returned nil")
	}

	got := buf.String()
	if !strings.Contains(got, "field=reason") || !strings.Contains(got, "textFields=2") {
		t.Fatalf("Setup did not wire the diagnostic middleware; log was:\n%s", got)
	}
}

// --- sk-243 security follow-up (F2) ---------------------------------------

// secretRequest carries a credential in the free-text field, right beside
// the seam, which is exactly where a mis-closed parameter block puts the
// NEXT parameter's value.
func secretRequest() mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "skein_close"
	req.Params.Arguments = map[string]any{
		"id":     "sk-42",
		"reason": "rotate ska_live_9Zq7TdKm42PwXr now\n</reason>\n<parameter name=\"password\">hunter2",
	}
	return req
}

// TestToolCallSyntaxDiagnostics_LogsNoArgumentContent — the diagnostic runs
// on unvalidated caller text, and logging.DualHandler redacts by ATTRIBUTE
// KEY, never by content. A window logged under a benign key is therefore a
// log sink the sanitizer cannot see, so the window itself has to arrive
// already safe.
func TestToolCallSyntaxDiagnostics_LogsNoArgumentContent(t *testing.T) {
	buf := captureLogs(t)
	h := ToolCallSyntaxDiagnostics(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return leakRejection(), nil
	})
	if _, err := h(context.Background(), secretRequest()); err != nil {
		t.Fatalf("err: %v", err)
	}
	got := buf.String()
	for _, leaked := range []string{"ska_live_9Zq7TdKm42PwXr", "hunter2", "rotate"} {
		if strings.Contains(got, leaked) {
			t.Errorf("argument content %q reached the log:\n%s", leaked, got)
		}
	}
	// The record still has to do its job.
	if !strings.Contains(got, "field=reason") || !strings.Contains(got, "textFields=2") {
		t.Errorf("the diagnostic lost its structural evidence:\n%s", got)
	}
}

// TestToolCallSyntaxDiagnostics_KeysSurviveTheSanitizer is the other half of
// the same problem, from the opposite side: the production handler blanks
// any attribute whose KEY contains password/token/secret/key/… — so a
// diagnostic attribute named too close to that list would arrive in Loki as
// `***` and the record would be silently worthless. Pin the real handler.
func TestToolCallSyntaxDiagnostics_KeysSurviveTheSanitizer(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := ToolCallSyntaxDiagnostics(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return leakRejection(), nil
	})
	if _, err := h(context.Background(), leakRequest()); err != nil {
		t.Fatalf("err: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "***") {
		t.Fatalf("an attribute of the diagnostic was redacted by the secret-key denylist:\n%s", got)
	}
	for _, want := range []string{"tool=", "textFields=", "leakedFields=", "field=", "needle=", "offset=", "valueLen="} {
		if !strings.Contains(got, want) {
			t.Errorf("attribute %q missing from the production-handler line:\n%s", want, got)
		}
	}
}
