package apikeys_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

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
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedUserDirect inserts a user row (active or deactivated) so we can
// attach an api key to them without going through users.Store. Cleanup
// via t.Cleanup.
func seedUserDirect(t *testing.T, pool *pgxpool.Pool, deactivated bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ak" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata, deactivated_at)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb,
        CASE WHEN $5 THEN now() ELSE NULL END)`,
		id, uname+"@test.local", uname, uname, deactivated); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestLookupByPrefix_DeactivatedUser_NotFound verifies the R3.1 W2 fix:
// even if an API key still has revoked_at IS NULL (e.g. a missed cascade
// in some hypothetical future flow), a key belonging to a deactivated
// user must NOT authenticate. The defence-in-depth filter
// `AND u.deactivated_at IS NULL` on the LookupByPrefix join guarantees
// this regardless of revocation state.
func TestLookupByPrefix_DeactivatedUser_NotFound(t *testing.T) {
	pool := newTestPool(t)
	store := apikeys.New(pool)

	// 1. Seed an ACTIVE user, issue a key, confirm lookup works.
	userID := seedUserDirect(t, pool, false)
	_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, _, err := testsupport.CreateOwnerKey(context.Background(), pool, userID, "test", prefix, hash); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Sanity: lookup succeeds while user is active and key is unrevoked.
	if _, err := store.LookupByPrefix(context.Background(), prefix); err != nil {
		t.Fatalf("LookupByPrefix (active user): %v", err)
	}

	// 2. Manually deactivate WITHOUT going through users.Store.Deactivate
	// (which cascades RevokeAllForUser). This simulates the failure mode
	// W2 defends against — an unrevoked key on a deactivated user.
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("manual deactivate: %v", err)
	}

	// 3. Lookup must now fail with ErrNotFound — the JOIN clause filters
	// out the row even though revoked_at is still NULL on the api_keys row.
	_, err = store.LookupByPrefix(context.Background(), prefix)
	if !errors.Is(err, apikeys.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (deactivated user must not authenticate)", err)
	}

	// Confirm the api_keys row itself is still revoked_at = NULL — the
	// filter must work without the key being explicitly revoked.
	var revokedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT revoked_at FROM api_keys WHERE prefix = $1`, prefix).Scan(&revokedAt); err != nil {
		t.Fatalf("read api_keys row: %v", err)
	}
	if revokedAt != nil {
		t.Fatalf("api_keys.revoked_at = %v, want NULL — test wiring would not exercise the deactivated_at filter otherwise", revokedAt)
	}
}

// --- Grants: test matrix row 10 (US-10, INV-9, SC-023) ---------------------
//
// Reference — the spec, written before the code: a key is created with its
// permissions, scope and a lifetime by the policy; it narrows only — a
// widening is refused and changes nothing; a reissue gives a new row and a
// new secret with the same label, permissions and scope and a lifetime by
// the current policy (also for a migrated key without expiry), and the old
// key keeps working; a revoked key is not reissued, an expired one may be;
// an expired key comes back to life neither by Narrow nor by Reissue;
// lowering the maximum lifetime does not shorten issued keys; Status gives
// the grant, the expiry, the owner's activity and the effective admin flag.

const day = 24 * time.Hour

var testPolicy = apikeys.TTLPolicy{Default: 90 * day, Max: 365 * day}

type keyMaterial struct{ prefix, hash string }

func newKeyMaterial(t *testing.T) keyMaterial {
	t.Helper()
	_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	return keyMaterial{prefix: prefix, hash: hash}
}

func createKey(t *testing.T, store *apikeys.Store, user uuid.UUID, label string, g apikeys.Grant, policy apikeys.TTLPolicy) apikeys.Created {
	t.Helper()
	m := newKeyMaterial(t)
	c, err := store.Create(context.Background(), apikeys.NewKey{UserID: user, Label: label, Prefix: m.prefix, Hash: m.hash, Grant: g}, policy)
	if err != nil {
		t.Fatalf("Create %s: %v", label, err)
	}
	return c
}

func mustStatus(t *testing.T, store *apikeys.Store, id uuid.UUID) apikeys.KeyStatus {
	t.Helper()
	st, err := store.Status(context.Background(), id)
	if err != nil {
		t.Fatalf("Status %s: %v", id, err)
	}
	return st
}

// near reports whether got lies within a minute of want: the store stamps
// times with its own clock.
func near(got *time.Time, want time.Time) bool {
	return got != nil && got.Sub(want).Abs() < time.Minute
}

func countKeys(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	return n
}

