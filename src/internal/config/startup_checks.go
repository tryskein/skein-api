package config

// Startup checks that need the database (K-19).
//
// Load rejects the contradictions of SAAS-PLAN 2.5 decided by the variables
// alone. The rows below are decided by the data, so they run against the
// database around the schema migration:
//
//   - PreMigration — before any migration touches the schema. A transition
//     to spaces that cannot succeed (projects without a space while the admin
//     role is off, or with no active admin to own the common space) must stop
//     the process with the database untouched: a migration cannot read the
//     environment, and a migration that fails half-way leaves the schema
//     dirty.
//   - PostMigration — on the current schema: role admin in the data while
//     the role is off (INV-20), an installation nobody can enter (INV-21),
//     a common space to create with no way to name its owner (US-2).
//   - SyncInstanceSettings — writes the admin flag into instance_settings and
//     reads it back (K-2), so the database access function reads the value
//     this process was started with.
//
// PrepareDatabase runs the three in order around the migration and is what
// main calls; every refusal is a *ConflictError naming the variables.
//
// The checks must work on any schema a deployment can start from: an empty
// database (no tables yet), the schema before spaces (no spaces table, no
// projects.space_id) and the schema after them. Table and column presence is
// therefore probed, never assumed.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// startupCheckTimeout bounds each database phase of PrepareDatabase except
// the migration, whose duration depends on the data and is not bounded here.
// The checks are a handful of EXISTS probes; a database that cannot answer
// them in this time must fail startup rather than hang it.
const startupCheckTimeout = 30 * time.Second

// PrepareDatabase is the database part of the api startup: data checks that
// must hold before the schema changes, the migration itself, data checks on
// the migrated schema, and the instance_settings write. migrate applies the
// pending migrations (storage.RunMigrations in production).
func PrepareDatabase(ctx context.Context, cfg *Config, pool *pgxpool.Pool, migrate func() error) error {
	if err := withTimeout(ctx, func(ctx context.Context) error { return PreMigration(ctx, cfg, pool) }); err != nil {
		return err
	}
	if err := migrate(); err != nil {
		return err
	}
	if err := withTimeout(ctx, func(ctx context.Context) error { return PostMigration(ctx, cfg, pool) }); err != nil {
		return err
	}
	return withTimeout(ctx, func(ctx context.Context) error { return SyncInstanceSettings(ctx, cfg, pool) })
}

func withTimeout(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, startupCheckTimeout)
	defer cancel()
	return f(ctx)
}

// PreMigration refuses a transition to spaces that has no common-space owner
// (SAAS-PLAN 2.5 row 5, E-24, US-2). A transition is pending while some
// project has no space — on the schema before spaces that is every project.
// Its common space is owned by the earliest active admin, so the admin role
// must be on and an active admin must exist; a deactivated admin does not
// count. Read-only: on refusal the database is exactly as it was.
func PreMigration(ctx context.Context, cfg *Config, pool *pgxpool.Pool) error {
	pending, err := transitionPending(ctx, pool)
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	if !cfg.InstanceAdminEnabled {
		return conflict("projects without a space must move into the common space, whose owner is an "+
			"instance admin, but the instance admin role is disabled",
			"SKEIN_INSTANCE_ADMIN_ENABLED")
	}
	active, err := activeAdminExists(ctx, pool)
	if err != nil {
		return err
	}
	if !active {
		return conflict("projects without a space must move into the common space, whose owner is the "+
			"earliest active instance admin, and there is no active admin",
			"SKEIN_INSTANCE_ADMIN_ENABLED")
	}
	return nil
}

