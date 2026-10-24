package auth

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// usernameRegex (mirrored in the DB CHECK on users.username) is
//
//	^[a-z0-9][a-z0-9_-]{2,29}$
//
// i.e. lowercase alphanumeric, underscore, or hyphen; length 3..30; first
// character must be alphanumeric.
const (
	usernameMinLen = 3
	usernameMaxLen = 30
)

// UsernameFromEmail derives a DB-valid username slug from an email address
// for the bootstrap path (when SKEIN_INITIAL_ADMIN_USERNAME isn't set).
//
// Algorithm:
//  1. Take the local part (before "@") and lowercase it.
//  2. Replace every char outside [a-z0-9_-] with "-".
//  3. Trim consecutive hyphens at start/end (cosmetic — they're still legal,
//     but ugly).
//  4. If the first char isn't [a-z0-9], prefix "u".
//  5. Truncate to 30 chars.
//  6. If the result is shorter than 3 chars, append a 4-char random hex
//     suffix to push past the minimum length without colliding.
//
// The function is deterministic for a given non-degenerate email, except
// for step 6 which uses crypto/rand. That randomness is fine for bootstrap:
// the result still has to clear the DB unique index on username, but the
// degenerate inputs that hit step 6 ("a@x.com", "@x") are exactly the ones
// where a deterministic short slug would collide between deployments.
func UsernameFromEmail(email string) string {
	local := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		local = email[:at]
	}
	local = strings.ToLower(local)

	// Step 2: replace every byte not in the allowed set with '-'.
	var b strings.Builder
	b.Grow(len(local))
	for i := 0; i < len(local); i++ {
		c := local[i]
		if isUsernameByte(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()

	// Step 3: cosmetic trim of leading/trailing hyphens. We keep underscores
	// because they're legal in the regex and useful in real local-parts.
	out = strings.Trim(out, "-")

	// Step 4: ensure first char is alphanumeric.
	if out == "" || !isAlnumByte(out[0]) {
		out = "u" + out
	}

	// Step 5: truncate.
	if len(out) > usernameMaxLen {
		out = out[:usernameMaxLen]
	}

	// Step 6: pad up to minimum length with a random hex suffix.
	if len(out) < usernameMinLen {
		var buf [2]byte
		if _, err := rand.Read(buf[:]); err == nil {
			out += hex.EncodeToString(buf[:]) // 4 hex chars
		} else {
			// rand.Read on Linux only fails on very unusual conditions; fall
			// back to deterministic padding so we still produce a valid slug.
			out += "0000"
		}
		if len(out) > usernameMaxLen {
			out = out[:usernameMaxLen]
		}
	}

	return out
}

func isUsernameByte(c byte) bool {
	return isAlnumByte(c) || c == '_' || c == '-'
}

func isAlnumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
