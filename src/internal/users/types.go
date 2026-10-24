// Package users implements the admin-driven user management surface for
// R3.1: create / list / get / update / deactivate / reactivate plus the
// self-service password change. Read-only surfaces (login, /me, whoami)
// continue to live in internal/auth and internal/handler — this package
// owns the admin write paths plus the soft-deactivation cascade.
//
// All store methods take context.Context first and accept a pgx.Tx-like
// interface where atomicity matters (Deactivate cascades into
// apikeys.RevokeAllForUser in the same transaction; UpdateUser /
// Deactivate run a SERIALIZABLE re-check of count(active admins) so a
// concurrent admin demotion can never leave the system with zero admins).
//
// The wire-shape DTO never includes password_hash, deactivated metadata
// other than the timestamp, or the keycloak subject — handler-side
// echo of any of those would be a regression.
package users

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"skein-api/internal/person"
)

// UserDTO is the wire shape returned by GET /api/v1/users/{id} and by the
// MCP tool family. Mirrors handler.userResponse plus the deactivated_at
// column added in migration 000011. Field set is closed: every key here
// is documented and serialised to JSON. Adding a new key is a contract
// change.
type UserDTO struct {
	ID            uuid.UUID  `json:"id"`
	Email         string     `json:"email"`
	Username      string     `json:"username"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastLoginAt   *time.Time `json:"lastLoginAt,omitempty"`
	DeactivatedAt *time.Time `json:"deactivatedAt,omitempty"`

	// IsExternal flags the account as an external collaborator (sk-163).
	// Surfaced on the admin user list / create response so the frontend can
	// render the "external" badge + filter (S8). External accounts are
	// permanently capped at role='user' (spec §5); the flag is set only at
	// create time — there is no conversion/update path in this iteration.
	IsExternal bool `json:"isExternal"`

	// The person marks (sk-674.18 T027, spaces/contract.go): a deleted
	// account is marked deleted with its pseudonym.
	person.Status

	// AuthMethod is a derived view of the user's auth-method state. We
	// never leak the raw `password_hash` or `keycloak_sub` columns — the
	// UI only needs to know HOW the account can authenticate, not the
	// underlying secret.
	//
	//   "password" — local password set; no SSO link
	//   "keycloak" — Keycloak SSO subject linked; no local password
	//   "both"     — both linked (e.g. SSO user who also set a fallback
	//                local password, rare but supported)
	//
	// Empty string is impossible — the users_auth_method CHECK on the
	// table forbids the all-null state.
	AuthMethod string `json:"authMethod"`
}

// CreateUserInput is the validated input shape for Store.CreateUser. The
// store enforces shape constraints (email regex, password min 8) but the
// handler is expected to canonicalise (trim/lowercase email) before
// calling.
type CreateUserInput struct {
	Email    string
	Username string
	Name     string
	Password string // plaintext; store hashes internally with cfg.BcryptCost.
	Role     string // "admin" | "user"

	// IsExternal marks the account as an external collaborator (sk-163).
	// Defaults to false, so existing callers (the POST /users handler and
	// skein_user(action="create") MCP tool) that do not yet set it create
	// internal
	// users exactly as before — admin-facing wiring of this field is S8.
	//
	// S5 uses it only for the reverse anti-takeover guard in CreateUser:
	// an external account may not be created for an email already bound to
	// a Keycloak SSO identity (keycloak_sub IS NOT NULL). The broader
	// never-admin/never-owner authz for external users is S3.
	IsExternal bool

	// SpaceIDs are the spaces (short ids) the new account joins at once —
	// the admin's checkboxes (sk-674.18 T037, US-6). nil = the default: the
	// common space, if any (INV-22); non-nil (even empty) = exactly these.
	// The "Personal" is created either way (not external).
	SpaceIDs []string
}

// UpdateUserInput is the partial-patch shape for Store.UpdateUser. Nil
// fields are skipped. Setting Password to a non-nil pointer rotates the
// hash; setting Role triggers the SERIALIZABLE last-admin recheck.
type UpdateUserInput struct {
	Name     *string
	Role     *string
	Password *string
}

// ListUsersInput is the cursor-paginated filter set for Store.ListUsers.
// Archived = false (default) returns only active users (deactivated_at
// IS NULL); Archived = true returns only deactivated; Archived = "all"
// returns both. The semantics mirror the projects list endpoint.
type ListUsersInput struct {
	Archived string // "false" (default) | "true" | "all"
	Search   string // ILIKE on email/username/name

	// External, when non-nil, filters on the is_external flag: true →
	// external collaborators only, false → internal users only. Nil (the
	// default) applies no filter — both classes are returned. sk-163 S8:
	// the admin user-list "External only" toggle drives this server-side so
	// the filter is correct across cursor pages, not just the first page.
	External *bool

	CursorTime *time.Time
	CursorID   *uuid.UUID
	Limit      int
}

// ListUsersResult is the cursor-paginated wrapper. NextCursor is non-nil
// only when HasMore is true.
type ListUsersResult struct {
	Items      []UserDTO `json:"items"`
	NextCursor *string   `json:"nextCursor,omitempty"`
	HasMore    bool      `json:"hasMore"`
}

// UserDirectoryEntry is the lean wire shape returned by
// GET /api/v1/users/directory — id + username + display name only.
//
// Unlike UserDTO it carries no email / role / auth-method / timestamps:
// the directory is readable by ANY authenticated user (it powers
// assignee pickers — e.g. the cross-project "All projects" Tasks
// surface where there is no single project membership to source from),
// and usernames + display names are already public across the product
// (mentions, the assignee column, the watchers list), whereas email and
// role stay admin-only. Adding a key here widens that public surface —
// do it deliberately.
type UserDirectoryEntry struct {
	ID       uuid.UUID `json:"id"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
	// The person marks (sk-674.18 T027, spaces/contract.go): no context
	// here, so only a deleted account is marked.
	person.Status
}

