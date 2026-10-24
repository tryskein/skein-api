package dependencies

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
)

// parentIdCurrent reads the canonical "current parent" of an issue: the
// EARLIEST parent-child outgoing edge by created_at — exactly the
// expression ListReady / the tree builder use. Returns "" when the issue
// has no parent edge.
func parentIdCurrent(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var parent string
	err := pool.QueryRow(context.Background(), `
SELECT to_id FROM dependencies
 WHERE from_id = $1 AND type = 'parent-child'
 ORDER BY created_at LIMIT 1`, id).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read parentIdCurrent for %s: %v", id, err)
	}
	return parent
}

// countParentEdges returns how many parent-child outgoing edges an issue
// has — the invariant we assert after a self-heal reparent (must be 1).
func countParentEdges(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM dependencies
 WHERE from_id = $1 AND type = 'parent-child'`, id).Scan(&n); err != nil {
		t.Fatalf("count parent edges for %s: %v", id, err)
	}
	return n
}

// issueVersion reads an issue's current version (to assert the bump).
func issueVersion(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		`SELECT version FROM issues WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version for %s: %v", id, err)
	}
	return v
}

// setLock sets locked_by on an issue so the lock-guard path can be tested.
func setLock(t *testing.T, pool *pgxpool.Pool, id, session string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE issues SET locked_by = $2, locked_at = now() WHERE id = $1`,
		id, session); err != nil {
		t.Fatalf("set lock on %s: %v", id, err)
	}
}

// countEventsOfType counts events of a given type scoped to one entity id.
func countEventsOfType(t *testing.T, pool *pgxpool.Pool, entityID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE entity_id = $1 AND type = $2`,
		entityID, eventType).Scan(&n); err != nil {
		t.Fatalf("count events %s/%s: %v", entityID, eventType, err)
	}
	return n
}

// TestReparent_HappyPath: a child under parent P1 is moved under P2.
// parentIdCurrent flips to P2, the issue version is bumped, and the
// reparent + dep events are written.
func TestReparent_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	seedOpenIssue(t, pool, pid, uid, p1, "p1")
	seedOpenIssue(t, pool, pid, uid, p2, "p2")
	seedParentChildDep(t, pool, child, p1, uid)

	if got := parentIdCurrent(t, pool, child); got != p1 {
		t.Fatalf("precondition: parentIdCurrent = %q, want %q", got, p1)
	}
	v0 := issueVersion(t, pool, child)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	res, err := store.Reparent(context.Background(), child, p2, v0,
		caller, HumanActor(uid, "tester"))
	if err != nil {
		t.Fatalf("Reparent: %v", err)
	}

	if got := parentIdCurrent(t, pool, child); got != p2 {
		t.Fatalf("after reparent: parentIdCurrent = %q, want %q", got, p2)
	}
	if n := countParentEdges(t, pool, child); n != 1 {
		t.Fatalf("after reparent: parent edge count = %d, want 1", n)
	}
	if v1 := issueVersion(t, pool, child); v1 != v0+1 {
		t.Fatalf("version not bumped: was %d, now %d", v0, v1)
	}
	if res.NewEdge.ToID != p2 || res.NewEdge.FromID != child {
		t.Fatalf("result NewEdge = %+v, want from=%s to=%s", res.NewEdge, child, p2)
	}
	if len(res.OldParentIDs) != 1 || res.OldParentIDs[0] != p1 {
		t.Fatalf("result OldParentIDs = %v, want [%s]", res.OldParentIDs, p1)
	}
	if c := countEventsOfType(t, pool, child, EventIssueReparented); c != 1 {
		t.Fatalf("issue.reparented events = %d, want 1", c)
	}
	if c := countEventsOfType(t, pool, dependencyEntityID(child, p2, DepParentChild), EventDependencyCreated); c != 1 {
		t.Fatalf("dependency.created for new edge = %d, want 1", c)
	}
	if c := countEventsOfType(t, pool, dependencyEntityID(child, p1, DepParentChild), EventDependencyDeleted); c != 1 {
		t.Fatalf("dependency.deleted for old edge = %d, want 1", c)
	}
}

