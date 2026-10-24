package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// testDatabaseURL is the env var an operator sets to point unit tests at
// a real Postgres instance. Tests that need DB access skip when it's
// unset so a developer running `go test ./...` without the local stack
// up doesn't see noise.
const testDatabaseURL = "TEST_DATABASE_URL"

// newTestPool returns a *pgxpool.Pool against the URL in TEST_DATABASE_URL,
// or skips the test when the env var is unset. The pool is closed via
// t.Cleanup so each test gets isolated lifecycle.
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

// seedUser inserts a fresh users row with a random uuid + username and
// returns the (id, username, role) triple. Tests use these to populate
// auth context. Cleaned up via t.Cleanup with cascading FK to all
// downstream rows the test creates.
func seedUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	// Use a chunk of the random id (full v4 = unique) plus a tiny static
	// prefix so the username matches the [a-z0-9_-]{3,30} regex.
	uname := "tu" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)
ON CONFLICT (id) DO NOTHING`,
		id, uname+"@test.local", uname, uname, role)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

// seedProject creates a project row + bootstraps the user as owner (so
// they can see it). Returns the project id and id_prefix. Cleanup
// removes the project (CASCADE handles project_members, components,
// issues, deps).
func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, namePrefix string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := namePrefix + "_" + suffix
	prefix := namePrefix + suffix[:6]
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
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
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

// seedIssue inserts an issues row with the given id (already prefixed)
// and minimal fields. Returns the id.
//
// closed status writes the closed_at/closed_reason consistency columns
// because the issues_closed_consistency CHECK constraint requires them.
//
// project_counters.counter is bumped to MAX(existing, parsed-suffix) so
// that subsequent CreateIssue calls in counter mode don't collide with
// seeded ids — `prefix-1` seeded directly, then CreateIssue claims the
// next counter, which would land on 1 and collide. Callers that seed
// numeric-suffix ids get the counter advanced for free; non-numeric
// suffixes (custom mode) are ignored.
func seedIssue(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, creator uuid.UUID, idStr, title, status string, priority int) string {
	t.Helper()
	ctx := context.Background()

	closedAt := "NULL"
	closedReason := "NULL"
	if status == "closed" {
		closedAt = "now()"
		closedReason = "''"
	}

	sql := `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, $3, '', 'task', $4, $5, false, 'human', NULL, NULL,
        '', '', '', NULL, NULL, NULL,
        NULL, ` + closedAt + `, ` + closedReason + `,
        NULL, $6, now(), now(), '{}'::jsonb)`
	_, err := pool.Exec(ctx, sql,
		idStr, projectID, title, priority, status, creator)
	if err != nil {
		t.Fatalf("seed issue %s: %v", idStr, err)
	}

	// Best-effort counter bump for ids that look like "<prefix>-<N>".
	// On parse failure we skip — custom-mode ids like "demo-fixed-001"
	// don't drive the project counter anyway.
	if dash := strings.LastIndex(idStr, "-"); dash > 0 {
		suffix := idStr[dash+1:]
		var n int
		if _, perr := fmt.Sscanf(suffix, "%d", &n); perr == nil && n > 0 {
			_, _ = pool.Exec(ctx, `
UPDATE project_counters
   SET counter = GREATEST(counter, $2)
 WHERE project_id = $1`, projectID, n)
		}
	}
	return idStr
}

// withCtx builds a context populated with the same auth values the
// production middleware would set. RequireUser inside the tool handlers
// reads UserIDFrom; visibility checks read UserRoleFrom.
func withCtx(t *testing.T, userID uuid.UUID, role string) context.Context {
	t.Helper()
	ctx := context.Background()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	ctx = auth.WithSessionID(ctx, "test-session-"+userID.String()[:8])
	// MCP tools run behind the api-key middleware, which stashes the
	// resolved api_keys.id in ctx. The handler uses this signal to
	// classify the caller as "agent" (vs cookie-auth "human"); without
	// it, agent-only behaviours (e.g. skein_create's `kind` requirement)
	// are silently skipped in tests.
	ctx = auth.WithAPIKeyID(ctx, uuid.Must(uuid.NewRandom()))
	return ctx
}
