package main

// mcp_no_ratelimit_test.go — row 66 (US-12 "MCP is not limited", SC-012):
// /mcp of the production router answers every call of one key, however many
// more than the REST limit of a key per minute.

import (
	"net/http"
	"testing"

	"skein-api/internal/testsupport"
)

func TestMCP_NotRateLimited(t *testing.T) {
	const restLimit = 3
	t.Setenv("SKEIN_PUBLIC_API_RATE_LIMIT_PER_MINUTE", "3")
	s := newParityStack(t)
	a := testsupport.ActorOnlyA

	sid := s.session(a)
	calls := restLimit * 4
	for i := 1; i <= calls; i++ {
		code, out, _ := s.mcp(a, sid, "tools/list", map[string]any{})
		if code == http.StatusTooManyRequests {
			t.Fatalf("call %d of %d on /mcp answered 429 (the REST limit is %d a minute)", i, calls, restLimit)
		}
		if code != http.StatusOK || out["error"] != nil {
			t.Fatalf("call %d: %d %v", i, code, out)
		}
	}
}
