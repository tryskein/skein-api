// Package monitorcreds persists the short-lived, scoped credentials that
// back the agent task-monitor (inter-agent comms over comments, sk-142).
//
// A monitor credential is the `skm_`-prefixed opaque token a headless
// agent's poll runner presents on the separate /agent/v1 api-key door — it
// is NOT a real ska_ api key. The store sees only the SHA-256 digest of the
// plaintext; the plaintext is returned to the minting caller exactly once
// (via Mint) and is never persisted.
//
// This package owns only the credential lifecycle (mint / validate-and-
// renew / revoke / cleanup). The /agent/v1 middleware, poll endpoint, and
// the skein_get_monitor_script MCP tool live in later sk-142 tasks.
package monitorcreds

import (
	"time"

	"github.com/google/uuid"
)

// MintParams carries the scope + ownership a new monitor credential is
// minted with. The caller (the skein_get_monitor_script MCP tool, T7) is
// responsible for validating ScopeIssueID / EventTypes before calling Mint;
// the store persists them verbatim.
type MintParams struct {
	// UserID is the credential owner — the user the minting api key belongs
	// to. The renew path enforces UserID == api_keys.user_id, so a token can
	// never outlive its owner's relationship to the minting key.
	UserID uuid.UUID

	// MintedByAPIKeyID is the ska_ api key that minted this credential. FK
	// → api_keys ON DELETE CASCADE; the renew path additionally rejects a
	// soft-revoked key, giving three revocation routes (explicit revoke,
	// key revoke, user deactivation).
	MintedByAPIKeyID uuid.UUID

	// Audience scopes the token to a single use ("notifications:poll" in
	// v1). The renew path matches on it, so a token minted for one audience
	// cannot be replayed against another.
	Audience string

	// Capability is the coarse permission tag ("read" in v1). Belt-and-
	// braces alongside the read-only route set; not an enforcement point.
	Capability string

	// ProjectID is the project the monitored issue belongs to.
	ProjectID uuid.UUID

	// ScopeIssueID is the hierarchical issue id the monitor targets. Always
	// set in v1 (project-wide scope is deferred — see the migration).
	ScopeIssueID string

	// ScopeSubtree, when true, widens the poll scope from the exact issue to
	// the issue plus its (id-prefix) subtree.
	ScopeSubtree bool

	// EventTypes is the set of notification types this monitor delivers,
	// constrained at mint time to what the poll endpoint can normalise.
	EventTypes []string

	// IssuedSessionID is the Mcp-Session-Id of the session that minted the
	// monitor, stored verbatim; "" (a client without a session) is stored as
	// '', never NULL — NULL is reserved for credentials minted before sk-579.
	IssuedSessionID string

	// TokenFilePath is the server-chosen literal path the agent writes the
	// token to (the same string the bootstrap carries).
	TokenFilePath string
}

// Credential is the audit-safe shape of a monitor_credentials row. It never
// includes token_digest (the secret material) — Mint returns the plaintext
// separately, and after that the token is unrecoverable from the row.
type Credential struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	MintedByAPIKeyID uuid.UUID
	Audience         string
	Capability       string
	ProjectID        uuid.UUID
	ScopeIssueID     string
	ScopeSubtree     bool
	EventTypes       []string
	CreatedAt        time.Time
	AbsoluteDeadline time.Time
	ExpiresAt        time.Time
}

// Claims is what ValidateAndRenew returns on a successful authentication:
// the scope the poll endpoint needs to build its query. Deliberately narrow
// — it carries no secret material and no sliding-lifecycle timestamps (the
// renew already slid expires_at server-side).
//
// CreatedAt is the one timestamp it does carry: the credential's immutable
// mint time, used by the poll endpoint as a server-owned lower-bound floor
// (a "not-before"). A monitor token is live-start by design — it must never
// surface notifications that predate its mint, even if the presenter forges
// an old (since_ts, since_id) cursor. The poll clamps the effective cursor
// lower bound up to CreatedAt, so a forged epoch cursor cannot backfill
// historical in-scope notifications. See PollForMonitor.
type Claims struct {
	// ID is the credential's own id (monitor_credentials.id) — the monitorId
	// the agent sees (sk-579, US-6). The poll endpoint needs it to mark the
	// successful poll and the root-closed state on this very credential.
	ID           uuid.UUID
	UserID       uuid.UUID
	ProjectID    uuid.UUID
	ScopeIssueID string
	ScopeSubtree bool
	EventTypes   []string
	CreatedAt    time.Time
}
