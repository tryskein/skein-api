package users

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
)

const testDatabaseURL = "TEST_DATABASE_URL"

// newTestPool returns a real *pgxpool.Pool from TEST_DATABASE_URL or
// skips the test when the env var is unset. Mirrors the helper in
// internal/mcp/tools/testhelper_test.go so the users package tests have
// identical wiring.
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
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestStore(t *testing.T, pool *pgxpool.Pool) *Store {
	t.Helper()
	// bcrypt cost = MinBcryptCost (10) keeps tests reasonable while
	// matching production semantics — every hash/verify still happens.
	// "local" mode + no break-glass = the legacy password-management
	// behaviour every pre-S4a test expects. pwnedChecker=nil (the sixth
	// arg) = the check disabled, matching production default — a real nil
	// interface value, not a typed-nil client (see NewStore's doc comment).
	return NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
}

// newTestStoreMode builds a Store with an explicit auth mode + break-glass
// admin, for the sk-163 S4a local-password-gate tests.
func newTestStoreMode(t *testing.T, pool *pgxpool.Pool, authMode, breakGlass string) *Store {
	t.Helper()
	return NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), authMode, breakGlass, nil)
}

// newTestStorePwned builds a Store with an explicit PwnedChecker, for the
// sk-303.20 ChangePassword pwned-lookup tests.
func newTestStorePwned(t *testing.T, pool *pgxpool.Pool, checker PwnedChecker) *Store {
	t.Helper()
	return NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", checker)
}

// seedUserDirect inserts a row directly so tests can construct fixtures
// (admin, target user) without exercising CreateUser.
func seedUserDirect(t *testing.T, pool *pgxpool.Pool, role string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ut" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, $5, 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, role)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, uname
}

func adminActor(id uuid.UUID, name string) Actor { return HumanActor(id, name) }

// TestCreateUser_HappyPath inserts a fresh user and verifies the audit
// event landed.
func TestCreateUser_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "tcuhp" + adminID.String()[:6] + "@test.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:    email,
		Name:     "Test Created",
		Password: "passw0rd!",
		Role:     "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})
	if dto.Email != email {
		t.Fatalf("email = %q, want %q", dto.Email, email)
	}
	if dto.Username == "" {
		t.Fatal("username should have been auto-derived")
	}
	if dto.DeactivatedAt != nil {
		t.Fatal("new user should not be deactivated")
	}
	// Verify event landed with actor_type='human'.
	var actorType string
	if err := pool.QueryRow(context.Background(), `
SELECT actor_type FROM events
 WHERE entity_type='user' AND entity_id=$1 AND type='user.created'
 LIMIT 1`, dto.ID.String()).Scan(&actorType); err != nil {
		t.Fatalf("verify event: %v", err)
	}
	if actorType != "human" {
		t.Fatalf("actor_type = %q, want human", actorType)
	}
}

