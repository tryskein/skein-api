package storage

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	// Migration database driver — registers "pgx5" scheme on the migrate side.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	// Migration source driver — registers "file://" scheme.
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// RunMigrations applies every pending up migration using the file source
// driver and the pgx/v5 database driver. Returns nil when the schema is
// already current (migrate.ErrNoChange).
//
// golang-migrate's pgx5 driver ignores the "postgres://" scheme in
// DATABASE_URL — it expects "pgx5://", so we do a single rewrite here. The
// rest of the URL (host, credentials, query params like sslmode) is
// preserved.
func RunMigrations(databaseURL, migrationsPath string) error {
	source := "file://" + migrationsPath
	dbURL, err := pgxMigrateURL(databaseURL)
	if err != nil {
		return fmt.Errorf("rewriting database URL for migrate: %w", err)
	}

	m, err := migrate.New(source, dbURL)
	if err != nil {
		return fmt.Errorf("opening migrator: %w", err)
	}
	defer func() {
		// Close releases the file source and the *separate* DB connection
		// the migrator opens (we don't share the pgxpool with it). Errors
		// here are logged but never block startup.
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			slog.Warn("migrator close returned errors",
				"sourceError", fmtErr(srcErr),
				"databaseError", fmtErr(dbErr),
			)
		}
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}

	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("reading migration version: %w", err)
	}
	slog.Info("migrations applied", "version", version, "dirty", dirty)
	return nil
}

// pgxMigrateURL converts a libpq URL ("postgres://...") into the form
// expected by golang-migrate's pgx5 driver ("pgx5://..."). Any other scheme
// is returned unchanged (we never want to silently rewrite an unexpected
// URL shape).
func pgxMigrateURL(databaseURL string) (string, error) {
	const (
		libpq = "postgres://"
		altpq = "postgresql://"
		pgx5  = "pgx5://"
	)
	switch {
	case len(databaseURL) >= len(libpq) && databaseURL[:len(libpq)] == libpq:
		return pgx5 + databaseURL[len(libpq):], nil
	case len(databaseURL) >= len(altpq) && databaseURL[:len(altpq)] == altpq:
		return pgx5 + databaseURL[len(altpq):], nil
	default:
		return "", fmt.Errorf("DATABASE_URL must start with postgres:// or postgresql://")
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
