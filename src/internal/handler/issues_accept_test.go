package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// newAcceptTestPool returns a pool against TEST_DATABASE_URL or skips
// when the env var is unset. Mirrors the convention used by the rest of
// the handler test files.
func newAcceptTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedAcceptUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ah" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, role); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

func seedAcceptProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, prefix string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := prefix + "_" + suffix
	idPrefix := prefix + suffix[:6]
	ctx := t.Context()
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, idPrefix, owner, seedSpaceID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
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
ON CONFLICT (space_id, user_id) DO NOTHING`, id, owner); err != nil {
		t.Fatalf("seed project member: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO project_counters (project_id, counter)
VALUES ($1, 0)
ON CONFLICT (project_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed counter: %v", err)
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
	return id
}

func seedAcceptIssue(t *testing.T, pool *pgxpool.Pool, projectID, creator uuid.UUID, idPrefix, suffix, assignee string) string {
	t.Helper()
	issueID := idPrefix + "-" + suffix
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at, metadata)
VALUES ($1, $2, NULL, 1, 'rest-accept', '', 'task', 2, 'open', false, 'human', NULL, $3,
        '', '', '', NULL, NULL, NULL, NULL, NULL, NULL,
        NULL, $4, now(), now(), '{}'::jsonb)`,
		issueID, projectID, assignee, creator); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	return issueID
}

