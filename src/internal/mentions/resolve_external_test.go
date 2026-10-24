package mentions

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedExternalUser inserts an is_external=true users row (role forced to
// 'user' — the 000033 CHECK forbids is_external + admin). Returns
// (id, username).
func seedExternalUser(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "mx" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, is_external, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', true, 'fake-hash', now(), '{}'::jsonb)`,
		id, uname+"@ext.local", uname, uname)
	if err != nil {
		t.Fatalf("seed external user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

// TestResolve_ExternalMemberResolves is the sk-163 S6 consistency guard
// for the `AND NOT u.is_external` clamp on the admin arm of Resolve().
//
// The clamp's ONLY behavioural target — a row that is BOTH role='admin'
// AND is_external=true — cannot be constructed: migration 000033 installs
// a CHECK forbidding exactly that combination. So this test instead proves
// the clamp did not regress the two real resolution paths:
//
//   - an EXTERNAL user who IS a project member still resolves (membership
//     arm, untouched by the clamp), and
//   - the existing TestResolve_AdminBypass continues to resolve an
//     INTERNAL admin via the admin arm (clamp keeps `role='admin' AND NOT
//     is_external`, and an internal admin has is_external=false).
//
// Together they pin the clamp to "external rows never short-circuit the
// admin arm" without breaking legitimate member / internal-admin mentions.
func TestResolve_ExternalMemberResolves(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	extID, extName := seedExternalUser(t, pool)
	projectID := seedProject(t, pool, owner)
	addMember(t, pool, projectID, extID, owner)

	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	resolved, err := Resolve(context.Background(), tx, projectID, []string{extName})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].UserID != extID {
		t.Fatalf("external member should resolve via the membership arm; got %v", resolved)
	}
}

// TestResolve_ExternalNonMemberHidden confirms an external user who is NOT
// a member does not resolve — the clamp must not accidentally open a new
// resolution path for external rows. (Symmetric to TestResolve_HiddenNonMember
// for internal non-members.)
func TestResolve_ExternalNonMemberHidden(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	_, extName := seedExternalUser(t, pool)
	projectID := seedProject(t, pool, owner)
	// external user is deliberately NOT added to project_members.

	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	resolved, err := Resolve(context.Background(), tx, projectID, []string{extName})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected 0 resolved (external non-member), got %d: %v", len(resolved), resolved)
	}
}
