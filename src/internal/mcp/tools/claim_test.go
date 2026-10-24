package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
	"skein-api/internal/issues"
)

// withCtxSession is the claim-test variant of withCtx that lets the
// caller pin a specific Mcp-Session-Id. The same user can run two
// distinct sessions to exercise concurrent-claim semantics.
func withCtxSession(t *testing.T, userID uuid.UUID, role, sessionID string) context.Context {
	t.Helper()
	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	ctx = auth.WithSessionID(ctx, sessionID)
	return ctx
}

// withCtxNoSession returns an auth ctx with NO Mcp-Session-Id (empty
// string). Used to verify the lock_required_session_id error.
func withCtxNoSession(t *testing.T, userID uuid.UUID, role string) context.Context {
	t.Helper()
	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	// Deliberately omit auth.WithSessionID — SessionIDFrom will return "".
	return ctx
}

// TestClaim_HappyPath_FreeIssue: free issue → success; lockedBy=session,
// lockedAt populated, version=2.
func TestClaim_HappyPath_FreeIssue(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clp")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "free issue", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-A")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	// Matrix row 84 (sk-282 T023): "identifier, state, version and
	// claim stamp, and nothing beyond" — the "nothing beyond" half is the
	// one a per-field assertion cannot make, so it is checked against the
	// tag-derived allowlist.
	assertAcknowledgementShape(t, body)
	if got := body["status"]; got != "in_progress" {
		t.Fatalf("status = %v, want in_progress", got)
	}
	if got := body["lockedBy"]; got != "sess-A" {
		t.Fatalf("lockedBy = %v, want sess-A", got)
	}
	if _, present := body["startedAt"]; !present {
		t.Fatalf("startedAt missing — the claim stamp is what proves the write applied")
	}
	if v, _ := body["version"].(float64); int(v) != 2 {
		t.Fatalf("version = %v, want 2", body["version"])
	}
	if got := body["assignee"]; got != uname {
		t.Fatalf("assignee = %v, want %s (defaulted to caller)", got, uname)
	}

	// Verify event written.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1
   AND type='issue.claimed' AND actor_session_id=$2`,
		issueID, "sess-A").Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected one issue.claimed event, got %d", n)
	}
}

// TestClaim_R22Issue_AssigneeMe_NoLock: R2.2-shaped issue (assignee set
// but no lock holder) → claim succeeds.
func TestClaim_R22Issue_AssigneeMe_NoLock(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clr22")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "assigned-no-lock", "open", 2)
	// Pre-set assignee directly to mimic R2.2 create-with-assignee.
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET assignee=$2 WHERE id=$1`, issueID, uname); err != nil {
		t.Fatalf("seed assignee: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-claim22")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("expected success, got err=%v isError=%v body=%+v",
			err, res != nil && res.IsError, func() any {
				if res == nil {
					return nil
				}
				return extractToolJSON(t, res)
			}())
	}
}

// TestClaim_AlreadyClaimedByOtherSession: lock held by another session →
// already_claimed.
func TestClaim_AlreadyClaimedByOtherSession(t *testing.T) {
	pool := newTestPool(t)
	userA, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userA, "clact")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userA, issueID, "owned", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)

	// First claim from session-A.
	ctxA := withCtxSession(t, userA, "admin", "sess-A")
	first := mcpgo.CallToolRequest{}
	first.Params.Arguments = map[string]any{"id": issueID}
	if r, err := handler(ctxA, first); err != nil || r.IsError {
		t.Fatalf("first claim failed: err=%v isError=%v", err, r != nil && r.IsError)
	}

	// Second claim attempt from a different session of the SAME user.
	ctxB := withCtxSession(t, userA, "admin", "sess-B")
	second := mcpgo.CallToolRequest{}
	second.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctxB, second)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on already-claimed")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeAlreadyClaimed {
		t.Fatalf("error = %v, want %s", got, ErrCodeAlreadyClaimed)
	}
}

