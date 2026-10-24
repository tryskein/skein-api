package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// TestUnclaim_LockHolderNameFollowsTheLock is sk-421.1 test-plan row 15
// (US-15, INV-7, SC-005), release half: each release path gets its own real
// claim (CLAIM-FIX item 1) and asserts the holder's name BEFORE releasing —
// otherwise «the field is absent afterwards» would pass with no mechanism.
func TestUnclaim_LockHolderNameFollowsTheLock(t *testing.T) {
	type env struct {
		pool      *pgxpool.Pool
		store     *issues.Store
		u         uuid.UUID
		uName     string
		admin     uuid.UUID
		adminName string
		issueID   string
	}
	setup := func(t *testing.T, hint string) env {
		t.Helper()
		pool := newTestPool(t)
		u, uName := seedUser(t, pool, "user")
		admin, adminName := seedUser(t, pool, "admin")
		pid, prefix := seedProject(t, pool, u, hint)
		issueID := prefix + "-1"
		seedIssue(t, pool, pid, u, issueID, "held", "open", 2)
		// The admin claims by his own role in the project: beyond it an
		// admin neither writes issues nor becomes the assignee (US-6, T066).
		addHolderMember(t, pool, pid, admin, u)
		store := issues.NewStore(pool)

		cl := mcpgo.CallToolRequest{}
		cl.Params.Arguments = map[string]any{"id": issueID}
		if r, err := handleClaim(pool, store)(withCtxSession(t, u, "user", "sess-U"), cl); err != nil || r.IsError {
			t.Fatalf("seed claim failed: %v %+v", err, r)
		}
		assertLockHolder(t, store, admin, issueID, uName, "before release")
		return env{pool, store, u, uName, admin, adminName, issueID}
	}
	unclaim := func(t *testing.T, e env, ctx context.Context, force bool) {
		t.Helper()
		uc := mcpgo.CallToolRequest{}
		args := map[string]any{"id": e.issueID, "nextStatus": "open", "reason": "row 15 release"}
		if force {
			args["force"] = true
		}
		uc.Params.Arguments = args
		res, err := handleUnclaim(e.pool, e.store)(ctx, uc)
		if err != nil || res.IsError {
			t.Fatalf("unclaim failed: %v %+v", err, res)
		}
	}

	t.Run("holder releases, then a new claim names its performer", func(t *testing.T) {
		e := setup(t, "ulhnrel")
		unclaim(t, e, withCtxSession(t, e.u, "user", "sess-U"), false)
		assertLockHolder(t, e.store, e.admin, e.issueID, "", "after the holder released")

		// A new claim by another permitted user: the admin, naming the
		// current assignee (U stays the assignee after release).
		cl := mcpgo.CallToolRequest{}
		cl.Params.Arguments = map[string]any{"id": e.issueID, "assignee": e.uName}
		if r, err := handleClaim(e.pool, e.store)(withCtxSession(t, e.admin, "admin", "sess-admin"), cl); err != nil || r.IsError {
			t.Fatalf("new claim failed: %v %+v", err, r)
		}
		assertLockHolder(t, e.store, e.admin, e.issueID, e.adminName, "after the new claim")
	})

	t.Run("admin force-releases another session's lock", func(t *testing.T) {
		e := setup(t, "ulhnforce")
		unclaim(t, e, withCtxSession(t, e.admin, "admin", "sess-admin"), true)
		assertLockHolder(t, e.store, e.admin, e.issueID, "", "after the forced release")
	})
}

// TestUnclaim_HappyPath: I hold the lock → unclaim → success, version
// bumps, locked_by cleared, event written.
func TestUnclaim_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulhp")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "to-unclaim", "open", 2)

	store := issues.NewStore(pool)
	claimHandler := handleClaim(pool, store)
	unclaimHandler := handleUnclaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-mine")

	// Claim first.
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctx, cl); err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}

	// Now unclaim — sk-19 requires nextStatus + reason.
	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "open",
		"reason":     "test happy path",
	}
	res, err := unclaimHandler(ctx, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if body["lockedBy"] != nil {
		t.Fatalf("lockedBy = %v, want nil", body["lockedBy"])
	}
	// Initial seed = v1; claim → v2; unclaim → v3.
	if v, _ := body["version"].(float64); int(v) != 3 {
		t.Fatalf("version = %v, want 3", body["version"])
	}

	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.unclaimed'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected one issue.unclaimed event, got %d", n)
	}
}

