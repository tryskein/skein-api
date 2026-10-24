package tools

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestAccept_HappyPath_Assignee: assignee → handler acknowledges with
// acceptedAt non-null, and the row records accepted_by = caller.
func TestAccept_HappyPath_Assignee(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "acchp")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "to be accepted", "open", 2)
	// Pre-set assignee = the user (same id) so the permission gate
	// passes. seedIssue inserts with assignee=NULL.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET assignee=$2 WHERE id=$1`, issueID, uname); err != nil {
		t.Fatalf("seed assignee: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-accept-hp")

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
	// sk-282 T020: the response is an acknowledgement, so the acceptance
	// STAMP is on the wire (it is authoritative post-state) but the
	// acceptor's id is not — that identity is read back from the row.
	if body["acceptedAt"] == nil {
		t.Fatalf("acceptedAt = nil, want non-null after accept")
	}
	if v, _ := body["version"].(float64); int(v) != 2 {
		t.Fatalf("version = %v, want 2", body["version"])
	}
	assertAcceptedBy(t, pool, issueID, userID)

	// Exactly one issue.accepted event.
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 issue.accepted event, got %d", n)
	}
}

// TestAccept_Idempotent: second call on already-accepted issue returns
// success with no new event row.
func TestAccept_Idempotent(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "accidem")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "double-accept", "open", 2)
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET assignee=$2 WHERE id=$1`, issueID, uname); err != nil {
		t.Fatalf("seed assignee: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-accept-idem")

	// First accept.
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	r1, err := handler(ctx, req)
	if err != nil || r1.IsError {
		t.Fatalf("first accept failed: %v %+v", err, r1)
	}
	body1 := extractToolJSON(t, r1)
	v1, _ := body1["version"].(float64)

	// Second accept — should be a silent no-op (same version).
	r2, err := handler(ctx, req)
	if err != nil || r2.IsError {
		t.Fatalf("second accept failed: %v %+v", err, r2)
	}
	body2 := extractToolJSON(t, r2)
	v2, _ := body2["version"].(float64)
	if v1 != v2 {
		t.Fatalf("version bumped on idempotent accept: %v -> %v", v1, v2)
	}

	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 issue.accepted event after idempotent re-call, got %d", n)
	}
}

// TestAccept_Forbidden_NonAssignee: caller is a project member but not
// the assignee, not admin → forbidden.
func TestAccept_Forbidden_NonAssignee(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	other, otherName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "accforb")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "delegated", "open", 2)
	// Assign to owner (not other).
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET assignee=(SELECT username FROM users WHERE id=$2) WHERE id=$1`,
		issueID, owner); err != nil {
		t.Fatalf("seed assignee: %v", err)
	}
	// Make `other` a member of the project so visibility passes.
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, other, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	_ = otherName

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, other, "user", "sess-accept-forb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-assignee accept")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeForbidden)
	}
}

// TestAccept_Admin_CannotAcceptOnBehalf: only the assignee accepts — the
// project owner's rights and role=admin do not let anyone accept for
// another (US-6; the admin override went in sk-674.18 T017 and stays gone
// after sk-741 gave the admin the owner's rights).
func TestAccept_Admin_CannotAcceptOnBehalf(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	admin, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, owner, "accadm")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "for-someone", "open", 2)
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET assignee=(SELECT username FROM users WHERE id=$2) WHERE id=$1`,
		issueID, owner); err != nil {
		t.Fatalf("seed assignee: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, admin, "admin", "sess-accept-adm")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError || extractToolJSON(t, res)["error"] != ErrCodeForbidden {
		t.Fatalf("admin accepted on behalf of the assignee: %+v", res)
	}
}

// assertAcceptedBy reads accepted_by off the row. The acknowledgement
// carries the stamp that MOVED (acceptedAt) but not the acceptor's id,
// so "who accepted" is proven where it is actually stored — which is
// also the stronger oracle: it survives any later change of wire shape.
func assertAcceptedBy(t *testing.T, pool *pgxpool.Pool, issueID string, want uuid.UUID) {
	t.Helper()
	var got uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`SELECT accepted_by FROM issues WHERE id = $1`, issueID).Scan(&got); err != nil {
		t.Fatalf("read accepted_by: %v", err)
	}
	if got != want {
		t.Fatalf("accepted_by = %v, want %v", got, want)
	}
}

// TestAccept_VersionConflict: stale expectedVersion → version_conflict
// with currentVersion echoed.
func TestAccept_VersionConflict(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "accvc")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "version-target", "open", 2)
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET assignee=$2 WHERE id=$1`, issueID, uname); err != nil {
		t.Fatalf("seed assignee: %v", err)
	}
	// Bump version by 1 → caller's expectedVersion=1 is stale.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET version=2, updated_at=now() WHERE id=$1`, issueID); err != nil {
		t.Fatalf("bump: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-accept-vc")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"id":              issueID,
		"expectedVersion": 1,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for stale version")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeVersionConflict {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeVersionConflict)
	}
	if cv, _ := body["currentVersion"].(float64); int(cv) != 2 {
		t.Fatalf("currentVersion = %v, want 2", body["currentVersion"])
	}
}