// TestClaim_IdempotentReClaimSameSession: same session re-claims → success,
// no version bump, no extra event.
func TestClaim_IdempotentReClaimSameSession(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clidp")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "idempotent", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-X")

	first := mcpgo.CallToolRequest{}
	first.Params.Arguments = map[string]any{"id": issueID}
	r1, err := handler(ctx, first)
	if err != nil || r1.IsError {
		t.Fatalf("first claim failed: %v", err)
	}
	body1 := extractToolJSON(t, r1)
	v1, _ := body1["version"].(float64)

	// Second call from SAME session.
	second := mcpgo.CallToolRequest{}
	second.Params.Arguments = map[string]any{"id": issueID}
	r2, err := handler(ctx, second)
	if err != nil {
		t.Fatalf("second handler: %v", err)
	}
	if r2.IsError {
		t.Fatalf("unexpected isError on re-claim: %+v", extractToolJSON(t, r2))
	}
	body2 := extractToolJSON(t, r2)
	v2, _ := body2["version"].(float64)
	if int(v2) != int(v1) {
		t.Fatalf("re-claim should not bump version: %v -> %v", v1, v2)
	}

	// Only ONE issue.claimed event should exist.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.claimed'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected one issue.claimed event after re-claim, got %d", n)
	}
}

// TestClaim_BareNumericIDRejected.
func TestClaim_BareNumericIDRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-N")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": "42"}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for bare numeric id")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestClaim_NotVisible: stranger trying to claim issue in a project they
// can't see gets issue_not_found.
func TestClaim_NotVisible(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "clnv")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "private", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, stranger, "user", "sess-stranger")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-member")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIssueNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeIssueNotFound)
	}
}

// TestClaim_MissingSessionID: ctx without Mcp-Session-Id →
// lock_required_session_id.
func TestClaim_MissingSessionID(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clmsid")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "msid", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxNoSession(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true without session id")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeLockRequiredSessionID {
		t.Fatalf("error = %v, want %s", got, ErrCodeLockRequiredSessionID)
	}
}

// TestClaim_AdminClaimOnBehalfOfOtherUser: admin can pass assignee to
// claim on behalf of another user.
func TestClaim_AdminClaimOnBehalfOfOtherUser(t *testing.T) {
	pool := newTestPool(t)
	admin, _ := seedUser(t, pool, "admin")
	other, otherName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, admin, "clobh")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, admin, issueID, "obh", "open", 2)
	// Add `other` as a project member so visibility doesn't matter for
	// the admin path (admins see everything; this is just hygiene).
	_, _ = pool.Exec(context.Background(),
		`WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
		VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`,
		pid, other, admin)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, admin, "admin", "sess-admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":       issueID,
		"assignee": otherName,
	}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("admin claim-on-behalf failed: err=%v body=%v",
			err, func() any {
				if res == nil {
					return nil
				}
				return extractToolJSON(t, res)
			}())
	}
	body := extractToolJSON(t, res)
	if got := body["assignee"]; got != otherName {
		t.Fatalf("assignee = %v, want %s", got, otherName)
	}
}

// lockHolderName reads the issue detail (as an admin reader, so visibility
// plays no part) and returns lockedByUsername — nil when the key is absent.
// Shared by the sk-421.1 T002 tests (test-plan rows 14–19).
func lockHolderName(t *testing.T, store *issues.Store, reader uuid.UUID, issueID string) *string {
	t.Helper()
	dto, err := store.GetIssue(withCtxSession(t, reader, "admin", "sess-reader-"+issueID), issueID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", issueID, err)
	}
	return dto.LockedByUsername
}

// assertLockHolder asserts lockedByUsername == want (want "" = absent).
func assertLockHolder(t *testing.T, store *issues.Store, reader uuid.UUID, issueID, want, when string) {
	t.Helper()
	got := lockHolderName(t, store, reader, issueID)
	switch {
	case want == "" && got != nil:
		t.Errorf("%s: lockedByUsername = %q, want the key absent", when, *got)
	case want != "" && got == nil:
		t.Errorf("%s: lockedByUsername absent, want %q", when, want)
	case want != "" && *got != want:
		t.Errorf("%s: lockedByUsername = %q, want %q", when, *got, want)
	}
}

