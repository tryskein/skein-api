package projects

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"

	"skein-api/internal/testsupport"
)

// sk-163 S3 — never-owner authz for external users, plus the cross-table
// trigger (skein_enforce_external_not_owner, migration 000034) DB backstop.
// App-level guard in UpsertMember is PRIMARY: it returns
// ErrExternalCannotBeOwner before the INSERT. The trigger tests drive raw SQL
// to prove the backstop rejects bypass paths with SQLSTATE 'SK001'.

// seedExternalUser inserts an external (is_external=true, role='user') user
// directly. Used as the target of never-owner assertions.
func seedExternalUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "tx" + strings.ReplaceAll(id.String()[:18], "-", "")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', true, now(), '{}'::jsonb)`,
		id, uname+"@customer.com", uname, uname); err != nil {
		t.Fatalf("seed external user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestUpsertMember_ExternalOwnerRejected — adding an external user as 'owner'
// is refused at the app layer with the clean business error.
func TestUpsertMember_ExternalOwnerRejected(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	owner := seedUserForRepoTest(t, pool)
	external := seedExternalUser(t, pool)
	pid := seedNamedProject(t, pool, owner, "s3owner-"+uuid.NewString()[:8], "s3o"+uuid.NewString()[:6])

	_, err := store.UpsertMember(context.Background(), pid, external, "owner", owner,
		Actor{UserID: owner, Username: "owner", Role: "human"})
	if !errors.Is(err, ErrExternalCannotBeOwner) {
		t.Fatalf("UpsertMember(external,owner) err = %v, want ErrExternalCannotBeOwner", err)
	}

	// No row must have been written.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`,
		pid, external).Scan(&n); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if n != 0 {
		t.Fatalf("external owner row persisted (count=%d), want 0", n)
	}
}

// TestUpsertMember_ExternalMemberOK — an external user CAN be a plain
// 'member'; the cap is at member, not exclusion.
func TestUpsertMember_ExternalMemberOK(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	owner := seedUserForRepoTest(t, pool)
	external := seedExternalUser(t, pool)
	pid := seedNamedProject(t, pool, owner, "s3memb-"+uuid.NewString()[:8], "s3m"+uuid.NewString()[:6])

	dto, err := store.UpsertMember(context.Background(), pid, external, "member", owner,
		Actor{UserID: owner, Username: "owner", Role: "human"})
	if err != nil {
		t.Fatalf("UpsertMember(external,member): %v", err)
	}
	if dto.Role != "member" {
		t.Fatalf("external member role = %q, want member", dto.Role)
	}
}

// TestUpsertMember_InternalOwnerUnaffected — no-regression: an internal user
// is still addable as owner.
func TestUpsertMember_InternalOwnerUnaffected(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	owner := seedUserForRepoTest(t, pool)
	internalCoOwner := seedUserForRepoTest(t, pool)
	pid := seedNamedProject(t, pool, owner, "s3int-"+uuid.NewString()[:8], "s3i"+uuid.NewString()[:6])
	// INV-5 (T024): a project member is a participant of its space.
	testsupport.JoinProjectSpace(t, pool, pid, internalCoOwner)

	dto, err := store.UpsertMember(context.Background(), pid, internalCoOwner, "owner", owner,
		Actor{UserID: owner, Username: "owner", Role: "human"})
	if err != nil {
		t.Fatalf("UpsertMember(internal,owner): %v", err)
	}
	if dto.Role != "owner" {
		t.Fatalf("internal owner role = %q, want owner (no regression)", dto.Role)
	}
}

// TestTrigger_DirectExternalOwnerInsertRejected drives the project_members
// edge of the trigger DIRECTLY (raw INSERT, bypassing UpsertMember) to prove
// the DB backstop rejects an external owner row with SQLSTATE 'SK001'. This is
// the import / direct-SQL path the app-level guard cannot cover.
func TestTrigger_DirectExternalOwnerInsertRejected(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUserForRepoTest(t, pool)
	external := seedExternalUser(t, pool)
	pid := seedNamedProject(t, pool, owner, "s3trig-"+uuid.NewString()[:8], "s3t"+uuid.NewString()[:6])

	_, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, external, owner)
	if err == nil {
		t.Fatalf("direct external owner INSERT succeeded, want trigger rejection")
	}
	if !isExternalOwnerViolation(err) {
		t.Fatalf("direct external owner INSERT err = %v, want SQLSTATE %s", err, pgExternalOwnerViolation)
	}
}

