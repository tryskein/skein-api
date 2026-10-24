package monitorcreds

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Token format: skm_<43 base64url chars>.
//
// 32 bytes (256 bits) of crypto/rand entropy → base64.RawURLEncoding (no
// padding) → 43 chars. Total length 47 incl. the literal "skm_" tag. The
// tag mirrors the ska_ convention (gh*, glpat-, …) so log scrapers can spot
// a leaked monitor token at a glance, and — critically — keeps the two auth
// doors disjoint: a skm_ token is not ska_-shape, so it can never
// authenticate against /mcp (spec D7/§5). These constants are deliberately
// separate from auth.APIKey* so a future format bump on either side is a
// one-place change.
const (
	// TokenTag prefixes every plaintext monitor token.
	TokenTag = "skm_"
	// TokenRandomBytes is the entropy budget (256 bits).
	TokenRandomBytes = 32

	// MaxLifetime is the immutable absolute cap: a credential cannot be
	// renewed past created_at + MaxLifetime regardless of use. Exported so
	// config can bound the renewal window by it (sk-579, INV-1).
	MaxLifetime = 12 * time.Hour
)

// ErrInvalidMonitorToken collapses every authentication failure into one
// sentinel: bad digest, wrong audience, expired, past absolute deadline,
// explicitly revoked, minting key revoked, or owner deactivated. The
// validate-and-renew UPDATE returns zero rows for all of them, and the
// caller (the /agent/v1 middleware, T2) maps this to 401 — we never tell
// the presenter which predicate failed.
var ErrInvalidMonitorToken = errors.New("invalid monitor token")

// ErrNotFound is returned by RevokeOwned when no credential of the caller
// has that id (another user's, never existed, or already purged) — one
// error for all of them, on purpose (INV-11).
var ErrNotFound = errors.New("monitor credential not found")

// Store wraps the shared pgxpool with the monitor-credential queries.
// Build once at startup; reuse across requests and the cleanup worker.
type Store struct {
	pool *pgxpool.Pool
	// idleTTL is the sliding renewal window (sk-579, INV-1): on mint and on
	// every successful validation expires_at = LEAST(now + idleTTL,
	// absolute_deadline).
	idleTTL time.Duration
}

// New returns a Store bound to pool with the renewal window idleTTL
// (cfg.MonitorTokenIdleTTL, validated by config.Load to 0 < idleTTL <=
// MaxLifetime). Caller owns pool lifetime.
func New(pool *pgxpool.Pool, idleTTL time.Duration) *Store {
	return &Store{pool: pool, idleTTL: idleTTL}
}

// Mint generates a fresh monitor credential, persists its SHA-256 digest +
// scope, and returns the plaintext token (shown to the caller exactly once)
// alongside the audit-safe row. created_at / absolute_deadline / expires_at
// are stamped from the DB clock (now()) so they're immune to app/DB skew:
// absolute_deadline = now + 12h (immutable), expires_at = LEAST(now +
// renewal window, absolute_deadline) (sliding; INV-1). The issuing session
// and the token-file path are stored verbatim — an empty session as ”, so
// it stays distinct from the NULL of a credential minted before sk-579.
//
// The plaintext is never persisted — only its digest. There is no recovery
// path; a lost token must be re-minted.
func (s *Store) Mint(ctx context.Context, p MintParams) (string, Credential, error) {
	buf := make([]byte, TokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", Credential{}, fmt.Errorf("monitor token entropy: %w", err)
	}
	plaintext := TokenTag + base64.RawURLEncoding.EncodeToString(buf)
	digest := sha256.Sum256([]byte(plaintext))

	id := uuid.New()
	var c Credential
	err := s.pool.QueryRow(ctx, `
INSERT INTO monitor_credentials (
    id, token_digest, user_id, minted_by_api_key_id, audience, capability,
    project_id, scope_issue_id, scope_subtree, event_types,
    created_at, absolute_deadline, expires_at,
    issued_session_id, token_file_path
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10,
    now(), now() + $11::interval, LEAST(now() + $12::interval, now() + $11::interval),
    $13, $14
)
RETURNING id, user_id, minted_by_api_key_id, audience, capability,
          project_id, scope_issue_id, scope_subtree, event_types,
          created_at, absolute_deadline, expires_at`,
		id, digest[:], p.UserID, p.MintedByAPIKeyID, p.Audience, p.Capability,
		p.ProjectID, p.ScopeIssueID, p.ScopeSubtree, p.EventTypes,
		MaxLifetime, s.idleTTL,
		p.IssuedSessionID, p.TokenFilePath,
	).Scan(
		&c.ID, &c.UserID, &c.MintedByAPIKeyID, &c.Audience, &c.Capability,
		&c.ProjectID, &c.ScopeIssueID, &c.ScopeSubtree, &c.EventTypes,
		&c.CreatedAt, &c.AbsoluteDeadline, &c.ExpiresAt,
	)
	if err != nil {
		return "", Credential{}, fmt.Errorf("inserting monitor credential: %w", err)
	}
	c.CreatedAt = c.CreatedAt.UTC()
	c.AbsoluteDeadline = c.AbsoluteDeadline.UTC()
	c.ExpiresAt = c.ExpiresAt.UTC()
	return plaintext, c, nil
}