// TestReparent_SelfHealsDoubleParent: an issue that starts with TWO
// parent-child outgoing edges (the bug state) ends with exactly ONE after
// reparent, and both old parents are recorded.
func TestReparent_SelfHealsDoubleParent(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	p3 := prefix + "-4"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	seedOpenIssue(t, pool, pid, uid, p1, "p1")
	seedOpenIssue(t, pool, pid, uid, p2, "p2")
	seedOpenIssue(t, pool, pid, uid, p3, "p3")
	// Two parent edges — the exact state Reparent must collapse.
	seedParentChildDep(t, pool, child, p1, uid)
	seedParentChildDep(t, pool, child, p2, uid)

	if n := countParentEdges(t, pool, child); n != 2 {
		t.Fatalf("precondition: parent edge count = %d, want 2", n)
	}
	v0 := issueVersion(t, pool, child)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	res, err := store.Reparent(context.Background(), child, p3, v0,
		caller, HumanActor(uid, "tester"))
	if err != nil {
		t.Fatalf("Reparent: %v", err)
	}

	if n := countParentEdges(t, pool, child); n != 1 {
		t.Fatalf("after self-heal: parent edge count = %d, want 1", n)
	}
	if got := parentIdCurrent(t, pool, child); got != p3 {
		t.Fatalf("after self-heal: parentIdCurrent = %q, want %q", got, p3)
	}
	if len(res.OldParentIDs) != 2 {
		t.Fatalf("result OldParentIDs = %v, want both old parents", res.OldParentIDs)
	}
	// Both old edges should have emitted a dependency.deleted.
	if c := countEventsOfType(t, pool, dependencyEntityID(child, p1, DepParentChild), EventDependencyDeleted); c != 1 {
		t.Fatalf("dependency.deleted for p1 = %d, want 1", c)
	}
	if c := countEventsOfType(t, pool, dependencyEntityID(child, p2, DepParentChild), EventDependencyDeleted); c != 1 {
		t.Fatalf("dependency.deleted for p2 = %d, want 1", c)
	}
}

// TestReparent_Cycle: moving a parent under its own descendant is
// rejected with *ErrDepCycle and leaves the graph unchanged.
func TestReparent_Cycle(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	a := prefix + "-1"
	b := prefix + "-2"
	seedOpenIssue(t, pool, pid, uid, a, "a")
	seedOpenIssue(t, pool, pid, uid, b, "b")
	// A is a child of B. Now try to reparent B under A → cycle.
	seedParentChildDep(t, pool, a, b, uid)
	vB := issueVersion(t, pool, b)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Reparent(context.Background(), b, a, vB,
		caller, HumanActor(uid, "tester"))
	var ce *ErrDepCycle
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ErrDepCycle, got %v", err)
	}
	// B should still have no parent edge (nothing was inserted).
	if n := countParentEdges(t, pool, b); n != 0 {
		t.Fatalf("after rejected cycle: B parent edge count = %d, want 0", n)
	}
}

// TestReparent_VersionConflict: stale expectedVersion → *ErrVersionConflict
// carrying the live version, with no mutation.
func TestReparent_VersionConflict(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	seedOpenIssue(t, pool, pid, uid, p1, "p1")
	seedOpenIssue(t, pool, pid, uid, p2, "p2")
	seedParentChildDep(t, pool, child, p1, uid)

	live := issueVersion(t, pool, child)
	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Reparent(context.Background(), child, p2, live+99,
		caller, HumanActor(uid, "tester"))
	var vc *ErrVersionConflict
	if !errors.As(err, &vc) {
		t.Fatalf("expected *ErrVersionConflict, got %v", err)
	}
	if vc.CurrentVersion != live {
		t.Fatalf("ErrVersionConflict.CurrentVersion = %d, want %d", vc.CurrentVersion, live)
	}
	// parent unchanged.
	if got := parentIdCurrent(t, pool, child); got != p1 {
		t.Fatalf("after version conflict: parentIdCurrent = %q, want unchanged %q", got, p1)
	}
}

// TestReparent_IgnoresLockByOther: reparent is NOT lock-gated — a lock
// held by a DIFFERENT session does not block the move (unlike update /
// close). Mirrors skein_dep_add / skein_dep_remove, which are also
// lock-free; concurrency is guarded by expectedVersion, not the lock.
func TestReparent_IgnoresLockByOther(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	seedOpenIssue(t, pool, pid, uid, p1, "p1")
	seedOpenIssue(t, pool, pid, uid, p2, "p2")
	seedParentChildDep(t, pool, child, p1, uid)
	setLock(t, pool, child, "session-OTHER")

	v0 := issueVersion(t, pool, child)
	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	// Caller's actor carries a DIFFERENT session id than the lock holder.
	actor := Actor{UserID: uid, Username: "tester", Role: "agent", SessionID: "session-MINE"}
	if _, err := store.Reparent(context.Background(), child, p2, v0, caller, actor); err != nil {
		t.Fatalf("reparent should ignore a lock held by another session, got: %v", err)
	}
	if got := parentIdCurrent(t, pool, child); got != p2 {
		t.Fatalf("reparent under other-session lock: parentIdCurrent = %q, want %q", got, p2)
	}
}

