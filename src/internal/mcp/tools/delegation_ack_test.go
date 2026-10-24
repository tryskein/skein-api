package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/issues"
)

// delegation_ack_test.go — the review finding on skein_drop_delegated
// / skein_resume_delegated, made checkable.
//
// The finding, verified by the critic on a live database: after T020 wired
// both tools to the common acknowledgement, they answered BYTE-FOR-BYTE
// identically — to each other, and to the state before either call. Neither
// store method bumps the version, neither moves status / assignee / the lock
// stamps, and dismissed_by was not on the row, so the confirmation carried
// nothing that the write had happened.
//
// That is the same rule the acceptance stamp was granted under (see
// ackFromAcceptedIssue) — "otherwise the acknowledgement does not prove the write
// applied" — and applying it to one member of the class while two others
// fail it silently is the inconsistency this file closes.
//
// The test is written against the OBSERVABLE property, not against the key:
// two calls that do opposite things must not produce the same bytes, and each
// answer must agree with what the database now holds. A test that only
// asserted `dismissed` would stay green if the key were later computed from
// the request instead of from the post-state.

// callDelegation runs one delegation action and returns (parsed, raw).
func callDelegation(
	t *testing.T,
	h func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error),
	ctx context.Context,
	args map[string]any,
) (map[string]any, string) {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError=true: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res), toolText(t, res)
}

// storedDismissed reads the caller's membership in dismissed_by straight
// from the row — the authority the acknowledgement is supposed to mirror.
func storedDismissed(t *testing.T, pool *pgxpool.Pool, issueID string, caller uuid.UUID) bool {
	t.Helper()
	var in bool
	if err := pool.QueryRow(context.Background(),
		`SELECT $2::uuid = ANY(dismissed_by) FROM issues WHERE id = $1`,
		issueID, caller).Scan(&in); err != nil {
		t.Fatalf("read dismissed_by: %v", err)
	}
	return in
}

// TestDelegationAck_ProvesTheWriteApplied is the regression gate.
func TestDelegationAck_ProvesTheWriteApplied(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, userID, "dlgack")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, userID, issueID, "delegation acknowledgement probe", "open", 2)

	store := issues.NewStore(pool)
	ctx := withCtxSession(t, userID, "admin", "sess-dlgack")

	dropH := handleDelegationAction(pool, store, true)
	resumeH := handleDelegationAction(pool, store, false)

	// A read of the pre-state through the same shape the acknowledgement
	// claims to report. Nothing has been dropped yet.
	if storedDismissed(t, pool, issueID, userID) {
		t.Fatalf("fixture is already dismissed — the probe would prove nothing")
	}

	dropOut, dropRaw := callDelegation(t, dropH, ctx, map[string]any{"id": issueID})
	if got := dropOut["dismissed"]; got != true {
		t.Errorf("drop acknowledgement dismissed = %#v, want true", got)
	}
	if !storedDismissed(t, pool, issueID, userID) {
		t.Fatalf("drop did not reach the database — the rest of this test is meaningless")
	}

	resumeOut, resumeRaw := callDelegation(t, resumeH, ctx, map[string]any{"id": issueID})
	if got := resumeOut["dismissed"]; got != false {
		t.Errorf("resume acknowledgement dismissed = %#v, want false", got)
	}
	if storedDismissed(t, pool, issueID, userID) {
		t.Fatalf("resume did not reach the database")
	}

	// The property the finding was stated as: two opposite writes cannot
	// answer with the same bytes.
	if dropRaw == resumeRaw {
		t.Errorf("drop and resume answered identically — the acknowledgement proves "+
			"nothing about which of the two ran:\n%s", dropRaw)
	}

	// And the idempotent repeat still reports the state it left behind: a
	// no-op is not an excuse to answer "nothing happened", because for the
	// caller the post-state is the same either way.
	repeatOut, _ := callDelegation(t, resumeH, ctx, map[string]any{"id": issueID})
	if got := repeatOut["dismissed"]; got != false {
		t.Errorf("idempotent resume dismissed = %#v, want false", got)
	}
	repeatDropOut, _ := callDelegation(t, dropH, ctx, map[string]any{"id": issueID})
	if got := repeatDropOut["dismissed"]; got != true {
		t.Errorf("re-drop dismissed = %#v, want true", got)
	}
	repeatDropAgain, _ := callDelegation(t, dropH, ctx, map[string]any{"id": issueID})
	if got := repeatDropAgain["dismissed"]; got != true {
		t.Errorf("idempotent re-drop dismissed = %#v, want true", got)
	}
}

// TestDelegationAck_FlagIsTheCallersOwnMembership checks the scoping half:
// `dismissed` reports THIS caller's row in dismissed_by, not "somebody
// dropped this issue". A shared-array reading would tell an agent it had
// dropped an issue it never touched.
func TestDelegationAck_FlagIsTheCallersOwnMembership(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "admin")
	otherID, otherName := seedUser(t, pool, "admin")
	pid, prefix := seedProject(t, pool, ownerID, "dlgown")
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, ownerID, issueID, "two-party delegation probe", "open", 2)

	// The other user is a PAST assignee — that is what puts them in the
	// delegation chain and lets them drop the issue at all.
	if _, err := pool.Exec(context.Background(), `
UPDATE issues
   SET previous_assignees = ARRAY[$2::uuid], assignee = $3
 WHERE id = $1`, issueID, otherID, otherName); err != nil {
		t.Fatalf("seed delegation chain: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`WITH pm_ins AS (
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, otherID, ownerID); err != nil {
		t.Fatalf("seed project member: %v", err)
	}

	store := issues.NewStore(pool)
	otherCtx := withCtxSession(t, otherID, "admin", "sess-dlgown-other")
	ownerCtx := withCtxSession(t, ownerID, "admin", "sess-dlgown-owner")

	// The other user drops it.
	out, _ := callDelegation(t, handleDelegationAction(pool, store, true), otherCtx,
		map[string]any{"id": issueID})
	if got := out["dismissed"]; got != true {
		t.Errorf("dropper's own dismissed = %#v, want true", got)
	}

	// The creator, who dropped nothing, must not be told they did. Their
	// own drop is what flips their flag.
	ownerOut, _ := callDelegation(t, handleDelegationAction(pool, store, false), ownerCtx,
		map[string]any{"id": issueID})
	if got := ownerOut["dismissed"]; got != false {
		t.Errorf("bystander's dismissed = %#v, want false — the flag is per-caller, "+
			"not \"someone dropped this issue\"", got)
	}
	// …and the other user's drop is still in place: the bystander's resume
	// must not have touched it.
	if !storedDismissed(t, pool, issueID, otherID) {
		t.Errorf("the other user's dismissal was lost")
	}
}
