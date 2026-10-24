// Package apikeys persists long-lived API keys (R2.1) and exposes the
// lookup primitives the MCP middleware needs to authenticate /mcp
// requests.
//
// The plain key is shown to the user exactly once in the create
// response and is never persisted — the store sees only (prefix, hash)
// pairs. See internal/auth/apikey.go for the format and the
// generation/verification helpers.
package apikeys

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a key id (or active prefix) does not match
// any row. Handlers map this to 404; the MCP middleware folds it into a
// generic 401 (we never tell the caller why their key didn't authenticate).
var ErrNotFound = errors.New("api key not found")

// touchThrottle gates last_used_at UPDATEs so we don't write a row on
// every single MCP request. A stale value within a minute is fine for
// "when was this key last used" UI; bursts of requests don't cause N
// UPDATE statements per minute. See TouchLastUsed for the SQL guard.
const touchThrottle = 1 * time.Minute

// Store wraps the pgxpool with the queries we run from the api-keys
// handler and middleware. Build once at startup; reuse across requests.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store bound to pool. Caller owns pool lifetime.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// APIKey is the audit-safe shape of a row — never includes key_hash, never
// includes the plain key. Handlers serialize this directly to JSON.
type APIKey struct {
	ID         uuid.UUID  `json:"id"`
	Label      string     `json:"label"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

// LookupResult is what the MCP middleware needs to authorize a request:
// the api_keys row id (for audit + last_used_at touch), the user's id and
// role (for project-visibility checks), and the bcrypt hash to compare
// against. All in one query so the request path is one round-trip plus
// the bcrypt verify.
type LookupResult struct {
	KeyID      uuid.UUID
	UserID     uuid.UUID
	UserRole   string
	IsExternal bool
	KeyHash    string
}

// Create inserts a new API key. Caller is responsible for generating
// (plain, prefix, hash) via internal/auth.GenerateAPIKey before calling.
//
// The id is generated here (UUIDv7-style is overkill for a low-volume
// table — plain v4 is fine) so handlers don't have to.
//
// On a prefix collision (partial UNIQUE INDEX violation; effectively
// never — 12 base64-url chars ≈ 71 bits) we return the raw pgx error
// (errors.As → *pgconn.PgError, Code "23505"). The handler is expected
// to detect this and retry with a fresh (plain, prefix, hash) tuple
// rather than surface 500 to the user. Other DB errors propagate
// verbatim wrapped in a fmt.Errorf for context.
func (s *Store) Create(ctx context.Context, userID uuid.UUID, label, prefix, hash string) (uuid.UUID, time.Time, error) {
	id := uuid.New()
	createdAt := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
`,
		id, userID, hash, prefix, label, createdAt)
	if err != nil {
		return uuid.Nil, time.Time{}, fmt.Errorf("inserting api key: %w", err)
	}
	return id, createdAt, nil
}

// ListByUser returns every key (active + revoked) belonging to userID,
// newest-first. Revoked keys stay in the list so the UI can show "you
// revoked X keys this week" without a separate query — the frontend
// styles them muted via the revokedAt field.
func (s *Store) ListByUser(ctx context.Context, userID uuid.UUID) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, label, prefix, created_at, last_used_at, revoked_at
  FROM api_keys
 WHERE user_id = $1
 ORDER BY created_at DESC
`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer rows.Close()

	out := make([]APIKey, 0, 8)
	for rows.Next() {
		var k APIKey
		var lastUsed, revokedAt *time.Time
		if err := rows.Scan(&k.ID, &k.Label, &k.Prefix, &k.CreatedAt, &lastUsed, &revokedAt); err != nil {
			return nil, fmt.Errorf("scanning api key: %w", err)
		}
		k.LastUsedAt = lastUsed
		k.RevokedAt = revokedAt
		k.CreatedAt = k.CreatedAt.UTC()
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating api keys: %w", err)
	}
	return out, nil
}

// Revoke marks the key as revoked (soft-delete via revoked_at). userID
// scopes the UPDATE so a user cannot revoke another user's key by id —
// the row simply won't match. ErrNotFound when no active row matches.
//
// Revoking a revoked key is a no-op (idempotent): the WHERE clause
// includes revoked_at IS NULL so the second call just returns ErrNotFound.
// Handlers map ErrNotFound on revoke to 404 — the user already revoked it.
func (s *Store) Revoke(ctx context.Context, userID, id uuid.UUID) error {
	cmd, err := s.pool.Exec(ctx, `
UPDATE api_keys
   SET revoked_at = now()
 WHERE id = $1
   AND user_id = $2
   AND revoked_at IS NULL
