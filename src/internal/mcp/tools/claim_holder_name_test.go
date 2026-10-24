package tools

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
)

// sk-421.1 T002 (US-15, INV-7, SC-005) — test-plan rows 17–19: the lock
// holder's name is the user who PERFORMED the claim that holds the lock now,
// and only a change of the lock can change it.
//
// Every scenario first takes a real claim through the MCP handler (the
// issue.claimed event lands in the claim's own transaction, CLAIM-FIX
// item 1) and asserts the holder's name BEFORE the step under test, so an
// «absent» or «unchanged» afterwards cannot pass without the mechanism.
//
// These rows once relied on a defect, and no longer do. When they were
// written the holder check on unclaim / update / close compared the SESSION
// ID alone, so another user presenting the holder's session passed it; the
// header said so, marked it sk-424 and out of scope, and used the property
// without endorsing it. sk-424 closed it: the holder is now the PAIR (owning
// user, session), and a foreign user presenting the holder's session is
// refused. The fixtures below were rebuilt on legitimate paths — the holder
// releases its own lock, or an admin uses force — because the property these
// rows actually pin is «the holder's name follows the LOCK, and names the
// performer of the claim that holds it», which never needed the takeover.
// The refusal itself is pinned in lock_owner_test.go, not here.

func toolCall(t *testing.T, h server.ToolHandlerFunc, ctx context.Context, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return res
}

func toolOK(t *testing.T, h server.ToolHandlerFunc, ctx context.Context, what string, args map[string]any) {
	t.Helper()
	if res := toolCall(t, h, ctx, args); res.IsError {
		t.Fatalf("%s failed: %+v", what, extractToolJSON(t, res))
	}
}

func addHolderMember(t *testing.T, pool *pgxpool.Pool, pid, user, addedBy uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, user, addedBy); err != nil {
		t.Fatalf("add member: %v", err)
	}
}

type lockRow struct {
	lockedBy *string
	lockedAt *time.Time
	version  int
}

func readLockRow(t *testing.T, pool *pgxpool.Pool, id string) lockRow {
	t.Helper()
	var r lockRow
	if err := pool.QueryRow(context.Background(),
		`SELECT locked_by, locked_at, version FROM issues WHERE id = $1`, id).
		Scan(&r.lockedBy, &r.lockedAt, &r.version); err != nil {
		t.Fatalf("read lock row %s: %v", id, err)
	}
	return r
}

func countClaimEvents(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.claimed'`, id).
		Scan(&n); err != nil {
		t.Fatalf("count claim events: %v", err)
	}
	return n
}