// TestUnclaim_NotLockHolder: caller is NOT the lock holder → not_lock_holder.
func TestUnclaim_NotLockHolder(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulnlh")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "owned-elsewhere", "open", 2)

	store := issues.NewStore(pool)
	claimHandler := handleClaim(pool, store)
	unclaimHandler := handleUnclaim(pool, store)

	// Session-A claims.
	ctxA := withCtxSession(t, userID, "admin", "sess-A")
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctxA, cl); err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}

	// Session-B (same user) tries to unclaim without force.
	ctxB := withCtxSession(t, userID, "admin", "sess-B")
	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "open",
		"reason":     "trying to steal",
	}
	res, err := unclaimHandler(ctxB, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-holder unclaim")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeNotLockHolder {
		t.Fatalf("error = %v, want %s", got, ErrCodeNotLockHolder)
	}
}

// TestUnclaim_AlreadyUnlockedIdempotent: unclaim of an issue that is
// not locked → success no-op (no version bump, no event).
func TestUnclaim_AlreadyUnlockedIdempotent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulidp")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "free", "open", 2)

	store := issues.NewStore(pool)
	handler := handleUnclaim(pool, store)
	ctx := withCtxSession(t, userID, "admin", "sess-X")

	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "open",
		"reason":     "idempotent no-op",
	}
	res, err := handler(ctx, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected idempotent success on already-unlocked: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if v, _ := body["version"].(float64); int(v) != 1 {
		t.Fatalf("version = %v, want 1 (no bump)", body["version"])
	}

	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.unclaimed'`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected zero events on no-op unclaim, got %d", n)
	}
}

// TestUnclaim_AdminForceOnOtherSession: admin uses force=true to drop
// another session's lock → success, event reason="force-overridden".
func TestUnclaim_AdminForceOnOtherSession(t *testing.T) {
	pool := newTestPool(t)
	admin, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, admin, "ulafo")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, admin, issueID, "stuck", "open", 2)

	store := issues.NewStore(pool)
	claimHandler := handleClaim(pool, store)
	unclaimHandler := handleUnclaim(pool, store)

	// Session-A holds the lock.
	ctxA := withCtxSession(t, admin, "admin", "sess-A")
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := claimHandler(ctxA, cl); err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}

	// Session-B (admin) forces unclaim.
	ctxB := withCtxSession(t, admin, "admin", "sess-B")
	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"force":      true,
		"nextStatus": "open",
		"reason":     "admin clearing stuck lock",
	}
	res, err := unclaimHandler(ctxB, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected admin force success: %+v", extractToolJSON(t, res))
	}

	var reason string
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(reason, '') FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.unclaimed'
 ORDER BY id DESC LIMIT 1`,
		issueID).Scan(&reason); err != nil {
		t.Fatalf("query event reason: %v", err)
	}
	// sk-19: reason is now the caller's input with " (force-overridden)" appended.
	wantReason := "admin clearing stuck lock (force-overridden)"
	if reason != wantReason {
		t.Fatalf("event reason = %q, want %q", reason, wantReason)
	}
}

// forceFixture is a project of owner with one open issue claimed by
// owner's session, and a member of the project — a participant of its
// space with no owner role (sk-674.18 T034): force_unlock is the project
// owner's, the space owner's and the instance admin's, never a member's.
type forceFixture struct {
	pool    *pgxpool.Pool
	issueID string
	member  uuid.UUID
	// owner is the project owner — force_unlock is his (US-16).
	owner uuid.UUID
}

func newForceFixture(t *testing.T, prefixHint string) forceFixture {
	t.Helper()
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	member, _ := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, prefixHint)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "x", "open", 2)
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET locked_by = 'sess-holder', locked_by_user_id = $2, locked_at = now() WHERE id = $1`,
		issueID, owner); err != nil {
		t.Fatalf("seed the owner's claim: %v", err)
	}
	addProjectMember(t, pool, pid, member)
	return forceFixture{pool: pool, issueID: issueID, member: member, owner: owner}
}

