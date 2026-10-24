package config

// startup_checks_test.go — sk-674.18, test matrix rows 4–7: the startup
// checks that depend on the data (K-19, US-2, E-24, INV-20, INV-21, SC-006).
//
// Reference: SAAS-PLAN 2.5 and spec US-2 / INV-20 / INV-21, written before
// the code. Every case runs the production startup step — PrepareDatabase
// with storage.RunMigrations, exactly what cmd/api/main.go calls — never
// PreMigration or PostMigration on their own, so a check wired in the wrong
// place or not at all turns the case red.
//
// The checks read GLOBAL state (is there any user, any admin, any project),
// which the shared test database cannot give: other packages' rows are
// there. Each case therefore gets its own database on the same server
// (freshDatabase): created, migrated to the version the case needs, seeded,
// and dropped afterwards.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/storage"
	"skein-api/internal/testsupport"
)

// migrationsDir is src/migrations relative to this package directory.
const migrationsDir = "../../migrations"

// migrationVersions lists the versions of src/migrations in ascending order.
func migrationVersions(t *testing.T) []uint {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil || len(files) < 2 {
		t.Fatalf("listing migrations in %s: %v (found %d)", migrationsDir, err, len(files))
	}
	var versions []uint
	for _, f := range files {
		head, _, _ := strings.Cut(filepath.Base(f), "_")
		v, err := strconv.ParseUint(head, 10, 32)
		if err != nil {
			t.Fatalf("migration file %s has no numeric version: %v", f, err)
		}
		versions = append(versions, uint(v))
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions
}

// spacesSchemaFile is the migration that introduced spaces (sk-674.18 T015).
const spacesSchemaFile = "_spaces_schema.up.sql"

// beforeSpacesVersion is the version right before the spaces schema
// migration — the schema an installation upgrades to spaces from.
func beforeSpacesVersion(t *testing.T, versions []uint) uint {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*"+spacesSchemaFile))
	if err != nil || len(files) != 1 {
		t.Fatalf("finding the spaces schema migration: %v (found %d)", err, len(files))
	}
	head, _, _ := strings.Cut(filepath.Base(files[0]), "_")
	v, err := strconv.ParseUint(head, 10, 32)
	if err != nil {
		t.Fatalf("spaces schema migration %s: %v", files[0], err)
	}
	for i, ver := range versions {
		if ver == uint(v) && i > 0 {
			return versions[i-1]
		}
	}
	t.Fatalf("spaces schema version %d has no predecessor in %v", v, versions)
	return 0
}

// seedProjectWithoutSpace inserts a project the way it existed before
// spaces: no space, the owner its member. The database is dropped with the
// case, so nothing is cleaned up here.
func seedProjectWithoutSpace(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	prefix := "cfgtr" + strings.ReplaceAll(id.String()[:6], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description, git_repos,
                      created_by, created_at, updated_at, metadata)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, $4, now(), now(), '{}'::jsonb)`,
		id, prefix, prefix, owner); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'owner', now(), $2)`, id, owner); err != nil {
		t.Fatalf("seed project member: %v", err)
	}
}

// testDB is a database of its own for one case.
type testDB struct {
	pool *pgxpool.Pool
	url  string
}

