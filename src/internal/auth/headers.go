package auth

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// Header names that flow from skein-web (BFF) into skein-api on every
// request to /api/v1/*. Centralizing them here means the BFF and the API
// can never disagree on capitalization or punctuation.
//
//   - HeaderInternalSecret authenticates the *channel* (BFF → API), not the
//     end user. Required on every /api/v1/* request.
//   - HeaderUserID/Email/Username/Role identify the *human* on whose behalf
//     the BFF is calling. Optional on auth bootstrap endpoints (login,
//     logout) — the user does not exist yet from the API's perspective —
//     and required on user-bound endpoints (RequireUser middleware).
//   - HeaderAPIKey carries the long-lived API key that authenticates the
//     /mcp endpoint (R2.1). Mutually exclusive with HeaderUserID-style auth:
//     the api-key middleware resolves user_id from the key itself and
//     stashes it in ctx, rather than trusting a forwarded header.
//   - HeaderMcpSessionID flows with /mcp requests only. Mcp-Session-Id is
//     an MCP standard (server-issued on initialize, echoed by the client
//     on subsequent calls). It is the cross-system join key for audit:
//     events.actor_session_id is populated from this header so external
//     systems that capture the agent's conversation can join
//     their logs to skein audit events on string equality.
const (
	HeaderInternalSecret = "X-Skein-Internal-Secret"
	HeaderUserID         = "X-Skein-User-Id"
	HeaderUserEmail      = "X-Skein-User-Email"
	HeaderUsername       = "X-Skein-Username"
	HeaderUserRole       = "X-Skein-User-Role"
	// HeaderUserIsExternal carries the caller's is_external flag (sk-163
	// S1). Value is the literal "true" / "false"; absent header is treated
	// as false (internal). Part of the X-Skein-User-* identity family the
	// BFF forwards from the cookie principal.
	HeaderUserIsExternal = "X-Skein-User-Is-External"
	HeaderAPIKey         = "X-Skein-Api-Key"
	HeaderMcpSessionID   = "Mcp-Session-Id"
	// HeaderClientIP carries the real client IP the BFF resolved from its
	// trusted proxy chain (sk-163 S9). skein-api keys its defense-in-depth
	// login limiter on this so the per-IP throttle reflects the real client,
	// not the single BFF egress address. Trusted only because the /api/v1/*
	// channel already passed RequireInternalSecret — a credential no internet
	// client holds — and the BFF strips any client-supplied X-Skein-* before
	// setting its own. Absent (a direct caller that did not set it) → the
	// handler falls back to the transport RemoteAddr. Must match the constant
	// SkeinHeaders.ClientIp in skein-web.
	HeaderClientIP = "X-Skein-Client-Ip"
	// HeaderSurface carries the fail-closed surface marker (sk-163 S7a,
	// spec §9). The BFF stamps the value SurfaceInternal ONLY on the
	// trusted/internal MCP path; on the public (internet) path the header is
	// ABSENT. skein-api treats ABSENCE as PUBLIC and rejects internal-owner
	// api-keys there (fail-closed). skein-api trusts this header only because
	// the /mcp route already requires RequireInternalSecret — a credential
	// no internet client holds — so the marker can only originate from the
	// BFF, which strips any client-supplied X-Skein-* before setting its own.
	HeaderSurface = "X-Skein-Surface"
	// HeaderSessionEpoch carries the session epoch of the browser session
	// on whose behalf the BFF calls (sk-674.18, K-16, INV-26). Part of the
	// X-Skein-User-* identity family: skein-web stamps it on every proxied
	// /api/v1/* request that carries a user, from the session's claims. The
	// value is a decimal integer — the users.session_epoch the session was
	// issued with, taken from the `sessionEpoch` field of the authentication
	// response (accounts/contract.go#AuthResponse) at login, signup or
	// password reset.
	//
	// Contract of the check — to be implemented in handler/middleware.go#
	// ParseUserHeaders by sk-674.18 T036; today no epoch is read or checked,
	// and skein-web will start sending the header with T048 in the same
	// release (D-11). Once implemented: on every request that carries a
	// user, skein-api will read the user by primary key and refuse with 401
	// and the body
	// `{"error":"session_revoked"}` ([CodeSessionRevoked]) when the user is
	// deactivated or deleted, or when this header is absent, not a decimal
	// integer, or differs from the stored epoch. No cache will stand in front
	// of the read, so the refusal will hold from the moment the operation
	// that raised the epoch answered, on every replica. skein-web answers
	// session_revoked by ending the session (logout), never by retrying.
	//
	// The epoch will rise (T036, T039, T041) on a password reset (every
	// session but the re-issued current one ends; API keys keep working), on
	// deactivation and on account deletion (sessions and API keys end). API-key requests (/mcp,
	// X-Skein-Api-Key) carry no epoch: a key is checked by its own status
	// and its user's.
	HeaderSessionEpoch = "X-Skein-Session-Epoch"
)