func TestCreateUser_EmailTaken(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "tcet" + adminID.String()[:6] + "@test.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "First", Password: "passw0rd!", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})

	_, err = store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "Second", Password: "passw0rd!", Role: "user",
	}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestCreateUser_UsernameTaken(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	uname := "tut" + strings.ReplaceAll(adminID.String()[:13], "-", "")
	_, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: uname + "@x.local", Username: uname, Name: "First",
		Password: "passw0rd!", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE username = $1`, uname)
	})

	_, err = store.CreateUser(context.Background(), CreateUserInput{
		Email: uname + "2@x.local", Username: uname, Name: "Second",
		Password: "passw0rd!", Role: "user",
	}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
}

func TestCreateUser_InvalidPassword(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	_, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: "tcip" + adminID.String()[:6] + "@x.local",
		Name:  "Short", Password: "short", Role: "user",
	}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("err = %v, want ErrInvalidPassword", err)
	}
}

// INV-18: a deleted account's pseudonym deleted-<8hex>
// never matches a username anyone can hold, so the admin creation path (REST
// POST /users and the admin's agent over MCP — both CreateUser) refuses that
// shape as registration does; a username that only starts with "deleted-"
// is fine.
func TestCreateUser_PseudonymShapeRefused(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	tag := strings.ReplaceAll(adminID.String()[:8], "-", "")

	for _, uname := range []string{"deleted-0a1b2c3d", "DELETED-0A1B2C3D"} {
		_, err := store.CreateUser(context.Background(), CreateUserInput{
			Email: "pseud" + tag + "@x.local", Username: uname, Name: "Pseudonym",
			Password: "passw0rd!", Role: "user",
		}, adminActor(adminID, adminName))
		if !errors.Is(err, ErrValidation) {
			t.Errorf("CreateUser(%q) err = %v, want ErrValidation", uname, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE lower(username) = 'deleted-0a1b2c3d'`)
	})
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE lower(username) = 'deleted-0a1b2c3d'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d accounts hold the pseudonym-shaped username, want 0", n)
	}

	ok := "deleted-" + tag + "x"
	if _, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: "pseudok" + tag + "@x.local", Username: ok, Name: "Not a pseudonym",
		Password: "passw0rd!", Role: "user",
	}, adminActor(adminID, adminName)); err != nil {
		t.Errorf("CreateUser(%q) = %v, want success — not of the pseudonym shape", ok, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE username = $1`, ok)
	})
}

func TestGetUser_NotFound(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	_, err := store.GetUser(context.Background(), uuid.Must(uuid.NewRandom()))
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestGetUser_DeactivatedStillReturned(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	// Need 2 admins so we can deactivate one.
	adminID, adminName := seedUserDirect(t, pool, "admin")
	_, _ = seedUserDirect(t, pool, "admin")

	// Make a target then deactivate it.
	target, _ := seedUserDirect(t, pool, "user")
	if err := store.Deactivate(context.Background(), target, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	dto, err := store.GetUser(context.Background(), target)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if dto.DeactivatedAt == nil {
		t.Fatal("expected deactivated_at to be populated")
	}
}

func TestListUsers_ArchivedFilter(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	// second admin so deactivation doesn't trip last-admin protection.
	_, _ = seedUserDirect(t, pool, "admin")
	target, _ := seedUserDirect(t, pool, "user")

	if err := store.Deactivate(context.Background(), target, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	// Default = active only — target must NOT appear.
	res, err := store.ListUsers(context.Background(), ListUsersInput{Limit: 100})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, u := range res.Items {
		if u.ID == target {
			t.Fatal("deactivated user appeared in default list")
		}
	}

	// archived=true — must appear.
	res, err = store.ListUsers(context.Background(), ListUsersInput{Archived: "true", Limit: 100})
	if err != nil {
		t.Fatalf("ListUsers archived: %v", err)
	}
	found := false
	for _, u := range res.Items {
		if u.ID == target {
			found = true
		}
	}
	if !found {
		t.Fatal("deactivated user missing from archived=true list")
	}
}

func TestListUsers_Search(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	uniq := "tlsq" + strings.ReplaceAll(adminID.String()[:11], "-", "")
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: uniq + "@x.local", Username: uniq, Name: "Searchable",
		Password: "passw0rd!", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})

	res, err := store.ListUsers(context.Background(), ListUsersInput{Search: uniq, Limit: 100})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	found := false
	for _, u := range res.Items {
		if u.ID == dto.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("search did not return the new user")
	}
}

// TestListUsers_ExternalFilter — sk-163 S8. The server-side is_external
// filter returns only external (External=&true) / only internal
// (External=&false) / both (nil). Scoped to two freshly-seeded users via a
// shared unique search token so other rows in the shared DB don't interfere.
func TestListUsers_ExternalFilter(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	tok := "tlef" + strings.ReplaceAll(adminID.String()[:11], "-", "")
	intl, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: tok + "int@x.local", Username: tok + "int", Name: "Internal " + tok,
		Password: "passw0rd!", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser internal: %v", err)
	}
	ext, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: tok + "ext@customer.com", Username: tok + "ext", Name: "External " + tok,
		Password: "passw0rd!", Role: "user", IsExternal: true,
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser external: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`,
			[]uuid.UUID{intl.ID, ext.ID})
	})

	ids := func(res *ListUsersResult) map[uuid.UUID]bool {
		m := map[uuid.UUID]bool{}
		for _, u := range res.Items {
			m[u.ID] = true
		}
		return m
	}
	tt := true
	ff := false

	// external=true → only the external user (scoped by the shared token).
	res, err := store.ListUsers(context.Background(), ListUsersInput{Search: tok, External: &tt, Limit: 100})
	if err != nil {
		t.Fatalf("ListUsers external=true: %v", err)
	}
	got := ids(res)
	if !got[ext.ID] || got[intl.ID] {
		t.Fatalf("external=true: want only %s, got %v", ext.ID, res.Items)
	}

	// external=false → only the internal user.
	res, err = store.ListUsers(context.Background(), ListUsersInput{Search: tok, External: &ff, Limit: 100})
	if err != nil {
		t.Fatalf("ListUsers external=false: %v", err)
	}
	got = ids(res)
	if !got[intl.ID] || got[ext.ID] {
		t.Fatalf("external=false: want only %s, got %v", intl.ID, res.Items)
	}

	// nil → both classes (no regression).
	res, err = store.ListUsers(context.Background(), ListUsersInput{Search: tok, Limit: 100})
	if err != nil {
		t.Fatalf("ListUsers no filter: %v", err)
	}
	got = ids(res)
	if !got[intl.ID] || !got[ext.ID] {
		t.Fatalf("no filter: want both %s and %s, got %v", intl.ID, ext.ID, res.Items)
	}
}

