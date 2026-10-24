package projects

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sentinelCache caches the sentinel-project UUID per (test, creator)
// pair so acquireBlockerIssue creates exactly one sentinel project per
// test instead of one per blocked-status seed. Without the cache,
// long-running tests with 6-10 blocked seeds × many tests × repeated CI
// runs accumulated hundreds of `sk31bk_*` projects in the shared
// TEST_DATABASE_URL — pure leak (test pool reset doesn't truncate
// arbitrary rows). The blocker issue id stays unique per call so dep
// edges remain distinct.
//
// Key is "t.Name()/creator" because parallel subtests may share the
// same `t.Name()` prefix but different creators, and a wrong reuse
// would route a blocker through a sentinel whose creator isn't a
// member, tripping unrelated visibility checks.
var (
	sentinelCache   = make(map[string]uuid.UUID)
	sentinelCacheMu sync.Mutex
)

// seedListMember registers `user` as project_member of `project` so the
// permission filter inside ListProjects lets the row through.
func seedListMember(t *testing.T, pool *pgxpool.Pool, project, user uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, project, user); err != nil {
		t.Fatalf("seed member: %v", err)
	}
}

// seedCounter is the counter-row companion every project needs before
// the issue-creation path can RETURNING-claim a sequence number.
func seedCounter(t *testing.T, pool *pgxpool.Pool, project uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO project_counters (project_id, counter) VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, project); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
}

// seedIssue inserts a minimal issues row with the supplied status. We
// don't bother with the full closed-consistency machinery — the only
// terminal status here is 'closed' (which the consistency check
// requires closed_at to be non-null when status='closed').
//
// sk-31: `blocked` is no longer a writable status. When the test passes
// "blocked" we insert the row as 'open' + create a companion blocker
// issue in a SEPARATE sentinel project (so it never inflates the
// asserted project's counts) + add a typed-deps 'blocks' edge. The
// sentinel project is shared across seedIssue calls within one test
// run via sentinelCache (one sentinel per t.Name()/creator pair).
func seedIssue(t *testing.T, pool *pgxpool.Pool, project, creator uuid.UUID, idStr, status, assignee string) {
	t.Helper()
	if status == "blocked" {
		seedIssueRaw(t, pool, project, creator, idStr, "open", assignee)
		blockerID := acquireBlockerIssue(t, pool, creator)
		if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())`,
			idStr, blockerID, creator); err != nil {
			t.Fatalf("seed blocks edge for %s: %v", idStr, err)
		}
		return
	}
	seedIssueRaw(t, pool, project, creator, idStr, status, assignee)
}

// acquireBlockerIssue returns a fresh blocker issue id, reusing one
// sentinel project per (test, creator) pair via sentinelCache. Each
// call yields a new blocker (different id) so multiple "blocked" rows
// in the same test don't share the same target. The sentinel project
// lives outside any of the tested projects, so it doesn't disturb
// their counts. Its creator gets registered as a member so the project
// shape is valid.
//
// NOTE on cleanup: t.Cleanup on the parent test pool drops the pool
// connection but not arbitrary rows. The sentinel project / blocker
// issues stay in TEST_DATABASE_URL until the next migration / explicit
// truncate. They're harmless (the per-test prefix uses pid-derived ids,
// no collisions) and the test database is treated as throw-away. Per-
// test sentinel-project caching keeps accumulation bounded at one row
// per test (or one per (test, creator) pair) instead of one per
// blocked-status seed.
func acquireBlockerIssue(t *testing.T, pool *pgxpool.Pool, creator uuid.UUID) string {
	t.Helper()
	cacheKey := t.Name() + "/" + creator.String()

	sentinelCacheMu.Lock()
	sentinelPID, ok := sentinelCache[cacheKey]
	sentinelCacheMu.Unlock()

	if !ok {
		pid, _ := seedProjectWithRepos(t, pool, creator, "sk31bk", []Repo{})
		seedListMember(t, pool, pid, creator)
		seedCounter(t, pool, pid)
		sentinelCacheMu.Lock()
		// Re-check under the lock in case a sibling goroutine raced
		// us to provision the same (test, creator) sentinel; keep the
		// first-write-wins version so dependent rows don't fan out to
		// a stale id. The extra rows from the racer are harmless leak
		// (same shape as the pre-cache behaviour).
		if existing, dup := sentinelCache[cacheKey]; dup {
			sentinelPID = existing
		} else {
			sentinelCache[cacheKey] = pid
			sentinelPID = pid
		}
		sentinelCacheMu.Unlock()
	}

	// Read back the actual id_prefix the seed helper synthesised so the
	// blocker issue id is well-formed (prefix-suffix shape).
	var sentinelPrefix string
	if err := pool.QueryRow(context.Background(),
		`SELECT id_prefix FROM projects WHERE id = $1`, sentinelPID).Scan(&sentinelPrefix); err != nil {
		t.Fatalf("read sentinel prefix: %v", err)
	}
	blockerID := sentinelPrefix + "-blkr" + uuid.Must(uuid.NewRandom()).String()[:6]
	seedIssueRaw(t, pool, sentinelPID, creator, blockerID, "open", "")
	return blockerID
}

// blockIssue inserts a typed-deps 'blocks' edge so the EXISTS predicate
// driving derived blocked-ness fires on `issueID`. Pairs an
// acquireBlockerIssue companion with the existing row. Used by the
// inline seedOnMeIssue closures to migrate their "blocked" branch off
// the now-rejected status='blocked' insert.
func blockIssue(t *testing.T, pool *pgxpool.Pool, issueID string, creator uuid.UUID) {
	t.Helper()
	blockerID := acquireBlockerIssue(t, pool, creator)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())`,
		issueID, blockerID, creator); err != nil {
		t.Fatalf("seed blocks edge for %s: %v", issueID, err)
	}
}