// TestTrigger_DirectInternalOwnerInsertOK — no-regression on the trigger: an
// INTERNAL user can be inserted as owner via raw SQL (the trigger only fires
// the RAISE when the referenced user is external).
func TestTrigger_DirectInternalOwnerInsertOK(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	owner := seedUserForRepoTest(t, pool)
	coOwner := seedUserForRepoTest(t, pool)
	pid := seedNamedProject(t, pool, owner, "s3trok-"+uuid.NewString()[:8], "s3k"+uuid.NewString()[:6])

	if _, err := pool.Exec(ctx, `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, coOwner, owner); err != nil {
		t.Fatalf("direct internal owner INSERT rejected, want success: %v", err)
	}
}

// TestTrigger_FlipUserToExternalWhileOwnerRejected drives the users edge of
// the trigger: flipping is_external false→true on a user who currently OWNS a
// project must be rejected with SQLSTATE 'SK001'. (The app layer never sets
// is_external today — this is purely the DB backstop for the conversion path.)
func TestTrigger_FlipUserToExternalWhileOwnerRejected(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// owner is internal and owns pid (seedNamedProject registers them as owner).
	owner := seedUserForRepoTest(t, pool)
	pid := seedNamedProject(t, pool, owner, "s3flip-"+uuid.NewString()[:8], "s3f"+uuid.NewString()[:6])
	_ = pid

	_, err := pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, owner)
	if err == nil {
		t.Fatalf("flipping owner to external succeeded, want trigger rejection")
	}
	if !isExternalOwnerViolation(err) {
		t.Fatalf("flip-to-external err = %v, want SQLSTATE %s", err, pgExternalOwnerViolation)
	}

	// The flag must remain false (the BEFORE trigger aborted the UPDATE).
	var isExternal bool
	if err := pool.QueryRow(ctx, `SELECT is_external FROM users WHERE id = $1`, owner).Scan(&isExternal); err != nil {
		t.Fatalf("read is_external: %v", err)
	}
	if isExternal {
		t.Fatalf("is_external = true after rejected flip, want false")
	}
}

// TestTrigger_FlipNonOwnerToExternalOK — no-regression on the users edge: a
// user who owns NO project can be flipped to external freely.
func TestTrigger_FlipNonOwnerToExternalOK(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	u := seedUserForRepoTest(t, pool) // not an owner of anything
	if _, err := pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, u); err != nil {
		t.Fatalf("flip non-owner to external rejected, want success: %v", err)
	}
}

// TestCreateProject_ExternalRejected — sk-163 S3 H1. An external caller
// (is_external in ctx) cannot create a project, because CreateProject would
// bootstrap them as owner. The PRIMARY app-gate must refuse cleanly with
// ErrExternalCannotCreateProject and persist NO project row (proving the
// owner-trigger SK001→500 path is never reached).
func TestCreateProject_ExternalRejected(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	external := seedExternalUser(t, pool)
	extCtx := auth.WithIsExternal(
		auth.WithUserRole(
			auth.WithUserID(context.Background(), external), "user"),
		true)

	prefix := "s3cp" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	_, err := store.CreateProject(extCtx, CreateProjectArgs{
		Name:          "s3-external-create-" + uuid.NewString()[:8],
		IDPrefix:      prefix,
		Description:   "",
		DefaultIDMode: "counter",
		CreatorID:     external,
		CreatorRole:   "user",
		CreatorName:   "ext",
		Actor:         Actor{UserID: external, Username: "ext", Role: "human"},
	})
	if !errors.Is(err, ErrExternalCannotCreateProject) {
		t.Fatalf("CreateProject(external) err = %v, want ErrExternalCannotCreateProject", err)
	}

	// No project row may have been written (gate runs before the INSERT).
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM projects WHERE created_by = $1`, external).Scan(&n); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if n != 0 {
		t.Fatalf("external caller created %d project row(s), want 0", n)
	}
}

// TestCreateProject_InternalUnaffected — no-regression: an INTERNAL caller
// (no is_external in ctx) still creates a project and is bootstrapped as owner.
func TestCreateProject_InternalUnaffected(t *testing.T) {
	pool := newTestPool(t)
	store := NewStore(pool)

	owner := seedUserForRepoTest(t, pool)
	intCtx := auth.WithUserRole(auth.WithUserID(context.Background(), owner), "user")

	prefix := "s3ip" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	// T024: a project is created in a space of its creator.
	space := testsupport.SeedSpace(t, pool, owner)
	dto, err := store.CreateProject(intCtx, CreateProjectArgs{
		SpaceID:       space,
		Visibility:    "members",
		Name:          "s3-internal-create-" + uuid.NewString()[:8],
		IDPrefix:      prefix,
		Description:   "",
		DefaultIDMode: "counter",
		CreatorID:     owner,
		CreatorRole:   "user",
		CreatorName:   "owner",
		Actor:         Actor{UserID: owner, Username: "owner", Role: "human"},
	})
	if err != nil {
		t.Fatalf("CreateProject(internal): %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM project_members WHERE project_id = $1`, dto.ID)
		_, _ = pool.Exec(c, `DELETE FROM project_counters WHERE project_id = $1`, dto.ID)
		_, _ = pool.Exec(c, `DELETE FROM projects WHERE id = $1`, dto.ID)
	})

	// The creator must be bootstrapped as owner (no regression).
	var role string
	if err := pool.QueryRow(context.Background(),
		`SELECT role FROM project_members WHERE project_id = $1 AND user_id = $2`,
		dto.ID, owner).Scan(&role); err != nil {
		t.Fatalf("read owner membership: %v", err)
	}
	if role != "owner" {
		t.Fatalf("creator role = %q, want owner (no regression)", role)
	}
}