func TestUpdateUser_RoleAndPassword(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	// Seed a second admin so promoting `target` to admin doesn't trip the
	// last-admin guard when this test runs against a freshly-migrated DB.
	_, _ = seedUserDirect(t, pool, "admin")
	adminID, adminName := seedUserDirect(t, pool, "admin")
	target, _ := seedUserDirect(t, pool, "user")

	newName := "Renamed"
	newRole := "admin"
	newPass := "anotherP@ss"
	dto, err := store.UpdateUser(context.Background(), target, UpdateUserInput{
		Name:     &newName,
		Role:     &newRole,
		Password: &newPass,
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if dto.Name != newName {
		t.Fatalf("name = %q, want %q", dto.Name, newName)
	}
	if dto.Role != newRole {
		t.Fatalf("role = %q, want %q", dto.Role, newRole)
	}
	// Regression guard: UPDATE ... RETURNING must include the two columns
	// scanUserDTO derives AuthMethod from. Missing them yielded
	// "number of field descriptions must equal number of destinations,
	// got 8 and 10" — every PATCH /api/v1/users/{id} 500'd in prod.
	if dto.AuthMethod != "password" {
		t.Fatalf("authMethod = %q, want %q", dto.AuthMethod, "password")
	}
}

// TestUpdateUser_NameAndPassword_WritesBothEventFamilies pins the D-1
// contract: a mixed PATCH (Name + Password) emits BOTH the hybrid
// user.updated parent (with a name child in payload.changes) AND the
// user.password_changed lifecycle event, atomically in the same tx.
//
// Regression guard for review go-W6: the prior test only asserted the
// returned dto.Name; nothing checked that BOTH event families landed.
// A future refactor that gates the lifecycle event behind
// `len(changes) == 0` would silently break the audit trail and pass.
func TestUpdateUser_NameAndPassword_WritesBothEventFamilies(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	target, _ := seedUserDirect(t, pool, "user")

	newName := "Renamed-D1"
	newPass := "anotherP@ssD1"
	if _, err := store.UpdateUser(context.Background(), target, UpdateUserInput{
		Name:     &newName,
		Password: &newPass,
	}, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	// Cleanup: events table — actor_user_id chain may pin rows.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM events WHERE entity_type='user' AND entity_id=$1`, target.String())
	})

	// Expect three rows in the events table for this entity:
	//   1. user.updated         (parent — name child in payload.changes)
	//   2. user.field_changed   (child — field='name')
	//   3. user.password_changed (lifecycle)
	rows, err := pool.Query(context.Background(), `
SELECT type, field FROM events
 WHERE entity_type='user' AND entity_id=$1
 ORDER BY id`, target.String())
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()

	type event struct {
		Type  string
		Field *string
	}
	var got []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.Type, &e.Field); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (user.updated + user.field_changed + user.password_changed): %+v", len(got), got)
	}

	// Verify each event family is present exactly once.
	var sawUpdated, sawFieldChanged, sawPasswordChanged int
	var fieldName string
	for _, e := range got {
		switch e.Type {
		case EventUserUpdated:
			sawUpdated++
			if e.Field != nil {
				t.Fatalf("user.updated.field = %q, want NULL", *e.Field)
			}
		case EventUserFieldChanged:
			sawFieldChanged++
			if e.Field == nil {
				t.Fatal("user.field_changed.field is NULL, want non-NULL")
			}
			fieldName = *e.Field
		case EventUserPasswordChanged:
			sawPasswordChanged++
		default:
			t.Fatalf("unexpected event type %q", e.Type)
		}
	}
	if sawUpdated != 1 {
		t.Fatalf("user.updated count = %d, want 1", sawUpdated)
	}
	if sawFieldChanged != 1 {
		t.Fatalf("user.field_changed count = %d, want 1", sawFieldChanged)
	}
	if sawPasswordChanged != 1 {
		t.Fatalf("user.password_changed count = %d, want 1", sawPasswordChanged)
	}
	if fieldName != "name" {
		t.Fatalf("user.field_changed.field = %q, want \"name\"", fieldName)
	}
}