// TestReparent_LockHolderSucceeds: a lock held by the caller's own session
// also does not block the move (reparent ignores the lock entirely).
func TestReparent_LockHolderSucceeds(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	seedOpenIssue(t, pool, pid, uid, p1, "p1")
	seedOpenIssue(t, pool, pid, uid, p2, "p2")
	seedParentChildDep(t, pool, child, p1, uid)
	setLock(t, pool, child, "session-MINE")

	v0 := issueVersion(t, pool, child)
	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	actor := Actor{UserID: uid, Username: "tester", Role: "agent", SessionID: "session-MINE"}
	if _, err := store.Reparent(context.Background(), child, p2, v0, caller, actor); err != nil {
		t.Fatalf("Reparent by lock holder: %v", err)
	}
	if got := parentIdCurrent(t, pool, child); got != p2 {
		t.Fatalf("lock-holder reparent: parentIdCurrent = %q, want %q", got, p2)
	}
}

// TestReparent_Self: id == newParentID → ErrReparentSelf.
func TestReparent_Self(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	v0 := issueVersion(t, pool, child)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Reparent(context.Background(), child, child, v0,
		caller, HumanActor(uid, "tester"))
	if !errors.Is(err, ErrReparentSelf) {
		t.Fatalf("expected ErrReparentSelf, got %v", err)
	}
}

// TestReparent_CrossProject: a parent in a different (visible) project →
// ErrReparentCrossProject.
func TestReparent_CrossProject(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	pid2, prefix2 := seedProject(t, pool, uid) // same owner → visible to caller
	child := prefix + "-1"
	parent := prefix2 + "-1"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	seedOpenIssue(t, pool, pid2, uid, parent, "parent in other project")
	v0 := issueVersion(t, pool, child)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Reparent(context.Background(), child, parent, v0,
		caller, HumanActor(uid, "tester"))
	if !errors.Is(err, ErrReparentCrossProject) {
		t.Fatalf("expected ErrReparentCrossProject, got %v", err)
	}
}

// TestReparent_NewParentNotFound: nonexistent parent → ErrTargetNotFound.
func TestReparent_NewParentNotFound(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	v0 := issueVersion(t, pool, child)

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	_, err := store.Reparent(context.Background(), child, prefix+"-999", v0,
		caller, HumanActor(uid, "tester"))
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("expected ErrTargetNotFound, got %v", err)
	}
}

// ----------------- Single-parent guard on Create -----------------

// TestCreate_SecondParentRejected: adding a SECOND parent-child edge to a
// different parent → ErrParentAlreadySet. Re-adding the SAME edge keeps
// ErrDependencyExists.
func TestCreate_SecondParentRejected(t *testing.T) {
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	child := prefix + "-1"
	p1 := prefix + "-2"
	p2 := prefix + "-3"
	seedOpenIssue(t, pool, pid, uid, child, "child")
	seedOpenIssue(t, pool, pid, uid, p1, "p1")
	seedOpenIssue(t, pool, pid, uid, p2, "p2")

	store := NewStore(pool)
	caller := CallerAccess{UserID: uid, Role: "user"}
	actor := HumanActor(uid, "tester")

	// First parent — OK.
	if _, err := store.Create(context.Background(), child,
		CreateDepInput{ToID: p1, Type: DepParentChild}, caller, actor); err != nil {
		t.Fatalf("first parent Create: %v", err)
	}

	// Second parent (different) — rejected.
	_, err := store.Create(context.Background(), child,
		CreateDepInput{ToID: p2, Type: DepParentChild}, caller, actor)
	if !errors.Is(err, ErrParentAlreadySet) {
		t.Fatalf("expected ErrParentAlreadySet, got %v", err)
	}

	// Re-adding the SAME edge — preserves ErrDependencyExists.
	_, err = store.Create(context.Background(), child,
		CreateDepInput{ToID: p1, Type: DepParentChild}, caller, actor)
	if !errors.Is(err, ErrDependencyExists) {
		t.Fatalf("re-add same edge: expected ErrDependencyExists, got %v", err)
	}

	// Invariant held: still exactly one parent edge.
	if n := countParentEdges(t, pool, child); n != 1 {
		t.Fatalf("after rejected 2nd parent: edge count = %d, want 1", n)
	}
}