// seedIssueRaw is the original direct-insert path. Kept as a helper so
// seedIssue's "blocked" translation can reuse it both for the visible
// issue and for the companion blocker.
func seedIssueRaw(t *testing.T, pool *pgxpool.Pool, project, creator uuid.UUID, idStr, status, assignee string) {
	t.Helper()
	var assigneeArg any
	if assignee != "" {
		assigneeArg = assignee
	}
	closedAt := "NULL"
	if status == "closed" {
		closedAt = "now()"
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    metadata)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, $3, false, 'human', NULL, $4::varchar,
        '', '', '', NULL, NULL, NULL, NULL, `+closedAt+`, NULL,
        NULL, $5, now(), now(), '{}'::jsonb)`,
		idStr, project, status, assigneeArg, creator); err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
}

// TestListProjects_PerStatusCounts verifies the new openCount /
// inProgressCount / blockedCount aggregates returned per row.
func TestListProjects_PerStatusCounts(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "lpc", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	// Mix of statuses. Issue IDs include the project UUID prefix so
	// concurrent test runs against the same DB don't collide on the
	// issues_pkey.
	prefix := "lpc-" + pid.String()[:8]
	seedIssue(t, pool, pid, owner, prefix+"-1", "open", "")
	seedIssue(t, pool, pid, owner, prefix+"-2", "open", "")
	seedIssue(t, pool, pid, owner, prefix+"-3", "in_progress", "")
	seedIssue(t, pool, pid, owner, prefix+"-4", "blocked", "")
	seedIssue(t, pool, pid, owner, prefix+"-5", "closed", "")

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var got *ProjectListItem
	for i := range res.Items {
		if res.Items[i].ID == pid {
			got = &res.Items[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("project %s not found in list (visible to owner)", pid)
	}
	if got.OpenCount != 2 {
		t.Errorf("openCount = %d, want 2", got.OpenCount)
	}
	if got.InProgressCount != 1 {
		t.Errorf("inProgressCount = %d, want 1", got.InProgressCount)
	}
	if got.BlockedCount != 1 {
		t.Errorf("blockedCount = %d, want 1", got.BlockedCount)
	}
}

// TestListProjects_ZeroIssues_AllCountsZero verifies that a project with
// no issues at all returns counts of zero (LEFT JOIN keeps the row).
func TestListProjects_ZeroIssues_AllCountsZero(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "lpz", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got *ProjectListItem
	for i := range res.Items {
		if res.Items[i].ID == pid {
			got = &res.Items[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("project %s not visible to owner", pid)
	}
	if got.OpenCount != 0 || got.InProgressCount != 0 || got.BlockedCount != 0 {
		t.Errorf("counts not zero: open=%d in_progress=%d blocked=%d",
			got.OpenCount, got.InProgressCount, got.BlockedCount)
	}
}

// TestGetProjectWithStats_FullBreakdown covers the per-status grid plus
// caller-scoped MyAssigned / MyCreated and LastActivity.
func TestGetProjectWithStats_FullBreakdown(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)

	pid, _ := seedProjectWithRepos(t, pool, owner, "gps", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	// Resolve owner's username so we can assign issues to it (assignee
	// matches LOWER(username) in the my-assigned subquery).
	var ownerUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, owner).Scan(&ownerUsername); err != nil {
		t.Fatalf("read username: %v", err)
	}

	prefix := "gps-" + pid.String()[:8]
	// 2 open assigned to owner (both count toward MyAssigned), 1
	// in_progress assigned to other (excluded), 1 blocked unassigned,
	// 1 deferred unassigned, 1 closed assigned to owner (excluded — my
	// load only counts non-closed).
	seedIssue(t, pool, pid, owner, prefix+"-1", "open", ownerUsername)
	seedIssue(t, pool, pid, owner, prefix+"-2", "open", ownerUsername)
	seedIssue(t, pool, pid, owner, prefix+"-3", "in_progress", "noone")
	seedIssue(t, pool, pid, owner, prefix+"-4", "blocked", "")
	seedIssue(t, pool, pid, owner, prefix+"-5", "deferred", "")
	seedIssue(t, pool, pid, owner, prefix+"-6", "closed", ownerUsername)
	// One created by `other` so MyCreated stays bounded — we created 6
	// above (all `created_by = owner`) so MyCreated for owner = 6.
	seedIssue(t, pool, pid, other, prefix+"-7", "open", "")

	store := NewStore(pool)
	dto, err := store.GetProjectWithStats(storeFixtureCtx(), pid, owner)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	want := struct{ open, inP, blk, def, closed, total, myA, myC int }{
		open: 3, inP: 1, blk: 1, def: 1, closed: 1, total: 7,
		myA: 2, myC: 6,
	}
	got := dto.Stats
	if got.Open != want.open ||
		got.InProgress != want.inP ||
		got.Blocked != want.blk ||
		got.Deferred != want.def ||
		got.Closed != want.closed ||
		got.Total != want.total ||
		got.MyAssigned != want.myA ||
		got.MyCreated != want.myC {
		t.Errorf("stats mismatch:\ngot  open=%d in_progress=%d blocked=%d deferred=%d closed=%d total=%d myAssigned=%d myCreated=%d\nwant open=%d in_progress=%d blocked=%d deferred=%d closed=%d total=%d myAssigned=%d myCreated=%d",
			got.Open, got.InProgress, got.Blocked, got.Deferred, got.Closed, got.Total, got.MyAssigned, got.MyCreated,
			want.open, want.inP, want.blk, want.def, want.closed, want.total, want.myA, want.myC)
	}
	if got.LastActivity == nil {
		t.Errorf("lastActivity nil — expected non-nil with 7 issues seeded")
	}
}

// TestGetProjectWithStats_NoIssues_NilLastActivity checks that an empty
// project surfaces zero counts and a nil last-activity (no issues to
// take max(updated_at) of).
func TestGetProjectWithStats_NoIssues_NilLastActivity(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "gpn", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	store := NewStore(pool)
	dto, err := store.GetProjectWithStats(storeFixtureCtx(), pid, owner)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dto.Stats.Total != 0 {
		t.Errorf("total = %d, want 0", dto.Stats.Total)
	}
	if dto.Stats.MyAssigned != 0 || dto.Stats.MyCreated != 0 {
		t.Errorf("my-load not zero: assigned=%d created=%d",
			dto.Stats.MyAssigned, dto.Stats.MyCreated)
	}
	if dto.Stats.LastActivity != nil {
		t.Errorf("lastActivity = %v, want nil", *dto.Stats.LastActivity)
	}
}

// TestListComponents_PerStatusCounts seeds two components and links a
// few issues to each via issue_components, then verifies the right
// counts surface per row.
func TestListComponents_PerStatusCounts(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "lcc", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	store := NewStore(pool)
	c1, err := store.CreateComponent(context.Background(), pid, owner,
		CreateComponentArgs{Name: "alpha"}, HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("create comp1: %v", err)
	}
	c2, err := store.CreateComponent(context.Background(), pid, owner,
		CreateComponentArgs{Name: "beta"}, HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("create comp2: %v", err)
	}

	prefix := "lcc-" + pid.String()[:8]
	// c1: 2 open, 1 blocked. c2: 1 in_progress, 1 closed (closed should
	// not be counted under any of the 3 fields).
	seedIssue(t, pool, pid, owner, prefix+"-1", "open", "")
	seedIssue(t, pool, pid, owner, prefix+"-2", "open", "")
	seedIssue(t, pool, pid, owner, prefix+"-3", "blocked", "")
	seedIssue(t, pool, pid, owner, prefix+"-4", "in_progress", "")
	seedIssue(t, pool, pid, owner, prefix+"-5", "closed", "")

	link := func(issue string, comp uuid.UUID) {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`, issue, comp); err != nil {
			t.Fatalf("link %s ↔ %s: %v", issue, comp, err)
		}
	}
	link(prefix+"-1", c1.ID)
	link(prefix+"-2", c1.ID)
	link(prefix+"-3", c1.ID)
	link(prefix+"-4", c2.ID)
	link(prefix+"-5", c2.ID)

	list, err := store.ListComponents(storeFixtureCtx(), pid, "", uuid.Nil, "")
	if err != nil {
		t.Fatalf("list components: %v", err)
	}

	byID := make(map[uuid.UUID]ComponentDTO, len(list))
	for _, c := range list {
		byID[c.ID] = c
	}

	got1, ok := byID[c1.ID]
	if !ok {
		t.Fatalf("comp1 not in list result")
	}
	if got1.OpenCount != 2 || got1.InProgressCount != 0 || got1.BlockedCount != 1 {
		t.Errorf("c1 counts wrong: open=%d in_progress=%d blocked=%d (want 2/0/1)",
			got1.OpenCount, got1.InProgressCount, got1.BlockedCount)
	}

	got2, ok := byID[c2.ID]
	if !ok {
		t.Fatalf("comp2 not in list result")
	}
	if got2.OpenCount != 0 || got2.InProgressCount != 1 || got2.BlockedCount != 0 {
		t.Errorf("c2 counts wrong: open=%d in_progress=%d blocked=%d (want 0/1/0)",
			got2.OpenCount, got2.InProgressCount, got2.BlockedCount)
	}
}