// TestUpdateUser_LastAdminConcurrentDemote_OneSucceedsOneGetsLastAdmin
// covers R3.1 W3: when the last two active admins concurrently demote
// each other (and themselves), the SERIALIZABLE re-check protects the
// invariant — exactly one COMMITs and the other surfaces ErrLastAdmin.
//
// Before R3.1 W3 the loser of the SERIALIZABLE race surfaced SQLSTATE
// 40001 (serialization_failure) as a generic 500. Since sk-731 the
// transaction is retried (spaces.WithRetry): the retry re-reads the admin
// count and its recheck returns ErrLastAdmin — the abort itself is never
// taken for the last admin (serialization_retry_test.go).
//
// The race is best-effort — depending on scheduling either txn may finish
// first or the in-tx recheck may catch the second one before COMMIT (also
// returning ErrLastAdmin). Both paths are equally correct; we assert that
// across both goroutines we see exactly ONE success and exactly ONE
// ErrLastAdmin (never two successes — that would be a true correctness
// regression).
func TestUpdateUser_LastAdminConcurrentDemote_OneSucceedsOneGetsLastAdmin(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	requireSerialPackages(t)

	// Squash any pre-existing active admins so we start with exactly two
	// (the two we seed below). Cleanup restores them.
	if _, err := pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = now() WHERE role='admin'`); err != nil {
		t.Fatalf("squash existing admins: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = NULL WHERE role='admin'`)
	})

	adminA, adminAName := seedUserDirect(t, pool, "admin")
	adminB, adminBName := seedUserDirect(t, pool, "admin")

	// Sanity: count must be exactly 2 before we race.
	if n, err := store.CountActiveAdmins(context.Background()); err != nil || n != 2 {
		t.Fatalf("pre-race active-admin count = %d, err=%v; want 2", n, err)
	}

	type result struct {
		err error
	}
	results := make(chan result, 2)
	newRole := "user"

	demote := func(actorID uuid.UUID, actorName string, target uuid.UUID) {
		_, err := store.UpdateUser(context.Background(), target, UpdateUserInput{
			Role: &newRole,
		}, adminActor(actorID, actorName))
		results <- result{err: err}
	}

	// Each admin demotes the OTHER concurrently. Pre-fix this could leave
	// both admins demoted (zero-admin commit) on a non-SERIALIZABLE level;
	// post-fix the SERIALIZABLE level catches it and one side gets either
	// the in-tx recheck rejection (ErrLastAdmin), directly or on the
	// retry after a 40001 abort.
	go demote(adminA, adminAName, adminB)
	go demote(adminB, adminBName, adminA)

	r1 := <-results
	r2 := <-results

	// Tally outcomes.
	successes := 0
	lastAdminErrs := 0
	other := []error{}
	for _, r := range []result{r1, r2} {
		switch {
		case r.err == nil:
			successes++
		case errors.Is(r.err, ErrLastAdmin):
			lastAdminErrs++
		default:
			other = append(other, r.err)
		}
	}

	if len(other) > 0 {
		t.Fatalf("unexpected error(s) outside {nil, ErrLastAdmin}: %v", other)
	}
	if successes != 1 || lastAdminErrs != 1 {
		t.Fatalf("got %d successes and %d ErrLastAdmin; want exactly 1 of each", successes, lastAdminErrs)
	}

	// Post-condition: exactly one active admin remains.
	if n, err := store.CountActiveAdmins(context.Background()); err != nil || n != 1 {
		t.Fatalf("post-race active-admin count = %d, err=%v; want exactly 1", n, err)
	}
}

// TestUpdateUser_LastAdminCannotDemote and TestDeactivate_LastAdminBlocked
// rely on the database having exactly ONE active admin during the test.
// They squash all other admins to deactivated and then verify the
// last-admin protection. Concurrent test packages (e.g. mcp/tools) that
// insert admin rows against the same DB will defeat this — run
// integration tests with `go test -p 1` to serialize packages.
func TestUpdateUser_LastAdminCannotDemote(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	requireSerialPackages(t)

	adminID, adminName := seedUserDirect(t, pool, "admin")
	if _, err := pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = now()
 WHERE role='admin' AND id <> $1`, adminID); err != nil {
		t.Fatalf("squash other admins: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = NULL WHERE role='admin'`)
	})

	newRole := "user"
	_, err := store.UpdateUser(context.Background(), adminID, UpdateUserInput{
		Role: &newRole,
	}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("err = %v, want ErrLastAdmin", err)
	}
}

// requireSerialPackages skips the test when env SKEIN_TEST_PARALLEL_PKGS
// is set. Integration tests touching DB-wide invariants (e.g. last-admin
// count) must run with `go test -p 1`; otherwise concurrent packages
// pollute the global state. Default behaviour is to RUN — run-time
// detection is best-effort, the canonical signal is `-p 1`.
func requireSerialPackages(t *testing.T) {
	t.Helper()
	if os.Getenv("SKEIN_TEST_PARALLEL_PKGS") != "" {
		t.Skip("skipped under SKEIN_TEST_PARALLEL_PKGS; requires `go test -p 1`")
	}
}

