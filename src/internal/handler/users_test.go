package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// newUsersTestPool reuses the convention from auth_test.go: tests skip
// when TEST_DATABASE_URL is unset so `go test ./...` without the local
// stack still passes.
func newUsersTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

// seedUserRow inserts a minimal users row (no password verification
// needed here — these tests bypass /auth/login and call GetMyMemberships
// directly with the user already in ctx).
func seedUserRow(t *testing.T, pool *pgxpool.Pool, role string) uuid.UUID {
	t.Helper()
	id, _ := seedUserRowFull(t, pool, role)
	return id
}

// seedUserRowFull is seedUserRow that also returns the canonical
// username it generated, for tests that look up the user by username
// (e.g. GetUserProfile).
func seedUserRowFull(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "um" + id.String()[:18]
	email := uname + "@test.local"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'unused', now(), '{}'::jsonb)`,
		id, email, uname, uname, role); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

// deactivateUserRow stamps deactivated_at = now() on the supplied user.
// Used by GetUserProfile tests to assert the admin-visible deactivatedAt
// surface comes back populated for archived users.
func deactivateUserRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("deactivate user: %v", err)
	}
}

// seedProject creates a minimal project row owned by ownerID. Returns the
// project id so tests can correlate against the membership response.
func seedProject(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID, idPrefix string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      created_by, created_at, updated_at, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, $4, now(), now(), $5)`,
		id, idPrefix, idPrefix, ownerID, seedSpaceID); err != nil {
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
ON CONFLICT (space_id, user_id) DO NOTHING`, id, ownerID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	t.Cleanup(func() {
		// project_members rows cascade via FK; rely on that.
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

// callGetMyMemberships invokes the handler with a context carrying the
// supplied userID + role headers.
func callGetMyMemberships(t *testing.T, h *UsersHandler, userID uuid.UUID, role string) (*http.Response, MembershipsResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/memberships", nil)
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, role)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.GetMyMemberships(rec, req)

	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	var resp MembershipsResponse
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
			t.Fatalf("decode body: %v", err)
		}
	}
	return res, resp
}

// TestGetMyMemberships_NonAdmin_ReturnsExplicitMembershipsOnly verifies
// the happy path for a regular user: the response carries only the
// project_members rows where user_id = caller, and excludes projects the
// user has no membership in.
func TestGetMyMemberships_NonAdmin_ReturnsExplicitMembershipsOnly(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	owner := seedUserRow(t, pool, "user")
	other := seedUserRow(t, pool, "user")

	pidOwned := seedProject(t, pool, owner, "u1ow"+owner.String()[:4])
	_ = seedProject(t, pool, other, "u1ot"+other.String()[:4])

	res, resp := callGetMyMemberships(t, h, owner, "user")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if len(resp.Memberships) != 1 {
		t.Fatalf("memberships = %d, want 1", len(resp.Memberships))
	}
	got := resp.Memberships[0]
	if got.ProjectID != pidOwned {
		t.Errorf("projectId = %v, want %v", got.ProjectID, pidOwned)
	}
	if got.Role != "owner" {
		t.Errorf("role = %q, want %q", got.Role, "owner")
	}
}

// TestGetMyMemberships_Admin_ReturnsAllProjectsAsSyntheticAdminRows
// verifies the G-1 / A-1 decision: admin sees ALL projects with role
// "admin", regardless of explicit project_members rows.
func TestGetMyMemberships_Admin_ReturnsAllProjectsAsSyntheticAdminRows(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	admin := seedUserRow(t, pool, "admin")
	other := seedUserRow(t, pool, "user")

	pid1 := seedProject(t, pool, other, "u2a"+admin.String()[:4])
	pid2 := seedProject(t, pool, other, "u2b"+admin.String()[:4])

	res, resp := callGetMyMemberships(t, h, admin, "admin")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// Admin path returns synthetic rows for ALL projects (including any
	// pre-existing rows in the DB from prior unrelated tests). Filter to
	// the two we seeded so we don't false-positive on global state.
	var seen1, seen2 bool
	for _, m := range resp.Memberships {
		if m.ProjectID == pid1 {
			seen1 = true
			if m.Role != "admin" {
				t.Errorf("pid1 role = %q, want admin", m.Role)
			}
		}
		if m.ProjectID == pid2 {
			seen2 = true
			if m.Role != "admin" {
				t.Errorf("pid2 role = %q, want admin", m.Role)
			}
		}
	}
	if !seen1 || !seen2 {
		t.Fatalf("admin missing seeded projects: seen1=%v seen2=%v (got %d rows total)",
			seen1, seen2, len(resp.Memberships))
	}
}

// TestGetMyMemberships_Unauthenticated returns 401 when no user is in
// context. Defensive — RequireUser middleware should already have
// returned 401 before the handler runs, but this verifies the local
// guard.
func TestGetMyMemberships_Unauthenticated(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/memberships", nil)
	rec := httptest.NewRecorder()
	h.GetMyMemberships(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// callGetUserProfile invokes the handler through a chi router so the
// {username} URL param is parsed correctly. ctx carries the auth headers
// so UserRoleFrom resolves to the supplied role.
func callGetUserProfile(t *testing.T, h *UsersHandler, callerID uuid.UUID, callerRole, username string) (*http.Response, []byte) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/v1/users/profile/{username}", h.GetUserProfile)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/profile/"+username, nil)
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUserRole(ctx, callerRole)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, rec.Body.Bytes()
}

// shareSpace puts the given users into one regular space with the first of
// them as its owner — INV-9 shows a person to a caller only through a
// common space (sk-674.18 T027), so a profile or directory test whose
// viewer must see the target seeds them together.
func shareSpace(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, members ...uuid.UUID) {
	t.Helper()
	space := testsupport.SeedSpace(t, pool, owner)
	for _, m := range members {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, space, m); err != nil {
			t.Fatalf("share space: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM space_members WHERE space_id = $1 AND user_id = ANY($2)`, space, members)
	})
}

