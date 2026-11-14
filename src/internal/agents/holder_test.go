package agents

// holder_test.go — review C5-04 (INV-10 of the lock holder). Reference — the
// spec R-5: holders of REST and MCP never coincide. An Mcp-Session-Id inside
// the REST namespace is moved out of it; every other id stays as sent.

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMCPSessionHolder_MovesTheRESTNamespaceOut(t *testing.T) {
	cases := []struct {
		sent, want string
	}{
		{"api:00000000-0000-4000-8000-000000000001:w", "mcp:api:00000000-0000-4000-8000-000000000001:w"},
		{"api:00000000-0000-4000-8000-000000000001", "mcp:api:00000000-0000-4000-8000-000000000001"},
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

// TestRESTHolder_WorkerIDFormat — review C5-06. Reference — the spec US-5:
// X-Skein-Worker-Id is 1–64 characters of [A-Za-z0-9._-]; without the
// header the key itself is the executor.
func TestRESTHolder_WorkerIDFormat(t *testing.T) {
	key := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	valid := map[string]string{
		"":                      "api:" + key.String(),
		"worker-1":              "api:" + key.String() + ":worker-1",
		"A.b_c-9":               "api:" + key.String() + ":A.b_c-9",
		strings.Repeat("x", 64): "api:" + key.String() + ":" + strings.Repeat("x", 64),
	}
	for worker, want := range valid {
		got, err := RESTHolder(key, worker)
		if err != nil || got != want {
			t.Errorf("RESTHolder(%q) = %q, %v; want %q", worker, got, err, want)
		}
	}
	for _, worker := range []string{"a:b", ":", " ", "w 1", "wörker", "w/1", strings.Repeat("x", 65)} {
		if got, err := RESTHolder(key, worker); !errors.Is(err, ErrInvalidWorkerID) {
			t.Errorf("RESTHolder(%q) = %q, %v; want ErrInvalidWorkerID", worker, got, err)
		}
	}
}