// PostMigration checks the migrated data against the settings.
func PostMigration(ctx context.Context, cfg *Config, pool *pgxpool.Pool) error {
	// SAAS-PLAN 2.5 row 3, INV-20, SC-006: with the role off no user may
	// hold it — deactivated users included, the access function would see
	// the role in the data whatever the flag says.
	if !cfg.InstanceAdminEnabled {
		var anyAdmin bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&anyAdmin); err != nil {
			return fmt.Errorf("startup check: reading admin users: %w", err)
		}
		if anyAdmin {
			return conflict("the instance admin role is disabled, but users with role admin exist; "+
				"change their role before disabling it",
				"SKEIN_INSTANCE_ADMIN_ENABLED")
		}
	}

	var usersEmpty bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM users)`).Scan(&usersEmpty); err != nil {
		return fmt.Errorf("startup check: counting users: %w", err)
	}

	// INV-21: an installation nobody can enter. Only an empty user base is
	// refused — an installation with users already has a way in.
	if !cfg.InstanceAdminEnabled && usersEmpty {
		if !cfg.SignupEnabled && cfg.AuthMode == "local" {
			return conflict("no user exists, registration is closed, the login is by password and no admin "+
				"can create accounts: nobody can enter the installation",
				"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_SIGNUP_ENABLED", "SKEIN_AUTH_MODE")
		}
		if cfg.AuthMode == "keycloak" && !cfg.AutoProvision {
			return conflict("no user exists, SSO creates no users and no admin can create accounts: "+
				"nobody can enter the installation",
				"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_AUTH_MODE", "SKEIN_AUTO_PROVISION")
		}
	}

	// SAAS-PLAN 2.5 row 6 and the spec's "common space outside the transition": the common
	// space is wanted and missing, and its owner must be an instance admin.
	if cfg.CommonSpaceEnabled {
		exists, err := commonSpaceExists(ctx, pool)
		if err != nil {
			return err
		}
		if !exists {
			if !cfg.InstanceAdminEnabled {
				return conflict("the common space must be created, and its owner is an instance admin, "+
					"but the instance admin role is disabled",
					"SKEIN_INSTANCE_ADMIN_ENABLED", "SKEIN_COMMON_SPACE_ENABLED")
			}
			active, err := activeAdminExists(ctx, pool)
			if err != nil {
				return err
			}
			if !active {
				appears, err := adminWillAppear(ctx, cfg, pool, usersEmpty)
				if err != nil {
					return err
				}
				if !appears {
					return conflict("the common space must be created and its owner is the earliest active "+
						"instance admin, but there is none and none can appear",
						"SKEIN_COMMON_SPACE_ENABLED", "SKEIN_INITIAL_ADMIN_EMAIL", "SKEIN_BOOTSTRAP_ADMIN_EMAIL",
						"SKEIN_AUTH_MODE", "SKEIN_AUTO_PROVISION", "SKEIN_OIDC_DEFAULT_ROLE")
				}
			}
		}
	}
	return nil
}

// adminWillAppear reports whether an instance admin is certain to be created
// by one of the paths that exist (spec US-2: "if no admin appears by
// any path" is a startup error):
//
//   - the initial admin — auth.Bootstrap creates it right after these checks,
//     but only into an EMPTY user base;
//   - the SSO bootstrap admin — the provisioner elevates
//     SKEIN_BOOTSTRAP_ADMIN_EMAIL only when it INSERTS that user
//     (auth/oidc.go#insertNew): that needs a login mode with SSO (keycloak or
//     hybrid), SKEIN_AUTO_PROVISION=true and no existing account with that
//     email (an existing one is matched and updated, never elevated);
//   - SSO provisioning with SKEIN_OIDC_DEFAULT_ROLE=admin — every user it
//     creates is an admin; same mode and auto-provisioning condition.
func adminWillAppear(ctx context.Context, cfg *Config, pool *pgxpool.Pool, usersEmpty bool) (bool, error) {
	if cfg.InitialAdminEmail != "" && usersEmpty {
		return true, nil
	}
	ssoCreatesUsers := (cfg.AuthMode == "keycloak" || cfg.AuthMode == "hybrid") && cfg.AutoProvision
	if !ssoCreatesUsers {
		return false, nil
	}
	if cfg.OIDCDefaultRole == "admin" {
		return true, nil
	}
	if cfg.BootstrapAdminEmail == "" {
		return false, nil
	}
	var taken bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower($1))`,
		strings.TrimSpace(cfg.BootstrapAdminEmail)).Scan(&taken); err != nil {
		return false, fmt.Errorf("startup check: reading the bootstrap admin account: %w", err)
	}
	return !taken, nil
}

