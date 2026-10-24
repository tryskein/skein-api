package tools

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// sk-294 — the tool-layer admin gate on `assignee` compares identity, not
// strings.
//
// skein_claim's schema documents three handle forms (username / e-mail /
// user UUID). The gate that decides "is this caller naming themselves, or
// claiming on behalf of somebody else?" understood only the username form,
// so a non-admin naming their OWN user UUID was told
// `forbidden: only admins may claim on behalf of other users` — for their
// own task.

// TestClaim_NonAdminAssigneeSelfByUUID: a non-admin names themselves by
// UUID. That is not claim-on-behalf and must not be refused.
func TestClaim_NonAdminAssigneeSelfByUUID(t *testing.T) {
	pool := newTestPool(t)
	caller, callerName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, caller, "clsid")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, caller, issueID, "self by uuid", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, caller, "user", "sess-self-uuid")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":       issueID,
		"assignee": caller.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("non-admin naming their own uuid was refused: %v (sk-294)", body)
	}

	// The handle is stored verbatim — the gate decides permission, it does
	// not normalise what the caller wrote.
	var assignee string
	if err := pool.QueryRow(t.Context(),
		`SELECT assignee FROM issues WHERE id = $1`, issueID).Scan(&assignee); err != nil {
		t.Fatalf("reading assignee: %v", err)
	}
	if assignee != caller.String() {
		t.Fatalf("assignee = %q, want the uuid handle %q (username is %q)",
			assignee, caller.String(), callerName)
	}
}

// TestClaim_NonAdminAssigneeOtherByUUID pins the other half: naming
// SOMEBODY ELSE's uuid is still claim-on-behalf, still admin-only.
func TestClaim_NonAdminAssigneeOtherByUUID(t *testing.T) {
	pool := newTestPool(t)
	caller, _ := seedUser(t, pool, "user")
	other, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, caller, "clonu")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, caller, issueID, "other by uuid", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, caller, "user", "sess-other-uuid")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":       issueID,
		"assignee": other.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected forbidden for non-admin naming another user's uuid")
	}
	if got := extractToolJSON(t, res)["error"]; got != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", got, ErrCodeForbidden)
	}
}