// ValidateAndRenew is the single atomic authentication step run on every
// /agent/v1 request. It validates the presented plaintext (by its SHA-256
// digest) against audience + every liveness predicate AND slides expires_at
// forward in the same UPDATE — server clock only, so there is no read-then-
// write race and no app/DB skew. Spec §5 verbatim.
//
// The WHERE clause folds in the cascade revocations via a join: the minting
// api key must still be active (ak.revoked_at IS NULL), the owner must still
// be active (u.deactivated_at IS NULL), and the credential's owner must
// match the key's owner (mc.user_id = ak.user_id, defence-in-depth). expires_at
// is re-pinned to LEAST(now + renewal window, absolute_deadline) so a renew can never
// push past the immutable 12h cap.
//
// Zero rows updated → ErrInvalidMonitorToken (every failure mode collapses
// here; the caller maps it to 401). A real DB error (timeout, conn loss) is
// wrapped and returned distinctly so the caller can map it to 5xx rather
// than masquerade as an auth failure.
func (s *Store) ValidateAndRenew(ctx context.Context, plaintext, audience string) (Claims, error) {
	digest := sha256.Sum256([]byte(plaintext))

	var c Claims
	err := s.pool.QueryRow(ctx, `
UPDATE monitor_credentials mc
   SET expires_at = LEAST(now() + $3::interval, mc.absolute_deadline),
       last_used_at = now()
  FROM api_keys ak
  JOIN users u ON u.id = ak.user_id
 WHERE mc.token_digest = $1
   AND mc.audience = $2
   AND mc.revoked_at IS NULL
   AND ak.id = mc.minted_by_api_key_id
   AND ak.revoked_at IS NULL
   AND u.deactivated_at IS NULL
   AND mc.user_id = ak.user_id
   AND now() < mc.expires_at
   AND now() < mc.absolute_deadline
RETURNING mc.id, mc.user_id, mc.project_id, mc.scope_issue_id, mc.scope_subtree, mc.event_types, mc.created_at`,
		digest[:], audience, s.idleTTL,
	).Scan(&c.ID, &c.UserID, &c.ProjectID, &c.ScopeIssueID, &c.ScopeSubtree, &c.EventTypes, &c.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Claims{}, ErrInvalidMonitorToken
		}
		return Claims{}, fmt.Errorf("validating monitor token: %w", err)
	}
	c.CreatedAt = c.CreatedAt.UTC()
	return c, nil
}

// RevokeOwned stops one monitor of userID (sk-579 US-6, INV-11): a single
// UPDATE filtered by the credential id AND its owner sets revoked_at only if
// it is still empty. It returns revoked=true when this call revoked the
// credential and false when it was already revoked (a repeat stop: success,
// revoked_at unchanged). Zero rows — another user's credential, an id that
// never existed, or a row already purged by Cleanup — all give the same
// ErrNotFound, so the caller cannot learn that someone else's monitor exists.
// The owner is the user, not the minting key: any key of the same user may
// stop it.
//
// revoked_at = now() in RETURNING tells the two success cases apart: now() is
// the transaction start, which only a revoke done by this statement can have
// written. That holds because the statement runs in its own implicit
// transaction on the pool; do not call it inside an outer pgx.Tx that may
// already have revoked the same row — a repeat there would read as fresh.
func (s *Store) RevokeOwned(ctx context.Context, id, userID uuid.UUID) (bool, error) {
	var revokedNow bool
	err := s.pool.QueryRow(ctx, `
UPDATE monitor_credentials
   SET revoked_at = COALESCE(revoked_at, now())
 WHERE id = $1
   AND user_id = $2
RETURNING revoked_at = now()`, id, userID).Scan(&revokedNow)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("revoking monitor credential %s: %w", id, err)
	}
	return revokedNow, nil
}

// Cleanup hard-deletes credentials that can never authenticate again:
// anything past its absolute deadline, plus revoked rows older than
// revokedRetention (kept that long only for short-term audit). Returns the
// number of rows deleted. Run periodically by the cleanup worker so the
// table doesn't accumulate dead short-lived tokens.
//
// A revokedRetention of 0 deletes revoked rows immediately on the next
// sweep (revoked_at < now()); negative values are nonsensical and rejected
// at config-load time.
func (s *Store) Cleanup(ctx context.Context, revokedRetention time.Duration) (int, error) {
	cmd, err := s.pool.Exec(ctx, `
DELETE FROM monitor_credentials
 WHERE now() > absolute_deadline
    OR (revoked_at IS NOT NULL AND revoked_at < now() - $1::interval)`,
		revokedRetention)
	if err != nil {
		return 0, fmt.Errorf("cleaning up monitor credentials: %w", err)
	}
	return int(cmd.RowsAffected()), nil
}
