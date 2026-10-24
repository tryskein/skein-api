package issues

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// TEST_DATABASE_URL gates DB-backed tests. When unset, tests are
// skipped — same convention as internal/mcp/tools.
const testDatabaseURL = "TEST_DATABASE_URL"

// newTestPool returns a *pgxpool.Pool against TEST_DATABASE_URL or
// skips the test when the env var is unset.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect %s: %v", testDatabaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping %s: %v", testDatabaseURL, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedUser inserts a fresh users row and registers cleanup.
func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "tu" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// seedProject creates a project with a unique id_prefix and registers
// cleanup. CASCADE handles components and junctions; issues live behind
// an ON DELETE RESTRICT FK (migration 000004) so they must be cleaned
// by their own helpers (seedIssue) — without that the leftover rows
// pollute every cross-project test in the shared DB.
func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "isstest_" + suffix
	prefix := "ist" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err = pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner)
	if err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed project counter: %v", err)
	}
	t.Cleanup(func() {
		// issues.project_id is ON DELETE RESTRICT (migration 000004), so
		// delete issues leaf-first (children before parents) before the
		// project, else the project DELETE silently fails and rows leak.
		for range 64 {
			ct, err := pool.Exec(context.Background(), `
DELETE FROM issues
 WHERE project_id = $1
   AND id NOT IN (SELECT parent_id FROM issues WHERE project_id = $1 AND parent_id IS NOT NULL)`, id)
			if err != nil || ct.RowsAffected() == 0 {
				break
			}
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id, prefix
}

// seedComponent inserts a component into the project and registers
// cleanup. Returns the component id.
func seedComponent(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, '', '[]'::jsonb, NULL, $4, now(), now())`,
		id, projectID, name, creator)
	if err != nil {
		t.Fatalf("seed component: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM components WHERE id = $1`, id)
	})
	return id
}

// seedIssue inserts a minimal issues row.
func seedIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idStr, title string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', 2, 'open', false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		idStr, projectID, title, creator)
	if err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issues WHERE id = $1`, idStr)
	})
}

// pinComponent attaches the issue to the component via issue_components.
func pinComponent(t *testing.T, pool *pgxpool.Pool, issueID string, componentID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO issue_components (issue_id, component_id) VALUES ($1, $2)`,
		issueID, componentID)
	if err != nil {
		t.Fatalf("pin component: %v", err)
	}
}

// TestListIssues_ComponentFilter_HappyPath verifies the new ComponentID
// filter returns only issues attached to the requested component, even
// when many other issues exist at the front of the order.
func TestListIssues_ComponentFilter_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	compID := seedComponent(t, pool, pid, owner, "alpha")

	// 5 unrelated open issues created first so they sort to the front
	// of (updated_at DESC, id DESC). Without the SQL filter, a Go-side
	// post-filter would drop the four targets after only seeing these.
	for i := 1; i <= 5; i++ {
		seedIssue(t, pool, pid, owner, prefix+"-"+itoa(i), "noise "+itoa(i))
	}
	// 2 issues pinned to the component, deliberately not first in the
	// order (they get higher numeric ids but the same updated_at; the
	// filter must still surface them).
	for i := 6; i <= 7; i++ {
		seedIssue(t, pool, pid, owner, prefix+"-"+itoa(i), "target "+itoa(i))
		pinComponent(t, pool, prefix+"-"+itoa(i), compID)
	}

	store := NewStore(pool)
	res, err := store.ListIssues(context.Background(), ListIssuesArgs{
		ProjectID:   pid,
		ComponentID: &compID,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("expected 2 component-pinned issues, got %d (ids: %v)", len(res.Items), idsOf(res.Items))
	}
	gotIDs := map[string]bool{}
	for _, it := range res.Items {
		gotIDs[it.ID] = true
	}
	for i := 6; i <= 7; i++ {
		want := prefix + "-" + itoa(i)
		if !gotIDs[want] {
			t.Errorf("missing pinned issue %s in result %v", want, idsOf(res.Items))
		}
	}
}

// TestListIssues_ComponentFilter_ZeroResults verifies that a component
// with no issues attached returns an empty result, even when the
// project has many issues.
func TestListIssues_ComponentFilter_ZeroResults(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	compID := seedComponent(t, pool, pid, owner, "lonely")

	for i := 1; i <= 4; i++ {
		seedIssue(t, pool, pid, owner, prefix+"-"+itoa(i), "no-component "+itoa(i))
	}

	store := NewStore(pool)
	res, err := store.ListIssues(context.Background(), ListIssuesArgs{
		ProjectID:   pid,
		ComponentID: &compID,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("expected 0 results, got %d", len(res.Items))
	}
}

// itoa is a tiny helper because we don't want to drag in strconv just
// to format two-digit suffixes.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func idsOf(items []IssueSummary) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}
