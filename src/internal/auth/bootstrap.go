package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/accounts"
	"skein-api/internal/storage/generated"
)

// pgUniqueViolationCode is the SQLSTATE class for "duplicate key value
// violates unique constraint" — emitted by Postgres when two replicas race
// the bootstrap insert and the second one loses on the unique index.
const pgUniqueViolationCode = "23505"

// BootstrapConfig is the small slice of skein-api configuration needed by
// the admin-bootstrap path. It is decoupled from internal/config so this
// package can be reused (and tested) without dragging in the whole config
// surface.
type BootstrapConfig struct {
	// AdminEmail is the email address of the very first user. When empty,
	// Bootstrap is a no-op (suitable for environments where admin will be
	// provisioned out-of-band, e.g. via Keycloak).
	AdminEmail string

	// AdminPassword is the plaintext password for the first user. Required
	// whenever AdminEmail is set — Bootstrap returns an error if it isn't,
	// rather than silently inserting a user with no auth path.
	AdminPassword string

	// AdminUsername (optional) — when blank, Bootstrap derives one from
	// AdminEmail via UsernameFromEmail.
	AdminUsername string

	// AdminName (optional) — when blank, Bootstrap falls back to the
	// resolved username.
	AdminName string

	// BcryptCost is the bcrypt work factor for the admin password hash.
	// The caller (config.Load) is expected to have validated this; values
	// outside [bcrypt.MinCost, bcrypt.MaxCost] would fail in Hash().
	BcryptCost int
}

// Bootstrap inserts the very first admin user when the users table is
// empty and SKEIN_INITIAL_ADMIN_EMAIL is set. It is safe to call on every
// startup: subsequent runs short-circuit on `count(users) > 0`.
//
// Race protection on multi-replica deployments is two-layered:
//  1. The check-and-insert runs inside a SERIALIZABLE transaction so two
//     replicas observing an empty table at the same instant cannot both
//     succeed.
//  2. As a backstop, a unique-violation on the email index (23505) is
//     treated as "another replica already bootstrapped" — logged at INFO
//     and returned as nil rather than escalating to a fatal startup error.
//
// Logging discipline: never log the admin email or password. Only the
// resolved userId and username (which the slog sanitizer cannot redact —
// "password"/"key"/"token" are masked, but plain values aren't).
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, cfg BootstrapConfig) error {
	if cfg.AdminEmail == "" {
		slog.DebugContext(ctx, "bootstrap skipped: SKEIN_INITIAL_ADMIN_EMAIL not set")
		return nil
	}
	if cfg.AdminPassword == "" {
		return errors.New("SKEIN_INITIAL_ADMIN_PASSWORD is required when SKEIN_INITIAL_ADMIN_EMAIL is set")
	}

	// Quick pre-check outside a transaction — avoids opening a SERIALIZABLE
	// txn on every startup once the table is populated.
	q := generated.New(pool)
	count, err := q.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("counting users for bootstrap: %w", err)
	}
	if count > 0 {
		slog.DebugContext(ctx, "bootstrap skipped: users already present", "userCount", count)
		return nil
	}

	username := cfg.AdminUsername
	if username == "" {
		username = UsernameFromEmail(cfg.AdminEmail)
	}
	name := cfg.AdminName
	if name == "" {
		name = username
	}

	hash, err := Hash(cfg.AdminPassword, cfg.BcryptCost)
	if err != nil {
		return fmt.Errorf("hashing bootstrap password: %w", err)
	}

	// UUIDv7 — time-ordered, indexable, monotonic across pods. Same choice
	// the rest of the schema uses for primary keys (see db-schema.md §1).
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generating user id: %w", err)
	}
	pgID := pgtype.UUID{Bytes: id, Valid: true}

	// SERIALIZABLE protects against the multi-replica race where N replicas
	// observe count==0 simultaneously: at most one COMMITs, the others get
	// a serialization failure (40001) on commit and we surface that.
	err = pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		txq := generated.New(tx)
		// Re-check inside the txn — without this, two replicas can both
		// pass the outer count() check and only collide at INSERT time.
		c, err := txq.CountUsers(ctx)
		if err != nil {
			return fmt.Errorf("re-counting users in bootstrap tx: %w", err)
		}
		if c > 0 {
			return nil
		}
		_, err = txq.CreateUser(ctx, generated.CreateUserParams{
			ID:           pgID,
			Email:        cfg.AdminEmail,
			Username:     username,
			Name:         name,
			Role:         "admin",
			PasswordHash: &hash,
		})
		if err != nil {
			return err
		}
		// sk-674.18 T020: the bootstrap admin gets its "Personal" like every
		// new account (INV-8); main creates the common space right after
		// (accounts.EnsureCommonSpace).
		return accounts.ProvisionInTx(ctx, tx, id, accounts.Provision{EmailConfirmed: true})
	})
	if err != nil {
		// Unique violation on email or username = another replica won the
		// race. Log and continue — not a startup failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolationCode {
			slog.InfoContext(ctx, "bootstrap admin already created by another replica, skipping",
				"constraint", pgErr.ConstraintName,
			)
			return nil
		}
		return fmt.Errorf("bootstrap insert: %w", err)
	}

	slog.InfoContext(ctx, "bootstrap admin created",
		"userId", id.String(),
		"username", username,
		"role", "admin",
	)
	return nil
}
