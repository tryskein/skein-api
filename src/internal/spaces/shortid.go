package spaces

// shortid.go — the space short id and the address slug (sk-674.18 T023,
// K-20).
//
// The short id is 8 characters of lowercase base32 (`k3x9p2qa`), unique by
// idx_spaces_short_id; it is drawn at random and drawn again when the
// insert finds it taken. The slug is derived from the name for the address
// `/spaces/<id>-<slug>` and is never stored or looked up: a route finds the
// space by the short id alone.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mozillazg/go-unidecode"
)

const (
	// shortIDAlphabet is lowercase base32 (RFC 4648 letters and 2–7): 32
	// symbols, so a byte modulo 32 is uniform.
	shortIDAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
	// shortIDLength — K-20: 8 characters.
	shortIDLength = 8
	// shortIDDraws bounds the redraws on a collision. With 32^8 ids a
	// second collision in a row is already beyond any real installation.
	shortIDDraws = 8
	// slugMaxLength keeps the address readable; the name itself may be
	// up to 200 characters.
	slugMaxLength = 60
	// slugFallback is the slug of a name with no transliterable letter or
	// digit.
	slugFallback = "space"
)

// drawShortID reads one short id from src.
func drawShortID(src io.Reader) (string, error) {
	b := make([]byte, shortIDLength)
	if _, err := io.ReadFull(src, b); err != nil {
		return "", fmt.Errorf("drawing a short id: %w", err)
	}
	for i := range b {
		b[i] = shortIDAlphabet[int(b[i])%len(shortIDAlphabet)]
	}
	return string(b), nil
}

// withFreshShortID draws short ids from src and offers each to insert until
// one is not taken (insert reports false for a taken id) — K-20 «generation
// with a redraw on a collision». It returns the id inserted.
func withFreshShortID(src io.Reader, insert func(short string) (inserted bool, err error)) (string, error) {
	for range shortIDDraws {
		short, err := drawShortID(src)
		if err != nil {
			return "", err
		}
		ok, err := insert(short)
		if err != nil {
			return "", err
		}
		if ok {
			return short, nil
		}
	}
	return "", fmt.Errorf("spaces: no free short id after %d draws", shortIDDraws)
}

// insertRegularSpace creates a regular space owned by owner, with the owner
// as its participant (INV-10), and returns its id and short id.
func insertRegularSpace(ctx context.Context, tx pgx.Tx, owner uuid.UUID, name string) (uuid.UUID, string, error) {
	var id uuid.UUID
	short, err := withFreshShortID(rand.Reader, func(short string) (bool, error) {
		err := tx.QueryRow(ctx, `
INSERT INTO spaces (short_id, kind, owner_id, name) VALUES ($1, 'regular', $2, $3)
ON CONFLICT (short_id) DO NOTHING
RETURNING id`, short, owner, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("creating the space: %w", err)
		}
		return true, nil
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO space_members (space_id, user_id, role) VALUES ($1, $2, 'owner')`,
		id, owner); err != nil {
		return uuid.Nil, "", fmt.Errorf("adding the owner of the space: %w", err)
	}
	return id, short, nil
}

// Slug is the address slug of a space name (K-20): transliterated to ASCII
// (go-unidecode), lowercased, every run of other characters one hyphen,
// trimmed, at most 60 characters; "space" when nothing is left.
// «Личное» → "lichnoe", "Acme Corp" → "acme-corp".
func Slug(name string) string {
	ascii := strings.ToLower(unidecode.Unidecode(name))
	var b strings.Builder
	hyphen := false
	for i := 0; i < len(ascii); i++ {
		c := ascii[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			hyphen = false
			continue
		}
		if !hyphen && b.Len() > 0 {
			b.WriteByte('-')
			hyphen = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > slugMaxLength {
		out = strings.TrimRight(out[:slugMaxLength], "-")
	}
	if out == "" {
		return slugFallback
	}
	return out
}