// TestAccept_IssueNotFound: missing id → issue_not_found.
func TestAccept_IssueNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-accept-nf")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": "nope-9999"}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing issue")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeIssueNotFound {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeIssueNotFound)
	}
}

// TestAccept_CannotAcceptClosed: accept on a closed issue → handler
// returns cannot_accept_closed and the row is not mutated (sk-25). The
// guard runs BEFORE the permission gate, so we use the assignee path —
// if the guard ever regresses to "forbidden", the test would still
// catch the misordering because it asserts on the precise code.
func TestAccept_CannotAcceptClosed(t *testing.T) {
	pool := newTestPool(t)
	userID, uname := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, userID, "accclosed")
	issueID := prefix + "-1"
	// Seed as open with valid priority, then flip to closed via UPDATE
	// (status is constrained at the row level; seedIssue only accepts
	// open/in_progress/etc., and we need the row to look exactly like
	// a real closed issue for Accept's guard to fire).
	seedIssue(t, pool, pid, userID, issueID, "already-done", "open", 2)
	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET assignee=$2, status='closed', closed_at=now(),
                  closed_reason='test setup', version=version+1,
                  updated_at=now()
 WHERE id=$1`, issueID, uname); err != nil {
		t.Fatalf("seed assignee+closed: %v", err)
	}
	var preVersion int
	if err := pool.QueryRow(t.Context(),
		`SELECT version FROM issues WHERE id=$1`, issueID).Scan(&preVersion); err != nil {
		t.Fatalf("read pre version: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-accept-closed")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for closed issue accept")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeCannotAcceptClosed {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeCannotAcceptClosed)
	}

	// Row was NOT mutated: version stays at preVersion, accepted_at
	// stays NULL, no issue.accepted event was written.
	var version int
	var acceptedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT version, accepted_at FROM issues WHERE id=$1`, issueID,
	).Scan(&version, &acceptedAt); err != nil {
		t.Fatalf("re-read row: %v", err)
	}
	if version != preVersion {
		t.Fatalf("version = %d, want %d (no bump after rejected Accept)", version, preVersion)
	}
	if acceptedAt != nil {
		t.Fatalf("accepted_at = %v, want NULL (no stamp)", acceptedAt)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 issue.accepted events, got %d", n)
	}
}

// TestAccept_CannotAcceptClosed_NonAssignee: closed-guard runs BEFORE
// the permission gate, so a non-assignee asking to accept a closed row
// gets the precise reason (cannot_accept_closed), not a misleading
// 'forbidden'. Mirrors sk-25 design intent.
func TestAccept_CannotAcceptClosed_NonAssignee(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	other, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "accclosednon")
	issueID := prefix + "-1"
	// Same seed-then-close trick as TestAccept_CannotAcceptClosed.
	seedIssue(t, pool, pid, owner, issueID, "done-and-foreign", "open", 2)
	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET assignee=(SELECT username FROM users WHERE id=$2),
                  status='closed', closed_at=now(), closed_reason='test setup',
                  version=version+1, updated_at=now()
 WHERE id=$1`, issueID, owner); err != nil {
		t.Fatalf("seed assignee+closed: %v", err)
	}
	// Make `other` a project member so visibility passes.
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, other, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, other, "user", "sess-accept-closed-non")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": issueID}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for closed issue accept by non-assignee")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeCannotAcceptClosed {
		t.Fatalf("error = %v, want %s (guard MUST precede permission gate)",
			body["error"], ErrCodeCannotAcceptClosed)
	}
}

// TestAccept_BareIDRejected: a bare numeric id (no prefix) is rejected
// at the looksLikeHierarchicalID gate before any DB call.
func TestAccept_BareIDRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := issues.NewStore(pool)
	handler := handleAccept(pool, store)
	ctx := withCtxSession(t, userID, "user", "sess-accept-bare")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"id": "42"}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for bare id")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
}