func TestStoreGrants(t *testing.T) {
	pool := newTestPool(t)
	store := apikeys.New(pool)
	ctx := context.Background()
	owner := seedUserDirect(t, pool, false)
	space := testsupport.SeedSpace(t, pool, owner)
	inSpace, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "akg")
	other, _ := testsupport.SeedProject(t, pool, owner, "akg")

	writeAll := apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}

	t.Run("Create stores the grant, the scope and the policy lifetime", func(t *testing.T) {
		g := apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{inSpace, other}}}
		c := createKey(t, store, owner, "create default", g, testPolicy)
		if !c.ExpiresAt.Equal(c.CreatedAt.Add(testPolicy.Default)) {
			t.Errorf("expires %v, want created %v + 90 days", c.ExpiresAt, c.CreatedAt)
		}
		st := mustStatus(t, store, c.ID)
		if st.Grant.Level != apikeys.LevelRead || st.Grant.Admin || st.Grant.Scope.Mode != apikeys.ScopeProjects {
			t.Errorf("grant %+v, want read, no admin, projects", st.Grant)
		}
		if len(st.Grant.Scope.Projects) != 2 || !slices.Contains(st.Grant.Scope.Projects, inSpace) || !slices.Contains(st.Grant.Scope.Projects, other) {
			t.Errorf("scope projects %v, want both seeded projects", st.Grant.Scope.Projects)
		}
		if !near(st.Grant.ExpiresAt, c.CreatedAt.Add(testPolicy.Default)) {
			t.Errorf("status expiry %v, want created + 90 days", st.Grant.ExpiresAt)
		}

		chosen := time.Now().UTC().Add(200 * day)
		g2 := writeAll
		g2.ExpiresAt = &chosen
		c2 := createKey(t, store, owner, "create chosen", g2, testPolicy)
		if !c2.ExpiresAt.Equal(chosen) {
			t.Errorf("chosen expiry stored as %v, want %v", c2.ExpiresAt, chosen)
		}
	})

	t.Run("Create refuses a lifetime beyond the maximum and a bad grant, writing nothing", func(t *testing.T) {
		before := countKeys(t, pool, owner)
		tooLong := time.Now().UTC().Add(366 * day)
		g := writeAll
		g.ExpiresAt = &tooLong
		m := newKeyMaterial(t)
		if _, err := store.Create(ctx, apikeys.NewKey{UserID: owner, Label: "too long", Prefix: m.prefix, Hash: m.hash, Grant: g}, testPolicy); !errors.Is(err, apikeys.ErrInvalidExpiry) {
			t.Errorf("lifetime of 366 days: err %v, want ErrInvalidExpiry", err)
		}
		m = newKeyMaterial(t)
		bad := apikeys.Grant{Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{inSpace}}}
		if _, err := store.Create(ctx, apikeys.NewKey{UserID: owner, Label: "admin projects", Prefix: m.prefix, Hash: m.hash, Grant: bad}, testPolicy); !errors.Is(err, apikeys.ErrAdminNeedsAll) {
			t.Errorf("admin with scope projects: err %v, want ErrAdminNeedsAll", err)
		}
		m = newKeyMaterial(t)
		missing := apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{uuid.New()}}}
		if _, err := store.Create(ctx, apikeys.NewKey{UserID: owner, Label: "missing project", Prefix: m.prefix, Hash: m.hash, Grant: missing}, testPolicy); !errors.Is(err, apikeys.ErrScopeUnavailable) {
			t.Errorf("unknown project in scope: err %v, want ErrScopeUnavailable", err)
		}
		if after := countKeys(t, pool, owner); after != before {
			t.Errorf("keys %d → %d, want no new row", before, after)
		}
	})

	t.Run("Narrow narrows; a widening is refused and changes nothing", func(t *testing.T) {
		c := createKey(t, store, owner, "narrow", apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{space}}}, testPolicy)
		before := mustStatus(t, store, c.ID)

		wider := apikeys.Grant{Level: apikeys.LevelManage, Scope: before.Grant.Scope, ExpiresAt: before.Grant.ExpiresAt}
		if err := store.Narrow(ctx, owner, c.ID, wider); !errors.Is(err, apikeys.ErrNotNarrowing) {
			t.Errorf("level write → manage: err %v, want ErrNotNarrowing", err)
		}
		outside := apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{other}}, ExpiresAt: before.Grant.ExpiresAt}
		if err := store.Narrow(ctx, owner, c.ID, outside); !errors.Is(err, apikeys.ErrNotNarrowing) {
			t.Errorf("space → a project of another space: err %v, want ErrNotNarrowing", err)
		}
		if st := mustStatus(t, store, c.ID); !reflect.DeepEqual(st.Grant, before.Grant) {
			t.Errorf("after refused changes grant %+v, want unchanged %+v", st.Grant, before.Grant)
		}

		sooner := before.Grant.ExpiresAt.Add(-10 * day)
		narrower := apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{inSpace}}, ExpiresAt: &sooner}
		if err := store.Narrow(ctx, owner, c.ID, narrower); err != nil {
			t.Fatalf("narrow to read, a project of the space, sooner expiry: %v", err)
		}
		st := mustStatus(t, store, c.ID)
		if st.Grant.Level != apikeys.LevelRead || st.Grant.Scope.Mode != apikeys.ScopeProjects ||
			!slices.Equal(st.Grant.Scope.Projects, []uuid.UUID{inSpace}) || len(st.Grant.Scope.Spaces) != 0 || !near(st.Grant.ExpiresAt, sooner) {
			t.Errorf("after narrowing grant %+v, want read, projects {%s}, expiry %v", st.Grant, inSpace, sooner)
		}

		stranger := seedUserDirect(t, pool, false)
		if err := store.Narrow(ctx, stranger, c.ID, narrower); !errors.Is(err, apikeys.ErrNotFound) {
			t.Errorf("narrowing someone else's key: err %v, want ErrNotFound", err)
		}
	})

	t.Run("Reissue gives a new key with the same grant; the old one keeps working", func(t *testing.T) {
		scope := apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{inSpace}}
		old := createKey(t, store, owner, "reissue me", apikeys.Grant{Level: apikeys.LevelWrite, Scope: scope}, testPolicy)
		// The current policy differs from the one the key was created under.
		current := apikeys.TTLPolicy{Default: 30 * day, Max: 60 * day}
		m := newKeyMaterial(t)
		fresh, err := store.Reissue(ctx, owner, old.ID, m.prefix, m.hash, current)
		if err != nil {
			t.Fatalf("Reissue: %v", err)
		}
		if fresh.ID == old.ID {
			t.Fatal("reissue reused the key row")
		}
		oldSt, newSt := mustStatus(t, store, old.ID), mustStatus(t, store, fresh.ID)
		if !oldSt.Usable(time.Now()) {
			t.Errorf("the old key is no longer usable after reissue: %+v", oldSt)
		}
		if !newSt.Usable(time.Now()) {
			t.Errorf("the new key is not usable: %+v", newSt)
		}
		if newSt.Grant.Level != apikeys.LevelWrite || newSt.Grant.Admin || !reflect.DeepEqual(newSt.Grant.Scope, oldSt.Grant.Scope) {
			t.Errorf("new grant %+v, want the old one %+v", newSt.Grant, oldSt.Grant)
		}
		if !near(newSt.Grant.ExpiresAt, fresh.CreatedAt.Add(30*day)) {
			t.Errorf("new expiry %v, want created + 30 days (current policy)", newSt.Grant.ExpiresAt)
		}
		var label, prefix string
		var from *uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT label, prefix, reissued_from_id FROM api_keys WHERE id = $1`, fresh.ID).Scan(&label, &prefix, &from); err != nil {
			t.Fatalf("read the new row: %v", err)
		}
		if label != "reissue me" || prefix != m.prefix || from == nil || *from != old.ID {
			t.Errorf("new row label %q prefix %q from %v, want the old label, the new prefix, from %s", label, prefix, from, old.ID)
		}
	})

	t.Run("a migrated key without expiry is reissued with the current policy lifetime", func(t *testing.T) {
		old := createKey(t, store, owner, "migrated", writeAll, testPolicy)
		if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = NULL WHERE id = $1`, old.ID); err != nil {
			t.Fatalf("make the key look migrated: %v", err)
		}
		m := newKeyMaterial(t)
		fresh, err := store.Reissue(ctx, owner, old.ID, m.prefix, m.hash, testPolicy)
		if err != nil {
			t.Fatalf("Reissue: %v", err)
		}
		if st := mustStatus(t, store, fresh.ID); !near(st.Grant.ExpiresAt, fresh.CreatedAt.Add(testPolicy.Default)) {
			t.Errorf("new expiry %v, want created + 90 days", st.Grant.ExpiresAt)
		}
		if st := mustStatus(t, store, old.ID); st.Grant.ExpiresAt != nil || !st.Usable(time.Now()) {
			t.Errorf("old migrated key %+v, want still without expiry and usable", st)
		}
	})

	t.Run("a revoked key is neither reissued nor narrowed", func(t *testing.T) {
		c := createKey(t, store, owner, "revoked", writeAll, testPolicy)
		if err := store.Revoke(ctx, owner, c.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		before := countKeys(t, pool, owner)
		m := newKeyMaterial(t)
		if _, err := store.Reissue(ctx, owner, c.ID, m.prefix, m.hash, testPolicy); !errors.Is(err, apikeys.ErrRevoked) {
			t.Errorf("Reissue of a revoked key: err %v, want ErrRevoked", err)
		}
		if err := store.Narrow(ctx, owner, c.ID, apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}); !errors.Is(err, apikeys.ErrRevoked) {
			t.Errorf("Narrow of a revoked key: err %v, want ErrRevoked", err)
		}
		if after := countKeys(t, pool, owner); after != before {
			t.Errorf("keys %d → %d, want no new row", before, after)
		}
		if st := mustStatus(t, store, c.ID); !st.Revoked() || st.Usable(time.Now()) {
			t.Errorf("status %+v, want revoked and not usable", st)
		}
	})

	t.Run("an expired key may be reissued and does not come back to life", func(t *testing.T) {
		c := createKey(t, store, owner, "expired", writeAll, testPolicy)
		if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 hour' WHERE id = $1`, c.ID); err != nil {
			t.Fatalf("expire the key: %v", err)
		}
		later := time.Now().UTC().Add(10 * day)
		if err := store.Narrow(ctx, owner, c.ID, apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}, ExpiresAt: &later}); !errors.Is(err, apikeys.ErrExpired) {
			t.Errorf("Narrow of an expired key: err %v, want ErrExpired", err)
		}
		m := newKeyMaterial(t)
		fresh, err := store.Reissue(ctx, owner, c.ID, m.prefix, m.hash, testPolicy)
		if err != nil {
			t.Fatalf("Reissue of an expired key: %v", err)
		}
		now := time.Now()
		if st := mustStatus(t, store, c.ID); !st.Expired(now) || st.Usable(now) {
			t.Errorf("old key %+v, want still expired", st)
		}
		if st := mustStatus(t, store, fresh.ID); !st.Usable(now) {
			t.Errorf("reissued key %+v, want usable", st)
		}
	})

	t.Run("a lower maximum lifetime does not shorten issued keys", func(t *testing.T) {
		long := time.Now().UTC().Add(300 * day)
		g := writeAll
		g.ExpiresAt = &long
		c := createKey(t, store, owner, "long lived", g, testPolicy)
		lowered := apikeys.TTLPolicy{Default: 10 * day, Max: 30 * day}
		_ = createKey(t, store, owner, "under the lowered policy", writeAll, lowered)
		st := mustStatus(t, store, c.ID)
		if !near(st.Grant.ExpiresAt, long) {
			t.Errorf("expiry %v after the maximum was lowered, want %v", st.Grant.ExpiresAt, long)
		}
		// Narrowing that keeps the expiry is still allowed.
		if err := store.Narrow(ctx, owner, c.ID, apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}, ExpiresAt: st.Grant.ExpiresAt}); err != nil {
			t.Errorf("Narrow keeping the expiry: %v", err)
		}
	})

	t.Run("Status reports the owner's activity and the effective admin flag", func(t *testing.T) {
		admin, _ := testsupport.SeedUser(t, pool, "admin")
		c := createKey(t, store, admin, "admin key", apikeys.Grant{Level: apikeys.LevelRead, Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}}, testPolicy)
		st := mustStatus(t, store, c.ID)
		if !st.Grant.Admin || !st.EffectiveAdmin || !st.OwnerActive || st.UserID != admin {
			t.Errorf("status %+v, want admin flag, effective admin, owner active", st)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET role = 'user' WHERE id = $1`, admin); err != nil {
			t.Fatalf("demote: %v", err)
		}
		st = mustStatus(t, store, c.ID)
		if !st.Grant.Admin || st.EffectiveAdmin {
			t.Errorf("after demotion %+v, want the admin flag kept on the key and not effective", st)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, admin); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		st = mustStatus(t, store, c.ID)
		if st.OwnerActive || st.Usable(time.Now()) {
			t.Errorf("after deactivation %+v, want owner inactive and key not usable", st)
		}
		if _, err := store.Status(ctx, uuid.New()); !errors.Is(err, apikeys.ErrNotFound) {
			t.Errorf("Status of an unknown id: err %v, want ErrNotFound", err)
		}
	})
}