// assertReleased checks that a forced call of the owner went through: the
// owner's claim-free state — no lock left on the issue.
func (f forceFixture) assertReleased(t *testing.T) {
	t.Helper()
	var lockedBy *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT locked_by FROM issues WHERE id = $1`, f.issueID).Scan(&lockedBy); err != nil {
		t.Fatalf("re-read issue: %v", err)
	}
	if lockedBy != nil {
		t.Errorf("the owner's forced call left the claim (locked_by %q)", *lockedBy)
	}
}

// assertForceRefused checks the refusal code and that the claim and the
// status are what the fixture left: nothing was written.
func (f forceFixture) assertForceRefused(t *testing.T, code any) {
	t.Helper()
	if code != ErrCodeForceUnlockForbidden {
		t.Errorf("error = %v, want %s", code, ErrCodeForceUnlockForbidden)
	}
	var lockedBy *string
	var status string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT locked_by, status FROM issues WHERE id = $1`, f.issueID).Scan(&lockedBy, &status); err != nil {
		t.Fatalf("re-read issue: %v", err)
	}
	if lockedBy == nil || *lockedBy != "sess-holder" || status != "open" {
		t.Errorf("a refused force changed the issue: locked_by=%v status=%s", lockedBy, status)
	}
}

// TestUnclaim_OwnerForceAllowed — row 188 (sk-674.18, US-16): the project
// owner, who is no instance admin, now holds force_unlock and releases
// another holder's claim.
func TestUnclaim_OwnerForceAllowed(t *testing.T) {
	f := newForceFixture(t, "ulown")
	handler := handleUnclaim(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.owner, "user", "sess-owner")
	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{"id": f.issueID, "force": true,
		"nextStatus": "open", "reason": "taking it over"}
	res, err := handler(ctx, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("the project owner's forced unclaim was refused: %+v", extractToolJSON(t, res))
	}
	f.assertReleased(t)
}

// TestUnclaim_MemberForceForbidden — force=true releases another holder's
// claim only with force_unlock (US-16, K-28): a project member gets
// force_unlock_forbidden and the claim stays.
func TestUnclaim_MemberForceForbidden(t *testing.T) {
	f := newForceFixture(t, "ulnaf")
	handler := handleUnclaim(f.pool, issues.NewStore(f.pool))
	ctx := withCtxSession(t, f.member, "user", "sess-Y")

	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{"id": f.issueID, "force": true,
		"nextStatus": "open", "reason": "taking it over"}
	res, err := handler(ctx, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for a member's force")
	}
	f.assertForceRefused(t, extractToolJSON(t, res)["error"])
}

// sk-19 — unclaim with required nextStatus enum + reason.

// claimAndExpectInProgress is a small test fixture: seed an open issue,
// claim it from `sessionID`, and assert the body shows status=in_progress
// (validating sk-18's claim semantics in passing). Returns the issue id.
func claimAndExpectInProgress(t *testing.T, pool *pgxpool.Pool, store *issues.Store, projectID uuid.UUID, owner uuid.UUID, prefix, sessionID string) string {
	t.Helper()
	issueID := prefix + "-1"
	seedIssue(t, pool, projectID, owner, issueID, "for-unclaim", "open", 2)

	ctx := withCtxSession(t, owner, "admin", sessionID)
	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	r, err := handleClaim(pool, store)(ctx, cl)
	if err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}
	body := extractToolJSON(t, r)
	if got := body["status"]; got != "in_progress" {
		t.Fatalf("after seed claim, status = %v, want in_progress", got)
	}
	return issueID
}

