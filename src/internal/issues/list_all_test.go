package issues

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/authz"
)

// TestListAllIssues_EmptyVisibility — non-admin caller with zero
// project_members rows sees nothing, with total = 0.
func TestListAllIssues_EmptyVisibility(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	caller := uuid.Must(uuid.NewRandom())

	res, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:  authz.ForUser(caller),
		Caller: caller,
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues: %v", err)
	}
	if len(res.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(res.Items))
	}
	if res.Total != 0 {
		t.Errorf("expected total=0, got %d", res.Total)
	}
	if res.HasMore {
		t.Errorf("expected HasMore=false")
	}
}

// TestListAllIssues_AdminSeesAll_MemberSeesSubset — seed two projects
// with caller as member of only one; admin sees both, caller sees only
// the projects they belong to.
func TestListAllIssues_AdminSeesAll_MemberSeesSubset(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	owner := seedUser(t, pool)
	member := seedUser(t, pool)

	pidA, prefA := seedProject(t, pool, owner)
	pidB, prefB := seedProject(t, pool, owner)
	// Make `member` a member of project A only.
	if _, err := pool.Exec(context.Background(), `
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pidA, member, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	seedIssue(t, pool, pidA, owner, prefA+"-1", "task in A")
	seedIssue(t, pool, pidA, owner, prefA+"-2", "task in A again")
	seedIssue(t, pool, pidB, owner, prefB+"-1", "task in B")

	// Member sees only project A.
	res, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:  authz.ForUser(member),
		Caller: member,
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues member: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("member: expected total=2, got %d", res.Total)
	}
	if len(res.Items) != 2 {
		t.Errorf("member: expected 2 items, got %d", len(res.Items))
	}
	for _, it := range res.Items {
		if it.ProjectID != pidA {
			t.Errorf("member: saw issue %s in project %s, expected only %s",
				it.ID, it.ProjectID, pidA)
		}
	}

	// Admin sees all 3.
	adminRes, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:  authz.Unrestricted(),
		Caller: owner,
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues admin: %v", err)
	}
	if adminRes.Total < 3 {
		t.Errorf("admin: expected total>=3, got %d", adminRes.Total)
	}
}

// TestListAllIssues_StatusTypeOriginFilters — status / type / origin
// filters narrow the result set correctly.
func TestListAllIssues_StatusTypeOriginFilters(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	// 3 open tasks (human), 1 closed bug (human), 1 open task
	// (agent_autonomous). status=open should drop the closed; type=bug
	// should drop the tasks; origin=agent-autonomous should drop the
	// human-sources.
	for i := 1; i <= 3; i++ {
		seedIssue(t, pool, pid, owner, prefix+"-"+strconv.Itoa(i), "open task "+strconv.Itoa(i))
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1::varchar, $2, NULL, 1, 'closed bug', '', 'bug', 2, 'closed', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, now(), 'fixed',
        NULL, $3, now(), now(), '{}'::jsonb)`,
		prefix+"-4", pid, owner); err != nil {
		t.Fatalf("seed closed bug: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1::varchar, $2, NULL, 1, 'agent task', '', 'task', 2, 'open', false, 'agent_autonomous', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $3, now(), now(), '{}'::jsonb)`,
		prefix+"-5", pid, owner); err != nil {
		t.Fatalf("seed agent autonomous: %v", err)
	}

	// status=open — should exclude the closed bug.
	statusRes, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:  authz.Unrestricted(),
		Caller: owner,
		Status: "open",
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues status=open: %v", err)
	}
	for _, it := range statusRes.Items {
		if it.ProjectID == pid && it.Status != "open" {
			t.Errorf("status=open returned non-open issue %s status=%s", it.ID, it.Status)
		}
	}

	// type=bug — should return only the closed bug from our seed.
	typeRes, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:  authz.Unrestricted(),
		Caller: owner,
		Type:   "bug",
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues type=bug: %v", err)
	}
	bugFound := false
	for _, it := range typeRes.Items {
		if it.ProjectID == pid {
			if it.Type != "bug" {
				t.Errorf("type=bug returned non-bug issue %s type=%s", it.ID, it.Type)
			}
			if it.ID == prefix+"-4" {
				bugFound = true
			}
		}
	}
	if !bugFound {
		t.Errorf("expected closed bug %s in type=bug result", prefix+"-4")
	}

	// origin=agent-autonomous — should return only our agent task.
	autoOrigin := OriginAgentAutonomous
	originRes, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:       authz.Unrestricted(),
		Caller:      owner,
		OriginGroup: &autoOrigin,
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues origin=agent-autonomous: %v", err)
	}
	autoFound := false
	for _, it := range originRes.Items {
		if it.ID == prefix+"-5" {
			autoFound = true
		}
	}
	if !autoFound {
		t.Errorf("expected agent_autonomous issue %s in origin filter result", prefix+"-5")
	}
}