// Directory paging bounds (sk-285.5.5). The values deliberately mirror the
// sibling admin listing on the SAME resource (Store.ListUsers: default 50,
// hard ceiling 100) rather than inventing a third pair — every bounded
// listing in this codebase caps at 100 (`skein_list` too), and the
// directory has no reason to be the outlier. 50 rows comfortably fill the
// assignee chip row + its "+N" overflow menu; anything past that is a
// search, not a scroll.
const (
	DirectoryDefaultLimit = 50
	DirectoryMaxLimit     = 100
)

// DirectoryTrigramMinLen is the shortest search term pg_trgm can serve
// from the GIN indexes added in migration 000037 — a pattern shorter than
// three characters contains no complete trigram, so `extractQuery` finds
// nothing to look up and the planner falls back to a full scan of `users`
// (measured on a test dataset, 39 782 active rows, PG18, the full
// statement including the ranking CASE: `q="ad"` as `ILIKE '%ad%'` →
// Seq Scan, shared hit=2910, 23.18 ms; the same term as `LIKE 'ad%'` over
// the migration-000038 indexes → BitmapOr, 13 buffers, 0.12 ms).
//
// That is not an edge case: 1–2 characters is what a typeahead sees on the
// FIRST keystrokes of every single search, so the unservable shape is the
// COMMON one. Terms this short therefore take a PREFIX branch instead of
// the substring one — see Store.listDirectory. Prefix is also the better
// answer for the reader: with one or two characters typed, "starts with"
// is what a person means, and "contains" would drown them in noise.
const DirectoryTrigramMinLen = 3

// DirectorySearchMaxRunes caps the `q` term. Any authenticated caller can
// hit the directory, and an unbounded term is an unbounded LIKE pattern:
// pattern matching is O(pattern x row) and a term this side of three
// characters is deliberately NOT index-served, so the cost of one request
// is bounded only by what the caller chose to send. 128 runes is far past
// anything real — `username` is CHECK-constrained to at most 30 characters
// (000001_init) and no display name approaches it — while keeping the
// per-request work trivially bounded. Counted in RUNES, not bytes, so a
// Cyrillic or CJK term is judged by what the reader typed.
const DirectorySearchMaxRunes = 128

