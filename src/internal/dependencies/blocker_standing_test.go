package dependencies

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-118 (F2f) — standing-as-blocker edge ban + readiness widening.
//
// Two layers, both exercised here:
//   - SOURCE BAN: CreateInTx rejects a `blocks`/`waits-for` edge whose TO-side
//     (target) issue is standing, with the stable `blocker_target_standing`
//     ValidationError code. `related` (and the other non-gating types) targeting
//     standing stay ALLOWED.
//   - READINESS WIDENING: status.GatesReadiness now excludes standing, so an
//     already-existing blocks edge whose target is (or becomes) standing no
//     longer gates the FROM issue. ListReady proves the FROM issue is ready.

// seedBlocksDep inserts one blocks edge directly so the readiness tests can
// assemble a "blocks → standing target" fixture WITHOUT going through CreateInTx
// (which now rejects exactly that at the source). Models the real-world case
// where the blocker was promoted to standing AFTER the edge already existed.
func seedBlocksDep(t *testing.T, pool *pgxpool.Pool, fromID, toID string, creator uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())`,
		fromID, toID, creator)
	if err != nil {
		t.Fatalf("seed blocks dep: %v", err)
	}
}

func assertBlockerTargetStanding(t *testing.T, err error) {
	t.Helper()
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "blocker_target_standing" {
		t.Fatalf("err = %v, want validation code blocker_target_standing", err)
	}
}

// TestDepAdd_BlocksTargetingStanding_Rejected — a `blocks` edge whose target is
// standing is rejected at create time, and no edge is written.
func TestDepAdd_BlocksTargetingStanding_Rejected(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	blocked := prefix + "-1"
	blocker := prefix + "-2"
	seedOpenIssue(t, pool, pid, uid, blocked, "blocked issue")
	seedOpenIssue(t, pool, pid, uid, blocker, "standing blocker")
	setStanding(t, pool, blocker)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Create(context.Background(), blocked,
		CreateDepInput{ToID: blocker, Type: DepBlocks},
		caller, HumanActor(uid, "tester"))
	assertBlockerTargetStanding(t, err)

	var n int
	if qerr := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'blocks'`,
		blocked, blocker).Scan(&n); qerr != nil {
		t.Fatalf("count edges: %v", qerr)
	}
	if n != 0 {
		t.Errorf("edge count = %d after rejected dep_add, want 0", n)
	}
}

// TestDepAdd_WaitsForTargetingStanding_Rejected — same ban for `waits-for`.
func TestDepAdd_WaitsForTargetingStanding_Rejected(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	waiter := prefix + "-1"
	awaited := prefix + "-2"
	seedOpenIssue(t, pool, pid, uid, waiter, "waiter issue")
	seedOpenIssue(t, pool, pid, uid, awaited, "standing awaited")
	setStanding(t, pool, awaited)

	gate := GateAllChildren
	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Create(context.Background(), waiter,
		CreateDepInput{ToID: awaited, Type: DepWaitsFor, Gate: &gate},
		caller, HumanActor(uid, "tester"))
	assertBlockerTargetStanding(t, err)

	var n int
	if qerr := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'waits-for'`,
		waiter, awaited).Scan(&n); qerr != nil {
		t.Fatalf("count edges: %v", qerr)
	}
	if n != 0 {
		t.Errorf("edge count = %d after rejected waits-for dep_add, want 0", n)
	}
}

// TestDepAdd_RelatedTargetingStanding_Allowed — control: a `related` edge (a
// non-gating type) targeting a standing issue is fine. Only blocks/waits-for are
// banned.
func TestDepAdd_RelatedTargetingStanding_Allowed(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	a := prefix + "-1"
	b := prefix + "-2"
	seedOpenIssue(t, pool, pid, uid, a, "open side A")
	seedOpenIssue(t, pool, pid, uid, b, "standing side B")
	setStanding(t, pool, b)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	if _, err := store.Create(context.Background(), a,
		CreateDepInput{ToID: b, Type: DepRelated},
		caller, HumanActor(uid, "tester")); err != nil {
		t.Fatalf("related edge targeting standing should be allowed, got: %v", err)
	}

	var n int
	if qerr := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2 AND type = 'related'`,
		a, b).Scan(&n); qerr != nil {
		t.Fatalf("count edges: %v", qerr)
	}
	if n != 1 {
		t.Errorf("related edge count = %d, want 1 (allowed)", n)
	}
}

// TestListReady_NotBlockedByStandingTarget — B has an EXISTING blocks edge whose
// target A is standing (seeded directly, since the source-ban prevents creating
// it fresh). Because GatesReadiness now excludes standing, A does NOT gate B, so
// B appears in ListReady.
func TestListReady_NotBlockedByStandingTarget(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	b := prefix + "-1" // the (formerly) blocked issue — should be ready
	a := prefix + "-2" // its blocker, promoted to standing
	seedOpenIssue(t, pool, pid, uid, b, "blocked issue B")
	seedOpenIssue(t, pool, pid, uid, a, "blocker A")
	seedBlocksDep(t, pool, b, a, uid) // B blocks-on A
	setStanding(t, pool, a)           // A becomes standing AFTER the edge exists

	store := NewStore(pool)
	res, err := store.ListReady(context.Background(), pid, ReadyFilters{}, 25)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	// Only B is open (A is standing → excluded from ready by `i.status='open'`).
	// B must be present because its sole blocker is standing → non-gating.
	got := map[string]bool{}
	for _, it := range res {
		got[it.ID] = true
	}
	if !got[b] {
		t.Errorf("issue %s missing from ListReady — standing blocker should not gate it (got %v)", b, idsOf(res))
	}
	if got[a] {
		t.Errorf("standing issue %s leaked into ListReady (status=open filter should exclude it)", a)
	}
}

// TestDepAddMany_BlocksTargetingStanding_RowRejected — the batch path
// (skein_dep_add_many → depsStore.Create → CreateInTx) shares the source ban, so
// a blocks-targeting-standing row is rejected with the same sentinel while a
// sibling related-targeting-standing row in the same batch succeeds. Asserted at
// the store layer (the row-level MCP code is mapped in classifyDepAddError).
func TestDepAddMany_BlocksTargetingStanding_RowRejected(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	blocked := prefix + "-1"
	related := prefix + "-2"
	standing := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, blocked, "blocked issue")
	seedOpenIssue(t, pool, pid, uid, related, "related issue")
	seedOpenIssue(t, pool, pid, uid, standing, "standing target")
	setStanding(t, pool, standing)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}

	// Row 1: blocks → standing — must be rejected with blocker_target_standing.
	_, err := store.Create(context.Background(), blocked,
		CreateDepInput{ToID: standing, Type: DepBlocks},
		caller, HumanActor(uid, "tester"))
	assertBlockerTargetStanding(t, err)

	// Row 2 (independent): related → standing — must succeed (control inside the
	// batch — each item is processed independently by dep_add_many).
	if _, err := store.Create(context.Background(), related,
		CreateDepInput{ToID: standing, Type: DepRelated},
		caller, HumanActor(uid, "tester")); err != nil {
		t.Fatalf("related → standing row should succeed in batch, got: %v", err)
	}
}