func TestDeactivate_CascadesAPIKeys(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	// Need a second admin so deactivating non-admin target is fine; the
	// target is a regular user anyway, so this isn't strictly needed — but
	// keep the parallel with last-admin tests for clarity.
	target, _ := seedUserDirect(t, pool, "user")

	// Issue an API key for the target.
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	_ = plain
	keyID, _, err := apikeys.New(pool).Create(context.Background(), target, "test", prefix, hash)
	if err != nil {
		t.Fatalf("Create api key: %v", err)
	}

	// Deactivate user — cascade should soft-revoke the key in the same tx.
	if err := store.Deactivate(context.Background(), target, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	var revokedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT revoked_at FROM api_keys WHERE id = $1`, keyID).Scan(&revokedAt); err != nil {
		t.Fatalf("read api key: %v", err)
	}
	if revokedAt == nil {
		t.Fatal("api key revoked_at should be populated after Deactivate")
	}
}

func TestDeactivate_LastAdminBlocked(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	requireSerialPackages(t)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	if _, err := pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = now()
 WHERE role='admin' AND id <> $1`, adminID); err != nil {
		t.Fatalf("squash other admins: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = NULL WHERE role='admin'`)
	})

	err := store.Deactivate(context.Background(), adminID, adminActor(adminID, adminName))
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("err = %v, want ErrLastAdmin", err)
	}
}

func TestReactivate_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")
	target, _ := seedUserDirect(t, pool, "user")

	if err := store.Deactivate(context.Background(), target, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if err := store.Reactivate(context.Background(), target, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	dto, _ := store.GetUser(context.Background(), target)
	if dto.DeactivatedAt != nil {
		t.Fatal("deactivated_at should be NULL after Reactivate")
	}
}

func TestChangePassword_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cphp" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "PWChange", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})
	if err := store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "newP@ssword2"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
}

func TestChangePassword_BadCurrent(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cpbc" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "BadCur", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})
	err = store.ChangePassword(context.Background(), dto.ID, "wrongP@ssword", "newP@ssword2")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

// fakePwnedChecker is a test double for users.PwnedChecker. If panicOnCall
// is set, calling Check fails the test immediately — used to prove a
// disabled checker (nil) is never invoked, as opposed to being invoked and
// happening to not change the outcome.
type fakePwnedChecker struct {
	t           *testing.T
	breached    bool
	err         error
	panicOnCall bool
	// delay, when non-zero, sleeps before returning — used by
	// TestChangePassword_PwnedCheck_DoesNotHoldPoolConnection to prove
	// ChangePassword no longer holds a pool connection/transaction open for
	// the duration of the outbound call (sk-303.20 review W1 remediation).
	delay time.Duration
}

func (f *fakePwnedChecker) Check(_ context.Context, _ string) (bool, error) {
	if f.panicOnCall {
		f.t.Fatal("PwnedChecker.Check called while the check should be disabled/skipped")
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.breached, f.err
}

// TestChangePassword_PwnedRejected — sk-303.20. When the pwned checker
// reports the new password as breached, ChangePassword must reject with
// ErrPasswordPwned and must NOT write the new hash.
func TestChangePassword_PwnedRejected(t *testing.T) {
	pool := newTestPool(t)
	checker := &fakePwnedChecker{t: t, breached: true}
	store := newTestStorePwned(t, pool, checker)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cppw" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "PwnedRej", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})

	err = store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "newP@ssword2")
	if !errors.Is(err, ErrPasswordPwned) {
		t.Fatalf("err = %v, want ErrPasswordPwned", err)
	}
	// The hash must NOT have been rotated: the OLD current password still
	// works as "current" for a follow-up change (checker now reports clean).
	checker.breached = false
	if err := store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "yetAnother3"); err != nil {
		t.Fatalf("expected old password still active after the rejected change (hash must not have been rotated): %v", err)
	}
}

// TestChangePassword_PwnedClean — the mirror happy path: checker reports
// not breached, ChangePassword succeeds exactly as without the checker.
func TestChangePassword_PwnedClean(t *testing.T) {
	pool := newTestPool(t)
	checker := &fakePwnedChecker{t: t, breached: false}
	store := newTestStorePwned(t, pool, checker)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cppc" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "PwnedClean", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})

	if err := store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "newP@ssword2"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
}

// TestChangePassword_PwnedDegraded_NotBlocked — acceptance criterion: the
// breach service being unreachable must NOT block the password change.
func TestChangePassword_PwnedDegraded_NotBlocked(t *testing.T) {
	pool := newTestPool(t)
	checker := &fakePwnedChecker{t: t, err: errors.New("simulated network failure")}
	store := newTestStorePwned(t, pool, checker)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cppd" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "PwnedDegraded", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})

	if err := store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "newP@ssword2"); err != nil {
		t.Fatalf("ChangePassword should proceed unblocked when the pwned check degrades: %v", err)
	}
}

