package accounts

// create.go — what every new account gets (sk-674.18 T020, INV-8, INV-22,
// US-2 "common space outside the transition", US-12, E-16).
//
// Every path that creates a user — the admin over REST, an admin's agent
// over MCP, SSO provisioning, the bootstrap admin (and, with T038/T040,
// signup) — calls ProvisionInTx in the transaction that inserted the user:
//
//   - a non-external user gets their "Personal": a personal space they own and
//     are the only participant of (INV-8); an external user gets no space at
//     all (E-16);
//   - with SKEIN_COMMON_SPACE_ENABLED (instance_settings.common_space_enabled,
//     written by the api at startup) a non-external user becomes a
//     participant of the common space, if it exists (INV-22) — and gets the
//     source "from the space" on its `space` projects (INV-7).
//
// EnsureCommonSpace creates the common space outside the transition: at
// startup when the flag is on and there is none, and the moment the first
// admin appears (SSO bootstrap). Its owner is the earliest active admin;
// every non-external user joins it (on a new installation — those who came
// through SSO before the first admin; when re-enabled — everybody: an empty
// space widens nobody's access to a project).
//
// The package is a leaf on purpose — pgx, uuid and, for the confirmation
// email of a signup, mail (which imports only config) — because the
// creation paths live in packages (auth, users) the space machinery itself
// depends on. A new account has no session yet, so nothing here signals.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is the part of pgx.Tx / *pgxpool.Pool the primitives use.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrUserNotFound — ProvisionInTx for a user id with no row.
var ErrUserNotFound = errors.New("accounts: user not found")

// Provision says what kind of creation the account came from.
type Provision struct {
	// EmailConfirmed: the email counts as confirmed (US-5, the closed set):
	// an account created by an admin (REST, an admin's agent), through SSO,
	// or the bootstrap admin. Signup is not (T038).
	EmailConfirmed bool
	// SkipCommon leaves out the common space: the caller names the spaces
	// itself (an admin's checkboxes, sk-674.18 T037).
	SkipCommon bool
}

// ProvisionInTx gives the user userID what a new account gets (see the
// file header), inside tx — the transaction that created the user. It is
// idempotent: an existing "Personal" or participation is kept.
func ProvisionInTx(ctx context.Context, tx Execer, userID uuid.UUID, p Provision) error {
	var external bool
	if err := tx.QueryRow(ctx, `SELECT is_external FROM users WHERE id = $1`, userID).Scan(&external); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("reading the new user: %w", err)
	}
	if p.EmailConfirmed {
		if _, err := tx.Exec(ctx, `
UPDATE users SET email_confirmed_at = COALESCE(email_confirmed_at, now()) WHERE id = $1`, userID); err != nil {
			return fmt.Errorf("confirming the email: %w", err)
		}
	}
	if external {
		return nil
	}
	if err := createPersonal(ctx, tx, userID); err != nil {
		return err
	}
	if p.SkipCommon {
		return nil
	}
	var commonID *uuid.UUID
	if err := tx.QueryRow(ctx, `
SELECT s.id FROM spaces s, instance_settings st
 WHERE s.kind = 'common' AND st.id = 1 AND st.common_space_enabled`).Scan(&commonID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reading the common space: %w", err)
	}
	if commonID != nil {
		if err := enrol(ctx, tx, *commonID, userID); err != nil {
			return err
		}
	}
	return nil
}

// createPersonal creates the user's "Personal" unless it exists.
func createPersonal(ctx context.Context, tx Execer, userID uuid.UUID) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM spaces WHERE owner_id = $1 AND kind = 'personal')`, userID).Scan(&exists); err != nil {
		return fmt.Errorf("reading the personal space: %w", err)
	}
	if exists {
		return nil
	}
	spaceID, err := insertSpace(ctx, tx, "personal", userID, "Personal")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'owner')`, spaceID, userID); err != nil {
		return fmt.Errorf("adding the owner of the personal space: %w", err)
	}
	return nil
}

