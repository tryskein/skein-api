package users

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/google/uuid"
)

// sk-163 S3 — never-admin authz for external users, plus the DB-level
// users_external_not_admin CHECK (000033) backstop. App-level guard is
// PRIMARY: CreateUser / UpdateUser must return ErrExternalCannotBeAdmin
// BEFORE the row reaches the constraint.

// TestCreateUser_ExternalAdminRejected — an external account requested with
// role='admin' is refused with the clean business error, not a raw CHECK 500.
func TestCreateUser_ExternalAdminRejected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "extadmin" + adminID.String()[:6] + "@customer.com"
	_, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:      email,
		Name:       "External Admin Attempt",
		Password:   "passw0rd!",
		Role:       "admin",
		IsExternal: true,
	}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrExternalCannotBeAdmin) {
		t.Fatalf("CreateUser err = %v, want ErrExternalCannotBeAdmin", err)
	}
}

// TestCreateUser_ExternalUserRoleOK — the no-regression control: an external
// account with role='user' creates normally (only admin is refused).
func TestCreateUser_ExternalUserRoleOK(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "extuserok" + adminID.String()[:6] + "@customer.com"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:      email,
		Name:       "External Member",
		Password:   "passw0rd!",
		Role:       "user",
		IsExternal: true,
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser(external,user): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})
}

// TestCreateUser_InternalAdminUnaffected — no-regression: an INTERNAL admin
// create is unchanged by S3.
func TestCreateUser_InternalAdminUnaffected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "intadmin" + adminID.String()[:6] + "@test.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:    email,
		Name:     "Internal Admin",
		Password: "passw0rd!",
		Role:     "admin",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser(internal,admin): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})
	if dto.Role != "admin" {
		t.Fatalf("internal admin role = %q, want admin", dto.Role)
	}
}

// TestUpdateUser_ExternalToAdminRejected — patching an existing external user
// to role='admin' is refused with ErrExternalCannotBeAdmin (guard runs inside
// the SERIALIZABLE tx, before the CHECK).
func TestUpdateUser_ExternalToAdminRejected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	// Create the external target via the store so is_external persists.
	email := "extupd" + adminID.String()[:6] + "@customer.com"
	target, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:      email,
		Name:       "External Target",
		Password:   "passw0rd!",
		Role:       "user",
		IsExternal: true,
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("seed external target: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, target.ID)
	})

	adminRole := "admin"
	_, err = store.UpdateUser(context.Background(), target.ID,
		UpdateUserInput{Role: &adminRole}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrExternalCannotBeAdmin) {
		t.Fatalf("UpdateUser(external→admin) err = %v, want ErrExternalCannotBeAdmin", err)
	}

	// The stored role must be unchanged (still 'user').
	var role string
	if err := pool.QueryRow(context.Background(),
		`SELECT role FROM users WHERE id = $1`, target.ID).Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != "user" {
		t.Fatalf("external target role = %q after rejected promote, want user", role)
	}
}

// TestUpdateUser_InternalToAdminUnaffected — no-regression: promoting an
// INTERNAL user to admin still succeeds.
func TestUpdateUser_InternalToAdminUnaffected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	targetID, _ := seedUserDirect(t, pool, "user")

	adminRole := "admin"
	dto, err := store.UpdateUser(context.Background(), targetID,
		UpdateUserInput{Role: &adminRole}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("UpdateUser(internal→admin): %v", err)
	}
	if dto.Role != "admin" {
		t.Fatalf("internal target role = %q, want admin (no regression)", dto.Role)
	}
}

// TestExternalNotAdmin_DBCheckBackstop drives the same-table CHECK
// (users_external_not_admin, 000033) DIRECTLY — bypassing the app layer — to
// prove the backstop rejects a tampered external+admin row at the DB. This is
// the S1 CHECK half; S3's trigger covers the cross-table owner half (tested
// in the projects package).
func TestExternalNotAdmin_DBCheckBackstop(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	id := uuid.Must(uuid.NewRandom())
	uname := "extck" + strings.ReplaceAll(id.String()[:18], "-", "")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})

	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'admin', 'h', true, now(), '{}'::jsonb)`,
		id, uname+"@t.l", uname, uname)
	if err == nil {
		t.Fatalf("direct external+admin insert succeeded, want CHECK rejection")
	}
	// Pin the EXACT constraint: a CHECK violation (23514) on
	// users_external_not_admin — not a unique violation or some other error
	// that would pass an err!=nil assertion as fake-green.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error is not a *pgconn.PgError: %v", err)
	}
	if pgErr.Code != "23514" {
		t.Fatalf("SQLSTATE = %q, want 23514 (check_violation); err=%v", pgErr.Code, err)
	}
	if !strings.Contains(pgErr.ConstraintName, "users_external_not_admin") {
		t.Fatalf("constraint = %q, want users_external_not_admin", pgErr.ConstraintName)
	}
}