// SyncInstanceSettings writes SKEIN_INSTANCE_ADMIN_ENABLED and
// SKEIN_COMMON_SPACE_ENABLED into the single instance_settings row and reads
// them back (K-2, D-2; the common space flag — T020, INV-22). The database access
// function reads the flag from there, never from its caller; a read-back
// that differs from the variable means another writer won the row, and the
// process refuses to serve with a flag it cannot vouch for.
func SyncInstanceSettings(ctx context.Context, cfg *Config, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		INSERT INTO instance_settings (id, admin_enabled, common_space_enabled, updated_at)
		VALUES (1, $1, $2, now())
		ON CONFLICT (id) DO UPDATE
		   SET admin_enabled        = EXCLUDED.admin_enabled,
		       common_space_enabled = EXCLUDED.common_space_enabled,
		       updated_at           = EXCLUDED.updated_at`, cfg.InstanceAdminEnabled, cfg.CommonSpaceEnabled); err != nil {
		return fmt.Errorf("writing instance_settings: %w", err)
	}
	var storedAdmin, storedCommon bool
	if err := pool.QueryRow(ctx,
		`SELECT admin_enabled, common_space_enabled FROM instance_settings WHERE id = 1`).Scan(&storedAdmin, &storedCommon); err != nil {
		return fmt.Errorf("reading back instance_settings: %w", err)
	}
	if storedAdmin != cfg.InstanceAdminEnabled {
		return conflict(fmt.Sprintf("instance_settings.admin_enabled reads %t after writing %t; "+
			"another api process is running with a different value", storedAdmin, cfg.InstanceAdminEnabled),
			"SKEIN_INSTANCE_ADMIN_ENABLED")
	}
	if storedCommon != cfg.CommonSpaceEnabled {
		return conflict(fmt.Sprintf("instance_settings.common_space_enabled reads %t after writing %t; "+
			"another api process is running with a different value", storedCommon, cfg.CommonSpaceEnabled),
			"SKEIN_COMMON_SPACE_ENABLED")
	}
	return nil
}

// transitionPending reports whether some project has no space. Before the
// spaces schema there is no projects.space_id, and every project is
// without a space; on an empty database there are no projects at all.
func transitionPending(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	hasProjects, err := tableExists(ctx, pool, "projects")
	if err != nil || !hasProjects {
		return false, err
	}
	hasSpaceID, err := columnExists(ctx, pool, "projects", "space_id")
	if err != nil {
		return false, err
	}
	q := `SELECT EXISTS (SELECT 1 FROM projects)`
	if hasSpaceID {
		q = `SELECT EXISTS (SELECT 1 FROM projects WHERE space_id IS NULL)`
	}
	var pending bool
	if err := pool.QueryRow(ctx, q).Scan(&pending); err != nil {
		return false, fmt.Errorf("startup check: reading projects without a space: %w", err)
	}
	return pending, nil
}

// activeAdminExists reports whether an active instance admin exists — the
// one definition shared with the owner choice of the common space
// (migration 000071, accounts.EnsureCommonSpace): role admin, not
// deactivated, not deleted, not external. An external admin cannot exist
// (CHECK users_external_not_admin); the condition keeps the definitions
// identical. deleted_at is read only where the column exists (000069): the
// pre-migration check runs on a schema before it, where nobody is deleted.
// False on a database without the users table.
func activeAdminExists(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	hasUsers, err := tableExists(ctx, pool, "users")
	if err != nil || !hasUsers {
		return false, err
	}
	hasDeleted, err := columnExists(ctx, pool, "users", "deleted_at")
	if err != nil {
		return false, err
	}
	q := `SELECT EXISTS (SELECT 1 FROM users
		 WHERE role = 'admin' AND deactivated_at IS NULL AND NOT is_external)`
	if hasDeleted {
		q = `SELECT EXISTS (SELECT 1 FROM users
		 WHERE role = 'admin' AND deactivated_at IS NULL AND deleted_at IS NULL AND NOT is_external)`
	}
	var active bool
	if err := pool.QueryRow(ctx, q).Scan(&active); err != nil {
		return false, fmt.Errorf("startup check: reading active admins: %w", err)
	}
	return active, nil
}

// commonSpaceExists reports whether a space of kind common exists. False
// before the spaces schema exists.
func commonSpaceExists(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	hasSpaces, err := tableExists(ctx, pool, "spaces")
	if err != nil || !hasSpaces {
		return false, err
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM spaces WHERE kind = 'common')`).Scan(&exists); err != nil {
		return false, fmt.Errorf("startup check: reading the common space: %w", err)
	}
	return exists, nil
}

func tableExists(ctx context.Context, pool *pgxpool.Pool, table string) (bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		return false, fmt.Errorf("startup check: probing table %s: %w", table, err)
	}
	return exists, nil
}

func columnExists(ctx context.Context, pool *pgxpool.Pool, table, column string) (bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = $1 AND column_name = $2)`, table, column).Scan(&exists); err != nil {
		return false, fmt.Errorf("startup check: probing column %s.%s: %w", table, column, err)
	}
	return exists, nil
}