// TestListAllIssues_FTSMatch — `q` matches title FTS hits.
func TestListAllIssues_FTSMatch(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "bug fix in authentication")
	seedIssue(t, pool, pid, owner, prefix+"-2", "feature request for export")
	seedIssue(t, pool, pid, owner, prefix+"-3", "another bug fix")

	res, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:  authz.Unrestricted(),
		Caller: owner,
		Q:      "bug fix",
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues q=bug fix: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, it := range res.Items {
		gotIDs[it.ID] = true
	}
	if !gotIDs[prefix+"-1"] {
		t.Errorf("q=\"bug fix\" missed %s (matching FTS)", prefix+"-1")
	}
	if !gotIDs[prefix+"-3"] {
		t.Errorf("q=\"bug fix\" missed %s (matching FTS)", prefix+"-3")
	}
	if gotIDs[prefix+"-2"] {
		t.Errorf("q=\"bug fix\" should not match feature/export row %s", prefix+"-2")
	}
}

// TestListAllIssues_ExactIDBoost — `q` looking like a full issue id
// matches the row by id even when the FTS layer would skip it.
func TestListAllIssues_ExactIDBoost(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	seedIssue(t, pool, pid, owner, prefix+"-1", "unrelated title with no terms")
	seedIssue(t, pool, pid, owner, prefix+"-2", "completely different content here")
	seedIssue(t, pool, pid, owner, prefix+"-3", "another row")

	// Query is the exact id `prefix-2` — FTS layer won't match (the id
	// isn't a tsvector term in the indexed columns), but the exact-id
	// boost should bring it in.
	q := prefix + "-2"
	res, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:  authz.Unrestricted(),
		Caller: owner,
		Q:      q,
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues q=%q: %v", q, err)
	}
	found := false
	for _, it := range res.Items {
		if it.ID == prefix+"-2" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("q=%q (exact id) failed to surface row %s; got ids=%v",
			q, prefix+"-2", idsOf(res.Items))
	}
}

// TestListAllIssues_CursorPaginationConsistent — seed enough rows in a
// single project, scope visibility to that one project (non-admin so
// the visibility predicate kicks in), walk every page and assert no
// duplicates, full coverage, stable total across pages.
func TestListAllIssues_CursorPaginationConsistent(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)

	// 12 issues, page size 5 → 3 pages (5+5+2).
	for i := 1; i <= 12; i++ {
		seedIssue(t, pool, pid, owner, prefix+"-"+strconv.Itoa(i), "row "+strconv.Itoa(i))
	}

	seen := map[string]int{}
	var cursor *string
	pages := 0
	wantTotal := 0
	for {
		pages++
		if pages > 10 {
			t.Fatalf("runaway pagination after %d pages", pages)
		}
		res, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
			Scope:  authz.ForUser(owner),
			Caller: owner,
			Cursor: cursor,
			Limit:  5,
		})
		if err != nil {
			t.Fatalf("ListAllIssues page %d: %v", pages, err)
		}
		if wantTotal == 0 {
			wantTotal = res.Total
		} else if res.Total != wantTotal {
			t.Errorf("page %d total drifted: was %d now %d", pages, wantTotal, res.Total)
		}
		for _, it := range res.Items {
			if seen[it.ID] > 0 {
				t.Errorf("page %d: duplicate issue id %s", pages, it.ID)
			}
			seen[it.ID]++
		}
		if !res.HasMore {
			break
		}
		if res.NextCursor == nil || *res.NextCursor == "" {
			t.Fatalf("page %d HasMore=true but no nextCursor", pages)
		}
		cursor = res.NextCursor
	}

	if wantTotal != 12 {
		t.Errorf("expected total=12, got %d", wantTotal)
	}
	if pages != 3 {
		t.Errorf("expected 3 pages, got %d", pages)
	}
	// Every seeded issue must have appeared exactly once.
	for i := 1; i <= 12; i++ {
		id := prefix + "-" + strconv.Itoa(i)
		if seen[id] != 1 {
			t.Errorf("issue %s seen %d times across pages (want 1)", id, seen[id])
		}
	}
}