// TestGetUserProfile_EmailOnlyToSelf — sk-674.18 T027, the decision of
// 2026-10-14 (INV-27: no answer carries another user's email except one's
// own account and the admin screens of users — GET /users, not the
// profile). Replaces TestGetUserProfile_AdminViewerSeesEmailAndDeactivatedAt,
// whose property — an admin gets the email and deactivatedAt in the profile —
// the decision retires: the profile carries the email only to its owner, and
// deactivatedAt to nobody. An admin who shares a space with the target sees
// the profile without the email; the owner sees his own email.
func TestGetUserProfile_EmailOnlyToSelf(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	adminID, _ := seedUserRowFull(t, pool, "admin")
	targetID, targetUname := seedUserRowFull(t, pool, "user")
	shareSpace(t, pool, adminID, targetID)
	deactivateUserRow(t, pool, targetID)

	res, body := callGetUserProfile(t, h, adminID, "admin", targetUname)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("admin viewer: status = %d, want 200; body=%s", res.StatusCode, string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"email", "deactivatedAt"} {
		if _, ok := raw[key]; ok {
			t.Errorf("admin viewer of another profile: %s key present (body=%s)", key, string(body))
		}
	}
	if got, _ := raw["username"].(string); got != targetUname {
		t.Errorf("username = %q, want %q", got, targetUname)
	}

	// The owner reads his own profile: the email is there.
	selfID, selfUname := seedUserRowFull(t, pool, "user")
	res, body = callGetUserProfile(t, h, selfID, "user", selfUname)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("own profile: status = %d, want 200; body=%s", res.StatusCode, string(body))
	}
	raw = map[string]any{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if email, _ := raw["email"].(string); email == "" {
		t.Errorf("own profile: email missing (body=%s)", string(body))
	}
}

// TestGetUserProfile_NonAdminViewerOmitsAdminOnlyFields verifies the
// non-admin path: caller with role="user" must NOT see `email` or
// `deactivatedAt` in the JSON. The handler builds the DTO with nil
// pointers + omitempty struct tags so the keys are absent, not just
// empty strings — a non-admin viewer cannot tell whether the target
// is deactivated.
func TestGetUserProfile_NonAdminViewerOmitsAdminOnlyFields(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	viewerID, _ := seedUserRowFull(t, pool, "user")
	targetID, targetUname := seedUserRowFull(t, pool, "user")
	shareSpace(t, pool, viewerID, targetID) // INV-9: a person of a common space

	res, body := callGetUserProfile(t, h, viewerID, "user", targetUname)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", res.StatusCode, string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := raw["email"]; ok {
		t.Errorf("non-admin viewer: email key present (body=%s)", string(body))
	}
	if _, ok := raw["deactivatedAt"]; ok {
		t.Errorf("non-admin viewer: deactivatedAt key present (body=%s)", string(body))
	}
	// Public fields stay populated.
	if got, _ := raw["username"].(string); got != targetUname {
		t.Errorf("username = %q, want %q", got, targetUname)
	}
	if _, ok := raw["role"]; !ok {
		t.Errorf("non-admin viewer: role key missing (body=%s)", string(body))
	}
}

