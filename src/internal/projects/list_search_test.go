package projects

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestListProjects_Total_Empty seeds nothing and checks that a non-admin
// caller with no project_members rows gets Total=0 (the membership
// filter narrows them down to nothing even though other test fixtures
// populate the projects table).
func TestListProjects_Total_Empty(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	// Deliberately do NOT seed any project for this user.
	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Total != 0 {
		t.Errorf("Total = %d, want 0 (caller has no member rows)", res.Total)
	}
	if len(res.Items) != 0 {
		t.Errorf("Items len = %d, want 0", len(res.Items))
	}
}

// TestListProjects_Total_Matches_CountStar verifies the Total field
// equals raw SELECT count(*) under the same WHERE clause (admin path,
// archived='false' default).
func TestListProjects_Total_Matches_CountStar(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	// Seed a small handful so a fresh DB has known rows.
	for i := 0; i < 3; i++ {
		seedProjectWithRepos(t, pool, owner, "tot", []Repo{})
	}
	// The admin path: since sk-674.18 T009 the list reads the access
	// relation, which takes the admin role from users.
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, owner); err != nil {
		t.Fatalf("make admin: %v", err)
	}
	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Now run the raw COUNT(*) with the same WHERE the store uses for
	// admin/non-mine path: just archived_at IS NULL.
	var raw int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM projects p WHERE p.archived_at IS NULL`).Scan(&raw); err != nil {
		t.Fatalf("count: %v", err)
	}
	if res.Total != raw {
		t.Errorf("Total = %d, raw count(*) = %d", res.Total, raw)
	}
}

// TestListProjects_Search_FTS_NameMatch verifies that a Query against a
// project's name returns it. The FTS layer's name boost (+0.2 ILIKE
// substring) catches even fragments inside a larger name.
func TestListProjects_Search_FTS_NameMatch(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	// Insert a project with a distinctive token nobody else has.
	uniqToken := "zorgon" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	pid, _ := seedProjectWithRepos(t, pool, owner, uniqToken, []Repo{})
	seedListMember(t, pool, pid, owner)

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner, Query: uniqToken, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Total < 1 {
		t.Errorf("Total = %d, want >= 1 (we seeded %s)", res.Total, uniqToken)
	}
	found := false
	for _, it := range res.Items {
		if it.ID == pid {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("project %s with token %s not in search results", pid, uniqToken)
	}
}

// TestListProjects_Search_AliasMatch — `q="demo"` returns a project whose
// alias is "demo" even when its name doesn't contain "demo". This is the
// core motivating case from the user feedback.
func TestListProjects_Search_AliasMatch(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	// Use a uniquely-suffixed alias to avoid collision with another test
	// run's residue.
	aliasUnique := "alx" + strings.ReplaceAll(uuid.New().String()[:6], "-", "")
	pid, _ := seedProjectWithRepos(t, pool, owner, "obfuscated", []Repo{})
	seedListMember(t, pool, pid, owner)

	// Add alias via raw SQL — store.AddAlias path checks ownership + has
	// side-effects we don't need for this assertion.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, 'human', now())`, aliasUnique, pid, owner); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	store := NewStore(pool)
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner, Query: aliasUnique, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, it := range res.Items {
		if it.ID == pid {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("project %s with alias %s not found in search results (items=%d)",
			pid, aliasUnique, len(res.Items))
	}
}

// TestListProjects_Search_NoMatch — random gibberish returns empty.
// We use a token that has no chance of appearing in any seeded fixture.
func TestListProjects_Search_NoMatch(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	store := NewStore(pool)
	// Use a token with characters and length that will not match any
	// real name. Doesn't need a project seeded for this caller — the
	// non-admin permission filter alone would still return 0 even with
	// matches in DB.
	res, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner,
		Query:    "qqxx" + strings.ReplaceAll(uuid.New().String(), "-", ""),
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Items) != 0 {
		t.Errorf("expected 0 items for gibberish query, got %d", len(res.Items))
	}
	if res.Total != 0 {
		t.Errorf("expected Total=0 for gibberish query, got %d", res.Total)
	}
}

// TestListProjects_Cursor_With_Search — paginate through a search result
// set. We seed enough rows with a shared search token to require >1 page
// at limit=2 and confirm the cursor advances honestly.
func TestListProjects_Cursor_With_Search(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	// Seed 5 projects with the same shared token in their name prefix
	// so the FTS layer can find them all.
	token := "cur" + strings.ReplaceAll(uuid.New().String()[:6], "-", "")
	var seededIDs []uuid.UUID
	for i := 0; i < 5; i++ {
		pid, _ := seedProjectWithRepos(t, pool, owner, token, []Repo{})
		seedListMember(t, pool, pid, owner)
		seededIDs = append(seededIDs, pid)
	}

	store := NewStore(pool)
	res1, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner, Query: token, Limit: 2,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if !res1.HasMore || res1.NextCursor == nil {
		t.Fatalf("expected hasMore + cursor on page 1; got hasMore=%v cursor=%v",
			res1.HasMore, res1.NextCursor)
	}

	// Page 2.
	cur, err := DecodeListCursor(*res1.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	res2, err := store.ListProjects(storeFixtureCtx(), ListProjectsArgs{
		CallerID: owner, Query: token, Limit: 2,
		Cursor: &cur,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}

	// First-page IDs must not appear on the second page.
	seenP1 := make(map[uuid.UUID]bool)
	for _, it := range res1.Items {
		seenP1[it.ID] = true
	}
	for _, it := range res2.Items {
		if seenP1[it.ID] {
			t.Errorf("project %s appeared on both page1 and page2", it.ID)
		}
	}
	// Total stays stable across pages — same WHERE.
	if res1.Total != res2.Total {
		t.Errorf("Total drift between pages: %d vs %d", res1.Total, res2.Total)
	}
	if res1.Total < len(seededIDs) {
		t.Errorf("Total = %d, want >= %d (we seeded that many)", res1.Total, len(seededIDs))
	}
}