// freshDatabase creates an empty database next to TEST_DATABASE_URL's,
// creates the extensions the schema needs (they are created at cluster
// bootstrap in every deployment, never by a migration), migrates it to
// version (0 = every migration) and drops it when the test ends. Skips like
// every DB-backed test when TEST_DATABASE_URL is unset.
func freshDatabase(t *testing.T, version uint) testDB {
	t.Helper()
	admin := testsupport.NewTestPool(t)
	name := "cfgtest_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("cleanup: DROP DATABASE %s: %v", name, err)
		}
	})

	u, err := url.Parse(os.Getenv(testsupport.TestDatabaseURL))
	if err != nil {
		t.Fatalf("parsing %s: %v", testsupport.TestDatabaseURL, err)
	}
	u.Path = "/" + name
	dbURL := u.String()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	for _, ext := range []string{"pg_trgm", "vector"} {
		if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS `+ext); err != nil {
			t.Fatalf("CREATE EXTENSION %s on %s: %v", ext, name, err)
		}
	}

	m, err := migrate.New("file://"+migrationsDir, "pgx5://"+strings.TrimPrefix(dbURL, u.Scheme+"://"))
	if err != nil {
		t.Fatalf("opening migrator on %s: %v", name, err)
	}
	defer m.Close()
	if version == 0 {
		err = m.Up()
	} else {
		err = m.Migrate(version)
	}
	if err != nil {
		t.Fatalf("migrating %s to %d: %v", name, version, err)
	}
	return testDB{pool: pool, url: dbURL}
}

// prepare runs the production startup step against db.
func prepare(t *testing.T, db testDB, cfg *Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return PrepareDatabase(ctx, cfg, db.pool, func() error {
		return storage.RunMigrations(db.url, migrationsDir)
	})
}

// schemaVersion reads golang-migrate's bookkeeping row.
func schemaVersion(t *testing.T, pool *pgxpool.Pool) (uint, bool) {
	t.Helper()
	var v int64
	var dirty bool
	if err := pool.QueryRow(context.Background(),
		`SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty); err != nil {
		t.Fatalf("reading schema_migrations: %v", err)
	}
	return uint(v), dirty
}

