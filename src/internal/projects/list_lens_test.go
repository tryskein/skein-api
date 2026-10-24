package projects

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestListProjects_LensOnMe_FiltersByHaving verifies the lens="on-me"
// narrowing: only projects with at least one accepted, other-created,
// caller-assigned issue (status in open/in_progress/blocked) survive
// the HAVING clause. Projects without such issues must be excluded
// from `items` AND from `total`.
func TestListProjects_LensOnMe_FiltersByHaving(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)

	var callerUsername, otherUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherUsername); err != nil {
		t.Fatalf("read other username: %v", err)
	}

	// Project A — has a matching on-me issue → survives the lens.
	pidA, _ := seedProjectWithRepos(t, pool, caller, "lonea", []Repo{})
	seedListMember(t, pool, pidA, caller)
	seedCounter(t, pool, pidA)

	// Project B — has only non-matching issues → dropped by the lens.
	pidB, _ := seedProjectWithRepos(t, pool, caller, "loneb", []Repo{})
	seedListMember(t, pool, pidB, caller)
	seedCounter(t, pool, pidB)

	// Project C — completely empty → dropped by the lens.
	pidC, _ := seedProjectWithRepos(t, pool, caller, "lonec", []Repo{})
	seedListMember(t, pool, pidC, caller)
	seedCounter(t, pool, pidC)

	seedOnMeIssue := func(project uuid.UUID, idStr, status, assignee string, accepted bool, creator uuid.UUID) {
		t.Helper()
		var acceptedByArg any
		acceptedAtSQL := "NULL"
		if accepted {
			acceptedAtSQL = "now()"
			acceptedByArg = caller
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
                    external_ref, created_by, created_at, updated_at, metadata,
                    accepted_at, accepted_by)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, $3, false, 'human', NULL, $4,
        '', '', '', NULL, NULL, NULL, NULL, `+closedAt+`, NULL,
        NULL, $5, now(), now(), '{}'::jsonb,
        `+acceptedAtSQL+`, $6)`,
			idStr, project, status, assignee, creator, acceptedByArg); err != nil {
			t.Fatalf("seed lens issue %s: %v", idStr, err)
		}
	}

	prefixA := "lonea-" + pidA.String()[:8]
	prefixB := "loneb-" + pidB.String()[:8]

	// Project A: one matching on-me issue (other → caller, accepted, open).
	seedOnMeIssue(pidA, prefixA+"-1", "open", callerUsername, true, other)
	// Project B: non-matching (caller created — outbox, not on-me).
	seedOnMeIssue(pidB, prefixB+"-1", "open", otherUsername, true, caller)
	// Project B: non-matching (other → caller, but unaccepted = Inbox prefix).
	seedOnMeIssue(pidB, prefixB+"-2", "open", callerUsername, false, other)
	// Project B: non-matching (other → caller, accepted but closed).
	seedOnMeIssue(pidB, prefixB+"-3", "closed", callerUsername, true, other)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: caller,
		Lens:     "on-me",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	containsByID := func(id uuid.UUID) bool {
		for _, it := range res.Items {
			if it.ID == id {
				return true
			}
		}
		return false
	}
	if !containsByID(pidA) {
		t.Errorf("project A (matching on-me) missing from lens=on-me list (items=%d)", len(res.Items))
	}
	if containsByID(pidB) {
		t.Errorf("project B (non-matching) returned in lens=on-me list — should be filtered out")
	}
	if containsByID(pidC) {
		t.Errorf("project C (empty) returned in lens=on-me list — should be filtered out")
	}

	// Total must mirror the same WHERE+HAVING — caller sees A across
	// all of A/B/C, but only A has matching issues.
	// (We can't be exact here because other tests in the suite may
	// have seeded matching projects under the same caller; assert
	// `total >= 1` and that A is included.)
	if res.Total < 1 {
		t.Errorf("total = %d, want >= 1 (project A matches lens=on-me)", res.Total)
	}

	// Cross-check: lens="" returns all three projects (no narrowing).
	resAll, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: caller,
	})
	if err != nil {
		t.Fatalf("list no-lens: %v", err)
	}
	containsAll := func(id uuid.UUID) bool {
		for _, it := range resAll.Items {
			if it.ID == id {
				return true
			}
		}
		return false
	}
	if !containsAll(pidA) || !containsAll(pidB) || !containsAll(pidC) {
		t.Errorf("no-lens list missing one of A/B/C (A=%v B=%v C=%v)",
			containsAll(pidA), containsAll(pidB), containsAll(pidC))
	}
}

// TestListProjects_LensDelegated_FiltersByHaving mirrors the on-me
// test for the "delegated" lens: only projects where the caller has
// at least one issue they created AND delegated to someone else (open
// / in_progress / blocked) survive the HAVING clause.
func TestListProjects_LensDelegated_FiltersByHaving(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)

	var callerUsername, otherUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, other).Scan(&otherUsername); err != nil {
		t.Fatalf("read other username: %v", err)
	}

	// A — caller-created delegated to other → matches.
	pidA, _ := seedProjectWithRepos(t, pool, caller, "ldela", []Repo{})
	seedListMember(t, pool, pidA, caller)
	seedCounter(t, pool, pidA)
	// B — caller-created but self-assigned → no delegation.
	pidB, _ := seedProjectWithRepos(t, pool, caller, "ldelb", []Repo{})
	seedListMember(t, pool, pidB, caller)
	seedCounter(t, pool, pidB)

	prefixA := "ldela-" + pidA.String()[:8]
	prefixB := "ldelb-" + pidB.String()[:8]
	seedIssue(t, pool, pidA, caller, prefixA+"-1", "open", otherUsername)
	seedIssue(t, pool, pidB, caller, prefixB+"-1", "open", callerUsername)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: caller,
		Lens:     "delegated",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var hasA, hasB bool
	for _, it := range res.Items {
		if it.ID == pidA {
			hasA = true
		}
		if it.ID == pidB {
			hasB = true
		}
	}
	if !hasA {
		t.Errorf("project A (delegated) missing from lens=delegated list")
	}
	if hasB {
		t.Errorf("project B (self-assigned) returned in lens=delegated list — should be filtered out")
	}
}

// TestListComponents_LensOnMe_FiltersByHaving verifies the
// component-level lens narrowing: components without a matching on-me
// issue (linked via issue_components) are excluded.
func TestListComponents_LensOnMe_FiltersByHaving(t *testing.T) {
	pool := newTestPool(t)
	caller := seedUserForRepoTest(t, pool)
	other := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, caller, "lclo", []Repo{})
	seedListMember(t, pool, pid, caller)
	seedCounter(t, pool, pid)

	var callerUsername string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, caller).Scan(&callerUsername); err != nil {
		t.Fatalf("read caller username: %v", err)
	}

	store := NewStore(pool)
	matching, err := store.CreateComponent(context.Background(), pid, caller,
		CreateComponentArgs{Name: "matching"}, HumanActor(caller, "caller"))
	if err != nil {
		t.Fatalf("create matching component: %v", err)
	}
	empty, err := store.CreateComponent(context.Background(), pid, caller,
		CreateComponentArgs{Name: "empty"}, HumanActor(caller, "caller"))
	if err != nil {
		t.Fatalf("create empty component: %v", err)
	}

	// Seed one on-me-matching issue and link to `matching`.
	prefix := "lclo-" + pid.String()[:8]
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata,
                    accepted_at, accepted_by)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, 'open', false, 'human', NULL, $3,
        '', '', '', NULL, NULL, NULL, NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb,
        now(), $5)`,
		prefix+"-1", pid, callerUsername, other, caller); err != nil {
		t.Fatalf("seed lens-matching issue: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`, prefix+"-1", matching.ID); err != nil {
		t.Fatalf("link issue: %v", err)
	}

	out, err := store.ListComponentsWithLens(storeFixtureCtx(), ListComponentsArgs{
		ProjectID: pid,
		Archived:  "",
		CallerID:  caller,
		Lens:      "on-me",
	})
	if err != nil {
		t.Fatalf("list components with lens: %v", err)
	}
	var hasMatching, hasEmpty bool
	for _, c := range out {
		if c.ID == matching.ID {
			hasMatching = true
		}
		if c.ID == empty.ID {
			hasEmpty = true
		}
	}
	if !hasMatching {
		t.Errorf("matching component missing from lens=on-me result")
	}
	if hasEmpty {
		t.Errorf("empty component returned in lens=on-me result — should be filtered out")
	}
}
