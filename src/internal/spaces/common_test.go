package spaces_test

// common_test.go — sk-674.18 T020, matrix row 55 (US-2 "common space outside
// the transition", INV-22). Reference — the spec paragraph "Creating a common
// space outside the transition", written before the code:
//
//   - the flag on and no common space: it is created at startup, its owner
//     the earliest active admin, its participants every non-external user;
//   - no admin yet but an SSO bootstrap email: it is created the moment the
//     first admin appears through SSO;
//   - re-enabled after the common space was deleted: an empty common space
//     again, with every non-external user.
//
// The mechanism is accounts.EnsureCommonSpace (a leaf package: the creation
// paths live in auth, which the space machinery depends on). Each case runs
// on a database of its own — an installation has one common space.

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

func freshCommonDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := testsupport.NewTestPool(t)
	name := "cmntest_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("DROP DATABASE %s: %v", name, err)
		}
	})
	u, err := url.Parse(os.Getenv(testsupport.TestDatabaseURL))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ext := range []string{"pg_trgm", "vector"} {
		if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS `+ext); err != nil {
			t.Fatalf("CREATE EXTENSION %s: %v", ext, err)
		}
	}
	m, err := migrate.New("file://../../migrations", "pgx5://"+strings.TrimPrefix(u.String(), u.Scheme+"://"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return pool
}

func cmnUser(t *testing.T, pool *pgxpool.Pool, role string, external, deactivated bool, ageDays int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var deact any
	if deactivated {
		deact = time.Now().UTC()
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, is_external, deactivated_at, created_at, metadata)
VALUES ($1, $2, $3, 'u', $4, 'x', $5, $6, now() - make_interval(days => $7), '{}'::jsonb)`,
		id, id.String()[:8]+"@cmn.test", "u"+id.String()[:8], role, external, deact, ageDays); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// commonOf reads the common space: owner and participants.
func commonOf(t *testing.T, pool *pgxpool.Pool) (exists bool, owner uuid.UUID, members map[uuid.UUID]string) {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id, owner_id FROM spaces WHERE kind = 'common'`).Scan(&id, &owner)
	if err != nil {
		return false, uuid.Nil, nil
	}
	members = map[uuid.UUID]string{}
	rows, err := pool.Query(ctx, `SELECT user_id, role FROM space_members WHERE space_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var u uuid.UUID
		var r string
		if err := rows.Scan(&u, &r); err != nil {
			t.Fatal(err)
		}
		members[u] = r
	}
	return true, owner, members
}

func TestCommonSpace_OutsideTheTransition(t *testing.T) {
	ctx := context.Background()

	t.Run("created at startup", func(t *testing.T) {
		pool := freshCommonDB(t)
		deactAdmin := cmnUser(t, pool, "admin", false, true, 30)
		earliest := cmnUser(t, pool, "admin", false, false, 20)
		later := cmnUser(t, pool, "admin", false, false, 10)
		user := cmnUser(t, pool, "user", false, false, 5)
		ext := cmnUser(t, pool, "user", true, false, 5)
		if err := accounts.EnsureCommonSpace(ctx, pool); err != nil {
			t.Fatalf("EnsureCommonSpace: %v", err)
		}
		exists, owner, members := commonOf(t, pool)
		if !exists {
			t.Fatal("no common space was created")
		}
		if owner != earliest {
			t.Errorf("owner = %s, want the earliest ACTIVE admin %s", owner, earliest)
		}
		for u, want := range map[uuid.UUID]string{earliest: "owner", later: "member", user: "member", deactAdmin: "member"} {
			if members[u] != want {
				t.Errorf("participant %s = %q, want %q", u, members[u], want)
			}
		}
		if _, in := members[ext]; in {
			t.Error("the external user is in the common space")
		}
	})

	t.Run("flag off — not created", func(t *testing.T) {
		pool := freshCommonDB(t)
		cmnUser(t, pool, "admin", false, false, 1)
		if _, err := pool.Exec(ctx, `UPDATE instance_settings SET common_space_enabled = false`); err != nil {
			t.Fatal(err)
		}
		if err := accounts.EnsureCommonSpace(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if exists, _, _ := commonOf(t, pool); exists {
			t.Error("a common space was created with the flag off")
		}
	})

	t.Run("created when the first admin appears through SSO", func(t *testing.T) {
		pool := freshCommonDB(t)
		early := cmnUser(t, pool, "user", false, false, 3)
		if err := accounts.EnsureCommonSpace(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if exists, _, _ := commonOf(t, pool); exists {
			t.Fatal("a common space without an admin to own it")
		}
		email := "boss@cmn.test"
		res, err := auth.NewProvisioner(pool, true, "user", email).Provision(ctx, auth.ProvisionInput{Email: email, Username: "boss"})
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		exists, owner, members := commonOf(t, pool)
		if !exists || owner != res.UserID {
			t.Fatalf("common space after the first admin: exists %v, owner %s, want the admin %s", exists, owner, res.UserID)
		}
		if members[early] != "member" {
			t.Errorf("the user who came before the admin is %q in the common space, want member", members[early])
		}
	})

	t.Run("re-enabled after deletion — empty, with everybody", func(t *testing.T) {
		pool := freshCommonDB(t)
		admin := cmnUser(t, pool, "admin", false, false, 2)
		user := cmnUser(t, pool, "user", false, false, 1)
		if err := accounts.EnsureCommonSpace(ctx, pool); err != nil {
			t.Fatal(err)
		}
		var commonID uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT id FROM spaces WHERE kind = 'common'`).Scan(&commonID); err != nil {
			t.Fatal(err)
		}
		if err := spaces.NewStore(pool).Delete(ctx, commonID, spaces.Actor{UserID: admin, Username: "admin", Type: "human"}); err != nil {
			t.Fatalf("delete the common space: %v", err)
		}
		if err := accounts.EnsureCommonSpace(ctx, pool); err != nil {
			t.Fatal(err)
		}
		exists, owner, members := commonOf(t, pool)
		if !exists || owner != admin || members[user] != "member" {
			t.Errorf("re-created common: exists %v, owner %s, members %v", exists, owner, members)
		}
		var projects int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects p JOIN spaces s ON s.id = p.space_id WHERE s.kind = 'common'`).Scan(&projects); err != nil {
			t.Fatal(err)
		}
		if projects != 0 {
			t.Errorf("the re-created common space has %d projects, want none", projects)
		}
	})
}