// dataFingerprint summarises the rows the transition would touch, so a
// refusal can be shown to leave them exactly as they were.
func dataFingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var users, projects, members int
	var roles string
	err := pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM users),
		       (SELECT count(*) FROM projects),
		       (SELECT count(*) FROM project_members),
		       (SELECT coalesce(string_agg(id::text || ':' || role || ':' ||
		                        (deactivated_at IS NULL)::text, ',' ORDER BY id), '') FROM users)`).
		Scan(&users, &projects, &members, &roles)
	if err != nil {
		t.Fatalf("fingerprinting data: %v", err)
	}
	return fmt.Sprintf("users=%d projects=%d members=%d roles=%s", users, projects, members, roles)
}

// startupConfig is a configuration that contradicts nothing by itself: the
// cases change only the fields their row is about.
func startupConfig() *Config {
	return &Config{
		AuthMode:             "local",
		InstanceAdminEnabled: true,
		CommonSpaceEnabled:   true,
		EmailEnabled:         true,
	}
}

func wantConflict(t *testing.T, err error) *ConflictError {
	t.Helper()
	if err == nil {
		t.Fatal("startup succeeded, want a refusal")
	}
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("startup failed with %v (%T), want a *ConflictError — a refusal by the settings, not a broken step", err, err)
	}
	return conflictErr
}

// TestPrepareDatabase_TransitionWithoutCommonOwnerRefusedBeforeMigrations is
// matrix row 4 (E-24, US-2, K-19 PreMigration). Projects without a space
// mean the transition to spaces is pending; its common space is owned by the
// earliest ACTIVE admin. With the admin role off, or with no active admin
// (a deactivated one does not count), startup refuses BEFORE any migration
// runs: the schema version and the data are exactly as before. The control
// — an active admin — migrates to the latest version.
//
// The case starts where an upgrading installation starts: on the schema
// before spaces (the version just before the spaces schema migration), with
// a project seeded the way it existed then — without a space.
func TestPrepareDatabase_TransitionWithoutCommonOwnerRefusedBeforeMigrations(t *testing.T) {
	versions := migrationVersions(t)
	latest, previous := versions[len(versions)-1], beforeSpacesVersion(t, versions)

	cases := []struct {
		name        string
		adminOn     bool
		adminActive bool // the seeded owner is an admin; false = a plain user
		deactivate  bool
		wantRefusal bool
	}{
		{name: "admin role off, projects without a space", adminOn: false, wantRefusal: true},
		{name: "admin role on, no admin at all", adminOn: true, wantRefusal: true},
		{name: "admin role on, only a deactivated admin", adminOn: true, adminActive: true, deactivate: true, wantRefusal: true},
		{name: "admin role on, an active admin (control)", adminOn: true, adminActive: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := freshDatabase(t, previous)
			role := "user"
			if tc.adminActive {
				role = "admin"
			}
			owner, _ := testsupport.SeedUser(t, db.pool, role)
			if tc.deactivate {
				if _, err := db.pool.Exec(context.Background(),
					`UPDATE users SET deactivated_at = now() WHERE id = $1`, owner); err != nil {
					t.Fatalf("deactivating: %v", err)
				}
			}
			seedProjectWithoutSpace(t, db.pool, owner)
			before := dataFingerprint(t, db.pool)

			cfg := startupConfig()
			cfg.InstanceAdminEnabled = tc.adminOn
			cfg.CommonSpaceEnabled = false // row 6 of 2.5 is not this row
			err := prepare(t, db, cfg)

			if !tc.wantRefusal {
				if err != nil {
					t.Fatalf("startup = %v, want success with an active admin", err)
				}
				if v, dirty := schemaVersion(t, db.pool); v != latest || dirty {
					t.Errorf("schema version after startup = %d (dirty %v), want %d", v, dirty, latest)
				}
				return
			}
			wantConflict(t, err)
			if v, dirty := schemaVersion(t, db.pool); v != previous || dirty {
				t.Errorf("schema version after the refusal = %d (dirty %v), want %d untouched — "+
					"the check must run before the migrations", v, dirty, previous)
			}
			if after := dataFingerprint(t, db.pool); after != before {
				t.Errorf("data changed by a refused startup:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// TestPrepareDatabase_AdminRoleInDataWithAdminOff is matrix row 5 (INV-20,
// SC-006): with SKEIN_INSTANCE_ADMIN_ENABLED=false a user holding role admin
// refuses startup; with the role on the same data starts.
func TestPrepareDatabase_AdminRoleInDataWithAdminOff(t *testing.T) {
	for _, adminOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin role enabled=%v", adminOn), func(t *testing.T) {
			db := freshDatabase(t, 0)
			testsupport.SeedUser(t, db.pool, "admin")
			cfg := startupConfig()
			cfg.InstanceAdminEnabled = adminOn
			cfg.CommonSpaceEnabled = false
			err := prepare(t, db, cfg)
			if adminOn {
				if err != nil {
					t.Fatalf("startup = %v, want success: the admin role is on", err)
				}
				return
			}
			c := wantConflict(t, err)
			if !sameNames(c.Vars, []string{"SKEIN_INSTANCE_ADMIN_ENABLED"}) {
				t.Errorf("refusal names %v, want [SKEIN_INSTANCE_ADMIN_ENABLED]", c.Vars)
			}
		})
	}
}

// TestPrepareDatabase_NobodyCanEnter is matrix row 6 (INV-21, by the data):
// with the admin role off, an EMPTY user base and no way in — registration
// closed with password login, or SSO that creates nobody — startup refuses;
// the same settings with an existing user start.
func TestPrepareDatabase_NobodyCanEnter(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantVars []string
	}{
		{
			name:     "local, registration closed",
			mutate:   func(c *Config) { c.AuthMode = "local"; c.SignupEnabled = false },
			wantVars: []string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_SIGNUP_ENABLED", "SKEIN_AUTH_MODE"},
		},
		{
			name:     "keycloak, no auto-provisioning",
			mutate:   func(c *Config) { c.AuthMode = "keycloak"; c.AutoProvision = false },
			wantVars: []string{"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_AUTH_MODE", "SKEIN_AUTO_PROVISION"},
		},
	}
	for _, tc := range cases {
		for _, withUser := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, existing user=%v", tc.name, withUser), func(t *testing.T) {
				db := freshDatabase(t, 0)
				if withUser {
					testsupport.SeedUser(t, db.pool, "user")
				}
				cfg := startupConfig()
				cfg.InstanceAdminEnabled = false
				cfg.CommonSpaceEnabled = false
				tc.mutate(cfg)
				err := prepare(t, db, cfg)
				if withUser {
					if err != nil {
						t.Fatalf("startup = %v, want success: an installation with users has a way in", err)
					}
					return
				}
				c := wantConflict(t, err)
				if !sameNames(c.Vars, tc.wantVars) {
					t.Errorf("refusal names %v, want exactly %v", c.Vars, tc.wantVars)
				}
			})
		}
	}
}

// TestPrepareDatabase_CommonSpaceWithoutAWayToNameItsOwner is matrix row 7
// (US-2 "outside the transition"): the common space must be created, there is no
// active admin, and no path is certain to create one — startup refuses,
// naming the common-space flag and the variables of every path. An admin
// appears only through: the initial admin into an empty user base; the SSO
// bootstrap admin, which the provisioner elevates only when it CREATES that
// user (SSO login mode, auto-provisioning on, no existing account with the
// email); SSO auto-provisioning with SKEIN_OIDC_DEFAULT_ROLE=admin.
func TestPrepareDatabase_CommonSpaceWithoutAWayToNameItsOwner(t *testing.T) {
	const bootstrapEmail = "root@example.com"
	allVars := []string{"SKEIN_COMMON_SPACE_ENABLED", "SKEIN_INITIAL_ADMIN_EMAIL", "SKEIN_BOOTSTRAP_ADMIN_EMAIL",
		"SKEIN_AUTH_MODE", "SKEIN_AUTO_PROVISION", "SKEIN_OIDC_DEFAULT_ROLE"}
	cases := []struct {
		name            string
		mutate          func(*Config)
		bootstrapExists bool // an account with the bootstrap email already exists
		// admin seeds the installation's only admin: "" none, "deleted" an
		// admin whose account is deleted, "active" an active admin.
		admin       string
		wantRefusal bool
	}{
		{name: "no path at all", mutate: func(*Config) {}, wantRefusal: true},
		// Review main/2+3 F7: the startup check and the owner choice of the
		// common space (000071, EnsureCommonSpace) share one definition of an
		// active admin. A deleted admin cannot own the common space, so he is
		// no active admin here either. The external half of the definition
		// is not seeded: an external admin is refused by the database itself
		// (CHECK users_external_not_admin).
		{name: "the only admin is deleted", mutate: func(*Config) {}, admin: "deleted", wantRefusal: true},
		{name: "an active admin (control)", mutate: func(*Config) {}, admin: "active"},
		{name: "bootstrap email, local mode (no SSO provisioning)", mutate: func(c *Config) {
			c.BootstrapAdminEmail = bootstrapEmail
		}, wantRefusal: true},
		{name: "bootstrap email, keycloak without auto-provisioning", mutate: func(c *Config) {
			c.AuthMode, c.AutoProvision, c.BootstrapAdminEmail = "keycloak", false, bootstrapEmail
		}, wantRefusal: true},
		{name: "bootstrap email, keycloak, auto-provisioning, account already exists", mutate: func(c *Config) {
			c.AuthMode, c.AutoProvision, c.BootstrapAdminEmail = "keycloak", true, bootstrapEmail
		}, bootstrapExists: true, wantRefusal: true},
		{name: "bootstrap email, keycloak, auto-provisioning (control)", mutate: func(c *Config) {
			c.AuthMode, c.AutoProvision, c.BootstrapAdminEmail = "keycloak", true, bootstrapEmail
		}},
		{name: "bootstrap email, hybrid, auto-provisioning (control)", mutate: func(c *Config) {
			c.AuthMode, c.AutoProvision, c.BootstrapAdminEmail = "hybrid", true, bootstrapEmail
		}},
		{name: "SSO default role admin with auto-provisioning (control)", mutate: func(c *Config) {
			c.AuthMode, c.AutoProvision, c.OIDCDefaultRole = "keycloak", true, "admin"
		}},
		{name: "SSO default role admin without auto-provisioning", mutate: func(c *Config) {
			c.AuthMode, c.AutoProvision, c.OIDCDefaultRole = "keycloak", false, "admin"
		}, wantRefusal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := freshDatabase(t, 0)
			testsupport.SeedUser(t, db.pool, "user")
			if tc.admin != "" {
				admin, _ := testsupport.SeedUser(t, db.pool, "admin")
				if tc.admin == "deleted" {
					if _, err := db.pool.Exec(context.Background(),
						`UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8) WHERE id = $1`, admin); err != nil {
						t.Fatalf("deleting the admin: %v", err)
					}
				}
			}
			if tc.bootstrapExists {
				uid, _ := testsupport.SeedUser(t, db.pool, "user")
				if _, err := db.pool.Exec(context.Background(),
					`UPDATE users SET email = $2 WHERE id = $1`, uid, "Root@Example.com"); err != nil {
					t.Fatalf("giving the account the bootstrap email: %v", err)
				}
			}
			cfg := startupConfig()
			cfg.InstanceAdminEnabled = true
			cfg.CommonSpaceEnabled = true
			cfg.OIDCDefaultRole = "user"
			tc.mutate(cfg)
			err := prepare(t, db, cfg)
			if !tc.wantRefusal {
				if err != nil {
					t.Fatalf("startup = %v, want success: an admin exists or will appear and own the common space", err)
				}
				return
			}
			c := wantConflict(t, err)
			if !sameNames(c.Vars, allVars) {
				t.Errorf("refusal names %v, want exactly %v", c.Vars, allVars)
			}
		})
	}
}

// TestMigration000080_WithoutActiveAdminRefuses — sk-674.18 T024 (the
// lead's decision on 000080): the projects left without a space move into
// the common space, whose owner is only an active non-external admin (one
// definition with 000071 and the startup check, INV-20). Run past the
// startup check — the migrate CLI — on a database without such an admin, the
// migration fails with that reason and moves nothing; it never picks a plain
// user as the owner. Lives here for freshDatabase: the case needs a database
// with no admin at all, which the shared test database never is.
func TestMigration000080_WithoutActiveAdminRefuses(t *testing.T) {
	db := freshDatabase(t, 79)
	owner, _ := testsupport.SeedUser(t, db.pool, "user")
	seedProjectWithoutSpace(t, db.pool, owner)

	u, err := url.Parse(db.url)
	if err != nil {
		t.Fatalf("parse %s: %v", db.url, err)
	}
	m, err := migrate.New("file://"+migrationsDir, "pgx5://"+strings.TrimPrefix(db.url, u.Scheme+"://"))
	if err != nil {
		t.Fatalf("opening migrator: %v", err)
	}
	defer m.Close()
	err = m.Migrate(80)
	// The refusal is the migration's RAISE — SQLSTATE P0001 (the code, not the wording); the text is only for the diagnosis.
	var dbErr database.Error
	var pgErr *pgconn.PgError
	if !errors.As(err, &dbErr) || !errors.As(dbErr.OrigErr, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("Migrate(80) without an active admin = %v, want the migration's own refusal (SQLSTATE P0001)", err)
	}
	var withoutSpace, commons int
	if err := db.pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM projects WHERE space_id IS NULL),
       (SELECT count(*) FROM spaces WHERE kind = 'common')`).Scan(&withoutSpace, &commons); err != nil {
		t.Fatalf("reading the projects: %v", err)
	}
	if withoutSpace != 1 || commons != 0 {
		t.Errorf("after the refusal: %d projects without a space, %d common spaces — want 1 and 0 (nothing moved)",
			withoutSpace, commons)
	}
}