// TestUnclaim_HandoffStaysInProgress: nextStatus="in_progress" — lock
// released, status stays in_progress, NO issue.status_changed event.
func TestUnclaim_HandoffStaysInProgress(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulho")
	store := issues.NewStore(pool)
	issueID := claimAndExpectInProgress(t, pool, store, pid, userID, prefix, "sess-h")

	ctx := withCtxSession(t, userID, "admin", "sess-h")
	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "in_progress",
		"reason":     "handing off — see comment",
	}
	res, err := handleUnclaim(pool, store)(ctx, uc)
	if err != nil || res.IsError {
		t.Fatalf("handoff unclaim failed: %v %+v", err, extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if got := body["status"]; got != "in_progress" {
		t.Fatalf("status = %v, want in_progress (handoff)", got)
	}
	if body["lockedBy"] != nil {
		t.Fatalf("lockedBy = %v, want nil", body["lockedBy"])
	}

	// Exactly one issue.unclaimed, ZERO issue.status_changed from unclaim
	// (the claim's status_changed must not be counted — we filter by
	// reason content to exclude the "claim" sentinel).
	var statusChangedFromUnclaim int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1
   AND type='issue.status_changed'
   AND COALESCE(reason,'') NOT IN ('claim')`,
		issueID).Scan(&statusChangedFromUnclaim); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if statusChangedFromUnclaim != 0 {
		t.Fatalf("expected NO status_changed on handoff unclaim, got %d", statusChangedFromUnclaim)
	}
}

// TestUnclaim_GiveUpRevertsToOpen: nextStatus="open" — lock released,
// status → open, status_changed event with the caller's reason.
func TestUnclaim_GiveUpRevertsToOpen(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulgu")
	store := issues.NewStore(pool)
	issueID := claimAndExpectInProgress(t, pool, store, pid, userID, prefix, "sess-g")

	ctx := withCtxSession(t, userID, "admin", "sess-g")
	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "open",
		"reason":     "stuck on auth flow, returning to pool",
	}
	res, err := handleUnclaim(pool, store)(ctx, uc)
	if err != nil || res.IsError {
		t.Fatalf("give-up unclaim failed: %v %+v", err, extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if got := body["status"]; got != "open" {
		t.Fatalf("status = %v, want open (give-up)", got)
	}

	var reason string
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(reason,'') FROM events
 WHERE entity_type='issue' AND entity_id=$1
   AND type='issue.status_changed'
   AND COALESCE(reason,'') NOT IN ('claim')
 ORDER BY id DESC LIMIT 1`, issueID).Scan(&reason); err != nil {
		t.Fatalf("query status_changed reason: %v", err)
	}
	if reason != "stuck on auth flow, returning to pool" {
		t.Fatalf("status_changed reason = %q, want caller's reason", reason)
	}
}

// TestUnclaim_PuntDeferred: nextStatus="deferred" — lock released,
// status → deferred.
func TestUnclaim_PuntDeferred(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulpd")
	store := issues.NewStore(pool)
	issueID := claimAndExpectInProgress(t, pool, store, pid, userID, prefix, "sess-p")

	ctx := withCtxSession(t, userID, "admin", "sess-p")
	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "deferred",
		"reason":     "wait for upstream API to ship",
	}
	res, err := handleUnclaim(pool, store)(ctx, uc)
	if err != nil || res.IsError {
		t.Fatalf("punt unclaim failed: %v %+v", err, extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	if got := body["status"]; got != "deferred" {
		t.Fatalf("status = %v, want deferred (punt)", got)
	}
}

// TestUnclaim_MissingNextStatus: required nextStatus param absent →
// validation_error before any store interaction.
func TestUnclaim_MissingNextStatus(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulmns")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "x", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-q")

	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{"id": issueID, "reason": "x"}
	res, err := handleUnclaim(pool, store)(ctx, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation_error for missing nextStatus")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestUnclaim_RejectsClosedNextStatus: nextStatus="closed" is rejected
// at the handler boundary — closing has its own tool.
func TestUnclaim_RejectsClosedNextStatus(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "ulrc")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "x", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-q")

	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "closed",
		"reason":     "should be rejected",
	}
	res, err := handleUnclaim(pool, store)(ctx, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation_error for nextStatus=closed")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestUnclaim_EmptyReasonRejected: reason="   " trims to empty →
// validation_error.
func TestUnclaim_EmptyReasonRejected(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "uler")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "x", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-q")

	uc := mcpgo.CallToolRequest{}
	uc.Params.Arguments = map[string]any{
		"id":         issueID,
		"nextStatus": "open",
		"reason":     "   ",
	}
	res, err := handleUnclaim(pool, store)(ctx, uc)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation_error for empty reason")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}