// TestClaim_LockHolderNameIsThePerformer is sk-421.1 test-plan row 14
// (US-15, INV-7, SC-005), single-claim half: a claim by user U with session S
// names U; an admin claiming on behalf of V names the ADMIN — the performer,
// not the assignee. Claims go through the real handler, so the issue.claimed
// event is written in the claim's own transaction (CLAIM-FIX item 1).
func TestClaim_LockHolderNameIsThePerformer(t *testing.T) {
	t.Run("user claims with own session", func(t *testing.T) {
		pool := newTestPool(t)
		u, uName := seedUser(t, pool, "user")
		pid, prefix := seedProject(t, pool, u, "clhn")
		issueID := prefix + "-1"
		seedIssue(t, pool, pid, u, issueID, "held", "open", 2)
		store := issues.NewStore(pool)

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"id": issueID}
		if res, err := handleClaim(pool, store)(withCtxSession(t, u, "user", "sess-U"), req); err != nil || res.IsError {
			t.Fatalf("claim failed: %v %+v", err, res)
		}

		dto, err := store.GetIssue(withCtxSession(t, u, "user", "sess-read"), issueID)
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if dto.LockedByUsername == nil || *dto.LockedByUsername != uName {
			t.Fatalf("lockedByUsername = %v, want %q", dto.LockedByUsername, uName)
		}
		if dto.LockedBy == nil || *dto.LockedBy != "sess-U" {
			t.Errorf("lockedBy = %v, want the session sess-U unchanged", dto.LockedBy)
		}
	})

	t.Run("admin claims on behalf of another user", func(t *testing.T) {
		pool := newTestPool(t)
		admin, adminName := seedUser(t, pool, "admin")
		other, otherName := seedUser(t, pool, "user")
		pid, prefix := seedProject(t, pool, admin, "clhnobh")
		issueID := prefix + "-1"
		seedIssue(t, pool, pid, admin, issueID, "held on behalf", "open", 2)
		_, _ = pool.Exec(context.Background(),
			`WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
			VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, other, admin)
		store := issues.NewStore(pool)

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"id": issueID, "assignee": otherName}
		if res, err := handleClaim(pool, store)(withCtxSession(t, admin, "admin", "sess-admin"), req); err != nil || res.IsError {
			t.Fatalf("admin claim-on-behalf failed: %v %+v", err, res)
		}

		dto, err := store.GetIssue(withCtxSession(t, admin, "admin", "sess-read"), issueID)
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if dto.Assignee == nil || *dto.Assignee != otherName {
			t.Fatalf("assignee = %v, want %q — fixture did not claim on behalf", dto.Assignee, otherName)
		}
		assertLockHolder(t, store, admin, issueID, adminName, "admin claimed on behalf of "+otherName)
	})
}

// TestClaim_NonAdminAssigneeOther: non-admin specifying assignee !=
// self → forbidden.
func TestClaim_NonAdminAssigneeOther(t *testing.T) {
	pool := newTestPool(t)
	caller, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, caller, "clnao")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, caller, issueID, "nao", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, caller, "user", "sess-caller")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":       issueID,
		"assignee": "someone-else",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-admin claim-on-behalf")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", got, ErrCodeForbidden)
	}
}

// sk-18 — claim transitions status atomically.

// TestClaim_OpenTransitionsToInProgress: claim on a status=open issue
// flips status to in_progress in the same tx, stamps started_at, and
// emits issue.status_changed alongside issue.claimed.
func TestClaim_OpenTransitionsToInProgress(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "claop")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "open→inprogress", "open", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-open")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("claim failed: err=%v body=%+v", err, extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if got := body["status"]; got != "in_progress" {
		t.Fatalf("status = %v, want in_progress", got)
	}
	if got := body["lockedBy"]; got != "sess-open" {
		t.Fatalf("lockedBy = %v, want sess-open", got)
	}
	if body["startedAt"] == nil {
		t.Fatalf("startedAt should be set on first in_progress entry; body=%+v", body)
	}

	// Both events must exist in the same actor session.
	var claimed, statusChanged int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FILTER (WHERE type='issue.claimed'),
       count(*) FILTER (WHERE type='issue.status_changed')
  FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND actor_session_id=$2`,
		issueID, "sess-open").Scan(&claimed, &statusChanged); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("issue.claimed count = %d, want 1", claimed)
	}
	if statusChanged != 1 {
		t.Fatalf("issue.status_changed count = %d, want 1", statusChanged)
	}
}

