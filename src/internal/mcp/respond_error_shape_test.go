// respond_error_shape_test.go locks the ToolError JSON envelope.
//
// Audit sk-194 (finding opt-A07-014) replaced the map[string]any build-up
// with a typed struct on the common "code + message" path. That is only a
// safe refactor while the emitted JSON stays identical — agents branch on
// the `error` code, so a shape change is a silent contract break for every
// MCP client. These tests are DB-free on purpose: they must run in the
// plain unit job, not only where TEST_DATABASE_URL is set.

package mcp

import (
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func toolErrorJSON(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatal("ToolError returned nil result")
	}
	if len(res.Content) == 0 {
		t.Fatal("ToolError result carries no content")
	}
	txt, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("first content block is %T, want mcpgo.TextContent", res.Content[0])
	}
	return txt.Text
}

// TestToolError_FixedEnvelopeBytes pins the exact bytes of the fast path.
// encoding/json sorts map keys, so the pre-refactor map produced
// `{"error":…,"message":…}`; the struct emits its fields in declaration
// order, which is the same sequence. An empty message was omitted before
// (`if message != ""`) and is omitted now (`omitempty`).
func TestToolError_FixedEnvelopeBytes(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
		want    string
	}{
		{
			name: "code and message",
			code: "version_conflict", message: "stale expectedVersion",
			want: `{"error":"version_conflict","message":"stale expectedVersion"}`,
		},
		{
			name: "empty message is omitted",
			code: "forbidden", message: "",
			want: `{"error":"forbidden"}`,
		},
		{
			name: "message needing escapes",
			code: "validation_error", message: `bad "quoted" value`,
			want: `{"error":"validation_error","message":"bad \"quoted\" value"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, fields := range []map[string]any{nil, {}} {
				got := toolErrorJSON(t, ToolError(tc.code, tc.message, fields))
				if got != tc.want {
					t.Fatalf("fields=%v\n got: %s\nwant: %s", fields, got, tc.want)
				}
			}
		})
	}
}

// TestToolError_ExtraFieldsMatchLegacyMap covers the branch that still
// builds a map (open extras like `field`, `currentVersion`). The assertion
// is semantic rather than byte-wise: that path was a map before and after,
// so only its content is contractual.
func TestToolError_ExtraFieldsMatchLegacyMap(t *testing.T) {
	fields := map[string]any{"currentVersion": 7, "field": "title"}
	got := toolErrorJSON(t, ToolError("version_conflict", "stale version", fields))

	legacy := map[string]any{"currentVersion": 7, "field": "title"}
	legacy["error"] = "version_conflict"
	legacy["message"] = "stale version"
	wantBytes, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshalling legacy shape: %v", err)
	}

	var gotAny, wantAny map[string]any
	if err := json.Unmarshal([]byte(got), &gotAny); err != nil {
		t.Fatalf("unmarshalling ToolError output %q: %v", got, err)
	}
	if err := json.Unmarshal(wantBytes, &wantAny); err != nil {
		t.Fatalf("unmarshalling legacy output: %v", err)
	}
	if len(gotAny) != len(wantAny) {
		t.Fatalf("key count differs\n got: %v\nwant: %v", gotAny, wantAny)
	}
	for k, w := range wantAny {
		g, ok := gotAny[k]
		if !ok {
			t.Fatalf("key %q missing from ToolError output %q", k, got)
		}
		if g != w {
			t.Fatalf("key %q = %v, want %v", k, g, w)
		}
	}

	// The caller's map must not be mutated — ToolError copies extras in.
	if _, leaked := fields["error"]; leaked {
		t.Fatal("ToolError wrote `error` into the caller's fields map")
	}
}
