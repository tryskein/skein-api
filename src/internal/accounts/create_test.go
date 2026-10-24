package accounts_test

// create_test.go — sk-674.18 T020, matrix row 54 (INV-8, INV-22, US-12,
// E-16). Reference — the spec, written before the primitive: every creation
// path (the admin over REST and an admin's agent over MCP — both
// users.Store.CreateUser; SSO provisioning, the bootstrap email included; the
// initial admin) × the common space flag gives exactly one "Personal", and the
// common space only with the flag on; an external user gets no space.
//
// Each case runs on a database of its own: the initial admin is created
// only into an empty user base, and an installation has one common space.

import (
	"context"
	"errors"
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
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

const migrationsDir = "../../migrations"

// freshDatabase is an empty database next to TEST_DATABASE_URL's, migrated
// to head and dropped when the test ends.
func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := testsupport.NewTestPool(t)
	name := "acctest_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
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
	m, err := migrate.New("file://"+migrationsDir, "pgx5://"+strings.TrimPrefix(u.String(), u.Scheme+"://"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return pool
}

func setCommonFlag(t *testing.T, pool *pgxpool.Pool, on bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE instance_settings SET common_space_enabled = $1 WHERE id = 1`, on); err != nil {
		t.Fatal(err)
	}
}

// seedCommon creates an active admin and the common space he owns.
func seedCommon(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES (gen_random_uuid(), 'owner@acc.test', 'acc-owner', 'owner', 'admin', 'x', now() - interval '1 day', '{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	setCommonFlag(t, pool, true)
	if err := accounts.EnsureCommonSpace(ctx, pool); err != nil {
		t.Fatalf("common space: %v", err)
	}
}

type spacesOf struct {
	personal, common, other int
}

func readSpaces(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) spacesOf {
	t.Helper()
	var s spacesOf
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM spaces sp WHERE sp.kind = 'personal' AND sp.owner_id = $1
           AND (SELECT count(*) FROM space_members m WHERE m.space_id = sp.id) = 1
           AND EXISTS (SELECT 1 FROM space_members m WHERE m.space_id = sp.id AND m.user_id = $1 AND m.role = 'owner')),
       (SELECT count(*) FROM space_members m JOIN spaces sp ON sp.id = m.space_id WHERE m.user_id = $1 AND sp.kind = 'common'),
       (SELECT count(*) FROM space_members m JOIN spaces sp ON sp.id = m.space_id WHERE m.user_id = $1 AND sp.kind = 'regular')`, userID).
		Scan(&s.personal, &s.common, &s.other); err != nil {
		t.Fatalf("read spaces: %v", err)
	}
	return s
}

type createPath struct {
	name string
	// create makes the account on pool and returns its id.
	create func(t *testing.T, pool *pgxpool.Pool, external bool) uuid.UUID
	// external is false where the path cannot create an external user.
	canExternal bool
	// joinsCommon: the path is one of INV-22's — an admin, an admin's
	// agent, SSO — whose accounts join the common space by the flag. A
	// self-service signup is not (US-8, SAAS-PLAN 2.6: "those who signed up do
	// not enter the common space").
	joinsCommon bool
	// confirmsEmail: the path is in the closed set "email confirmed"
	// (US-5, plan 6) — admin, SSO, the initial admin; not a signup.
	confirmsEmail bool
}

var paths = []createPath{
	{"admin (REST and the admin's agent over MCP)", func(t *testing.T, pool *pgxpool.Pool, external bool) uuid.UUID {
		store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
		dto, err := store.CreateUser(context.Background(), users.CreateUserInput{
			Email: "u-" + uuid.NewString()[:8] + "@acc.test", Name: "New Account", Password: "correct horse battery staple 42",
			Role: "user", IsExternal: external,
		}, users.HumanActor(uuid.Nil, "admin"))
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		return dto.ID
	}, true, true, true},
	{"SSO provisioning", func(t *testing.T, pool *pgxpool.Pool, _ bool) uuid.UUID {
		email := "sso-" + uuid.NewString()[:8] + "@acc.test"
		res, err := auth.NewProvisioner(pool, true, "user", "").Provision(context.Background(), auth.ProvisionInput{
			Email: email, Username: strings.Split(email, "@")[0],
		})
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		return res.UserID
	}, false, true, true},
	{"SSO bootstrap email (the first admin)", func(t *testing.T, pool *pgxpool.Pool, _ bool) uuid.UUID {
		email := "boot-" + uuid.NewString()[:8] + "@acc.test"
		res, err := auth.NewProvisioner(pool, true, "user", email).Provision(context.Background(), auth.ProvisionInput{
			Email: email, Username: strings.Split(email, "@")[0],
		})
		if err != nil {
			t.Fatalf("Provision (bootstrap): %v", err)
		}
		return res.UserID
	}, false, true, true},
	{"self-service signup", func(t *testing.T, pool *pgxpool.Pool, _ bool) uuid.UUID {
		return signupViaService(t, pool)
	}, false, false, false},
}

// signupViaService registers an account through the real accounts.Service.
func signupViaService(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	svc := accounts.NewService(accounts.ServiceConfig{
		SignupEnabled: true, EmailEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local",
	}, pool, store, accounts.NewDirectory(pool), accounts.NewTurnstile("", accounts.TurnstileVerifyURL), accounts.NewRateLimiter())
	resp, err := svc.Signup(context.Background(), accounts.SignupRequest{
		Email: "self-" + uuid.NewString()[:8] + "@acc.test", Username: "self-" + uuid.NewString()[:8],
		Password: "correct horse battery staple 42", TermsAccepted: true, Language: accounts.LanguageEN,
	}, "")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	return uuid.MustParse(resp.ID)
}

// seedCommonProject puts a project of visibility `space` into the common
// space: its participants reach it with the source "from the space".
func seedCommonProject(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var spaceID, owner uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id, owner_id FROM spaces WHERE kind = 'common'`).Scan(&spaceID, &owner); err != nil {
		t.Fatalf("read the common space: %v", err)
	}
	id, _ := testsupport.SeedProjectInSpace(t, pool, owner, spaceID, "common")
	if _, err := pool.Exec(context.Background(), `UPDATE projects SET visibility = 'space' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// reachesProject reports whether the user is a member of the project.
func reachesProject(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM project_members WHERE project_id = $1 AND user_id = $2)`,
		projectID, userID).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// Row 54 — the creation paths × the flag.
func TestProvision_EveryCreationPath(t *testing.T) {
	for _, p := range paths {
		for _, flag := range []bool{true, false} {
			for _, external := range []bool{false, true} {
				if external && !p.canExternal {
					continue
				}
				name := p.name
				if external {
					name += "/external"
				}
				if flag {
					name += "/common on"
				} else {
					name += "/common off"
				}
				t.Run(name, func(t *testing.T) {
					pool := freshDatabase(t)
					seedCommon(t, pool)
					project := seedCommonProject(t, pool)
					setCommonFlag(t, pool, flag)
					id := p.create(t, pool, external)
					got := readSpaces(t, pool, id)
					want := spacesOf{personal: 1}
					if flag && p.joinsCommon {
						want.common = 1
					}
					if external {
						want.personal, want.common = 0, 0
					}
					if got != want {
						t.Errorf("spaces of the new account = %+v, want %+v", got, want)
					}
					if reach := reachesProject(t, pool, id, project); reach != (want.common == 1) {
						t.Errorf("the new account reaches the common space's project: %v, want %v", reach, want.common == 1)
					}
				})
			}
		}
	}

	t.Run("initial admin", func(t *testing.T) {
		for _, flag := range []bool{true, false} {
			pool := freshDatabase(t)
			setCommonFlag(t, pool, flag)
			email := "first@acc.test"
			if err := auth.Bootstrap(context.Background(), pool, auth.BootstrapConfig{
				AdminEmail: email, AdminPassword: "correct horse battery staple 42", BcryptCost: auth.MinBcryptCost,
			}); err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			// main creates the common space right after the bootstrap.
			if err := accounts.EnsureCommonSpace(context.Background(), pool); err != nil {
				t.Fatalf("EnsureCommonSpace: %v", err)
			}
			var id uuid.UUID
			if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
				t.Fatal(err)
			}
			got := readSpaces(t, pool, id)
			want := spacesOf{personal: 1}
			if flag {
				want.common = 1
			}
			if got != want {
				t.Errorf("flag %v: spaces of the initial admin = %+v, want %+v", flag, got, want)
			}
		}
	})
}

// acceptFromInbox invites account id to a fresh regular space and accepts
// from its Inbox with mail on (POST /me/invitations/{id}/accept through the
// invitations service, T040): "" when accepted — the account is then a
// participant — or the refusal code.
func acceptFromInbox(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, owner)
	var username, email string
	if err := pool.QueryRow(ctx, `SELECT username, lower(email) FROM users WHERE id = $1`, id).Scan(&username, &email); err != nil {
		t.Fatal(err)
	}
	var inv uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '1 day') RETURNING id`, space, email, owner).Scan(&inv); err != nil {
		t.Fatalf("seeding the invitation: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM space_invitations WHERE id = $1`, inv) })
	svc := invitations.NewService(pool, spaces.NewService(pool, spaces.Options{}),
		accounts.NewTurnstile("", accounts.TurnstileVerifyURL),
		invitations.Options{EmailEnabled: true, InviteTTL: time.Hour, DeliveryTTL: time.Hour})
	_, err := svc.AcceptMine(ctx, spaces.Caller{UserID: id, Username: username}, inv)
	var ref *spaces.Refusal
	switch {
	case err == nil:
		if !reachesSpace(t, pool, space, id) {
			t.Error("accepted, but the account is not a participant of the space")
		}
		return ""
	case errors.As(err, &ref):
		if reachesSpace(t, pool, space, id) {
			t.Errorf("refused %s, but the account joined the space", ref.Code)
		}
		return ref.Code
	}
	t.Fatalf("accepting from the Inbox: %v", err)
	return ""
}

func reachesSpace(t *testing.T, pool *pgxpool.Pool, space, user uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`, space, user).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// confirmedAt reports whether the account's email is confirmed.
func confirmedAt(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var confirmed bool
	if err := pool.QueryRow(context.Background(),
		`SELECT email_confirmed_at IS NOT NULL FROM users WHERE id = $1`, id).Scan(&confirmed); err != nil {
		t.Fatalf("read email_confirmed_at: %v", err)
	}
	return confirmed
}

// Row 54a — the closed set "email confirmed" (spec US-5, plan 6): an
// account created by an admin (REST, the admin's agent over MCP — both
// users.Store.CreateUser), through SSO provisioning (the bootstrap email
// included) or as the initial admin has its email confirmed; an ordinary
// signup does not — and so, with mail on, its accept from the Inbox is
// refused email_unconfirmed while the others' is accepted (T040, INV-16).
func TestProvision_EmailConfirmedClosedSet(t *testing.T) {
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			pool := freshDatabase(t)
			seedCommon(t, pool)
			id := p.create(t, pool, false)
			if got := confirmedAt(t, pool, id); got != p.confirmsEmail {
				t.Errorf("email confirmed after %s = %v, want %v", p.name, got, p.confirmsEmail)
			}
			want := "email_unconfirmed"
			if p.confirmsEmail {
				want = ""
			}
			if got := acceptFromInbox(t, pool, id); got != want {
				t.Errorf("accepting an invitation from the Inbox after %s with mail on: refusal %q, want %q", p.name, got, want)
			}
		})
	}

	t.Run("initial admin", func(t *testing.T) {
		pool := freshDatabase(t)
		email := "first@acc.test"
		if err := auth.Bootstrap(context.Background(), pool, auth.BootstrapConfig{
			AdminEmail: email, AdminPassword: "correct horse battery staple 42", BcryptCost: auth.MinBcryptCost,
		}); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
		var id uuid.UUID
		if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if !confirmedAt(t, pool, id) {
			t.Error("email_confirmed_at is NULL for the initial admin, want set")
		}
	})

}