// TestClaim_DeferredTransitionsToInProgress: claim on a status=deferred
// issue un-defers it (status → in_progress) and emits status_changed.
func TestClaim_DeferredTransitionsToInProgress(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "cladef")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "deferred→inprogress", "deferred", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-def")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("claim failed: err=%v body=%+v", err, extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if got := body["status"]; got != "in_progress" {
		t.Fatalf("status = %v, want in_progress", got)
	}

	var statusChanged int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1
   AND type='issue.status_changed' AND actor_session_id=$2`,
		issueID, "sess-def").Scan(&statusChanged); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if statusChanged != 1 {
		t.Fatalf("issue.status_changed count = %d, want 1", statusChanged)
	}
}

// TestClaim_RejectsClosedIssue: closed is terminal — claim returns
// cannot_claim_closed and does NOT mutate the row.
func TestClaim_RejectsClosedIssue(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clacls")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "closed-issue", "closed", 2)

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-closed")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true claiming a closed issue")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeCannotClaimClosed {
		t.Fatalf("error = %v, want %s", got, ErrCodeCannotClaimClosed)
	}

	// Row must remain unchanged: still closed, no lock, no events.
	var status string
	var lockedBy *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, locked_by FROM issues WHERE id=$1`, issueID).Scan(&status, &lockedBy); err != nil {
		t.Fatalf("query issue: %v", err)
	}
	if status != "closed" {
		t.Fatalf("status mutated to %q after failed claim, want closed", status)
	}
	if lockedBy != nil {
		t.Fatalf("lockedBy set to %v after failed claim, want NULL", *lockedBy)
	}
	var events int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND actor_session_id=$2`,
		issueID, "sess-closed").Scan(&events); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if events != 0 {
		t.Fatalf("expected zero events for rejected claim, got %d", events)
	}
}

// sk-118 (F2d) — skein_claim rejects a standing issue.

// seedStandingIssue creates a status='standing' row directly (the DB CHECK
// accepts it post-000029) so the claim path can be exercised without the
// promote machinery.
func seedStandingIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title string) string {
	t.Helper()
	id := seedIssue(t, pool, projectID, creator, idStr, title, "standing", 2)
	return id
}

// TestClaim_StandingRejected: skein_claim on a standing issue is rejected
// (isError=true) and leaves the row wholly untouched — no status flip, no
// lock, no event. This is the MCP-surface counterpart to the store-level
// regression in internal/issues.
func TestClaim_StandingRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clstand")
	issueID := seedStandingIssue(t, pool, pid, userID, prefix+"-1", "standing-issue")

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-standing")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true claiming a standing issue")
	}

	// Row must remain unchanged: still standing, no lock, no started_at, no events.
	var status string
	var lockedBy, startedAt *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, locked_by, started_at::text FROM issues WHERE id=$1`, issueID).
		Scan(&status, &lockedBy, &startedAt); err != nil {
		t.Fatalf("query issue: %v", err)
	}
	if status != "standing" {
		t.Fatalf("status mutated to %q after failed claim, want standing", status)
	}
	if lockedBy != nil {
		t.Fatalf("lockedBy set to %v after failed claim, want NULL", *lockedBy)
	}
	if startedAt != nil {
		t.Fatalf("startedAt set to %v after failed claim, want NULL", *startedAt)
	}
	var events int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND actor_session_id=$2`,
		issueID, "sess-standing").Scan(&events); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if events != 0 {
		t.Fatalf("expected zero events for rejected standing claim, got %d", events)
	}
}

// TestClaim_StandingReturnsCannotClaimStanding: the MCP envelope `error` is
// EXACTLY `cannot_claim_standing` (the specific code, NOT the generic
// validation_error) and the message carries the spawn-a-child directive.
func TestClaim_StandingReturnsCannotClaimStanding(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "clstandcode")
	issueID := seedStandingIssue(t, pool, pid, userID, prefix+"-1", "standing-code")

	store := issues.NewStore(pool)
	handler := handleClaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-standing-code")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true claiming a standing issue")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeCannotClaimStanding {
		t.Fatalf("error = %v, want %s (specific, not generic validation_error)", got, ErrCodeCannotClaimStanding)
	}
	// Guard against the regression where the *ValidationError flattens to the
	// generic code with the specific cause hidden under detail.code.
	if got := body["error"]; got == ErrCodeValidationError {
		t.Fatalf("error must not be the generic %s", ErrCodeValidationError)
	}
	msg, _ := body["message"].(string)
	for _, want := range []string{"spawn", "child", "parentId"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to mention %q (directive)", msg, want)
		}
	}
}

// NOTE: the REST-boundary 422 assertion for cannot_claim_standing lives in the
// handler package (TestClaim_StandingRejected_REST in internal/handler) because
// skein-api exposes claim only through the MCP tool surface — there is no
// standalone REST POST /claim route; the web UI claims via the /mcp proxy, which
// calls this very handleClaim → Store.Claim path. The handler test exercises the
// real shared REST error-mapper (mapIssueValidation) so the 422 contract is
// asserted against production code, not a test-local mapping.

// sk-202 coderev — the serialization_retry contract at the MCP boundary.

// TestMapClaimError_Codes pins what each store-level claim error becomes on
// the wire. The pairing that matters is the sk-202 one: a claim that lost a
// database serialization race must surface as `serialization_retry`
// ("nothing was written, repeat the call verbatim") and must NEVER reuse
// `already_claimed`, which tells the agent to back off because somebody else
// owns the issue. Getting those two confused is the whole of sk-202, so the
// mapping is asserted directly rather than left implicit.
func TestMapClaimError_Codes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "serialization retry",
			err:  issues.ErrSerializationRetry,
			want: ErrCodeSerializationRetry,
		},
		{
			// Store wraps the sentinel; the mapping must see through it.
			name: "wrapped serialization retry",
			err:  fmt.Errorf("claiming issue: %w", issues.ErrSerializationRetry),
			want: ErrCodeSerializationRetry,
		},
		{
			name: "already claimed with row-state detail",
			err: &issues.AlreadyClaimedError{
				Branch:   "locked_by_other",
				Status:   "in_progress",
				LockedBy: "sess-other",
				Assignee: "someone",
			},
			want: ErrCodeAlreadyClaimed,
		},
		{
			name: "bare already claimed sentinel",
			err:  issues.ErrAlreadyClaimed,
			want: ErrCodeAlreadyClaimed,
		},
		{
			name: "not found",
			err:  issues.ErrNotFound,
			want: ErrCodeIssueNotFound,
		},
		{
			name: "cannot claim closed",
			err:  issues.ErrCannotClaimClosed,
			want: ErrCodeCannotClaimClosed,
		},
		{
			name: "unknown error is internal",
			err:  errors.New("connection reset"),
			want: ErrCodeInternalError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mapClaimError(context.Background(), tc.err, "user-1", "sess-caller", "sk-202")
			if res == nil {
				t.Fatalf("mapClaimError returned nil")
			}
			if !res.IsError {
				t.Fatalf("mapClaimError(%v) produced a non-error result", tc.err)
			}
			body := extractToolJSON(t, res)
			if got := body["error"]; got != tc.want {
				t.Fatalf("error = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestMapClaimError_SerializationRetryIsNotAlreadyClaimed states the sk-202
// invariant as its own assertion so a regression names itself: the false
// `already_claimed` on an unheld issue is exactly what the production
// incident looked like.
func TestMapClaimError_SerializationRetryIsNotAlreadyClaimed(t *testing.T) {
	res := mapClaimError(context.Background(), issues.ErrSerializationRetry,
		"user-1", "sess-caller", "sk-202")
	body := extractToolJSON(t, res)
	if got := body["error"]; got == ErrCodeAlreadyClaimed {
		t.Fatalf("a lost serialization race reported as %s — sk-202 regression: "+
			"the issue is not held by anyone", ErrCodeAlreadyClaimed)
	}
}
