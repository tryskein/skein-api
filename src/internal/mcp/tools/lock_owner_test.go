package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// sk-424 — the right to dispose of a claimed issue belongs to the user who
// claimed it, not to whoever can name its MCP session.
//
// # What these tests pin, and why the negative half is the point
//
// The lock holder used to be identified by ONE value: the `Mcp-Session-Id`
// string in `issues.locked_by`. That string is not a credential. The
// credential is the api-key, which resolves to a user; the session id names
// an agent INSTANCE, rotates on reconnect, is shown to every reader of the
// issue (`lockedBy` on the card, `actorSessionId` in the event feed), and is
// owned by nobody — `agents` is keyed on the PAIR (user_id, session_id), so
// the same session string legitimately exists under two users at once. A
// check that compared only that string therefore let one user release, close
// and edit another user's claimed issue.
//
// So a test that only shows the legitimate holder CAN act proves nothing
// about this: the old, defective code passed that test too. Each case below
// asserts BOTH halves against the same lock —
//
//	(1) a DIFFERENT user, presenting the holder's own session id, is refused
//	    AND the lock is still standing afterwards;
//	(2) the real holder still succeeds.
//
// Half (1) is what regresses if the owner check is ever dropped; half (2) is
// what regresses if the check is made too strict.
//
// # Why the assertions name an exact error code
//
// Refusal is checked by code, never by "some error happened". Before it
// reaches the lock check every one of these tools resolves the issue and
// tests project visibility, and a caller who cannot see the project is told
// `issue_not_found`. A test whose other user was not a project member would
// be refused for THAT reason and go green with the lock check never
// consulted — a pass indistinguishable from a real one. The setup therefore
// adds the other user to the project and asserts they can read the issue
// before any attempt is made, and the attempts assert the lock code itself.

type lockOwnerEnv struct {
	pool     *pgxpool.Pool
	store    *issues.Store
	holder   uuid.UUID // claimed the issue
	holderNm string
	other    uuid.UUID // a different user who can SEE the issue
	otherNm  string
	issueID  string
	session  string // the holder's session id
}

// setupLockOwnerEnv builds one project containing one issue, claimed by
// `holder` through the real claim tool, plus a second project member who did
// not claim it. It asserts the claim actually took and that the other user
// can read the issue, so a later refusal cannot be mistaken for invisibility.
func setupLockOwnerEnv(t *testing.T, hint string) lockOwnerEnv {
	t.Helper()
	pool := newTestPool(t)
	holder, holderNm := seedUser(t, pool, "user")
	other, otherNm := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, holder, hint)

	// The other user must be a project member, otherwise every tool below
	// refuses with issue_not_found long before the lock check — see the
	// file header.
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, other, holder); err != nil {
		t.Fatalf("add %s to the project: %v", otherNm, err)
	}

	issueID := prefix + "-1"
	seedIssue(t, pool, pid, holder, issueID, "held", "open", 2)
	store := issues.NewStore(pool)
	session := "sess-holder-" + hint

	cl := mcpgo.CallToolRequest{}
	cl.Params.Arguments = map[string]any{"id": issueID}
	if r, err := handleClaim(pool, store)(
		withCtxSession(t, holder, "user", session), cl); err != nil || r.IsError {
		t.Fatalf("seed claim failed: %v %+v", err, r)
	}
	// The lock is real and owned by the holder: without this the negative
	// halves below could be passing against an unlocked issue.
	assertLockHolder(t, store, holder, issueID, holderNm, "after the holder claimed")

	// The other user really can read the issue, so a refusal below is the
	// lock talking and not visibility.
	sh := mcpgo.CallToolRequest{}
	sh.Params.Arguments = map[string]any{"id": issueID}
	shRes, err := handleShow(pool, store, dependencies.NewStore(pool))(
		withCtxSession(t, other, "user", "sess-other-read"), sh)
	if err != nil || shRes.IsError {
		t.Fatalf("the other user cannot read the issue, so every refusal below "+
			"would be issue_not_found rather than the lock check: %v %+v", err, shRes)
	}

	return lockOwnerEnv{
		pool: pool, store: store,
		holder: holder, holderNm: holderNm,
		other: other, otherNm: otherNm,
		issueID: issueID, session: session,
	}
}

// assertRefusedWithCode fails unless the tool refused with exactly wantCode.
func assertRefusedWithCode(t *testing.T, res *mcpgo.CallToolResult, err error, wantCode, when string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: transport error %v, want a refusal carrying %q", when, err, wantCode)
	}
	if res == nil || !res.IsError {
		t.Fatalf("%s: the call SUCCEEDED, want refusal %q — "+
			"another user disposed of a lock that is not theirs", when, wantCode)
	}
	if got := extractToolJSON(t, res)["error"]; got != wantCode {
		t.Fatalf("%s: refused with %v, want %q", when, got, wantCode)
	}
}

