package projects

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// sk-159 — an external user (is_external=true) must not exist without at
// least one project membership. RemoveMember deactivates the account
// (deactivated_at) and revokes every live API key, in the same transaction,
// when the external is removed from their LAST project. Internal / OIDC users
// are untouched.

// seedActiveAPIKey inserts one live (revoked_at IS NULL) api_keys row for the
// user and returns its id. Prefix is id-derived so it never trips the partial
// UNIQUE(prefix) WHERE revoked_at IS NULL index across parallel fixtures.
func seedActiveAPIKey(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at)
VALUES ($1, $2, $3, $4, 'sk-159-test', now())`,
		id, userID, "hash-"+id.String(), "p"+id.String()[:11]); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return id
}

// keyRevoked reports whether the api_keys row has revoked_at set.
func keyRevoked(t *testing.T, pool *pgxpool.Pool, keyID uuid.UUID) bool {
	t.Helper()
	var revoked bool
	if err := pool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1`, keyID).Scan(&revoked); err != nil {
		t.Fatalf("read api key revoked state: %v", err)
	}
	return revoked
}

// userDeactivated reports whether users.deactivated_at is set.
func userDeactivated(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) bool {
	t.Helper()
	var deactivated bool
	if err := pool.QueryRow(context.Background(),
		`SELECT deactivated_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&deactivated); err != nil {
		t.Fatalf("read user deactivated state: %v", err)
	}
	return deactivated
}

// deactivationEventCount counts user.deactivated audit events for the user.
func deactivationEventCount(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE type = 'user.deactivated' AND entity_type = 'user' AND entity_id = $1`,
		userID.String()).Scan(&n); err != nil {
		t.Fatalf("count deactivation events: %v", err)
	}
	return n
}

// TestRemoveMember_ExternalLastProject_Deactivates: an external member in
// exactly one project, removed from it, is deactivated and its keys revoked,
// with one user.deactivated audit event.
func TestRemoveMember_ExternalLastProject_Deactivates(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	owner := seedUserForRepoTest(t, pool)
	external := seedExternalUser(t, pool)
	pid := seedNamedProject(t, pool, owner, "s159last-"+uuid.NewString()[:8], "sla"+uuid.NewString()[:6])

	if _, err := store.UpsertMember(ctx, pid, external, "member", owner,
		Actor{UserID: owner, Username: "owner", Role: "human"}); err != nil {
		t.Fatalf("add external member: %v", err)
	}
	key := seedActiveAPIKey(t, pool, external)

	if err := store.RemoveMember(ctx, pid, external, Actor{UserID: owner, Username: "owner", Role: "human"}); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	if !userDeactivated(t, pool, external) {
		t.Errorf("external removed from last project: deactivated_at not set")
	}
	if !keyRevoked(t, pool, key) {
		t.Errorf("external removed from last project: api key not revoked")
	}
	if got := deactivationEventCount(t, pool, external); got != 1 {
		t.Errorf("user.deactivated event count = %d, want 1", got)
	}
}

// TestRemoveMember_ExternalStillHasProject_StaysActive: an external member in
// two projects, removed from one, stays active with live keys and no event.
func TestRemoveMember_ExternalStillHasProject_StaysActive(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	owner := seedUserForRepoTest(t, pool)
	external := seedExternalUser(t, pool)
	pid1 := seedNamedProject(t, pool, owner, "s159a-"+uuid.NewString()[:8], "sa"+uuid.NewString()[:7])
	pid2 := seedNamedProject(t, pool, owner, "s159b-"+uuid.NewString()[:8], "sb"+uuid.NewString()[:7])

	act := Actor{UserID: owner, Username: "owner", Role: "human"}
	if _, err := store.UpsertMember(ctx, pid1, external, "member", owner, act); err != nil {
		t.Fatalf("add external to pid1: %v", err)
	}
	if _, err := store.UpsertMember(ctx, pid2, external, "member", owner, act); err != nil {
		t.Fatalf("add external to pid2: %v", err)
	}
	key := seedActiveAPIKey(t, pool, external)

	if err := store.RemoveMember(ctx, pid1, external, act); err != nil {
		t.Fatalf("RemoveMember(pid1): %v", err)
	}

	if userDeactivated(t, pool, external) {
		t.Errorf("external still a member of pid2 was deactivated; want active")
	}
	if keyRevoked(t, pool, key) {
		t.Errorf("external still a member of pid2 had its key revoked; want live")
	}
	if got := deactivationEventCount(t, pool, external); got != 0 {
		t.Errorf("user.deactivated event count = %d, want 0 (still a member)", got)
	}
}

// TestRemoveMember_InternalLastProject_StaysActive: the deactivation rule is
// external-only — an internal user removed from their last project keeps their
// account and keys (internal users may legitimately hold zero memberships).
func TestRemoveMember_InternalLastProject_StaysActive(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	owner := seedUserForRepoTest(t, pool)
	internal := seedUserForRepoTest(t, pool) // is_external=false
	pid := seedNamedProject(t, pool, owner, "s159int-"+uuid.NewString()[:8], "sni"+uuid.NewString()[:6])
	// INV-5 (T024): a project member is a participant of its space.
	testsupport.JoinProjectSpace(t, pool, pid, internal)

	act := Actor{UserID: owner, Username: "owner", Role: "human"}
	if _, err := store.UpsertMember(ctx, pid, internal, "member", owner, act); err != nil {
		t.Fatalf("add internal member: %v", err)
	}
	key := seedActiveAPIKey(t, pool, internal)

	if err := store.RemoveMember(ctx, pid, internal, act); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	if userDeactivated(t, pool, internal) {
		t.Errorf("internal user removed from last project was deactivated; want active (external-only rule)")
	}
	if keyRevoked(t, pool, key) {
		t.Errorf("internal user's api key was revoked; want live")
	}
	if got := deactivationEventCount(t, pool, internal); got != 0 {
		t.Errorf("user.deactivated event count = %d, want 0 (internal user)", got)
	}
}