// CodeSessionRevoked is the `error` of the 401 a request gets when its
// session was revoked (see [HeaderSessionEpoch]). skein-web logs the user
// out on it.
const CodeSessionRevoked = "session_revoked"

// SurfaceInternal is the only value of HeaderSurface that asserts the
// trusted/internal surface. Any other value — including the empty string
// when the header is absent — is treated as the PUBLIC surface. Must match
// the literal the BFF stamps (skein-web SkeinHeaders.SurfaceInternal).
const SurfaceInternal = "internal"

// SurfaceIsInternal reports whether the supplied HeaderSurface value asserts
// the trusted internal surface. Comparison is trim+case-insensitive so a
// stray space or capitalisation from a proxy never silently downgrades an
// internal request to public. Absence ("") → false → public → fail-closed.
func SurfaceIsInternal(headerVal string) bool {
	return strings.EqualFold(strings.TrimSpace(headerVal), SurfaceInternal)
}

// ctxKey is an unexported named type so context values inserted by this
// package cannot collide with values from other packages — Go's
// context.WithValue contract requires a distinct key type.
type ctxKey int

const (
	ctxKeyUserID ctxKey = iota
	ctxKeyUserEmail
	ctxKeyUsername
	ctxKeyUserRole
	ctxKeyIsExternal
	ctxKeyAPIKeyID
	ctxKeyMcpSessionID
)

// WithUserID stores a parsed user UUID in ctx. Used by ParseUserHeaders
// middleware after it validates the header value.
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, id)
}

// WithUserEmail, WithUsername, WithUserRole stash the corresponding
// X-Skein-* header value in ctx for handlers that need to log it. Email is
// stored lowercased so downstream comparisons are case-insensitive.
func WithUserEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, ctxKeyUserEmail, email)
}

func WithUsername(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, ctxKeyUsername, username)
}

func WithUserRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, ctxKeyUserRole, role)
}

// WithIsExternal stashes the caller's is_external flag in ctx (sk-163 S1).
// Both transports set it: ParseUserHeaders from the X-Skein-User-Is-External
// trust-header, RequireAPIKey from the api-key lookup/cache. Consumed via
// IsExternalFrom and the centralized Principal (PrincipalFrom).
func WithIsExternal(ctx context.Context, external bool) context.Context {
	return context.WithValue(ctx, ctxKeyIsExternal, external)
}

// UserIDFrom returns the user UUID stashed in ctx by ParseUserHeaders, plus
// a boolean indicating presence. Returning the bool keeps callers from
// having to check uuid.Nil — a real (very unlikely) Nil UUID stays
// distinguishable from "no header was set".
func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(ctxKeyUserID).(uuid.UUID)
	return v, ok
}

func UserEmailFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyUserEmail).(string)
	return v, ok
}

func UsernameFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyUsername).(string)
	return v, ok
}

func UserRoleFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyUserRole).(string)
	return v, ok
}

// IsExternalFrom returns the caller's is_external flag from ctx, defaulting
// to false when no value was stashed (absent header / non-api-key path).
// Returning a bare bool rather than (bool, bool) is deliberate: "absent" and
// "explicitly false" are the same to every consumer — internal is the safe
// default, so callers never need to distinguish the two.
func IsExternalFrom(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyIsExternal).(bool)
	return v
}

// WithAPIKeyID stashes the resolved api_keys.id (UUID) in ctx. The middleware
// fills this so handlers can attribute audit events to a specific key
// (rather than just "the user"). Stored as uuid.UUID for type-safety; the
// stringification happens at log/render time.
func WithAPIKeyID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyAPIKeyID, id)
}

// APIKeyIDFrom returns the resolved api_keys.id stashed by the api-key
// middleware. The bool distinguishes "no api-key auth on this request"
// (e.g. cookie-authenticated calls into /api/v1/*) from a real Nil UUID.
func APIKeyIDFrom(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(ctxKeyAPIKeyID).(uuid.UUID)
	return v, ok
}

// WithSessionID / SessionIDFrom carry the MCP Mcp-Session-Id header value.
// On the first message of a session (the initialize request) the client
// hasn't been told its id yet — the SDK generates and returns one in the
// response header — so middleware MUST tolerate the empty string and
// only UPSERT the agents row when sessionID != "".
func WithSessionID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, ctxKeyMcpSessionID, sid)
}

// SessionIDFrom returns the MCP session id from ctx, or empty string if
// none is set. Empty string and "no session" are deliberately the same:
// every consumer (agents UPSERT, locked_by writes, events.actor_session_id)
// treats both as "skip" without distinguishing the two.
func SessionIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyMcpSessionID).(string)
	return v
}