// seedForeignClaimEvent writes an issue.claimed record that LOOKS like a
// claim of session `session` by user X — new_data.lockedBy and
// actor_session_id both carry the session — at a moment that is not the
// lock's moment (CLAIM-FIX item 4: without new_data.lockedBy such a record
// would not even tempt the mechanism).
func seedForeignClaimEvent(t *testing.T, pool *pgxpool.Pool, issueID string, projectID, x uuid.UUID, xName, session string, occurredAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO events (type, actor, actor_user_id, actor_session_id, actor_type,
                    entity_type, entity_id, project_id, field, old_data, new_data, reason, payload, occurred_at)
VALUES ('issue.claimed', $1::text, $2, $3::text, 'agent',
        'issue', $4, $5, 'assignee',
        '{"assignee":"","lockedBy":null}'::jsonb,
        jsonb_build_object('assignee', $1::text, 'lockedBy', $3::text),
        NULL, '{}'::jsonb, $6)`,
		xName, x, session, issueID, projectID, occurredAt); err != nil {
		t.Fatalf("seed foreign claim event: %v", err)
	}
}

type holderEnv struct {
	pool    *pgxpool.Pool
	store   *issues.Store
	pid     uuid.UUID
	issueID string
	claim   server.ToolHandlerFunc
	unclaim server.ToolHandlerFunc
}

func newHolderEnv(t *testing.T, owner uuid.UUID, pool *pgxpool.Pool, hint string) holderEnv {
	t.Helper()
	pid, prefix := seedProject(t, pool, owner, hint)
	issueID := prefix + "-1"
	seedIssue(t, pool, pid, owner, issueID, "held", "open", 2)
	store := issues.NewStore(pool)
	return holderEnv{
		pool: pool, store: store, pid: pid, issueID: issueID,
		claim: handleClaim(pool, store), unclaim: handleUnclaim(pool, store),
	}
}

// Row 17 — another user V, presenting the holder's session S, sends requests
// that do not change the lock: show, comment, an update refused on a stale
// version, and an accepted update of a field outside the lock. The lock's
// session and moment stay, and so does the name U.
func TestLockHolderName_ForeignRequestsWithHolderSessionDoNotRename(t *testing.T) {
	pool := newTestPool(t)
	u, uName := seedUser(t, pool, "user")
	v, _ := seedUser(t, pool, "user")
	e := newHolderEnv(t, u, pool, "lhn17")
	addHolderMember(t, pool, e.pid, v, u)

	const s = "sess-holder-S"
	toolOK(t, e.claim, withCtxSession(t, u, "user", s), "claim by U", map[string]any{"id": e.issueID})
	assertLockHolder(t, e.store, u, e.issueID, uName, "after U claimed")
	lockBefore := readLockRow(t, pool, e.issueID)

	ctxV := withCtxSession(t, v, "user", s)
	callShow(t, handleShow(pool, e.store, dependencies.NewStore(pool)), ctxV,
		map[string]any{"id": e.issueID, "includeComments": false})
	toolOK(t, handleComment(pool, e.store), ctxV, "comment by V",
		map[string]any{"issueId": e.issueID, "body": "V speaks with S", "kind": "autonomous"})

	// Refused: V is not the holder. Since sk-424 the holder is the pair
	// (owning user, session), so presenting U's session id no longer makes V
	// one — and the lock refuses before the version is ever consulted.
	res := toolCall(t, handleUpdate(pool, e.store), ctxV,
		map[string]any{"id": e.issueID, "expectedVersion": 1, "notes": "stale"})
	if !res.IsError {
		t.Fatalf("update by V succeeded; want %s", ErrCodeLockedByOther)
	}
	if body := extractToolJSON(t, res); body["error"] != ErrCodeLockedByOther {
		t.Fatalf("update by V: error = %v, want %s (%v)", body["error"], ErrCodeLockedByOther, body)
	}

	// Refused on the VERSION, by the real holder: the other kind of request
	// that gets turned away without touching the lock.
	ctxU := withCtxSession(t, u, "user", s)
	res = toolCall(t, handleUpdate(pool, e.store), ctxU,
		map[string]any{"id": e.issueID, "expectedVersion": 1, "notes": "stale"})
	if !res.IsError {
		t.Fatalf("stale update by U succeeded; want version_conflict")
	}
	if body := extractToolJSON(t, res); body["error"] != "version_conflict" {
		t.Fatalf("stale update by U: error = %v, want version_conflict (%v)", body["error"], body)
	}

	// Accepted: a field outside the lock, written by the holder.
	current := readLockRow(t, pool, e.issueID).version
	toolOK(t, handleUpdate(pool, e.store), ctxU, "update notes by U",
		map[string]any{"id": e.issueID, "expectedVersion": current, "notes": "U edited notes with S"})

	lockAfter := readLockRow(t, pool, e.issueID)
	if lockAfter.lockedBy == nil || *lockAfter.lockedBy != s ||
		lockAfter.lockedAt == nil || !lockAfter.lockedAt.Equal(*lockBefore.lockedAt) {
		t.Fatalf("the lock changed: before (%v, %v) after (%v, %v) — fixture no longer isolates "+
			"«a request that does not change the lock»", lockBefore.lockedBy, lockBefore.lockedAt,
			lockAfter.lockedBy, lockAfter.lockedAt)
	}
	assertLockHolder(t, e.store, u, e.issueID, uName, "after V's requests, read by U")
	assertLockHolder(t, e.store, v, e.issueID, uName, "after V's requests, read by V")
}

// Row 18 — another user V, presenting S, releases and claims again.
func TestLockHolderName_ForeignUserReleasesAndReclaimsWithHolderSession(t *testing.T) {
	const s = "sess-holder-S"

	t.Run("(a) V is the assignee: the new claim is executed and names V", func(t *testing.T) {
		pool := newTestPool(t)
		admin, adminName := seedUser(t, pool, "admin")
		v, vName := seedUser(t, pool, "user")
		e := newHolderEnv(t, admin, pool, "lhn18a")
		addHolderMember(t, pool, e.pid, v, admin)

		// The first claim is performed by admin U on behalf of V, so V is the
		// assignee and U the performer (FOR-PLAN: a plain claim by U would
		// make U the assignee, and seeding assignee=V first would refuse U).
		toolOK(t, e.claim, withCtxSession(t, admin, "admin", s), "admin claims on behalf of V",
			map[string]any{"id": e.issueID, "assignee": vName})
		assertLockHolder(t, e.store, admin, e.issueID, adminName, "after U claimed on behalf of V")

		// The HOLDER releases (sk-424: a release by V carrying U's session is
		// refused, and this row never needed it — it is about the name after
		// the lock changes hands).
		toolOK(t, e.unclaim, withCtxSession(t, admin, "admin", s), "unclaim by the holder",
			map[string]any{"id": e.issueID, "nextStatus": "open", "reason": "row 18a"})
		ctxV := withCtxSession(t, v, "user", "sess-V")
		toolOK(t, e.claim, ctxV, "claim by V with V's own session", map[string]any{"id": e.issueID})
		assertLockHolder(t, e.store, admin, e.issueID, vName, "after the release and V's new claim")
	})

	t.Run("(a) V is an admin naming the current assignee: names V", func(t *testing.T) {
		pool := newTestPool(t)
		u, uName := seedUser(t, pool, "user")
		v, vName := seedUser(t, pool, "admin")
		e := newHolderEnv(t, u, pool, "lhn18aa")
		// V claims by his own role in the project (US-6, T066).
		addHolderMember(t, pool, e.pid, v, u)

		toolOK(t, e.claim, withCtxSession(t, u, "user", s), "claim by U", map[string]any{"id": e.issueID})
		assertLockHolder(t, e.store, u, e.issueID, uName, "after U claimed")

		// Releasing a lock you do not hold is the FORCE path — the one
		// legitimate way to do it, admin-only, which sk-424 deliberately kept.
		ctxV := withCtxSession(t, v, "admin", "sess-V")
		toolOK(t, e.unclaim, ctxV, "admin V force-releases U's lock",
			map[string]any{"id": e.issueID, "nextStatus": "open",
				"reason": "row 18a admin", "force": true})
		toolOK(t, e.claim, ctxV, "claim by admin V naming the current assignee",
			map[string]any{"id": e.issueID, "assignee": uName})
		assertLockHolder(t, e.store, u, e.issueID, vName, "after admin V force-released and claimed again")
	})

	t.Run("(b) V is a plain non-assignee: the claim is refused, the issue stays released", func(t *testing.T) {
		pool := newTestPool(t)
		u, uName := seedUser(t, pool, "user")
		v, _ := seedUser(t, pool, "user")
		e := newHolderEnv(t, u, pool, "lhn18b")
		addHolderMember(t, pool, e.pid, v, u)

		toolOK(t, e.claim, withCtxSession(t, u, "user", s), "claim by U", map[string]any{"id": e.issueID})
		assertLockHolder(t, e.store, u, e.issueID, uName, "after U claimed")

		// U releases its own lock; the subject of this row is the CLAIM that
		// follows being refused, not who may release.
		toolOK(t, e.unclaim, withCtxSession(t, u, "user", s), "unclaim by the holder",
			map[string]any{"id": e.issueID, "nextStatus": "open", "reason": "row 18b"})
		ctxV := withCtxSession(t, v, "user", "sess-V")
		if res := toolCall(t, e.claim, ctxV, map[string]any{"id": e.issueID}); !res.IsError {
			t.Fatalf("claim by non-assignee V succeeded; the fixture expects the assignee check to refuse it")
		}
		if l := readLockRow(t, pool, e.issueID).lockedBy; l != nil {
			t.Fatalf("issue still locked by %q after the refused claim", *l)
		}
		assertLockHolder(t, e.store, u, e.issueID, "", "after V's refused claim")
	})
}

// Row 19 — a repeated claim by the SAME session, by outcome on the server.
func TestLockHolderName_RepeatedClaimBySameSession(t *testing.T) {
	const s = "sess-holder-S"

	t.Run("(a) in_progress: executed without changing the lock, name unchanged", func(t *testing.T) {
		pool := newTestPool(t)
		u, uName := seedUser(t, pool, "user")
		e := newHolderEnv(t, u, pool, "lhn19a")
		ctx := withCtxSession(t, u, "user", s)

		toolOK(t, e.claim, ctx, "first claim", map[string]any{"id": e.issueID})
		assertLockHolder(t, e.store, u, e.issueID, uName, "after the first claim")
		before := readLockRow(t, pool, e.issueID)

		toolOK(t, e.claim, ctx, "repeated claim", map[string]any{"id": e.issueID})
		after := readLockRow(t, pool, e.issueID)
		if !after.lockedAt.Equal(*before.lockedAt) {
			t.Fatalf("repeated claim moved locked_at %v → %v; want the no-op", before.lockedAt, after.lockedAt)
		}
		if n := countClaimEvents(t, pool, e.issueID); n != 1 {
			t.Fatalf("issue.claimed events = %d, want 1 (the repeat writes none)", n)
		}
		assertLockHolder(t, e.store, u, e.issueID, uName, "after the no-op repeat")
	})

	t.Run("(b) open with the lock still held: executed as new, names the performer, not X", func(t *testing.T) {
		pool := newTestPool(t)
		u, uName := seedUser(t, pool, "user")
		x, xName := seedUser(t, pool, "user")
		e := newHolderEnv(t, u, pool, "lhn19b")
		ctx := withCtxSession(t, u, "user", s)

		// X's record for the same session, EARLIER than any real claim — the
		// first record for S.
		seedForeignClaimEvent(t, pool, e.issueID, e.pid, x, xName, s, time.Now().UTC().Add(-time.Hour))

		toolOK(t, e.claim, ctx, "first claim", map[string]any{"id": e.issueID})
		assertLockHolder(t, e.store, u, e.issueID, uName, "after the first claim")

		// Only the status goes back to open; the lock stays with S.
		if _, err := pool.Exec(context.Background(),
			`UPDATE issues SET status = 'open' WHERE id = $1`, e.issueID); err != nil {
			t.Fatalf("status → open: %v", err)
		}
		before := readLockRow(t, pool, e.issueID)
		toolOK(t, e.claim, ctx, "claim executed as new", map[string]any{"id": e.issueID})
		after := readLockRow(t, pool, e.issueID)
		if after.lockedAt.Equal(*before.lockedAt) {
			t.Fatalf("locked_at did not move — the repeat was not executed as a new claim")
		}

		// X's record for the same session, LATER than the real claim and at a
		// moment that is not locked_at — the last record for S.
		seedForeignClaimEvent(t, pool, e.issueID, e.pid, x, xName, s, after.lockedAt.Add(time.Minute))

		assertLockHolder(t, e.store, u, e.issueID, uName,
			"after the new claim, with foreign records for S before and after it")
	})

	t.Run("(c) standing: refused, name unchanged", func(t *testing.T) {
		pool := newTestPool(t)
		u, uName := seedUser(t, pool, "user")
		e := newHolderEnv(t, u, pool, "lhn19c")
		ctx := withCtxSession(t, u, "user", s)

		toolOK(t, e.claim, ctx, "first claim", map[string]any{"id": e.issueID})
		assertLockHolder(t, e.store, u, e.issueID, uName, "after the first claim")

		if _, err := pool.Exec(context.Background(),
			`UPDATE issues SET status = 'standing' WHERE id = $1`, e.issueID); err != nil {
			t.Fatalf("status → standing: %v", err)
		}
		if res := toolCall(t, e.claim, ctx, map[string]any{"id": e.issueID}); !res.IsError {
			t.Fatalf("claim of a standing issue succeeded; want a refusal")
		}
		assertLockHolder(t, e.store, u, e.issueID, uName, "after the refused claim of a standing issue")
	})
}
