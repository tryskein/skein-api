package auth

import (
	"context"
	"strings"

	"skein-api/internal/monitorcreds"
)

// Monitor-token format: skm_<43 base64url chars> = 47 chars total.
//
// The plaintext is minted by internal/monitorcreds (32 bytes of crypto/rand
// → base64.RawURLEncoding, no padding → 43 chars, prefixed with the literal
// "skm_" tag). These constants are deliberately SEPARATE from the ska_
// api-key constants in apikey.go: that disjointness is the whole point of
// the two-door auth design (spec §5 / D7). A skm_ token is NOT ska_-shape,
// so it can never authenticate against /mcp via RequireAPIKey; an ska_ key
// is NOT skm_-shape, so it can never authenticate against /agent/v1 via
// RequireMonitorToken. Keeping the format knobs here (not shared with
// apikey.go) means a future bump on either side is a one-place change and
// cannot accidentally widen the other door.
const (
	// MonitorTokenTag prefixes every plaintext monitor token. Mirrors the
	// ska_ convention so log scrapers can spot a leaked monitor token.
	MonitorTokenTag = "skm_"

	// monitorTokenRandomChars is the base64.RawURLEncoding length of
	// monitorcreds.TokenRandomBytes (32) bytes: ceil(32*4/3) = 43. A unit
	// test pins this against base64.RawURLEncoding.EncodedLen so the two
	// packages cannot silently drift.
	monitorTokenRandomChars = 43

	// MonitorTokenLen is the exact plaintext length: tag (4) + 43 = 47.
	MonitorTokenLen = len(MonitorTokenTag) + monitorTokenRandomChars
)

// IsMonitorTokenShape reports whether s has the exact shape of a monitor
// token: the "skm_" tag, the fixed-length body, AND a body that is valid
// base64url (the alphabet monitorcreds mints via base64.RawURLEncoding:
// A-Z, a-z, 0-9, '-', '_'). It is a cheap pre-filter the middleware uses to
// reject obviously-wrong input (missing header, an ska_ api key presented at
// the wrong door, garbage) before any DB round-trip. Not a security check —
// a well-formed but unknown/expired token still fails at the
// validate-and-renew step; but a body containing a non-base64url character
// can never match a minted digest, so rejecting it here short-circuits a
// needless DB hit and keeps the door's shape filter honest to its contract.
//
// The check is intentionally independent of IsAPIKeyShape: the two shapes
// are disjoint (different tag, and skm_ tokens are a fixed 47 chars while
// ska_ keys are 26), which is what makes a monitor token useless on /mcp
// and an api key useless on /agent/v1.
func IsMonitorTokenShape(s string) bool {
	if len(s) != MonitorTokenLen || !strings.HasPrefix(s, MonitorTokenTag) {
		return false
	}
	return isBase64URL(s[len(MonitorTokenTag):])
}

// isBase64URL reports whether every byte of s is in the base64url alphabet
// (A-Z, a-z, 0-9, '-', '_'). Monitor token bodies are minted with
// base64.RawURLEncoding (no padding), so a valid body contains only these
// characters; '+' / '/' (standard base64) or any other byte means the input
// is not a minted token.
func isBase64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// MaskMonitorToken renders a monitor token for safe logging. The full
// plaintext is a credential and must NEVER appear in logs; the masked form
// keeps only the tag and the last 4 chars so audit entries stay
// correlatable without leaking material usable to authenticate. Mirrors
// MaskAPIKey.
//
// Examples:
//
//	"skm_abc…xyz" (47 chars) → "skm_xxxx…wxyz"
//	""                       → "<empty>"
//	"skm_short"              → "<malformed>"
func MaskMonitorToken(plain string) string {
	if plain == "" {
		return "<empty>"
	}
	if len(plain) < len(MonitorTokenTag)+8 {
		return "<malformed>"
	}
	return MonitorTokenTag + "xxxx…" + plain[len(plain)-4:]
}

// monitorClaimsKey is a private context-key type, distinct from the ctxKey
// values in headers.go. Using a separate, unexported struct type guarantees
// monitor claims live in their own context namespace and can never be read
// via the MCP user-context helpers (UserIDFrom / APIKeyIDFrom / …) — a
// /agent/v1 request never populates the MCP identity keys, so a monitor
// token cannot impersonate an api-key-authenticated MCP caller.
type monitorClaimsKey struct{}

// WithMonitorClaims stashes the validated monitor-credential claims in ctx.
// Set by RequireMonitorToken after a successful ValidateAndRenew; read by
// the /agent/v1 handlers via MonitorClaimsFrom.
func WithMonitorClaims(ctx context.Context, claims monitorcreds.Claims) context.Context {
	return context.WithValue(ctx, monitorClaimsKey{}, claims)
}

// MonitorClaimsFrom returns the monitor claims stashed by RequireMonitorToken,
// plus a presence bool. The bool distinguishes "this request did not come
// through the monitor door" from a zero-value Claims, so handlers mounted
// only under /agent/v1 can defensively assert the middleware ran.
func MonitorClaimsFrom(ctx context.Context) (monitorcreds.Claims, bool) {
	c, ok := ctx.Value(monitorClaimsKey{}).(monitorcreds.Claims)
	return c, ok
}