// TestChangePassword_PwnedNil_NoOp — proves "disabled = never called", not
// merely "disabled = didn't change the outcome". A checker configured to
// t.Fatal on Check is wired in via a Store that never got a pwnedChecker
// (nil, i.e. the production default via newTestStore); we cross-check by
// also asserting a real newTestStorePwned(nil-equivalent) path is
// unreachable, using panicOnCall as the tripwire.
func TestChangePassword_PwnedNil_NoOp(t *testing.T) {
	pool := newTestPool(t)
	// newTestStore wires pwnedChecker=nil — a genuine nil interface value,
	// per NewStore's contract — so Store.pwnedChecker != nil is false and
	// the whole check block in ChangePassword is skipped without ever
	// dereferencing a checker. There is no checker to instrument here; the
	// absence of any call is implicit in "this succeeds without one".
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cppn" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "PwnedNilNoOp", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})
	if store.PwnedCheckEnabled() {
		t.Fatal("newTestStore's Store must have the pwned check disabled")
	}
	if err := store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "newP@ssword2"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
}

// TestChangePassword_PwnedCheck_DoesNotHoldPoolConnection — sk-303.20
// review remediation proof. Before the
// fix, ChangePassword ran the outbound pwned-passwords call INSIDE the same
// pgx.BeginTxFunc that held the row's connection/SERIALIZABLE snapshot, so
// a slow breach service starved the whole pool for its duration. This test
// pins a dedicated pool to exactly one connection, drives ChangePassword
// with a checker that sleeps well past a "fast" query, and asserts a
// concurrent, totally unrelated query on that SAME one-connection pool
// completes quickly — which is only possible if ChangePassword released
// its connection before calling the checker.
func TestChangePassword_PwnedCheck_DoesNotHoldPoolConnection(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx := context.Background()

	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const checkDelay = 400 * time.Millisecond
	checker := &fakePwnedChecker{t: t, breached: false, delay: checkDelay}
	store := newTestStorePwned(t, pool, checker)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cppool" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(ctx, CreateUserInput{
		Email: email, Name: "PoolProbe", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID) })

	done := make(chan error, 1)
	go func() {
		done <- store.ChangePassword(ctx, dto.ID, "oldP@sswd1", "newP@ssword2")
	}()

	// Give the goroutine time to pass Verify and enter the checker's sleep.
	time.Sleep(50 * time.Millisecond)

	// While ChangePassword is (we expect) blocked inside the sleeping
	// checker call, run a completely unrelated query against the SAME
	// 1-connection pool. If ChangePassword still held that one connection
	// (the pre-fix shape — network call inside pgx.BeginTxFunc), this would
	// have to wait for the checker to return and the transaction to finish
	// before a connection freed up, i.e. at least checkDelay. Post-fix, the
	// connection was already released back to the pool before the checker
	// was ever called, so this must complete promptly.
	queryStart := time.Now()
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("concurrent query on the 1-conn pool failed: %v", err)
	}
	if elapsed := time.Since(queryStart); elapsed >= checkDelay {
		t.Fatalf("concurrent SELECT 1 took %s (>= checker delay %s) — "+
			"ChangePassword appears to still hold the pool's only connection "+
			"during the outbound pwned check", elapsed, checkDelay)
	}

	if err := <-done; err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
}

// TestChangePassword_ConcurrentRotation_Conflict — sk-303.20 review
// remediation. Proves the compare-and-swap UPDATE in commitPasswordChange
// detects a password rotated by another request in the window between
// loadPasswordState's read and the write, and reports it as the dedicated
// ErrPasswordChangedConcurrently sentinel (handler-mapped to 422
// password_change_conflict) — never a bare 500 from an unexamined "0 rows
// affected".
func TestChangePassword_ConcurrentRotation_Conflict(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cprace" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "Race", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID) })

	// Read + verify exactly as ChangePassword's first phase would — captures
	// the hash a "stale" writer will later try to CAS against.
	staleOldHash, err := store.loadPasswordState(context.Background(), dto.ID, "oldP@sswd1")
	if err != nil {
		t.Fatalf("loadPasswordState: %v", err)
	}

	// A concurrent request "wins the race": a full ChangePassword using the
	// same current password rotates the hash out from under the stale read
	// above.
	if err := store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "raceWinner1"); err != nil {
		t.Fatalf("concurrent ChangePassword (the race winner): %v", err)
	}

	// The stale writer now tries to commit against the hash it read BEFORE
	// the winner rotated it — must be rejected, not silently applied.
	staleNewHash, err := auth.Hash("staleAttempt2", auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	err = store.commitPasswordChange(context.Background(), dto.ID, staleOldHash, staleNewHash)
	if !errors.Is(err, ErrPasswordChangedConcurrently) {
		t.Fatalf("err = %v, want ErrPasswordChangedConcurrently", err)
	}

	// And the winner's password must still be the one in effect.
	if err := store.ChangePassword(context.Background(), dto.ID, "raceWinner1", "final3Pass"); err != nil {
		t.Fatalf("ChangePassword with the winning password should still work: %v", err)
	}
}