// DirectoryInput is the filter set for Store.ListDirectory.
//
// Search is a case-insensitive match applied to username AND display name;
// empty (or blank) means "no filter" — the first page in plain username
// order. The match MODE depends on the term's length: SUBSTRING from
// DirectoryTrigramMinLen characters up, PREFIX below it (see
// DirectoryTrigramMinLen and Store.listDirectory for why). Terms longer
// than DirectorySearchMaxRunes are rejected by the REST handler; the store
// does not truncate them, because returning results for a term the caller
// did not send would be worse than refusing. Limit <= 0 falls back to
// DirectoryDefaultLimit and
// anything above DirectoryMaxLimit is clamped; the REST handler rejects
// out-of-range values outright (400 invalid_limit) so a client never
// silently gets a different page size than it asked for, and the clamp
// here is the store-level backstop for non-REST callers.
type DirectoryInput struct {
	Search string
	Limit  int
}

// DirectoryResult is the wire envelope of GET /api/v1/users/directory.
//
// sk-285.5.5 — the endpoint used to return a BARE ARRAY of every active
// user. It now returns a bounded page, and a bounded page without a
// completeness flag is a silent truncation: a client cannot tell "these
// are all the people" from "these are the first 50 of 39 782". HasMore is
// that flag, computed with the limit+1 probe-row trick used by
// ListUsersResult, so no COUNT(*) is paid. `len(Items)` is the "how many
// came back" half of the signal.
//
// The envelope is the shape every other bounded listing in this service
// already uses (ListUsersResult, the issues list, search); it is NOT
// wire-compatible with the previous bare array — see the handler comment.
type DirectoryResult struct {
	Items   []UserDirectoryEntry `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

// ClampDirectoryLimit resolves a requested directory page size to the one
// actually used: <= 0 (i.e. "unset") becomes DirectoryDefaultLimit, and
// anything above DirectoryMaxLimit is capped there.
//
// Exported and separated from the query builder so the default/ceiling
// rule is testable without a database — the SQL around it is not.
func ClampDirectoryLimit(n int) int {
	if n < 1 {
		return DirectoryDefaultLimit
	}
	if n > DirectoryMaxLimit {
		return DirectoryMaxLimit
	}
	return n
}

// DirectorySearchTooLong reports whether a raw `q` term exceeds
// DirectorySearchMaxRunes. Leading/trailing whitespace is ignored — it is
// stripped before the term reaches the query builder, so it must not
// decide the verdict either.
//
// A separate predicate rather than an inline length test in the handler so
// the rule is testable without a database and cannot drift between the
// REST handler and any future caller — the same reason
// ClampDirectoryLimit is exported.
func DirectorySearchTooLong(q string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(q)) > DirectorySearchMaxRunes
}

// DirectorySearchIsPrefix reports whether a search term takes the PREFIX
// branch of Store.listDirectory instead of the substring one: true when
// the trimmed term is shorter than DirectoryTrigramMinLen characters, i.e.
// exactly when pg_trgm cannot serve it. Empty terms are not searches at
// all and report false.
//
// Exported and separated from the query builder for the same reason as
// ClampDirectoryLimit: the threshold rule is testable without a database,
// the SQL around it is not.
func DirectorySearchIsPrefix(q string) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(q))
	return n > 0 && n < DirectoryTrigramMinLen
}

// Actor describes who triggered the event. Mirrors projects.Actor and
// issues.Actor — every event-writing package in skein-api uses the same
// shape so audit rows have a uniform attribution.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	Role      string // 'human' | 'agent' | 'system'
	SessionID string // optional, agent only
}

// HumanActor is the constructor used by REST handlers (admin acting on
// behalf of themselves). Username is required — events.actor is NOT
// NULL.
func HumanActor(userID uuid.UUID, username string) Actor {
	return Actor{UserID: userID, Username: username, Role: "human"}
}

// AgentActor is the constructor used by MCP tools (admin acting through
// an MCP session). The SessionID flows through to events.actor_session_id
// so audit rows can be filtered per-agent.
func AgentActor(userID uuid.UUID, username, sessionID string) Actor {
	return Actor{UserID: userID, Username: username, Role: "agent", SessionID: sessionID}
}
