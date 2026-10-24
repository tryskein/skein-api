package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// API key format: ska_<22 base64url chars>.
//
// We use 16 bytes of crypto/rand entropy → base64.RawURLEncoding (no
// padding) → 22 chars. Total length 26 incl. the literal "ska_" tag.
// 16 bytes (128 bits) is the same entropy budget as a UUIDv4 — well past
// any realistic brute-force attack. The "ska_" prefix is for human
// recognition (similar to gh*, glpat-, etc.) and lets log scrapers spot
// leaked keys at a glance.
//
// The first 12 chars (prefix) are stored alongside the bcrypt hash so the
// middleware can do an O(1) DB lookup before the (relatively expensive)
// bcrypt verify. 12 chars covers "ska_" plus the first 8 random chars,
// which is more than enough to make collisions impossible in practice
// while still giving the audit log something distinctive to display.
//
// All these constants live together so a future format bump (e.g.
// ska2_, longer prefix) is one place to change.
const (
	APIKeyTag         = "ska_"
	APIKeyRandomBytes = 16 // → 22 base64url chars
	APIKeyPrefixLen   = 12 // first 12 chars of the plain key
)

// ErrInvalidAPIKeyFormat is returned by ExtractPrefix when the supplied
// string is too short to even contain a prefix. Real auth failures come
// from the LookupByPrefix → bcrypt path; this sentinel is for malformed
// input that we want to reject before hitting the DB.
var ErrInvalidAPIKeyFormat = errors.New("invalid api key format")

// GenerateAPIKey returns a fresh API key plus the derived prefix and
// bcrypt hash, suitable for storage. The plain string is shown to the
// user exactly once (via the create response) and never persisted.
//
// Errors:
//   - rand.Read failures (extremely rare; surfaced verbatim).
//   - bcrypt.GenerateFromPassword failures (cost out of range, etc.).
func GenerateAPIKey(bcryptCost int) (plain, prefix, hash string, err error) {
	buf := make([]byte, APIKeyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("api key entropy: %w", err)
	}
	plain = APIKeyTag + base64.RawURLEncoding.EncodeToString(buf)
	prefix = ExtractPrefix(plain)

	hashBytes, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", "", "", fmt.Errorf("bcrypt hash api key: %w", err)
	}
	return plain, prefix, string(hashBytes), nil
}

// ExtractPrefix slices off the first APIKeyPrefixLen characters of plain.
// Returns empty string for too-short input — callers should treat this as
// a malformed key and return 401 without hitting the DB. We intentionally
// don't validate the "ska_" tag here: forwarding a wrong-tag key into
// LookupByPrefix gives the same 401 outcome and means the format check
// lives in exactly one place (ParseAndAuthenticate-side, not here).
func ExtractPrefix(plain string) string {
	if len(plain) < APIKeyPrefixLen {
		return ""
	}
	return plain[:APIKeyPrefixLen]
}

// VerifyAPIKey compares the supplied plaintext against a stored bcrypt
// hash. Returns ErrPasswordMismatch on a clean negative, a wrapped error
// on malformed-hash / oversized-input cases, and nil on success. We
// reuse the password Verify primitive instead of duplicating bcrypt
// branching logic — the mismatch sentinel is shared so the middleware
// can map both password and api-key failures to the same 401.
func VerifyAPIKey(hash, plain string) error {
	return Verify(hash, plain)
}

// MaskAPIKey renders an API key for safe logging. The full plaintext is a
// credential and must NEVER appear in logs; the masked form keeps just
// enough to correlate audit entries (last 4 chars) without leaking
// material that could be used to authenticate.
//
// Examples:
//
//	"ska_abc123def456ghi789jklm" → "ska_xxxx…jklm"
//	""                            → "<empty>"
//	"too-short"                   → "<malformed>"
func MaskAPIKey(plain string) string {
	if plain == "" {
		return "<empty>"
	}
	if len(plain) < len(APIKeyTag)+8 {
		return "<malformed>"
	}
	last4 := plain[len(plain)-4:]
	return APIKeyTag + "xxxx…" + last4
}

// IsAPIKeyShape returns true when s is at least long enough and starts with
// the "ska_" tag — a cheap filter the middleware can use to short-circuit
// obviously-invalid inputs before doing a DB lookup. Not a security check;
// only a hint.
func IsAPIKeyShape(s string) bool {
	return len(s) >= len(APIKeyTag)+8 && strings.HasPrefix(s, APIKeyTag)
}