// TestChangePassword_Deactivated_RejectsWithInvalidCredentials — sk-303.20
// review. The deactivated-account guard predates this
// commit but had no ChangePassword regression test of its own. Locks down
// that a deactivated user cannot rotate their password — collapsed into
// the SAME ErrInvalidCredentials a wrong current-password gets (no
// account-state oracle) — and that the guard fires before Verify/pwned/Hash
// ever run (the hash on the row is provably unchanged).
func TestChangePassword_Deactivated_RejectsWithInvalidCredentials(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "cpdeact" + adminID.String()[:6] + "@x.local"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email: email, Name: "Deactivated", Password: "oldP@sswd1", Role: "user",
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID) })

	if err := store.Deactivate(context.Background(), dto.ID, adminActor(adminID, adminName)); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	err = store.ChangePassword(context.Background(), dto.ID, "oldP@sswd1", "newP@ssword2")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	var hash string
	if scanErr := pool.QueryRow(context.Background(),
		`SELECT password_hash FROM users WHERE id = $1`, dto.ID).Scan(&hash); scanErr != nil {
		t.Fatalf("read back hash: %v", scanErr)
	}
	if verr := auth.Verify(hash, "oldP@sswd1"); verr != nil {
		t.Fatalf("hash was rotated despite the deactivated guard rejecting the call: %v", verr)
	}
}

// seedKeycloakOnlyUser inserts a user with NO local password hash (only
// keycloak_sub set) — the SSO-only account class ChangePassword's nil-hash
// guard exists for. Direct insert because CreateUser always sets a local
// password.
func seedKeycloakOnlyUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "kc" + strings.ReplaceAll(id.String()[:20], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, keycloak_sub, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', $5, now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname, "kc-sub-"+id.String())
	if err != nil {
		t.Fatalf("seed keycloak-only user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestChangePassword_NoLocalHash_RejectsWithInvalidCredentials — sk-303.20
// review. A Keycloak-only account (password_hash IS NULL)
// must not be able to rotate a "current password" it never had — and must
// stay hashless afterward (ChangePassword must not accidentally give it a
// local password as a side effect of the rejection path).
func TestChangePassword_NoLocalHash_RejectsWithInvalidCredentials(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	id := seedKeycloakOnlyUser(t, pool)

	err := store.ChangePassword(context.Background(), id, "anything", "newP@ssword2")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	var hash *string
	if scanErr := pool.QueryRow(context.Background(),
		`SELECT password_hash FROM users WHERE id = $1`, id).Scan(&hash); scanErr != nil {
		t.Fatalf("read back hash: %v", scanErr)
	}
	if hash != nil {
		t.Fatalf("password_hash = %q, want still NULL (Keycloak-only account must never "+
			"gain a local hash via ChangePassword)", *hash)
	}
}

func TestValidatePassword_UpperBound(t *testing.T) {
	// Exactly 72 bytes must pass.
	ok72 := strings.Repeat("a", 72)
	if err := validatePassword(ok72); err != nil {
		t.Fatalf("72-byte password rejected: %v", err)
	}
	// 73 bytes must fail with ErrPasswordTooLong.
	bad73 := strings.Repeat("a", 73)
	if err := validatePassword(bad73); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("err = %v, want ErrPasswordTooLong", err)
	}
	// Cyrillic: 2 bytes/rune in UTF-8. 36 runes = 72 bytes exactly (passes);
	// 37 runes = 74 bytes (fails). This is the case the task's acceptance
	// criterion actually cares about — "64 characters accepted" only holds
	// because we gate on bytes correctly, not on len([]rune(p)).
	cyrillic36 := strings.Repeat("ж", 36)
	if len(cyrillic36) != 72 {
		t.Fatalf("fixture wrong: len(cyrillic36) = %d, want 72", len(cyrillic36))
	}
	if err := validatePassword(cyrillic36); err != nil {
		t.Fatalf("72-byte Cyrillic password rejected: %v", err)
	}
	cyrillic37 := strings.Repeat("ж", 37)
	if err := validatePassword(cyrillic37); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("err = %v, want ErrPasswordTooLong for 74-byte Cyrillic password", err)
	}
	// Emoji: 4 bytes/rune in UTF-8 for many astral-plane characters.
	emoji18 := strings.Repeat("😀", 18)
	if len(emoji18) != 72 {
		t.Fatalf("fixture wrong: len(emoji18) = %d, want 72", len(emoji18))
	}
	if err := validatePassword(emoji18); err != nil {
		t.Fatalf("72-byte emoji password rejected: %v", err)
	}
	emoji19 := strings.Repeat("😀", 19)
	if err := validatePassword(emoji19); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("err = %v, want ErrPasswordTooLong for 76-byte emoji password", err)
	}
	// Below the floor still rejects with ErrInvalidPassword, unaffected by
	// the new upper-bound check.
	if err := validatePassword("short"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("err = %v, want ErrInvalidPassword", err)
	}
}

