package users

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// sk-163 S4a — local-password management gate on the two password paths:
// self-service ChangePassword and admin UpdateUser(password). When OIDC is
// active (hybrid/keycloak) an INTERNAL account may neither rotate nor be
// assigned a local password (that would be a Keycloak bypass). External
// accounts and the break-glass admin are exempt; local mode is unchanged.

const gatePassword = "passw0rd!"

// createExternalUser seeds an external user THROUGH the store so is_external
// + a real bcrypt password hash both persist. Returns id + email.
func createExternalUser(t *testing.T, store *Store, adminID uuid.UUID, adminName, label string) (uuid.UUID, string) {
	t.Helper()
	email := label + adminID.String()[:6] + "@customer.com"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:      email,
		Name:       "External " + label,
		Password:   gatePassword,
		Role:       "user",
		IsExternal: true,
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("seed external user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})
	return dto.ID, email
}

func mustHash(t *testing.T, plaintext string) string {
	t.Helper()
	h, err := auth.Hash(plaintext, auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// --- ChangePassword (self-service) ---

func TestChangePasswordS4a_HybridInternalRejected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStoreMode(t, pool, "hybrid", "")
	// Internal user seeded directly (is_external=false). The gate fires
	// before the current-password bcrypt verify, so the placeholder hash is
	// irrelevant — the call must reject on account class.
	targetID, _ := seedUserDirect(t, pool, "user")

	err := store.ChangePassword(context.Background(), targetID, "whatever", "new-passw0rd!")
	if !errors.Is(err, ErrLocalPasswordDisabled) {
		t.Fatalf("ChangePassword(hybrid,internal) err = %v, want ErrLocalPasswordDisabled", err)
	}
}

func TestChangePasswordS4a_HybridExternalAllowed(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStoreMode(t, pool, "hybrid", "")
	adminID, adminName := seedUserDirect(t, pool, "admin")

	uid, _ := createExternalUser(t, store, adminID, adminName, "chgext")
	if err := store.ChangePassword(context.Background(), uid, gatePassword, "new-passw0rd!"); err != nil {
		t.Fatalf("ChangePassword(hybrid,external) = %v, want nil", err)
	}
}

func TestChangePasswordS4a_HybridBreakGlassAllowed(t *testing.T) {
	pool := newTestPool(t)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	// Resolve the break-glass admin's email, then build a store keyed on it.
	var bgEmail string
	if err := pool.QueryRow(context.Background(),
		`SELECT email FROM users WHERE id = $1`, adminID).Scan(&bgEmail); err != nil {
		t.Fatalf("read admin email: %v", err)
	}
	store := newTestStoreMode(t, pool, "hybrid", bgEmail)

	// Give the break-glass admin a real password to rotate.
	hash := mustHash(t, gatePassword)
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET password_hash = $2 WHERE id = $1`, adminID, hash); err != nil {
		t.Fatalf("set admin password: %v", err)
	}
	_ = adminName

	if err := store.ChangePassword(context.Background(), adminID, gatePassword, "new-passw0rd!"); err != nil {
		t.Fatalf("ChangePassword(hybrid,break-glass) = %v, want nil", err)
	}
}

func TestChangePasswordS4a_LocalInternalUnchanged(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStoreMode(t, pool, "local", "")
	adminID, adminName := seedUserDirect(t, pool, "admin")
	// Internal user with a real password, created via the store.
	email := "chgloc" + adminID.String()[:6] + "@test.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "Local Internal", Password: gatePassword, Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("seed internal user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID) })

	if err := store.ChangePassword(context.Background(), dto.ID, gatePassword, "new-passw0rd!"); err != nil {
		t.Fatalf("ChangePassword(local,internal) = %v, want nil (no regression)", err)
	}
}

// --- UpdateUser (admin set password) ---

func TestUpdateUserS4a_HybridInternalPasswordRejected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStoreMode(t, pool, "hybrid", "")
	adminID, adminName := seedUserDirect(t, pool, "admin")
	targetID, _ := seedUserDirect(t, pool, "user") // internal

	newPw := "admin-set-passw0rd!"
	_, err := store.UpdateUser(context.Background(), targetID,
		UpdateUserInput{Password: &newPw}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrLocalPasswordDisabled) {
		t.Fatalf("UpdateUser(hybrid,internal,password) err = %v, want ErrLocalPasswordDisabled", err)
	}
}

func TestUpdateUserS4a_HybridExternalPasswordAllowed(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStoreMode(t, pool, "hybrid", "")
	adminID, adminName := seedUserDirect(t, pool, "admin")
	uid, _ := createExternalUser(t, store, adminID, adminName, "updext")

	newPw := "admin-set-passw0rd!"
	dto, err := store.UpdateUser(context.Background(), uid,
		UpdateUserInput{Password: &newPw}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("UpdateUser(hybrid,external,password) = %v, want nil", err)
	}
	if dto.AuthMethod == "keycloak" {
		t.Fatalf("external user authMethod = %q after password set, want password/both", dto.AuthMethod)
	}
}

func TestUpdateUserS4a_LocalInternalPasswordUnchanged(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStoreMode(t, pool, "local", "")
	adminID, adminName := seedUserDirect(t, pool, "admin")
	targetID, _ := seedUserDirect(t, pool, "user")

	newPw := "admin-set-passw0rd!"
	if _, err := store.UpdateUser(context.Background(), targetID,
		UpdateUserInput{Password: &newPw}, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("UpdateUser(local,internal,password) = %v, want nil (no regression)", err)
	}
}
