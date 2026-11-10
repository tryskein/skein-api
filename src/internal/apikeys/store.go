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
	"github.com/jackc/pgx/v5/pgconn"
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
// includes the plain key — with its grant and the names of its scope
// members, for the owner's own list.
type APIKey struct {
	ID         uuid.UUID
	Label      string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	Grant      Grant
	// ReissuedFromID is the key this one was reissued from.
	ReissuedFromID *uuid.UUID
	// EffectiveAdmin is the admin flag as it applies now: set on the key
	// and the owner an internal instance admin.
	EffectiveAdmin bool
	// ProjectNames and SpaceNames name the scope members.
	ProjectNames map[uuid.UUID]string
	SpaceNames   map[uuid.UUID]string
	// SpaceShortIDs gives each scope space its short id, the form every
	// surface identifies a space by.
	SpaceShortIDs map[uuid.UUID]string
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

// NewKey is what Create stores: the owner, the label, the (prefix, hash)
// pair from internal/auth.GenerateAPIKey and the grant. Grant.ExpiresAt is
// the requested end of the lifetime; nil gives the policy default.
type NewKey struct {
	UserID uuid.UUID
	Label  string
	Prefix string
	Hash   string
	Grant  Grant
}

// Created is the server-side part of a new key row.
type Created struct {
	ID        uuid.UUID
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Create inserts a new API key with its grant and scope members. Caller is
// responsible for generating (plain, prefix, hash) via
// internal/auth.GenerateAPIKey before calling.
//
// The grant must pass Grant.Validate. The lifetime follows policy: the
// requested expiry must be after now and no later than now + policy.Max;
// without one the key lives policy.Default. Every new key has an expiry.
// A scope member that does not exist gives ErrScopeUnavailable.
//
// On a prefix collision (partial UNIQUE INDEX violation; effectively
// never — 12 base64-url chars ≈ 71 bits) we return the raw pgx error
// (errors.As → *pgconn.PgError, Code "23505"). The handler is expected
// to detect this and retry with a fresh (plain, prefix, hash) tuple
// rather than surface 500 to the user. Other DB errors propagate
// wrapped in a fmt.Errorf for context.
func (s *Store) Create(ctx context.Context, k NewKey, policy TTLPolicy) (Created, error) {
	g := k.Grant.normalized()
	if err := g.Validate(); err != nil {
		return Created{}, err
	}
	now := time.Now().UTC()
	expires, err := policy.expiry(now, g.ExpiresAt)
	if err != nil {
		return Created{}, err
	}
	c := Created{ID: uuid.New(), CreatedAt: now, ExpiresAt: expires}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at,
                      level, is_admin, scope_mode, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`,
			c.ID, k.UserID, k.Hash, k.Prefix, k.Label, c.CreatedAt,
			levelColumn(g.Level), g.Admin, string(g.Scope.Mode), c.ExpiresAt); err != nil {
			return fmt.Errorf("inserting api key: %w", err)
		}
		return insertScope(ctx, tx, c.ID, g.Scope)
	})
	if err != nil {
		return Created{}, err
	}
	return c, nil
}

// levelColumn maps LevelNone to SQL NULL.
func levelColumn(l Level) any {
	if l == LevelNone {
		return nil
	}
	return string(l)
}

// insertScope writes the scope members of key id. A member that does not
// exist (foreign key violation) gives ErrScopeUnavailable.
func insertScope(ctx context.Context, tx pgx.Tx, id uuid.UUID, sc Scope) error {
	var err error
	switch sc.Mode {
	case ScopeProjects:
		_, err = tx.Exec(ctx, `
INSERT INTO api_key_scope_projects (api_key_id, project_id)
SELECT $1, unnest($2::uuid[])`, id, sc.Projects)
	case ScopeSpaces:
		_, err = tx.Exec(ctx, `
INSERT INTO api_key_scope_spaces (api_key_id, space_id)
SELECT $1, unnest($2::uuid[])`, id, sc.Spaces)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrScopeUnavailable
	}
	if err != nil {
		return fmt.Errorf("writing api key scope: %w", err)
	}
	return nil
}

// Key state errors of Narrow and Reissue.
var (
	ErrRevoked = errors.New("api key is revoked")
	ErrExpired = errors.New("api key has expired")
)

// lockedKey is the row Narrow and Reissue read under FOR UPDATE.
type lockedKey struct {
	label     string
	grant     Grant
	revokedAt *time.Time
}

// lockOwnKey reads the key id of userID under a row lock, with its grant.
// ErrNotFound when the user has no such key.
func lockOwnKey(ctx context.Context, tx pgx.Tx, userID, id uuid.UUID) (lockedKey, error) {
	var k lockedKey
	var level *string
	var mode string
	err := tx.QueryRow(ctx, `
SELECT label, level, is_admin, scope_mode, expires_at, revoked_at,
       ARRAY(SELECT project_id FROM api_key_scope_projects WHERE api_key_id = k.id),
       ARRAY(SELECT space_id FROM api_key_scope_spaces WHERE api_key_id = k.id)
  FROM api_keys k
 WHERE id = $1 AND user_id = $2
   FOR UPDATE`, id, userID).Scan(&k.label, &level, &k.grant.Admin, &mode, &k.grant.ExpiresAt,
		&k.revokedAt, &k.grant.Scope.Projects, &k.grant.Scope.Spaces)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedKey{}, ErrNotFound
	}
	if err != nil {
		return lockedKey{}, fmt.Errorf("reading api key %s: %w", id, err)
	}
	if level != nil {
		k.grant.Level = Level(*level)
	}
	k.grant.Scope.Mode = ScopeMode(mode)
	k.grant = k.grant.normalized()
	return k, nil
}

// Narrow replaces the grant of the user's key id with next, which may only
// narrow it (CheckNarrowing): level, admin flag, scope and expiry. The new
// expiry, when set, must be in the future. A revoked key gives ErrRevoked,
// an expired one ErrExpired — neither comes back to life. A refused change
// leaves the key as it was. ErrNotFound when the user has no such key.
func (s *Store) Narrow(ctx context.Context, userID, id uuid.UUID, next Grant) error {
	next = next.normalized()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockOwnKey(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if cur.revokedAt != nil {
			return ErrRevoked
		}
		if cur.grant.ExpiresAt != nil && !cur.grant.ExpiresAt.After(now) {
			return ErrExpired
		}
		if next.ExpiresAt != nil && !next.ExpiresAt.After(now) {
			return fmt.Errorf("%w: the expiry is not in the future", ErrInvalidExpiry)
		}
		spaceOf, err := projectSpaces(ctx, tx, cur.grant.Scope, next.Scope)
		if err != nil {
			return err
		}
		if err := CheckNarrowing(cur.grant, next, spaceOf); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE api_keys
   SET level = $2, is_admin = $3, scope_mode = $4, expires_at = $5
 WHERE id = $1`, id, levelColumn(next.Level), next.Admin, string(next.Scope.Mode), next.ExpiresAt); err != nil {
			return fmt.Errorf("narrowing api key %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM api_key_scope_projects WHERE api_key_id = $1`, id); err != nil {
			return fmt.Errorf("clearing api key scope: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM api_key_scope_spaces WHERE api_key_id = $1`, id); err != nil {
			return fmt.Errorf("clearing api key scope: %w", err)
		}
		return insertScope(ctx, tx, id, next.Scope)
	})
}

// projectSpaces reads the space of each project of next when cur is a set of
// spaces and next a set of projects — the one case CheckNarrowing needs it.
func projectSpaces(ctx context.Context, tx pgx.Tx, cur, next Scope) (map[uuid.UUID]uuid.UUID, error) {
	if cur.Mode != ScopeSpaces || next.Mode != ScopeProjects {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, space_id FROM projects WHERE id = ANY($1)`, next.Projects)
	if err != nil {
		return nil, fmt.Errorf("reading project spaces: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]uuid.UUID, len(next.Projects))
	for rows.Next() {
		var p, sp uuid.UUID
		if err := rows.Scan(&p, &sp); err != nil {
			return nil, fmt.Errorf("scanning project space: %w", err)
		}
		out[p] = sp
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating project spaces: %w", err)
	}
	return out, nil
}

// Reissue creates a new key that replaces the user's key id: a new row and a
// new secret (prefix, hash) with the same label, level, admin flag and scope,
// and a lifetime of policy.Default from now — also when the old key had no
// expiry. The old key is left as it is and works until its own expiry or a
// revocation. A revoked key gives ErrRevoked; an expired one may be reissued
// and stays expired. ErrNotFound when the user has no such key. A prefix
// collision is returned raw, as in Create.
func (s *Store) Reissue(ctx context.Context, userID, id uuid.UUID, prefix, hash string, policy TTLPolicy) (Created, error) {
	var c Created
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		old, err := lockOwnKey(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if old.revokedAt != nil {
			return ErrRevoked
		}
		now := time.Now().UTC()
		c = Created{ID: uuid.New(), CreatedAt: now, ExpiresAt: now.Add(policy.Default)}
		g := old.grant
		if _, err := tx.Exec(ctx, `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at,
                      level, is_admin, scope_mode, expires_at, reissued_from_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`,
			c.ID, userID, hash, prefix, old.label, c.CreatedAt,
			levelColumn(g.Level), g.Admin, string(g.Scope.Mode), c.ExpiresAt, id); err != nil {
			return fmt.Errorf("inserting reissued api key: %w", err)
		}
		return insertScope(ctx, tx, c.ID, g.Scope)
	})
	if err != nil {
		return Created{}, err
	}
	return c, nil
}

// ListByUser returns every key (active, expired and revoked) belonging to
// userID, newest-first, with its grant and the names of its scope members.
// Revoked keys stay in the list so the UI can show them muted.
func (s *Store) ListByUser(ctx context.Context, userID uuid.UUID) ([]APIKey, error) {
	return s.listOwn(ctx, userID, nil)
}

// GetByUser returns the user's key id in the ListByUser shape; ErrNotFound
// when the user has no such key.
func (s *Store) GetByUser(ctx context.Context, userID, id uuid.UUID) (APIKey, error) {
	keys, err := s.listOwn(ctx, userID, &id)
	if err != nil {
		return APIKey{}, err
	}
	if len(keys) == 0 {
		return APIKey{}, ErrNotFound
	}
	return keys[0], nil
}

func (s *Store) listOwn(ctx context.Context, userID uuid.UUID, only *uuid.UUID) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
SELECT k.id, k.label, k.prefix, k.created_at, k.last_used_at, k.revoked_at,
       k.level, k.is_admin, k.scope_mode, k.expires_at, k.reissued_from_id,
       u.role = 'admin' AND NOT u.is_external,
       ARRAY(SELECT p.id FROM api_key_scope_projects sp JOIN projects p ON p.id = sp.project_id
              WHERE sp.api_key_id = k.id ORDER BY p.id),
       ARRAY(SELECT p.name FROM api_key_scope_projects sp JOIN projects p ON p.id = sp.project_id
              WHERE sp.api_key_id = k.id ORDER BY p.id),
       ARRAY(SELECT sx.id FROM api_key_scope_spaces ss JOIN spaces sx ON sx.id = ss.space_id
              WHERE ss.api_key_id = k.id ORDER BY sx.id),
       ARRAY(SELECT sx.name FROM api_key_scope_spaces ss JOIN spaces sx ON sx.id = ss.space_id
              WHERE ss.api_key_id = k.id ORDER BY sx.id),
       ARRAY(SELECT sx.short_id FROM api_key_scope_spaces ss JOIN spaces sx ON sx.id = ss.space_id
              WHERE ss.api_key_id = k.id ORDER BY sx.id)
  FROM api_keys k
  JOIN users u ON u.id = k.user_id
 WHERE k.user_id = $1
   AND ($2::uuid IS NULL OR k.id = $2)
 ORDER BY k.created_at DESC
`, userID, only)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer rows.Close()

	out := make([]APIKey, 0, 8)
	for rows.Next() {
		var k APIKey
		var level *string
		var mode string
		var ownerAdmin bool
		var projectNames, spaceNames, spaceShortIDs []string
		if err := rows.Scan(&k.ID, &k.Label, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt,
			&level, &k.Grant.Admin, &mode, &k.Grant.ExpiresAt, &k.ReissuedFromID, &ownerAdmin,
			&k.Grant.Scope.Projects, &projectNames, &k.Grant.Scope.Spaces, &spaceNames, &spaceShortIDs); err != nil {
			return nil, fmt.Errorf("scanning api key: %w", err)
		}
		if level != nil {
			k.Grant.Level = Level(*level)
		}
		k.Grant.Scope.Mode = ScopeMode(mode)
		k.ProjectNames = make(map[uuid.UUID]string, len(projectNames))
		for i, id := range k.Grant.Scope.Projects {
			k.ProjectNames[id] = projectNames[i]
		}
		k.SpaceNames = make(map[uuid.UUID]string, len(spaceNames))
		k.SpaceShortIDs = make(map[uuid.UUID]string, len(spaceShortIDs))
		for i, id := range k.Grant.Scope.Spaces {
			k.SpaceNames[id] = spaceNames[i]
			k.SpaceShortIDs[id] = spaceShortIDs[i]
		}
		k.Grant = k.Grant.normalized()
		k.EffectiveAdmin = k.Grant.Admin && ownerAdmin
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

// Candidate is a stored key whose prefix matches a presented key: its id and
// the bcrypt hash to verify the presented key against.
type Candidate struct {
	KeyID   uuid.UUID
	KeyHash string
}

// CandidatesByPrefix returns every key stored with this prefix, whatever its
// state — active, revoked or expired, its owner active or not — the active
// one first. Authentication verifies the presented key against them and only
// then reads the matching key's Status, so a revoked or expired key and an
// inactive owner are told apart from an unknown key only to a caller that
// holds the key itself. The prefix is unique among active keys; a revoked key
// may share it, hence a slice. Empty slice, nil error, when no key has it.
func (s *Store) CandidatesByPrefix(ctx context.Context, prefix string) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, key_hash
  FROM api_keys
 WHERE prefix = $1
 ORDER BY revoked_at IS NOT NULL, created_at DESC`, prefix)
	if err != nil {
		return nil, fmt.Errorf("listing api keys by prefix: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Candidate, error) {
		var c Candidate
		err := row.Scan(&c.KeyID, &c.KeyHash)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading api keys by prefix: %w", err)
	}
	return out, nil
}

// KeyStatus is what a request authenticated by a key may act as, read
// fresh on every request (sk-674.18 K-16): the key's grant and state and its
// owner's role, external flag and activity.
type KeyStatus struct {
	KeyID      uuid.UUID
	UserID     uuid.UUID
	UserRole   string
	IsExternal bool
	Grant      Grant
	RevokedAt  *time.Time
	// OwnerActive is false once the owner is deactivated or deleted.
	OwnerActive bool
	// EffectiveAdmin is the admin flag of the key as it applies now: set on
	// the key and the owner an internal instance admin.
	EffectiveAdmin bool
}

// Revoked reports whether the key was revoked.
func (st KeyStatus) Revoked() bool { return st.RevokedAt != nil }

// Expired reports whether the key's lifetime ended at or before now.
func (st KeyStatus) Expired(now time.Time) bool {
	return st.Grant.ExpiresAt != nil && !st.Grant.ExpiresAt.After(now)
}

// Usable reports whether the key authenticates at now: not revoked, not
// expired, its owner active.
func (st KeyStatus) Usable(now time.Time) bool {
	return !st.Revoked() && !st.Expired(now) && st.OwnerActive
}

// Status reads, by the key's primary key, the key's grant, expiry and
// revocation and its owner's role, external flag and activity (sk-674.18
// K-16, INV-26). One indexed read per request: the middleware's cache maps a
// plain key to its id only, so a revocation, a deactivation, a deletion, a
// role change or a narrowed grant takes effect on the next request on every
// replica. A revoked or expired key and an inactive owner are reported, not
// hidden; ErrNotFound only when no key has this id.
func (s *Store) Status(ctx context.Context, keyID uuid.UUID) (KeyStatus, error) {
	st := KeyStatus{KeyID: keyID}
	var level *string
	var mode string
	err := s.pool.QueryRow(ctx, `
SELECT k.user_id, u.role, u.is_external,
       u.deactivated_at IS NULL AND u.deleted_at IS NULL,
       k.level, k.is_admin, k.scope_mode, k.expires_at, k.revoked_at,
       ARRAY(SELECT project_id FROM api_key_scope_projects WHERE api_key_id = k.id),
       ARRAY(SELECT space_id FROM api_key_scope_spaces WHERE api_key_id = k.id)
  FROM api_keys k
  JOIN users u ON u.id = k.user_id
 WHERE k.id = $1`, keyID).Scan(&st.UserID, &st.UserRole, &st.IsExternal, &st.OwnerActive,
		&level, &st.Grant.Admin, &mode, &st.Grant.ExpiresAt, &st.RevokedAt,
		&st.Grant.Scope.Projects, &st.Grant.Scope.Spaces)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyStatus{}, ErrNotFound
	}
	if err != nil {
		return KeyStatus{}, fmt.Errorf("reading the status of api key %s: %w", keyID, err)
	}
	if level != nil {
		st.Grant.Level = Level(*level)
	}
	st.Grant.Scope.Mode = ScopeMode(mode)
	st.Grant = st.Grant.normalized()
	st.EffectiveAdmin = st.Grant.Admin && st.UserRole == "admin" && !st.IsExternal
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