// enrol makes the user a participant of the space with the source "from
// the space" on its `space` projects (INV-7). It is a membership
// operation of the space, so it first takes the space's row lock (K-7):
// a concurrent visibility switch of a common project
// either finishes before — and this reads the switched visibility — or
// waits. The caller's transaction is retried on a serialization conflict.
func enrol(ctx context.Context, tx Execer, spaceID, userID uuid.UUID) error {
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM spaces WHERE id = $1 FOR UPDATE`, spaceID).Scan(&locked); err != nil {
		return fmt.Errorf("locking space %s: %w", spaceID, err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'member')
ON CONFLICT (space_id, user_id) DO NOTHING`, spaceID, userID); err != nil {
		return fmt.Errorf("joining space %s: %w", spaceID, err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO project_members (project_id, user_id, role, added_at, added_by, via_manual, via_space)
SELECT p.id, $2, 'member', now(), $2, false, true
  FROM projects p
 WHERE p.space_id = $1 AND p.visibility = 'space'
ON CONFLICT (project_id, user_id) DO UPDATE SET via_space = true`, spaceID, userID); err != nil {
		return fmt.Errorf("adding the space source in space %s: %w", spaceID, err)
	}
	return nil
}

// EnsureCommonSpace creates the common space when the flag is on, there is
// none and an active admin exists to own it; otherwise it does nothing
// (startup checks refuse a configuration in which no admin can appear,
// config/startup_checks.go#PostMigration). db opens the transaction.
//
// Two replicas starting together may race: the loser's transaction fails on
// the uniqueness of the common space or on serialisation, and is retried —
// the retry sees the space and does nothing.
func EnsureCommonSpace(ctx context.Context, db interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = ensureCommonSpaceOnce(ctx, db)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "23505" && pgErr.Code != "40001" && pgErr.Code != "40P01") {
			return err
		}
	}
	return err
}

func ensureCommonSpaceOnce(ctx context.Context, db interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}) error {
	return pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx pgx.Tx) error {
		var enabled, exists bool
		if err := tx.QueryRow(ctx, `
SELECT (SELECT common_space_enabled FROM instance_settings WHERE id = 1),
       EXISTS (SELECT 1 FROM spaces WHERE kind = 'common')`).Scan(&enabled, &exists); err != nil {
			return fmt.Errorf("reading the common space state: %w", err)
		}
		if !enabled || exists {
			return nil
		}
		var owner uuid.UUID
		err := tx.QueryRow(ctx, `
SELECT id FROM users
 WHERE role = 'admin' AND deactivated_at IS NULL AND deleted_at IS NULL AND NOT is_external
 ORDER BY created_at, id LIMIT 1`).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // the first admin will create it
		}
		if err != nil {
			return fmt.Errorf("reading the earliest active admin: %w", err)
		}
		spaceID, err := insertSpace(ctx, tx, "common", owner, "Common")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, role)
SELECT $1, u.id, CASE WHEN u.id = $2 THEN 'owner' ELSE 'member' END
  FROM users u
 WHERE NOT u.is_external AND u.deleted_at IS NULL`, spaceID, owner); err != nil {
			return fmt.Errorf("adding the participants of the common space: %w", err)
		}
		return nil
	})
}

// insertSpace inserts a space with a fresh short id (K-20: 8 characters of
// lowercase base32), drawing again on the rare collision.
func insertSpace(ctx context.Context, tx Execer, kind string, owner uuid.UUID, name string) (uuid.UUID, error) {
	for attempt := 0; attempt < 8; attempt++ {
		short, err := newShortID()
		if err != nil {
			return uuid.Nil, err
		}
		var id uuid.UUID
		err = tx.QueryRow(ctx, `
INSERT INTO spaces (short_id, kind, owner_id, name) VALUES ($1, $2, $3, $4)
ON CONFLICT (short_id) DO NOTHING
RETURNING id`, short, kind, owner, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the short id is taken
		}
		if err != nil {
			return uuid.Nil, fmt.Errorf("creating the %s space: %w", kind, err)
		}
		return id, nil
	}
	return uuid.Nil, fmt.Errorf("creating the %s space: no free short id after 8 draws", kind)
}

const shortIDAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// newShortID draws a space short id (K-20). The alphabet has 32 symbols, so
// a byte modulo 32 is uniform.
func newShortID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("drawing a short id: %w", err)
	}
	for i := range b {
		b[i] = shortIDAlphabet[int(b[i])%len(shortIDAlphabet)]
	}
	return string(b), nil
}
