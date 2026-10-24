package mentions

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// Integration tests for Resolve(). Skipped unless TEST_DATABASE_URL is
// set — same convention as notifications/fanout_integration_test.go so
// `go test ./...` without the local stack stays quiet.

const testDBURL = "TEST_DATABASE_URL"

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDBURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDBURL)
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

// seedUser inserts a users row with the given role and a fresh
// auto-generated username. Returns (id, username).
func seedUser(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "mu" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-hash', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, role)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "mp_" + suffix
	prefix := "mp" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      archived_at, created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, _ = pool.Exec(ctx, `
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
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

func addMember(t *testing.T, pool *pgxpool.Pool, projectID, userID, addedBy uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
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
ON CONFLICT (space_id, user_id) DO NOTHING`, projectID, userID, addedBy)
	if err != nil {
		t.Fatalf("add project member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, projectID, userID)
	})
}

// TestResolve_VisibleMember covers the happy path: a member of the
// project resolves correctly.
func TestResolve_VisibleMember(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	memberID, memberName := seedUser(t, pool, "user")
	projectID := seedProject(t, pool, owner)
	addMember(t, pool, projectID, memberID, owner)

	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	resolved, err := Resolve(context.Background(), tx, projectID, []string{memberName})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(resolved))
	}
	if resolved[0].UserID != memberID {
		t.Errorf("resolved user id mismatch: got %v, want %v", resolved[0].UserID, memberID)
	}
	if resolved[0].Username != memberName {
		t.Errorf("resolved username mismatch: got %q, want %q", resolved[0].Username, memberName)
	}
}

// TestResolve_HiddenNonMember covers the visibility leak fix: a real
// user who is NOT a project member doesn't resolve.
func TestResolve_HiddenNonMember(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	_, outsiderName := seedUser(t, pool, "user")
	projectID := seedProject(t, pool, owner)
	// Note: NOT adding outsider to project_members.

	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	resolved, err := Resolve(context.Background(), tx, projectID, []string{outsiderName})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected 0 resolved (non-member), got %d: %v", len(resolved), resolved)
	}
}

// TestResolve_MixedVisibility checks a mixed list — some resolve, some
// don't — and the unresolved candidates are silently dropped.
func TestResolve_MixedVisibility(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	memberID, memberName := seedUser(t, pool, "user")
	_, outsiderName := seedUser(t, pool, "user")
	projectID := seedProject(t, pool, owner)
	addMember(t, pool, projectID, memberID, owner)

	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	candidates := []string{memberName, outsiderName, "ghost-user-no-such"}
	resolved, err := Resolve(context.Background(), tx, projectID, candidates)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].UserID != memberID {
		t.Fatalf("expected only the project member to resolve, got %v", resolved)
	}
}

// TestResolve_DeactivatedUserDropped covers the deactivated-user
// filter: a user whose deactivated_at IS NOT NULL never resolves, even
// if they're a project member or admin. Without this gate a deactivated
// user could be mentioned, accumulate inbox rows, and receive a flood
// on reactivation.
func TestResolve_DeactivatedUserDropped(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	memberID, memberName := seedUser(t, pool, "user")
	adminID, adminName := seedUser(t, pool, "admin")
	projectID := seedProject(t, pool, owner)
	addMember(t, pool, projectID, memberID, owner)

	// Deactivate both the project member and the admin so we cover both
	// branches of the visibility predicate.
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET deactivated_at = now() WHERE id = ANY($1)`,
		[]uuid.UUID{memberID, adminID}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	resolved, err := Resolve(context.Background(), tx, projectID,
		[]string{memberName, adminName})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected 0 resolved (both deactivated), got %d: %v",
			len(resolved), resolved)
	}
}

// TestResolve_Empty confirms an empty candidate slice short-circuits.
func TestResolve_Empty(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	projectID := seedProject(t, pool, owner)

	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	resolved, err := Resolve(context.Background(), tx, projectID, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved != nil {
		t.Errorf("expected nil for empty input, got %v", resolved)
	}
}