// TestLockOwner_OtherUserWithHolderSessionIsRefused is the sk-424 regression:
// for each lock-guarded tool, a different user presenting the holder's own
// session id is refused and the lock survives, while the holder still
// succeeds on that same lock.
func TestLockOwner_OtherUserWithHolderSessionIsRefused(t *testing.T) {
	t.Run("unclaim", func(t *testing.T) {
		e := setupLockOwnerEnv(t, "lounc")
		args := map[string]any{
			"id": e.issueID, "nextStatus": "open", "reason": "sk-424 row",
		}

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := handleUnclaim(e.pool, e.store)(
			withCtxSession(t, e.other, "user", e.session), req)
		assertRefusedWithCode(t, res, err, ErrCodeNotLockHolder,
			"another user released the holder's lock")
		assertLockHolder(t, e.store, e.holder, e.issueID, e.holderNm,
			"after the refused release the lock must still stand")

		// The holder's own release still works.
		own := mcpgo.CallToolRequest{}
		own.Params.Arguments = args
		if r, err := handleUnclaim(e.pool, e.store)(
			withCtxSession(t, e.holder, "user", e.session), own); err != nil || r.IsError {
			t.Fatalf("the holder could not release their own lock: %v %+v", err, r)
		}
		assertLockHolder(t, e.store, e.holder, e.issueID, "",
			"after the holder released")
	})

	t.Run("close", func(t *testing.T) {
		e := setupLockOwnerEnv(t, "loclo")
		args := map[string]any{
			"id": e.issueID, "expectedVersion": 2, "closeOutcome": "done", "reason": "sk-424 row",
		}

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := handleClose(e.pool, e.store)(
			withCtxSession(t, e.other, "user", e.session), req)
		assertRefusedWithCode(t, res, err, ErrCodeLockedByOther,
			"another user closed the holder's claimed issue")
		assertLockHolder(t, e.store, e.holder, e.issueID, e.holderNm,
			"after the refused close the lock must still stand")

		own := mcpgo.CallToolRequest{}
		own.Params.Arguments = args
		if r, err := handleClose(e.pool, e.store)(
			withCtxSession(t, e.holder, "user", e.session), own); err != nil || r.IsError {
			t.Fatalf("the holder could not close their own issue: %v %+v", err, r)
		}
	})

	t.Run("update", func(t *testing.T) {
		e := setupLockOwnerEnv(t, "loupd")

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"id": e.issueID, "expectedVersion": 2, "title": "edited by the other user",
		}
		res, err := handleUpdate(e.pool, e.store)(
			withCtxSession(t, e.other, "user", e.session), req)
		assertRefusedWithCode(t, res, err, ErrCodeLockedByOther,
			"another user edited the holder's claimed issue")
		assertLockHolder(t, e.store, e.holder, e.issueID, e.holderNm,
			"after the refused edit the lock must still stand")

		// The refusal wrote nothing.
		var title string
		if err := e.pool.QueryRow(context.Background(),
			`SELECT title FROM issues WHERE id = $1`, e.issueID).Scan(&title); err != nil {
			t.Fatalf("re-reading the title: %v", err)
		}
		if title != "held" {
			t.Fatalf("title = %q after a refused edit, want it untouched (%q)", title, "held")
		}

		own := mcpgo.CallToolRequest{}
		own.Params.Arguments = map[string]any{
			"id": e.issueID, "expectedVersion": 2, "title": "edited by the holder",
		}
		if r, err := handleUpdate(e.pool, e.store)(
			withCtxSession(t, e.holder, "user", e.session), own); err != nil || r.IsError {
			t.Fatalf("the holder could not edit their own issue: %v %+v", err, r)
		}
	})

	t.Run("claim is not a takeover either", func(t *testing.T) {
		e := setupLockOwnerEnv(t, "loclm")

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"id": e.issueID}
		res, err := handleClaim(e.pool, e.store)(
			withCtxSession(t, e.other, "user", e.session), req)
		assertRefusedWithCode(t, res, err, ErrCodeAlreadyClaimed,
			"another user re-claimed the holder's lock as if it were their own")
		assertLockHolder(t, e.store, e.holder, e.issueID, e.holderNm,
			"after the refused claim the lock must still stand")
	})
}

// TestLockOwner_LegitimatePathsUnchanged guards the other direction: the fix
// tightens WHO the holder is, and must not narrow the paths that were always
// allowed to act on somebody else's lock, nor widen the one that never was.
func TestLockOwner_LegitimatePathsUnchanged(t *testing.T) {
	t.Run("admin force still releases another user's lock", func(t *testing.T) {
		e := setupLockOwnerEnv(t, "lofrc")
		admin, _ := seedUser(t, e.pool, "admin")

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"id": e.issueID, "nextStatus": "open",
			"reason": "sk-424 admin override", "force": true,
		}
		if r, err := handleUnclaim(e.pool, e.store)(
			withCtxSession(t, admin, "admin", "sess-admin"), req); err != nil || r.IsError {
			t.Fatalf("admin force-release refused: %v %+v", err, r)
		}
		assertLockHolder(t, e.store, e.holder, e.issueID, "",
			"after the admin force-released")
	})

	// Documented, pre-existing behaviour: locks are per AGENT INSTANCE, so a
	// sibling session of the SAME user is not the holder either. sk-424 kept
	// the session half of the check precisely so this stays true — had the
	// basis been widened to the user alone, every sibling instance would have
	// gained authority over the others' locks.
	t.Run("same user, different session, is still not the holder", func(t *testing.T) {
		e := setupLockOwnerEnv(t, "losib")

		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"id": e.issueID, "nextStatus": "open", "reason": "sibling session",
		}
		res, err := handleUnclaim(e.pool, e.store)(
			withCtxSession(t, e.holder, "user", e.session+"-sibling"), req)
		assertRefusedWithCode(t, res, err, ErrCodeNotLockHolder,
			"a sibling session of the same user released the lock")
		assertLockHolder(t, e.store, e.holder, e.issueID, e.holderNm,
			"after the refused sibling release the lock must still stand")
	})
}
