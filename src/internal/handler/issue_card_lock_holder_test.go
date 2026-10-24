package handler

import (
	"strings"
	"testing"

	"skein-api/internal/issues"
	"skein-api/internal/testsupport"
)

// sk-421.1 T002 (US-15, US-8 server half, INV-4), test-plan row 21, REST
// half: the web card reads REST, so the lock holder's name must be on the
// REST detail — not only on the MCP projection (the rejected sk-282 route).
// The claim goes through Store.Claim with a session, so the issue.claimed
// record exists (FIX-ZAKHVAT item 1).
func TestIssueCardREST_LockHolderName(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	u, uName := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, u, "clhrest")
	locked := testsupport.SeedIssue(t, pool, pid, u, prefix+"-1", "locked", "open", 2)
	free := testsupport.SeedIssue(t, pool, pid, u, prefix+"-2", "free", "open", 2)

	const session = "mcp-session-rest-holder"
	if _, err := issues.NewStore(pool).Claim(t.Context(), issues.ClaimInput{
		ID:        locked,
		SessionID: session,
		Actor:     issues.HumanActor(u, uName),
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	t.Run("claimed issue carries the performer's name", func(t *testing.T) {
		body := callIssueGet(t, pool, u, "user", pid, locked)
		if body["lockedByUsername"] != uName {
			t.Fatalf("lockedByUsername = %v, want %q", body["lockedByUsername"], uName)
		}
		if body["lockedBy"] != session {
			t.Errorf("lockedBy = %v, want the session %q unchanged", body["lockedBy"], session)
		}
		if name, _ := body["lockedByUsername"].(string); strings.Contains(name, session) {
			t.Errorf("lockedByUsername %q repeats the session id (INV-4)", name)
		}
	})

	t.Run("unclaimed issue has no key", func(t *testing.T) {
		body := callIssueGet(t, pool, u, "user", pid, free)
		if v, present := body["lockedByUsername"]; present {
			t.Errorf("lockedByUsername = %v on an unclaimed issue, want the key absent", v)
		}
	})
}
