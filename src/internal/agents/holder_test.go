package agents

// holder_test.go — review C5-04 (INV-10 of the lock holder). Reference — the
// spec R-5: holders of REST and MCP never coincide. An Mcp-Session-Id inside
// the REST namespace is moved out of it; every other id stays as sent.

import "testing"

func TestMCPSessionHolder_MovesTheRESTNamespaceOut(t *testing.T) {
	cases := []struct {
		sent, want string
	}{
		{"api:0b6f3a8e-6f0e-4a57-9d4b-5a0d5f1e2c3d:w", "mcp:api:0b6f3a8e-6f0e-4a57-9d4b-5a0d5f1e2c3d:w"},
		{"api:0b6f3a8e-6f0e-4a57-9d4b-5a0d5f1e2c3d", "mcp:api:0b6f3a8e-6f0e-4a57-9d4b-5a0d5f1e2c3d"},
		{"mcp-session-7c1e9a2b-1111-4222-8333-944455556666", "mcp-session-7c1e9a2b-1111-4222-8333-944455556666"},
		{"", ""},
	}
	for _, c := range cases {
		got := MCPSessionHolder(c.sent)
		if got != c.want {
			t.Errorf("MCPSessionHolder(%q) = %q, want %q", c.sent, got, c.want)
		}
		if c.sent != "" && IsRESTHolder(got) {
			t.Errorf("MCPSessionHolder(%q) = %q is still a REST holder", c.sent, got)
		}
	}
}