// TestListComponents_ZeroIssues verifies a component with no linked
// issues still surfaces with all-zero counts (LEFT JOIN, not INNER).
func TestListComponents_ZeroIssues(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "lcz", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	store := NewStore(pool)
	c, err := store.CreateComponent(context.Background(), pid, owner,
		CreateComponentArgs{Name: "lone"}, HumanActor(owner, "owner"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := store.ListComponents(storeFixtureCtx(), pid, "", uuid.Nil, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
	if list[0].ID != c.ID {
		t.Fatalf("id mismatch: got %s want %s", list[0].ID, c.ID)
	}
	if list[0].OpenCount != 0 || list[0].InProgressCount != 0 || list[0].BlockedCount != 0 {
		t.Errorf("counts not zero: %+v", list[0])
	}
}

// TestListProjects_MyOutboxCounts verifies the My-assigned-mode (outbox)
// per-status counts on each list row are scoped to issues the caller
// would see in their Delegated list.
//
// sk-34 review C1: the counter must align with the list predicate in
// issues/store.go::ListIssues which surfaces creator-with-no-assignee
// rows as well. Aligning here removes the pre-sk-34 discrepancy where
// the pill read 0 while the list showed 1.
//
// Predicate (creator branch only — chain-member branch covered by
// delegation_chain_test.go):
//
//	created_by = caller
//	AND (assignee IS NULL OR LOWER(assignee) <> LOWER(callerUsername))
//
// Mix:
//   - caller-created, delegated to other → counted
//   - caller-created, assigned to self    → NOT counted (current
//     assignee → On-me lens, not Delegated)
//   - caller-created, unassigned          → counted (creator branch
//     does NOT require an assignee; matches the Delegated list)
//   - other-created, assigned to anyone   → NOT counted (not author)
func TestListProjects_MyOutboxCounts(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, caller, "lpo", []Repo{})
	seedListMember(t, pool, pid, caller)
	seedCounter(t, pool, pid)

	// Resolve usernames so we can craft assignee values that
	// match / mismatch the caller correctly.
	var callerUsername, otherUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherUsername); err != nil {
		t.Fatalf("read other username: %v", err)
	}

	prefix := "lpo-" + pid.String()[:8]
	// Caller-created, delegated to other → counted under each status:
	//   2 open + 1 in_progress + 1 blocked counted in MyOutbox*.
	seedIssue(t, pool, pid, caller, prefix+"-1", "open", otherUsername)
	seedIssue(t, pool, pid, caller, prefix+"-2", "open", otherUsername)
	seedIssue(t, pool, pid, caller, prefix+"-3", "in_progress", otherUsername)
	seedIssue(t, pool, pid, caller, prefix+"-4", "blocked", otherUsername)
	// Caller-created, assigned to self → NOT counted (self-delegation
	// belongs in On-me, not Delegated).
	seedIssue(t, pool, pid, caller, prefix+"-5", "open", callerUsername)
	// demo-5: Caller-created, unassigned → NOT counted in
	// MyOutbox. A task with no assignee isn't delegated to anyone — it's
	// just unowned. The canonical ListIssues Delegated predicate
	// (internal/issues/store.go: "i.assignee IS NOT NULL AND
	// LOWER(i.assignee) <> me") excludes it, and the project-row outbox
	// counters were tightened to agree (store.go / list_search.go: the
	// creator branch carries the same "i.assignee IS NOT NULL" guard). The
	// row still appears in the UNSCOPED OpenCount below — only the outbox
	// lens excludes it.
	seedIssue(t, pool, pid, caller, prefix+"-6", "open", "")
	// Other-created, assigned to other → NOT counted (not authored
	// by caller, so doesn't belong in caller's outbox).
	seedIssue(t, pool, pid, other, prefix+"-7", "open", otherUsername)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: caller,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got *ProjectListItem
	for i := range res.Items {
		if res.Items[i].ID == pid {
			got = &res.Items[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("project %s not visible to caller", pid)
	}

	// Unscoped counts: 5 open (-1, -2, -5, -6, -7) +
	// 1 in_progress (-3) + 1 blocked (-4).
	if got.OpenCount != 5 {
		t.Errorf("openCount = %d, want 5", got.OpenCount)
	}
	if got.InProgressCount != 1 {
		t.Errorf("inProgressCount = %d, want 1", got.InProgressCount)
	}
	if got.BlockedCount != 1 {
		t.Errorf("blockedCount = %d, want 1", got.BlockedCount)
	}

	// MyOutbox-scoped counts (demo-5 alignment):
	//   open: -1, -2 → 2 (the -6 unassigned row is EXCLUDED — unowned,
	//                      not delegated)
	//   in_progress: -3 → 1
	//   blocked: -4 → 1
	if got.MyOutboxOpenCount != 2 {
		t.Errorf("myOutboxOpenCount = %d, want 2", got.MyOutboxOpenCount)
	}
	if got.MyOutboxInProgressCount != 1 {
		t.Errorf("myOutboxInProgressCount = %d, want 1", got.MyOutboxInProgressCount)
	}
	if got.MyOutboxBlockedCount != 1 {
		t.Errorf("myOutboxBlockedCount = %d, want 1", got.MyOutboxBlockedCount)
	}
}

// sk-524 — TWO DIFFERENT ZEROS, and the test must tell them apart.
//
// The earlier version of this test was called `…_NilCaller` and passed
// a REAL CallerID with an empty name, expecting zeros. It asserted
// exactly the defect sk-524 was opened for: the counter collapsed
// not because there are no issues but because there was no name to substitute.
// On the MCP transport there is never a name, so zeros were seen there
// always — with real issues and a real user.
//
// Now there are two cases, and they are kept apart:
//
//  1. the caller COULD NOT be resolved (zero CallerID) — zeros;
//  2. the caller is resolved, there is no name (the MCP case) — REAL numbers.
//
// Without the second half the first proves nothing: today's
// behaviour looks like "no issues" but is "identity lost".
func TestListProjects_MyOutboxCounts_UnresolvedCaller(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "lpn", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "lpn-" + pid.String()[:8]
	seedIssue(t, pool, pid, owner, prefix+"-1", "open", "anyone")

	store := NewStore(pool)
	// A zero identifier — no identity. Since sk-674.18 T009 the visibility of
	// rows is decided by the access relation for the caller, and for an empty
	// caller the relation is empty: a list without an identity returns no
	// rows — neither counters nor MyOutbox numbers (before, the row was kept
	// visible by the argument's admin role, which no longer exists).
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: uuid.Nil,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, it := range res.Items {
		if it.ID == pid {
			t.Fatalf("project %s listed for an unresolved caller", pid)
		}
	}
	if len(res.Items) != 0 {
		t.Errorf("unresolved caller listed %d projects, want none", len(res.Items))
	}
}

// sk-524 — the second half: the caller is resolved, there is no name. This is
// literally what arrives from the MCP transport, where `auth.UsernameFrom`
// returns nothing. The numbers must be real — the same as the
// same user sees through REST.
func TestListProjects_MyOutboxCounts_ResolvedCallerWithoutUsername(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "lpu", []Repo{})
	seedListMember(t, pool, pid, owner)
	seedCounter(t, pool, pid)

	prefix := "lpu-" + pid.String()[:8]
	// Created by the caller, assigned to someone else — that is "delegated".
	seedIssue(t, pool, pid, owner, prefix+"-1", "open", "anyone")

	store := NewStore(pool)
	// There is no name in the arguments at all — the field for it no longer exists.
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := findProjectRow(t, res.Items, pid)
	if got.MyOutboxOpenCount != 1 {
		t.Errorf("myOutboxOpenCount = %d, want 1: the name is not passed, but the identity is known — "+
			"the counter must count (sk-524)", got.MyOutboxOpenCount)
	}
}

// findProjectRow — the project row from the result, or a test failure.
func findProjectRow(t *testing.T, items []ProjectListItem, pid uuid.UUID) *ProjectListItem {
	t.Helper()
	for i := range items {
		if items[i].ID == pid {
			return &items[i]
		}
	}
	t.Fatalf("project %s not in result", pid)
	return nil
}

// TestListComponents_MyOutboxCounts mirrors the project-level test for
// the component list path. Two components, one issue per shape; verify
// caller-created-delegated-to-other counts surface in MyOutbox* and
// caller-self-assigned / other-created issues don't.
func TestListComponents_MyOutboxCounts(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, caller, "lco", []Repo{})
	seedListMember(t, pool, pid, caller)
	seedCounter(t, pool, pid)

	var callerUsername, otherUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherUsername); err != nil {
		t.Fatalf("read other username: %v", err)
	}

	store := NewStore(pool)
	c1, err := store.CreateComponent(context.Background(), pid, caller,
		CreateComponentArgs{Name: "alpha"}, HumanActor(caller, "caller"))
	if err != nil {
		t.Fatalf("create comp1: %v", err)
	}

	prefix := "lco-" + pid.String()[:8]
	// All three issues live on c1.
	// Caller→other: should count.
	seedIssue(t, pool, pid, caller, prefix+"-1", "open", otherUsername)
	// Caller→self: should NOT count.
	seedIssue(t, pool, pid, caller, prefix+"-2", "open", callerUsername)
	// Other→other: should NOT count.
	seedIssue(t, pool, pid, other, prefix+"-3", "blocked", otherUsername)

	link := func(issue string, comp uuid.UUID) {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`, issue, comp); err != nil {
			t.Fatalf("link %s ↔ %s: %v", issue, comp, err)
		}
	}
	link(prefix+"-1", c1.ID)
	link(prefix+"-2", c1.ID)
	link(prefix+"-3", c1.ID)

	list, err := store.ListComponents(storeFixtureCtx(), pid, "", caller, callerUsername)
	if err != nil {
		t.Fatalf("list components: %v", err)
	}

	var got *ComponentDTO
	for i := range list {
		if list[i].ID == c1.ID {
			got = &list[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("component %s not in list", c1.ID)
	}
	// Unscoped: 2 open (1 caller→other, 1 caller→self), 1 blocked
	// (other→other).
	if got.OpenCount != 2 {
		t.Errorf("openCount = %d, want 2", got.OpenCount)
	}
	if got.BlockedCount != 1 {
		t.Errorf("blockedCount = %d, want 1", got.BlockedCount)
	}
	// MyOutbox: only 1 open (caller→other), 0 blocked (the blocked one
	// was created by `other`, not the caller).
	if got.MyOutboxOpenCount != 1 {
		t.Errorf("myOutboxOpenCount = %d, want 1", got.MyOutboxOpenCount)
	}
	if got.MyOutboxBlockedCount != 0 {
		t.Errorf("myOutboxBlockedCount = %d, want 0", got.MyOutboxBlockedCount)
	}
	if got.MyOutboxInProgressCount != 0 {
		t.Errorf("myOutboxInProgressCount = %d, want 0", got.MyOutboxInProgressCount)
	}
}

// TestListProjects_OnMeCounts verifies the "On me" lens (sk-30) per-
// status aggregates: only issues assigned to the caller by SOMEONE
// ELSE that have already been accepted (accepted_at IS NOT NULL).
// Inbox prefix (unaccepted) and self-assigned tasks must NOT count.
func TestListProjects_OnMeCounts(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, caller, "lpm", []Repo{})
	seedListMember(t, pool, pid, caller)
	seedCounter(t, pool, pid)

	var callerUsername, otherUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherUsername); err != nil {
		t.Fatalf("read other username: %v", err)
	}

	seedOnMeIssue := func(idStr, status, assignee string, accepted bool, creator uuid.UUID) {
		t.Helper()
		// sk-31: 'blocked' is derived. Insert as open + dep edge.
		needsBlocker := status == "blocked"
		insertStatus := status
		if needsBlocker {
			insertStatus = "open"
		}
		var acceptedAtArg, acceptedByArg any
		if accepted {
			acceptedAtArg = "now()"
			acceptedByArg = caller
		}
		closedAt := "NULL"
		if insertStatus == "closed" {
			closedAt = "now()"
		}
		acceptedAtSQL := "NULL"
		if accepted {
			acceptedAtSQL = "now()"
			_ = acceptedAtArg
		}
		if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata,
                    accepted_at, accepted_by)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, $3, false, 'human', NULL, $4,
        '', '', '', NULL, NULL, NULL, NULL, `+closedAt+`, NULL,
        NULL, $5, now(), now(), '{}'::jsonb,
        `+acceptedAtSQL+`, $6)`,
			idStr, pid, insertStatus, assignee, creator, acceptedByArg); err != nil {
			t.Fatalf("seed on-me issue %s: %v", idStr, err)
		}
		if needsBlocker {
			blockIssue(t, pool, idStr, creator)
		}
	}

	prefix := "lpm-" + pid.String()[:8]
	// Counted: other → caller, accepted, status=open / in_progress / blocked.
	seedOnMeIssue(prefix+"-1", "open", callerUsername, true, other)
	seedOnMeIssue(prefix+"-2", "in_progress", callerUsername, true, other)
	seedOnMeIssue(prefix+"-3", "blocked", callerUsername, true, other)
	// Not counted: other → caller, NOT accepted (Inbox prefix).
	seedOnMeIssue(prefix+"-4", "open", callerUsername, false, other)
	// Not counted: caller → caller, accepted (self-delegation, not "on me from someone else").
	seedOnMeIssue(prefix+"-5", "open", callerUsername, true, caller)
	// Not counted: other → other, accepted (someone else's task).
	seedOnMeIssue(prefix+"-6", "open", otherUsername, true, other)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: caller,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got *ProjectListItem
	for i := range res.Items {
		if res.Items[i].ID == pid {
			got = &res.Items[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("project %s not visible to caller", pid)
	}
	if got.OnMeOpenCount != 1 {
		t.Errorf("onMeOpenCount = %d, want 1", got.OnMeOpenCount)
	}
	if got.OnMeInProgressCount != 1 {
		t.Errorf("onMeInProgressCount = %d, want 1", got.OnMeInProgressCount)
	}
	if got.OnMeBlockedCount != 1 {
		t.Errorf("onMeBlockedCount = %d, want 1", got.OnMeBlockedCount)
	}
}

// TestListComponents_OnMeCounts mirrors the project-level test for the
// component list path.
func TestListComponents_OnMeCounts(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, caller, "lcm", []Repo{})
	seedListMember(t, pool, pid, caller)
	seedCounter(t, pool, pid)

	var callerUsername, otherUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherUsername); err != nil {
		t.Fatalf("read other username: %v", err)
	}

	store := NewStore(pool)
	c1, err := store.CreateComponent(context.Background(), pid, caller,
		CreateComponentArgs{Name: "beta"}, HumanActor(caller, "caller"))
	if err != nil {
		t.Fatalf("create comp: %v", err)
	}

	prefix := "lcm-" + pid.String()[:8]
	seedOnMeIssue := func(idStr, status, assignee string, accepted bool, creator uuid.UUID) {
		t.Helper()
		needsBlocker := status == "blocked"
		insertStatus := status
		if needsBlocker {
			insertStatus = "open"
		}
		var acceptedByArg any
		acceptedAtSQL := "NULL"
		if accepted {
			acceptedAtSQL = "now()"
			acceptedByArg = caller
		}
		if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata,
                    accepted_at, accepted_by)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, $3, false, 'human', NULL, $4,
        '', '', '', NULL, NULL, NULL, NULL, NULL, NULL,
        NULL, $5, now(), now(), '{}'::jsonb,
        `+acceptedAtSQL+`, $6)`,
			idStr, pid, insertStatus, assignee, creator, acceptedByArg); err != nil {
			t.Fatalf("seed on-me issue %s: %v", idStr, err)
		}
		if needsBlocker {
			blockIssue(t, pool, idStr, creator)
		}
	}

	seedOnMeIssue(prefix+"-1", "open", callerUsername, true, other)       // counted
	seedOnMeIssue(prefix+"-2", "open", callerUsername, false, other)      // unaccepted — not counted
	seedOnMeIssue(prefix+"-3", "blocked", callerUsername, true, caller)   // self-delegation — not counted
	seedOnMeIssue(prefix+"-4", "in_progress", otherUsername, true, other) // someone else's task

	link := func(issue string) {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`, issue, c1.ID); err != nil {
			t.Fatalf("link %s: %v", issue, err)
		}
	}
	link(prefix + "-1")
	link(prefix + "-2")
	link(prefix + "-3")
	link(prefix + "-4")

	list, err := store.ListComponents(storeFixtureCtx(), pid, "", caller, callerUsername)
	if err != nil {
		t.Fatalf("list components: %v", err)
	}
	var got *ComponentDTO
	for i := range list {
		if list[i].ID == c1.ID {
			got = &list[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("component %s not in list", c1.ID)
	}
	if got.OnMeOpenCount != 1 {
		t.Errorf("onMeOpenCount = %d, want 1", got.OnMeOpenCount)
	}
	if got.OnMeInProgressCount != 0 {
		t.Errorf("onMeInProgressCount = %d, want 0", got.OnMeInProgressCount)
	}
	if got.OnMeBlockedCount != 0 {
		t.Errorf("onMeBlockedCount = %d, want 0", got.OnMeBlockedCount)
	}
}