`, id, userID)
	if err != nil {
		return fmt.Errorf("revoking api key: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeAllForUser is the cascade hook for users.Store.Deactivate. It
// soft-revokes every active key belonging to userID by setting
// revoked_at = now() on rows where revoked_at IS NULL. Returns the number
// of rows affected so callers can log it.
//
// Accepts a pgx.Tx so the cascade runs in the SAME transaction as the
// user UPDATE — if the deactivation commits, the revocations commit; if
// it rolls back, the keys stay live. Calling against the pool would lose
// that atomicity guarantee.
//
// Reactivation does NOT un-revoke keys (they need to be re-issued by the
// user); that's intentional per R3.1 plan Q-4 — audit trail is preserved
// and a previously-revoked key never re-arms.
func (s *Store) RevokeAllForUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int64, error) {
	cmd, err := tx.Exec(ctx, `
UPDATE api_keys
   SET revoked_at = now()
 WHERE user_id = $1
   AND revoked_at IS NULL
`, userID)
	if err != nil {
		return 0, fmt.Errorf("revoking api keys for user %s: %w", userID, err)
	}
	return cmd.RowsAffected(), nil
}

// LookupByPrefix returns the (key_id, user_id, role, is_external, hash)
// tuple for the active key whose stored prefix exactly matches `prefix`.
// JOIN onto users so the middleware doesn't need a second query for role /
// is_external (sk-163 S1: the surface-gate needs the flag on the hot path).
//
// ErrNotFound when no active row matches — the middleware folds this and
// "wrong key entirely" into the same 401. We do NOT distinguish the
// two: either way, the supplied credential failed to authenticate.
//
// R3.1 W2 — defence-in-depth: filter out deactivated users at the JOIN.
// users.Store.Deactivate already cascades RevokeAllForUser in the same tx,
// so post-commit every key for a deactivated user has revoked_at set; this
// extra clause closes the failure mode if anything ever leaves an
// unrevoked key on a deactivated user (a missed cascade, a manual SQL
// fix that resets deactivated_at without re-issuing keys, etc). The
// partial index idx_users_active makes the cost essentially free.
func (s *Store) LookupByPrefix(ctx context.Context, prefix string) (LookupResult, error) {
	var r LookupResult
	row := s.pool.QueryRow(ctx, `
SELECT k.id, k.user_id, u.role, u.is_external, k.key_hash
  FROM api_keys k
  JOIN users u ON u.id = k.user_id
 WHERE k.prefix = $1
   AND k.revoked_at IS NULL
   AND u.deactivated_at IS NULL
   AND u.deleted_at IS NULL
`, prefix)
	if err := row.Scan(&r.KeyID, &r.UserID, &r.UserRole, &r.IsExternal, &r.KeyHash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LookupResult{}, ErrNotFound
		}
		return LookupResult{}, fmt.Errorf("looking up api key by prefix: %w", err)
	}
	return r, nil
}

// KeyStatus is what a request authenticated by a key may act as, read
// fresh on every request (sk-674.18 K-16).
type KeyStatus struct {
	UserID     uuid.UUID
	UserRole   string
	IsExternal bool
}

// Status reads, by the key's primary key, whether the key is still usable —
// not revoked, its user active and not deleted — and the user's role and
// external flag (sk-674.18 K-16, INV-26). One indexed read per request: the
// middleware's cache maps a plain key to its id only, so a revocation, a
// deactivation, a deletion or a role change takes effect on the next
// request on every replica. ErrNotFound when the key is not usable.
func (s *Store) Status(ctx context.Context, keyID uuid.UUID) (KeyStatus, error) {
	var st KeyStatus
	err := s.pool.QueryRow(ctx, `
SELECT k.user_id, u.role, u.is_external
  FROM api_keys k
  JOIN users u ON u.id = k.user_id
 WHERE k.id = $1
   AND k.revoked_at IS NULL
   AND u.deactivated_at IS NULL
   AND u.deleted_at IS NULL`, keyID).Scan(&st.UserID, &st.UserRole, &st.IsExternal)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyStatus{}, ErrNotFound
	}
	if err != nil {
		return KeyStatus{}, fmt.Errorf("reading the status of api key %s: %w", keyID, err)
	}
	return st, nil
}

// TouchLastUsed bumps last_used_at to now() but only when the previous
// touch was over `touchThrottle` ago (or never). The throttle lives in
// SQL (single round-trip), not in process memory, so it works correctly
// across multiple replicas without coordination.
//
// Errors are returned to the caller but the middleware should treat
// them as soft-fail (log and continue) — a missed last_used_at is fine,
// blocking the request would punish the user for an audit-only write.
func (s *Store) TouchLastUsed(ctx context.Context, id uuid.UUID) error {
	// pgx encodes time.Duration as Postgres interval (pgx v5 / pgtype.Interval).
	_, err := s.pool.Exec(ctx, `
UPDATE api_keys
   SET last_used_at = now()
 WHERE id = $1
   AND (last_used_at IS NULL OR now() - last_used_at > $2)
`, id, touchThrottle)
	if err != nil {
		return fmt.Errorf("touching api key last_used_at: %w", err)
	}
	return nil
}