// ----------------- classifyInsertError unit tests -----------------

// pgErr builds a *pgconn.PgError so we can unit-test the classifier
// without provoking real concurrency / FK races.
func pgErr(code, constraint string) error {
	return &pgconn.PgError{Code: code, ConstraintName: constraint}
}

func TestClassifyInsertError(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error // nil means "falls through to internal_error"
	}{
		{"unique", pgErr(pgUniqueViolation, "dependencies_pkey"), ErrDependencyExists},
		{"serialization", pgErr(pgSerializationFailure, ""), ErrSerializationRetry},
		{"fk_to_id", pgErr(pgForeignKeyViolation, depToIDFKConstraint), ErrDependencyTargetNotReady},
		{"fk_from_id", pgErr(pgForeignKeyViolation, "dependencies_from_id_fkey"), ErrFromNotFound},
		{"other_pg", pgErr("42P01", ""), nil},
		{"non_pg", errors.New("boom"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyInsertError(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("classifyInsertError(%v) = %v, want nil", tc.in, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("classifyInsertError(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---- sk-622.21, T012, matrix row No. 22a (Reparent part): Reparent absorbs a
// serialization failure through its own retry (withReparentRetry, budget
// reparentMaxAttempts) and, when every attempt collides, keeps its
// classification — ErrSerializationRetry, nothing written. The conflict is
// built as in issues/serialization_repro_test.go: a concurrent writer
// touches the CHILD's issues row (which Reparent reads and bumps) without
// moving version, Reparent blocks on it (reparentWaitForBlocked is the
// positive control), the writer commits, the attempt takes 40001. Those
// helpers live in package issues; these are their equivalents here.

const reparentTouchSQL = `UPDATE issues SET updated_at = now() WHERE id = $1`

func reparentTaggedPool(t *testing.T, appName string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// reparentWaitForBlocked polls until a backend of appName (or, with pid > 0,
// that backend pid) waits on an ungranted lock.
func reparentWaitForBlocked(t *testing.T, pool *pgxpool.Pool, appName string, pid int32) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE NOT l.granted AND (a.application_name = $1 OR a.pid = $2)`, appName, pid).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// reparentFixture: child under p1, a member watching the child with the
// «reparent» feed switched on (the fan-out's recipient), and p2 to move to.
type reparentFixture struct {
	pool          *pgxpool.Pool
	uid, watcher  uuid.UUID
	child, p1, p2 string
	version       int
}

func newReparentFixture(t *testing.T) reparentFixture {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	watcher := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, watcher, uid); err != nil {
		t.Fatalf("add member: %v", err)
	}
	f := reparentFixture{pool: pool, uid: uid, watcher: watcher,
		child: prefix + "-1", p1: prefix + "-2", p2: prefix + "-3"}
	seedOpenIssue(t, pool, pid, uid, f.child, "child")
	seedOpenIssue(t, pool, pid, uid, f.p1, "p1")
	seedOpenIssue(t, pool, pid, uid, f.p2, "p2")
	seedParentChildDep(t, pool, f.child, f.p1, uid)
	if err := notifications.NewStore(pool).AddIssueWatch(ctx, f.child, watcher, false); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, $2, true, false)`, watcher, string(notifications.ReasonKeyReparent)); err != nil {
		t.Fatalf("enable reparent feed: %v", err)
	}
	f.version = issueVersion(t, pool, f.child)
	return f
}

func (f reparentFixture) reparentNotifications(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `
SELECT count(*) FROM notifications n JOIN events e ON e.id = n.event_id
 WHERE e.entity_id = $1 AND e.type = $2 AND n.user_id = $3`,
		f.child, EventIssueReparented, f.watcher).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// TestReparent_SerializationFailure_Absorbs — No. 22a (a).
func TestReparent_SerializationFailure_Absorbs(t *testing.T) {
	ctx := context.Background()
	f := newReparentFixture(t)
	appName := "sk63414-a-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	opStore := NewStore(reparentTaggedPool(t, appName))

	writer, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, reparentTouchSQL, f.child); err != nil {
		t.Fatalf("writer touch: %v", err)
	}
	resCh := make(chan error, 1)
	go func() {
		_, err := opStore.Reparent(ctx, f.child, f.p2, f.version,
			CallerAccess{UserID: f.uid, Role: "user"}, HumanActor(f.uid, "tester"))
		resCh <- err
	}()
	if !reparentWaitForBlocked(t, f.pool, appName, 0) {
		t.Fatalf("Reparent never blocked on the writer's row lock — the race was not built")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	select {
	case err = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("Reparent did not return within 30s")
	}
	if err != nil {
		t.Fatalf("Reparent surfaced %v after ONE concurrent writer (budget %d)", err, reparentMaxAttempts)
	}
	if got := parentIdCurrent(t, f.pool, f.child); got != f.p2 {
		t.Errorf("parentIdCurrent = %q, want %q", got, f.p2)
	}
	if n := countParentEdges(t, f.pool, f.child); n != 1 {
		t.Errorf("parent edges = %d, want 1", n)
	}
	if c := countEventsOfType(t, f.pool, f.child, EventIssueReparented); c != 1 {
		t.Errorf("issue.reparented events = %d, want exactly 1", c)
	}
	if n := f.reparentNotifications(t); n != 1 {
		t.Errorf("watcher's reparent notifications = %d, want exactly 1", n)
	}
}

// TestReparent_SerializationFailure_Exhausts — No. 22a (b).
func TestReparent_SerializationFailure_Exhausts(t *testing.T) {
	ctx := context.Background()
	f := newReparentFixture(t)
	appName := "sk63414-b-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	opStore := NewStore(reparentTaggedPool(t, appName))

	writers := make([]pgx.Tx, reparentMaxAttempts)
	t.Cleanup(func() {
		for _, w := range writers {
			if w != nil {
				_ = w.Rollback(ctx)
			}
		}
	})
	w0, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer 0: %v", err)
	}
	writers[0] = w0
	if _, err := w0.Exec(ctx, reparentTouchSQL, f.child); err != nil {
		t.Fatalf("writer 0 touch: %v", err)
	}
	resCh := make(chan error, 1)
	go func() {
		_, err := opStore.Reparent(ctx, f.child, f.p2, f.version,
			CallerAccess{UserID: f.uid, Role: "user"}, HumanActor(f.uid, "tester"))
		resCh <- err
	}()
	if !reparentWaitForBlocked(t, f.pool, appName, 0) {
		t.Fatalf("Reparent never blocked behind writer 0")
	}
	// One fresh writer per further attempt: it queues behind its
	// predecessor, takes the row when the predecessor commits, and the
	// retried Reparent blocks on it again.
	for round := 0; round < reparentMaxAttempts-1; round++ {
		wi, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin writer %d: %v", round+1, err)
		}
		writers[round+1] = wi
		var wpid int32
		if err := wi.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&wpid); err != nil {
			t.Fatalf("writer pid: %v", err)
		}
		execDone := make(chan error, 1)
		go func() { _, e := wi.Exec(ctx, reparentTouchSQL, f.child); execDone <- e }()
		if !reparentWaitForBlocked(t, f.pool, "-no-app-", wpid) {
			t.Fatalf("writer %d never queued behind writer %d", round+1, round)
		}
		if err := writers[round].Commit(ctx); err != nil {
			t.Fatalf("commit writer %d: %v", round, err)
		}
		select {
		case e := <-execDone:
			if e != nil {
				t.Fatalf("writer %d touch: %v", round+1, e)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("writer %d never dequeued", round+1)
		}
		if !reparentWaitForBlocked(t, f.pool, appName, 0) {
			t.Fatalf("Reparent never re-blocked behind writer %d", round+1)
		}
	}
	if err := writers[reparentMaxAttempts-1].Commit(ctx); err != nil {
		t.Fatalf("commit last writer: %v", err)
	}
	select {
	case err = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("Reparent did not return within 30s")
	}
	if !errors.Is(err, ErrSerializationRetry) {
		t.Fatalf("Reparent error = %v, want ErrSerializationRetry after %d colliding attempts", err, reparentMaxAttempts)
	}
	if got := parentIdCurrent(t, f.pool, f.child); got != f.p1 {
		t.Errorf("parentIdCurrent = %q after exhaustion, want %q", got, f.p1)
	}
	if c := countEventsOfType(t, f.pool, f.child, EventIssueReparented); c != 0 {
		t.Errorf("issue.reparented events = %d after exhaustion, want 0", c)
	}
	if n := f.reparentNotifications(t); n != 0 {
		t.Errorf("reparent notifications = %d after exhaustion, want 0", n)
	}
}