// TestGetUserProfile_UnknownUsernameReturnsBusinessError verifies that
// looking up a username with no matching row returns 422
// `user_not_found` — NOT 404. 404 is reserved for unknown routes;
// business "not found" is 422.
func TestGetUserProfile_UnknownUsernameReturnsBusinessError(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	viewerID, _ := seedUserRowFull(t, pool, "user")

	// Random username that won't match any row. The seedUserRow helper
	// uses an `um<uuid>` prefix; "ghost-..." stays distinct.
	res, body := callGetUserProfile(t, h, viewerID, "user",
		"ghost"+uuid.Must(uuid.NewRandom()).String()[:8])
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", res.StatusCode, string(body))
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, _ := raw["error"].(string); got != "user_not_found" {
		t.Errorf("error code = %q, want %q (body=%s)", got, "user_not_found", string(body))
	}
}

// callListDirectory invokes the directory handler with the supplied caller
// in context and raw query string (without the leading "?"). The directory
// is non-admin — exercising it with role="user" proves there is no admin
// gate.
func callListDirectory(t *testing.T, h *UsersHandler, callerID uuid.UUID, callerRole, query string) (*http.Response, []byte) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/v1/users/directory", h.ListDirectory)

	target := "/api/v1/users/directory"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := auth.WithUserID(req.Context(), callerID)
	ctx = auth.WithUserRole(ctx, callerRole)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, rec.Body.Bytes()
}

// decodeDirectory unwraps the sk-285.5.5 `{items, hasMore}` envelope.
// Fails the test when the body is not that envelope — the endpoint used to
// answer with a bare array and no consumer should silently tolerate both.
func decodeDirectory(t *testing.T, body []byte) users.DirectoryResult {
	t.Helper()
	var out users.DirectoryResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode directory envelope: %v (body=%s)", err, string(body))
	}
	return out
}

// directoryUsernames maps the page's entries by username for membership
// assertions.
func directoryUsernames(t *testing.T, body []byte) map[string]users.UserDirectoryEntry {
	t.Helper()
	res := decodeDirectory(t, body)
	seen := make(map[string]users.UserDirectoryEntry, len(res.Items))
	for _, it := range res.Items {
		seen[it.Username] = it
	}
	return seen
}

// TestListDirectory_NonAdminSeesActiveExcludesDeactivated — sk-79. The
// directory backs the cross-project assignee picker and must be readable
// by a non-admin caller (200, not 403), include active users with their
// id/username/name, and exclude deactivated ones.
//
// sk-285.5.5 — the page is now bounded (default 50) over a shared test DB
// that carries unrelated rows, so "is my seeded user in the response?"
// is asked with a `q=` narrowed to that user instead of against an
// unbounded dump. The deactivated arm gets the same treatment and is
// strictly stronger for it: searching for the archived username by name
// must come back empty, not merely "absent from page 1".
func TestListDirectory_NonAdminSeesActiveExcludesDeactivated(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	callerID, callerUname := seedUserRowFull(t, pool, "user")
	activeID, activeUname := seedUserRowFull(t, pool, "user")
	deactID, deactUname := seedUserRowFull(t, pool, "user")
	deactivateUserRow(t, pool, deactID)
	// INV-9: the people are visible through a common space; the deactivated
	// one shares it too, so his absence is the active-only rule, not
	// visibility.
	shareSpace(t, pool, callerID, activeID, deactID)

	// role="user" — proves the endpoint is NOT admin-gated.
	res, body := callListDirectory(t, h, callerID, "user", "q="+callerUname)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", res.StatusCode, string(body))
	}
	if _, ok := directoryUsernames(t, body)[callerUname]; !ok {
		t.Errorf("caller %q missing from directory (body=%s)", callerUname, string(body))
	}

	_, body = callListDirectory(t, h, callerID, "user", "q="+activeUname)
	if e, ok := directoryUsernames(t, body)[activeUname]; !ok {
		t.Errorf("active user %q missing from directory (body=%s)", activeUname, string(body))
	} else if e.Name == "" || e.ID == uuid.Nil {
		t.Errorf("directory entry %q has empty id/name: %+v", activeUname, e)
	}

	_, body = callListDirectory(t, h, callerID, "user", "q="+deactUname)
	if got := decodeDirectory(t, body); len(got.Items) != 0 {
		t.Errorf("deactivated user %q must not appear in directory, got %+v", deactUname, got.Items)
	}
}