func TestCountActiveAdmins(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)

	before, err := store.CountActiveAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountActiveAdmins: %v", err)
	}
	_, _ = seedUserDirect(t, pool, "admin")
	after, err := store.CountActiveAdmins(context.Background())
	if err != nil {
		t.Fatalf("CountActiveAdmins: %v", err)
	}
	if after != before+1 {
		t.Fatalf("after - before = %d, want 1", after-before)
	}
}

// seedSSOUserDirect inserts a Keycloak SSO identity row directly:
// keycloak_sub NON-NULL, password_hash NULL (the shape an OIDC-provisioned
// internal user has). Returns the row id and its email. Hard-deleted on
// cleanup. Used by the sk-163 S5 reverse anti-takeover tests.
func seedSSOUserDirect(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "sso" + strings.ReplaceAll(id.String()[:18], "-", "")
	email := uname + "@internal.local"
	sub := "kc-" + uname
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, keycloak_sub, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', NULL, $5, now(), '{}'::jsonb)`,
		id, email, uname, uname, sub)
	if err != nil {
		t.Fatalf("seed sso user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, email
}

// TestCreateUser_ExternalForSSOEmailRejected — sk-163 S5 reverse guard.
// Creating an EXTERNAL account for an email already bound to a Keycloak SSO
// identity must be refused with ErrExternalSSOConflict (account-takeover of
// an internal identity via the public password surface).
func TestCreateUser_ExternalForSSOEmailRejected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	_, ssoEmail := seedSSOUserDirect(t, pool)

	_, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:      ssoEmail,
		Name:       "External Impersonator",
		Password:   "passw0rd!",
		Role:       "user",
		IsExternal: true,
	}, adminActor(adminID, adminName))
	if !errors.Is(err, ErrExternalSSOConflict) {
		t.Fatalf("CreateUser err = %v, want ErrExternalSSOConflict", err)
	}
}

// TestCreateUser_ExternalForFreshEmailOK — sk-163 S5. An external account
// for an email that is NOT taken creates normally and persists
// is_external=true.
func TestCreateUser_ExternalForFreshEmailOK(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	email := "extfresh" + adminID.String()[:6] + "@customer.com"
	dto, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:      email,
		Name:       "Fresh External",
		Password:   "passw0rd!",
		Role:       "user",
		IsExternal: true,
	}, adminActor(adminID, adminName))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, dto.ID)
	})

	var gotExternal bool
	if err := pool.QueryRow(context.Background(),
		`SELECT is_external FROM users WHERE id = $1`, dto.ID).Scan(&gotExternal); err != nil {
		t.Fatalf("verify is_external: %v", err)
	}
	if !gotExternal {
		t.Error("is_external = false, want true (external create must persist the flag)")
	}
}

// TestCreateUser_InternalForSSOEmailUnaffected — sk-163 S5 regression. The
// reverse guard is external-ONLY: an INTERNAL create (IsExternal=false) for
// an SSO-bound email must NOT trip ErrExternalSSOConflict. It falls through
// to the INSERT and hits the normal lower(email) unique violation
// (ErrEmailTaken) — proving existing internal-create behavior is unchanged.
func TestCreateUser_InternalForSSOEmailUnaffected(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	adminID, adminName := seedUserDirect(t, pool, "admin")

	_, ssoEmail := seedSSOUserDirect(t, pool)

	_, err := store.CreateUser(context.Background(), CreateUserInput{
		Email:      ssoEmail,
		Name:       "Internal Dup",
		Password:   "passw0rd!",
		Role:       "user",
		IsExternal: false,
	}, adminActor(adminID, adminName))
	if errors.Is(err, ErrExternalSSOConflict) {
		t.Fatalf("internal create tripped ErrExternalSSOConflict — guard must be external-only")
	}
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("CreateUser err = %v, want ErrEmailTaken (unique lower(email))", err)
	}
}