// callAccept builds an httptest.NewRequest, splices the chi RouteContext
// {issueID}, attaches the auth ctx, and invokes the handler.
func callAccept(t *testing.T, h *IssuesHandler, userID uuid.UUID, role, issueID, body string) *http.Response {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issueID+"/accept", bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("issueID", issueID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.Accept(rec, req)

	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// TestAcceptREST_HappyPath: assignee POSTs /accept → 200 with updated DTO.
func TestAcceptREST_HappyPath(t *testing.T) {
	pool := newAcceptTestPool(t)
	owner, _ := seedAcceptUser(t, pool, "user")
	pid := seedAcceptProject(t, pool, owner, "rh")
	// Lookup id_prefix for issue id derivation.
	var prefix string
	if err := pool.QueryRow(t.Context(),
		`SELECT id_prefix FROM projects WHERE id = $1`, pid).Scan(&prefix); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	uname := readUname(t, pool, owner)
	issueID := seedAcceptIssue(t, pool, pid, owner, prefix, "1", uname)

	projectsStore := projects.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	h := NewIssuesHandler(pool, projectsStore, depsStore)

	res := callAccept(t, h, owner, "user", issueID, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["acceptedAt"] == nil {
		t.Fatalf("acceptedAt = nil, want non-null")
	}
	if body["acceptedBy"] != owner.String() {
		t.Fatalf("acceptedBy = %v, want %v", body["acceptedBy"], owner.String())
	}
}

// TestAcceptREST_Forbidden: caller is not the assignee, not admin → 422
// forbidden.
func TestAcceptREST_Forbidden(t *testing.T) {
	pool := newAcceptTestPool(t)
	owner, _ := seedAcceptUser(t, pool, "user")
	other, _ := seedAcceptUser(t, pool, "user")
	pid := seedAcceptProject(t, pool, owner, "rf")
	// Add other as project member so project visibility passes.
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, other, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	var prefix string
	if err := pool.QueryRow(t.Context(),
		`SELECT id_prefix FROM projects WHERE id = $1`, pid).Scan(&prefix); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	ownerName := readUname(t, pool, owner)
	issueID := seedAcceptIssue(t, pool, pid, owner, prefix, "1", ownerName)

	projectsStore := projects.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	h := NewIssuesHandler(pool, projectsStore, depsStore)

	// `other` calls accept on owner's issue.
	res := callAccept(t, h, other, "user", issueID, "")
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "forbidden" {
		t.Fatalf("error = %v, want forbidden", body["error"])
	}
}

// TestAcceptREST_VersionConflict: stale expectedVersion → 422
// version_conflict with currentVersion echoed.
func TestAcceptREST_VersionConflict(t *testing.T) {
	pool := newAcceptTestPool(t)
	owner, _ := seedAcceptUser(t, pool, "user")
	pid := seedAcceptProject(t, pool, owner, "rv")
	var prefix string
	if err := pool.QueryRow(t.Context(),
		`SELECT id_prefix FROM projects WHERE id = $1`, pid).Scan(&prefix); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	uname := readUname(t, pool, owner)
	issueID := seedAcceptIssue(t, pool, pid, owner, prefix, "1", uname)
	// Bump version so caller's expectedVersion=1 is stale.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET version=2, updated_at=now() WHERE id=$1`, issueID); err != nil {
		t.Fatalf("bump: %v", err)
	}

	projectsStore := projects.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	h := NewIssuesHandler(pool, projectsStore, depsStore)

	res := callAccept(t, h, owner, "user", issueID, `{"expectedVersion": 1}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "version_conflict" {
		t.Fatalf("error = %v, want version_conflict", body["error"])
	}
	if cv, _ := body["currentVersion"].(float64); int(cv) != 2 {
		t.Fatalf("currentVersion = %v, want 2", body["currentVersion"])
	}
}

// TestAcceptREST_NotFound: missing id → 404 issue_not_found.
func TestAcceptREST_NotFound(t *testing.T) {
	pool := newAcceptTestPool(t)
	owner, _ := seedAcceptUser(t, pool, "user")

	projectsStore := projects.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	h := NewIssuesHandler(pool, projectsStore, depsStore)

	res := callAccept(t, h, owner, "user", "nope-9999", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "issue_not_found" {
		t.Fatalf("error = %v, want issue_not_found", body["error"])
	}
}

// TestAcceptREST_CannotAcceptClosed: assignee POSTs /accept on a
// closed issue → 422 cannot_accept_closed and the row is untouched
// (sk-25).
func TestAcceptREST_CannotAcceptClosed(t *testing.T) {
	pool := newAcceptTestPool(t)
	owner, _ := seedAcceptUser(t, pool, "user")
	pid := seedAcceptProject(t, pool, owner, "rc")
	var prefix string
	if err := pool.QueryRow(t.Context(),
		`SELECT id_prefix FROM projects WHERE id = $1`, pid).Scan(&prefix); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	uname := readUname(t, pool, owner)
	issueID := seedAcceptIssue(t, pool, pid, owner, prefix, "1", uname)
	// Flip the seeded row from 'open' to 'closed'. seedAcceptIssue
	// always inserts status='open', so we move it terminal here in a
	// separate UPDATE to keep the helper simple.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET status='closed', closed_at=now(), closed_reason='test',
		                   version=version+1 WHERE id=$1`, issueID); err != nil {
		t.Fatalf("close issue: %v", err)
	}

	projectsStore := projects.NewStore(pool)
	depsStore := dependencies.NewStore(pool)
	h := NewIssuesHandler(pool, projectsStore, depsStore)

	res := callAccept(t, h, owner, "user", issueID, "")
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "cannot_accept_closed" {
		t.Fatalf("error = %v, want cannot_accept_closed", body["error"])
	}

	// accepted_at MUST still be NULL.
	var acceptedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT accepted_at FROM issues WHERE id=$1`, issueID).Scan(&acceptedAt); err != nil {
		t.Fatalf("re-read row: %v", err)
	}
	if acceptedAt != nil {
		t.Fatalf("accepted_at = %v, want NULL", acceptedAt)
	}
}

// readUname loads users.username for the given id.
func readUname(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var u string
	if err := pool.QueryRow(t.Context(),
		`SELECT username FROM users WHERE id = $1`, id).Scan(&u); err != nil {
		t.Fatalf("read username: %v", err)
	}
	if u == "" {
		t.Fatalf("empty username for %s", id)
	}
	return u
}
