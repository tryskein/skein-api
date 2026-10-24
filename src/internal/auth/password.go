// Package auth implements skein-api's standalone authentication primitives:
// password hashing/verification, header parsing for the
// skein-web (BFF) → skein-api hop, and the admin bootstrap path that seeds
// the very first user from environment variables.
//
// All public symbols here are stable across the auth-related slices (R1.1,
// R5.x). Higher-level orchestration (login, /me, logout) lives in the
// handler package — this package only provides primitives.
package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// MinBcryptCost is the floor for password hashing. The project guidelines
// pin production bcrypt cost at 12; allowing values below this floor in
// non-test contexts would silently weaken every password.
const MinBcryptCost = bcrypt.DefaultCost

// MaxBcryptCost is the ceiling enforced by golang.org/x/crypto/bcrypt itself
// (cost 31). Re-exported as a constant so config validation has a single
// source of truth.
const MaxBcryptCost = bcrypt.MaxCost

// ErrPasswordMismatch is returned by Verify when the supplied plaintext
// does not match the stored bcrypt hash. Wrapping the bcrypt sentinel into
// our own error keeps callers from importing golang.org/x/crypto just to
// branch on auth failure.
var ErrPasswordMismatch = errors.New("password mismatch")

// Hash returns a bcrypt hash of plain at the given cost. The cost should
// come from config (default 12) — tests may pass a lower value (e.g. 4) to
// keep suites fast, but production code paths must use the configured
// SKEIN_BCRYPT_COST.
func Hash(plain string, cost int) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", fmt.Errorf("bcrypt hash: %w", err)
	}
	return string(b), nil
}

// Verify checks that plain matches the bcrypt hash. Returns
// ErrPasswordMismatch on a clean negative result and a wrapped error on
// any other bcrypt failure (e.g. malformed hash). Callers must treat the
// mismatch case as a generic "invalid credentials" — never echo back which
// side failed (existence vs password).
func Verify(hashed, plain string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain))
	if err == nil {
		return nil
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrPasswordMismatch
	}
	return fmt.Errorf("bcrypt compare: %w", err)
}