// TestListAllIssues_OnMeFilter — sk-30. The combined filter set
// (CreatedByExclude=caller AND AcceptedAtNotNull=true) must surface
// only tasks assigned to the caller by someone else that have already
// been accepted. Mirror of the wire shape `assignee=me&createdBy=not-me
// &acceptedAt=not-null&status=open`.
func TestListAllIssues_OnMeFilter(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	caller := seedUser(t, pool)
	other := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, caller)

	// Resolve usernames so we can craft assignee values that match the
	// LEFT JOIN ua resolution.
	var callerUsername, otherUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherUsername); err != nil {
		t.Fatalf("read other username: %v", err)
	}

	// Helper to seed an issue with explicit assignee + accepted_at.
	seedOnMe := func(idStr, status, assignee string, acceptedBy *uuid.UUID, creator uuid.UUID) {
		t.Helper()
		var acceptedAtArg, acceptedByArg any
		if acceptedBy != nil {
			acceptedAtArg = time.Now().UTC()
			acceptedByArg = *acceptedBy
		}
		closedAt := "NULL"
		closedReason := "NULL"
		if status == "closed" {
			closedAt = "now()"
			closedReason = "'done'"
		}
		_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata,
                    accepted_at, accepted_by)
VALUES ($1::varchar, $2, NULL, 1, $3, '', 'task', 2, $4, false, 'human', NULL, $5,
        '', '', '', NULL, NULL, NULL,
        NULL, `+closedAt+`, `+closedReason+`,
        NULL, $6, now(), now(), '{}'::jsonb,
        $7, $8)`,
			idStr, pid, "title "+idStr, status, assignee, creator, acceptedAtArg, acceptedByArg)
		if err != nil {
			t.Fatalf("seed on-me issue %s: %v", idStr, err)
		}
	}

	// (A) Other → caller, accepted: SHOULD appear in On-me.
	seedOnMe(prefix+"-1", "open", callerUsername, &caller, other)
	// (B) Other → caller, NOT yet accepted: Inbox prefix, must NOT appear.
	seedOnMe(prefix+"-2", "open", callerUsername, nil, other)
	// (C) Caller → caller, accepted: self-assigned, not "delegated to me".
	seedOnMe(prefix+"-3", "open", callerUsername, &caller, caller)
	// (D) Other → other, accepted: someone else's row, must NOT appear.
	seedOnMe(prefix+"-4", "open", otherUsername, &other, other)
	// (E) Other → caller, accepted, status=closed — On-me UI filters to
	//     open, but at the store level status is a separate axis. We only
	//     pass Status="open" below, so it should be filtered out.
	seedOnMe(prefix+"-5", "closed", callerUsername, &caller, other)

	assigneeID := caller
	excludeID := caller
	t1 := true
	res, err := store.ListAllIssues(context.Background(), ListAllIssuesArgs{
		Scope:             authz.Unrestricted(),
		Caller:            caller,
		CallerUsername:    callerUsername,
		AssigneeUserID:    &assigneeID,
		CreatedByExclude:  &excludeID,
		AcceptedAtNotNull: &t1,
		Status:            "open",
		Limit:             50,
	})
	if err != nil {
		t.Fatalf("ListAllIssues on-me: %v", err)
	}

	gotIDs := make(map[string]bool, len(res.Items))
	for _, it := range res.Items {
		gotIDs[it.ID] = true
	}
	if !gotIDs[prefix+"-1"] {
		t.Errorf("expected on-me issue %s in result", prefix+"-1")
	}
	for _, forbidden := range []string{prefix + "-2", prefix + "-3", prefix + "-4", prefix + "-5"} {
		if gotIDs[forbidden] {
			t.Errorf("on-me filter unexpectedly returned %s", forbidden)
		}
	}
}

// TestListAllIssues_TotalWithAndWithoutAssigneeLens — the list's total is
// the number of issues the list holds, with and without the assignee=me
// filter (sk-674.18, EXPLAIN gate: the count joins users only under
// that filter). Reference — the list contract (total counts the listed
// issues, each once) and INV-30 as T028 words it, written before the code:
// assignee=me resolves to a user only while he may work in the project;
// a handle in any of its three forms names the same person.
//
// Fixture: one project; the worker is a member, the outsider is not. Five
// issues: on the worker by username, on the worker by upper-cased e-mail,
// on the outsider, unassigned, on a handle nobody has. Counterexamples:
// a count that lost the join under the filter (SQL error, or the filter
// dropped); a count that lost the access condition (the outsider's issue
// counted for him); a join that multiplied or dropped an unfiltered row.
func TestListAllIssues_TotalWithAndWithoutAssigneeLens(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	owner := seedUser(t, pool)
	worker := seedUser(t, pool)
	outsider := seedUser(t, pool)
	pid, pref := seedProject(t, pool, owner)
	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, worker, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	handle := func(id uuid.UUID, col string) string {
		var v string
		if err := pool.QueryRow(ctx, `SELECT `+col+` FROM users WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("%s of %s: %v", col, id, err)
		}
		return v
	}
	assignees := []*string{
		ptrStr(handle(worker, "username")),
		ptrStr(strings.ToUpper(handle(worker, "email"))),
		ptrStr(handle(outsider, "username")),
		nil,
		ptrStr("nobody-has-this-handle"),
	}
	for n, a := range assignees {
		id := pref + "-" + strconv.Itoa(n+1)
		seedIssue(t, pool, pid, owner, id, "lens total "+id)
		if _, err := pool.Exec(ctx, `UPDATE issues SET assignee = $2 WHERE id = $1`, id, a); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}

	list := func(t *testing.T, on *uuid.UUID) *ListResult {
		t.Helper()
		res, err := store.ListAllIssues(ctx, ListAllIssuesArgs{
			Scope: authz.ForUser(owner), Caller: owner, Limit: 50, AssigneeUserID: on,
		})
		if err != nil {
			t.Fatalf("ListAllIssues: %v", err)
		}
		if res.Total != len(res.Items) || res.HasMore {
			t.Fatalf("total %d, listed %d (hasMore %v) — the total is not the list's", res.Total, len(res.Items), res.HasMore)
		}
		return res
	}
	ids := func(res *ListResult) map[string]bool {
		m := map[string]bool{}
		for _, it := range res.Items {
			m[it.ID] = true
		}
		return m
	}

	t.Run("no filter: every issue once", func(t *testing.T) {
		res := list(t, nil)
		if res.Total != len(assignees) || len(ids(res)) != len(assignees) {
			t.Errorf("total %d, distinct %d, want %d", res.Total, len(ids(res)), len(assignees))
		}
	})
	t.Run("assignee=me: the worker by both handles", func(t *testing.T) {
		got := ids(list(t, &worker))
		if len(got) != 2 || !got[pref+"-1"] || !got[pref+"-2"] {
			t.Errorf("listed %v, want %s-1 and %s-2", got, pref, pref)
		}
	})
	t.Run("assignee=me: the outsider resolves to nobody", func(t *testing.T) {
		if res := list(t, &outsider); res.Total != 0 {
			t.Errorf("total %d for a user with no access to the project, want 0", res.Total)
		}
	})

	// The same totals on the project list and the per-user lists (sk-712:
	// their counts join users only under the assignee filter as well).
	ownerCtx := callerCtx(t, owner, "user")
	project := func(t *testing.T, on *uuid.UUID) *ListResult {
		t.Helper()
		res, err := store.ListIssues(ownerCtx, ListIssuesArgs{ProjectID: pid, Caller: &owner, Limit: 50, AssigneeUserID: on})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		if res.Total != len(res.Items) || res.HasMore {
			t.Fatalf("project list: total %d, listed %d (hasMore %v)", res.Total, len(res.Items), res.HasMore)
		}
		return res
	}
	t.Run("project list: no filter, every issue once", func(t *testing.T) {
		res := project(t, nil)
		if res.Total != len(assignees) || len(ids(res)) != len(assignees) {
			t.Errorf("total %d, distinct %d, want %d", res.Total, len(ids(res)), len(assignees))
		}
	})
	t.Run("project list: assignee=me", func(t *testing.T) {
		if got := ids(project(t, &worker)); len(got) != 2 || !got[pref+"-1"] || !got[pref+"-2"] {
			t.Errorf("worker: listed %v, want %s-1 and %s-2", got, pref, pref)
		}
		if res := project(t, &outsider); res.Total != 0 {
			t.Errorf("outsider: total %d, want 0", res.Total)
		}
	})
	byUser := func(t *testing.T, user uuid.UUID, mode string) *ListResult {
		t.Helper()
		res, err := store.ListByUser(callerCtx(t, user, "user"), ListByUserArgs{UserID: user, Mode: mode, Scope: authz.ForUser(user), Limit: 50})
		if err != nil {
			t.Fatalf("ListByUser %s: %v", mode, err)
		}
		if res.Total != len(res.Items) || res.HasMore {
			t.Fatalf("%s: total %d, listed %d (hasMore %v)", mode, res.Total, len(res.Items), res.HasMore)
		}
		return res
	}
	t.Run("per-user lists", func(t *testing.T) {
		if got := ids(byUser(t, worker, ModeAssignedTo)); len(got) != 2 || !got[pref+"-1"] || !got[pref+"-2"] {
			t.Errorf("assigned_to the worker: listed %v, want %s-1 and %s-2", got, pref, pref)
		}
		if res := byUser(t, owner, ModeCreatedBy); res.Total != len(assignees) {
			t.Errorf("created_by the owner: total %d, want %d", res.Total, len(assignees))
		}
	})
}
