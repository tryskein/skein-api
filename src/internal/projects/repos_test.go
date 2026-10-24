package projects

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// TEST_DATABASE_URL gates DB-backed tests for the projects package.
// Mirrors the env-name used by internal/issues and internal/mcp/tools
// so a single export drives every integration suite.
const testDatabaseURL = "TEST_DATABASE_URL"

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
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedUserForRepoTest is a minimal user creator for these integration
// tests — duplicated here rather than imported from another package's
// test helpers (which aren't exported).
func seedUserForRepoTest(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
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

// seedProjectWithRepos inserts a project pre-populated with the supplied
// git_repos[] array. Returns project id and the (possibly empty) list of
// repo ids in the same order.
func seedProjectWithRepos(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, prefix string, repos []Repo) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := prefix + "_" + suffix
	idPrefix := prefix + suffix[:6]

	repoBytes, err := json.Marshal(repos)
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}

	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err = pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', $4::jsonb, NULL, $5, now(), now(), '{}'::jsonb, $6)`,
		id, name, idPrefix, string(repoBytes), owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})

	ids := make([]uuid.UUID, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	return id, ids
}

// TestResolveRepoForCommit_NoRepos verifies that a project with zero
// git_repos[] entries returns ErrRepoAmbiguous when no hint is supplied.
func TestResolveRepoForCommit_NoRepos(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfc0", []Repo{})

	_, err := ResolveRepoForCommit(context.Background(), pool, pid, "", nil)
	if !errors.Is(err, ErrRepoAmbiguous) {
		t.Fatalf("err = %v, want ErrRepoAmbiguous", err)
	}
}

// TestResolveRepoForCommit_OneRepo_NoHint: project with exactly one
// repo and no hint → resolves to that repo.
func TestResolveRepoForCommit_OneRepo_NoHint(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	repoID := uuid.Must(uuid.NewV7())
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfc1", []Repo{
		{ID: repoID, Name: "orion-api", URL: "https://example.com/orion-api.git"},
	})

	got, err := ResolveRepoForCommit(context.Background(), pool, pid, "", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != repoID {
		t.Fatalf("got = %s, want %s", got, repoID)
	}
}

// TestResolveRepoForCommit_MultiRepo_NoHint: ambiguous → error.
func TestResolveRepoForCommit_MultiRepo_NoHint(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	r1 := uuid.Must(uuid.NewV7())
	r2 := uuid.Must(uuid.NewV7())
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfcm", []Repo{
		{ID: r1, Name: "api", URL: "https://example.com/api.git"},
		{ID: r2, Name: "web", URL: "https://example.com/web.git"},
	})

	_, err := ResolveRepoForCommit(context.Background(), pool, pid, "", nil)
	if !errors.Is(err, ErrRepoAmbiguous) {
		t.Fatalf("err = %v, want ErrRepoAmbiguous", err)
	}
}

// TestResolveRepoForCommit_RepoURLMatch: repoUrl matches → resolves.
func TestResolveRepoForCommit_RepoURLMatch(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	r1 := uuid.Must(uuid.NewV7())
	r2 := uuid.Must(uuid.NewV7())
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfcu", []Repo{
		{ID: r1, Name: "api", URL: "https://example.com/api.git"},
		{ID: r2, Name: "web", URL: "https://example.com/web.git"},
	})

	got, err := ResolveRepoForCommit(context.Background(), pool, pid, "https://example.com/web.git", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != r2 {
		t.Fatalf("got = %s, want %s", got, r2)
	}
}

// TestResolveRepoForCommit_RepoURLMiss: no match → ErrRepoNotFound.
func TestResolveRepoForCommit_RepoURLMiss(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfcum", []Repo{
		{ID: uuid.Must(uuid.NewV7()), Name: "api", URL: "https://example.com/api.git"},
	})

	_, err := ResolveRepoForCommit(context.Background(), pool, pid, "https://other.example.com/x.git", nil)
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

// TestResolveRepoForCommit_RepoIDMatch: explicit repoId resolves.
func TestResolveRepoForCommit_RepoIDMatch(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	r1 := uuid.Must(uuid.NewV7())
	r2 := uuid.Must(uuid.NewV7())
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfci", []Repo{
		{ID: r1, Name: "a", URL: "https://example.com/a.git"},
		{ID: r2, Name: "b", URL: "https://example.com/b.git"},
	})

	got, err := ResolveRepoForCommit(context.Background(), pool, pid, "", &r2)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != r2 {
		t.Fatalf("got = %s, want %s", got, r2)
	}
}

// TestResolveRepoForCommit_RepoIDWrong: repoId not in project →
// ErrRepoNotFound (and we do NOT silently fall through to repoUrl).
func TestResolveRepoForCommit_RepoIDWrong(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	r1 := uuid.Must(uuid.NewV7())
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfciw", []Repo{
		{ID: r1, Name: "a", URL: "https://example.com/a.git"},
	})

	bogus := uuid.Must(uuid.NewV7())
	_, err := ResolveRepoForCommit(context.Background(), pool, pid, "https://example.com/a.git", &bogus)
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

// TestResolveRepoForCommit_RepoIDPrecedesURL: when both are supplied,
// repoId wins. We pass a valid repoId but a wrong repoUrl — the
// resolver must still pick the repoId-matching entry.
func TestResolveRepoForCommit_RepoIDPrecedesURL(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUserForRepoTest(t, pool)
	r1 := uuid.Must(uuid.NewV7())
	r2 := uuid.Must(uuid.NewV7())
	pid, _ := seedProjectWithRepos(t, pool, owner, "rfcip", []Repo{
		{ID: r1, Name: "a", URL: "https://example.com/a.git"},
		{ID: r2, Name: "b", URL: "https://example.com/b.git"},
	})

	// repoId points to r1 but repoUrl points to r2 — repoId wins.
	got, err := ResolveRepoForCommit(context.Background(), pool, pid, "https://example.com/b.git", &r1)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != r1 {
		t.Fatalf("got = %s, want %s (repoId must take precedence over repoUrl)", got, r1)
	}
}
