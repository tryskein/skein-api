package accounts

// username_suggest.go — the username rule of registration and the free
// usernames offered for it (sk-674.18 T038, US-8, INV-18; screens 2.6
// "Generate from email" and the variants of `username_taken`).
//
// Taken means: the username (or, for an email, the email) of any account,
// or a reserved identity of a deleted one (reserved_identities,
// sha256(lower(value)), K-21) — deleted accounts' emails and usernames are
// never issued again. A suggestion reserves nothing: the unique index
// decides at creation.

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// UsernameRegex is the username rule of the whole installation (the CHECK
// of users.username since migration 000001).
var UsernameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,29}$`)

// pseudonymRegex is the shape of a deleted account's pseudonym (users.
// pseudonym CHECK): never a username anyone can take, so that a pseudonym
// never matches a resolvable username (INV-18).
var pseudonymRegex = regexp.MustCompile(`^deleted-[0-9a-f]{8}$`)

// usernameMaxLen / usernameMinLen bound the rule above.
const (
	usernameMaxLen = 30
	usernameMinLen = 3
)

// ValidNewUsername reports whether u (already lower-cased) may be given to
// a new account — by registration and by an admin alike: the username
// rule, and not of the pseudonym shape (INV-18).
func ValidNewUsername(u string) bool {
	return UsernameRegex.MatchString(u) && !pseudonymRegex.MatchString(u)
}

// usernameBase turns the part of email before @ into a username stem:
// lower-cased, every character outside [a-z0-9_-] becomes '-', runs of '-'
// collapse, a leading run of '_' and '-' and a trailing run of '-' go, the
// stem is cut to the length bound and padded with "user" when too short.
func usernameBase(email string) string {
	local := strings.ToLower(strings.TrimSpace(email))
	if at := strings.IndexByte(local, '@'); at >= 0 {
		local = local[:at]
	}
	return normaliseStem(local)
}

func normaliseStem(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			if !dash {
				b.WriteByte('-')
			}
			dash = true
			continue
		}
		b.WriteRune(r)
		dash = false
	}
	out := strings.TrimLeft(b.String(), "-_")
	out = strings.TrimRight(out, "-")
	if len(out) > usernameMaxLen {
		out = strings.TrimRight(out[:usernameMaxLen], "-")
	}
	if len(out) < usernameMinLen {
		out = strings.TrimRight("user"+out, "-")
		if len(out) < usernameMinLen {
			out = "user"
		}
	}
	return out
}

// domainWord is the first label of the email's domain as a stem
// ("dev@client.io" → "client"); "" when there is none.
func domainWord(email string) string {
	at := strings.IndexByte(email, '@')
	if at < 0 {
		return ""
	}
	label, _, _ := strings.Cut(strings.ToLower(email[at+1:]), ".")
	var b strings.Builder
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fit cuts base so base+suffix stays within the length bound. A suffix
// that leaves no room for at least one character of base (a long domain
// label: DNS allows 63) gives "" — no candidate, which the username rule
// then rejects like any other invalid one.
func fit(base, suffix string) string {
	if len(base)+len(suffix) <= usernameMaxLen {
		return base + suffix
	}
	keep := usernameMaxLen - len(suffix)
	if keep < 1 {
		return ""
	}
	return strings.TrimRight(base[:keep], "-_") + suffix
}

// usernameCandidates lists, best first and without duplicates, the
// candidates for stem with email's domain: the stem, stem2, stem-domain,
// stem + the domain's initial, stem3 … stem9. Every candidate obeys the
// signup rule.
func usernameCandidates(stem, email string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(c string) {
		if seen[c] || !ValidNewUsername(c) {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	add(stem)
	add(fit(stem, "2"))
	if d := domainWord(email); d != "" && d != stem {
		add(fit(stem, "-"+d))
		add(fit(stem, d[:1]))
	}
	for n := 3; n <= 9; n++ {
		add(fit(stem, strconv.Itoa(n)))
	}
	return out
}

// suggestionCount is how many free usernames an answer carries (2–3).
const suggestionCount = 3

// numberedMax bounds the numbered fallback (stem10 … stem99).
const numberedMax = 99

// randomRounds / randomBatch bound the last fallback: at most 4 batches of
// 10 random-numbered candidates, one directory query each.
const (
	randomRounds = 4
	randomBatch  = 10
)

// randomSuffixMin / randomSuffixSpan: the random numbers are 100…999999,
// above the numbered fallback.
const (
	randomSuffixMin  = 100
	randomSuffixSpan = 999900
)

// suggestFree picks up to suggestionCount free usernames for stem: the
// candidates first, then the numbered fallback, then — on an instance where
// all of those are taken — stem with random numbers (crypto/rand), in a
// bounded number of rounds; exclude is never offered (the username just
// refused as taken). Every suggestion is distinct and obeys the signup rule.
func suggestFree(ctx context.Context, ids Identities, stem, email, exclude string) ([]string, error) {
	var out []string
	offered := map[string]bool{exclude: true}
	try := func(batch []string) error {
		var probe []string
		for _, c := range batch {
			if !offered[c] {
				probe = append(probe, c)
			}
		}
		if len(probe) == 0 {
			return nil
		}
		taken, err := ids.TakenUsernames(ctx, probe)
		if err != nil {
			return err
		}
		for _, c := range probe {
			if !taken[c] && !offered[c] && len(out) < suggestionCount {
				out = append(out, c)
				offered[c] = true
			}
		}
		return nil
	}
	if err := try(usernameCandidates(stem, email)); err != nil {
		return nil, err
	}
	for n := 10; len(out) < suggestionCount && n <= numberedMax; n += 10 {
		var batch []string
		for k := n; k < n+10 && k <= numberedMax; k++ {
			if c := fit(stem, strconv.Itoa(k)); ValidNewUsername(c) {
				batch = append(batch, c)
			}
		}
		if err := try(batch); err != nil {
			return nil, err
		}
	}
	for round := 0; len(out) < suggestionCount && round < randomRounds; round++ {
		batch := make([]string, 0, randomBatch)
		for k := 0; k < randomBatch; k++ {
			n, err := rand.Int(rand.Reader, big.NewInt(randomSuffixSpan))
			if err != nil {
				return nil, fmt.Errorf("drawing a username suffix: %w", err)
			}
			if c := fit(stem, strconv.FormatInt(n.Int64()+randomSuffixMin, 10)); ValidNewUsername(c) {
				batch = append(batch, c)
			}
		}
		if err := try(batch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Identities answers which emails and usernames are taken.
type Identities interface {
	// EmailTaken reports whether email (lower-cased) belongs to an account
	// or is reserved by a deleted one.
	EmailTaken(ctx context.Context, email string) (bool, error)
	// TakenUsernames reports which of candidates (lower-cased) belong to an
	// account or are reserved by a deleted one.
	TakenUsernames(ctx context.Context, candidates []string) (map[string]bool, error)
}

// Querier is the part of *pgxpool.Pool / pgx.Tx the directory reads with.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Directory is Identities over the database.
type Directory struct {
	db Querier
}

// NewDirectory builds the directory on db.
func NewDirectory(db Querier) *Directory { return &Directory{db: db} }

// emailTakenSQL: an account with the email (idx on lower(email)) or a
// reserved email hash (the PK of reserved_identities).
const emailTakenSQL = `
SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = $1)
    OR EXISTS (SELECT 1 FROM reserved_identities
                WHERE kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))`

// EmailTaken implements Identities.
func (d *Directory) EmailTaken(ctx context.Context, email string) (bool, error) {
	return emailTakenIn(ctx, d.db, email)
}

func emailTakenIn(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, email string) (bool, error) {
	var taken bool
	if err := q.QueryRow(ctx, emailTakenSQL, email).Scan(&taken); err != nil {
		return false, fmt.Errorf("checking the email: %w", err)
	}
	return taken, nil
}

// takenUsernamesSQL: of the candidates, those an account carries
// (idx_users_username_lower) or a deleted account reserved.
const takenUsernamesSQL = `
SELECT c FROM unnest($1::text[]) AS c
 WHERE EXISTS (SELECT 1 FROM users WHERE lower(username) = c)
    OR EXISTS (SELECT 1 FROM reserved_identities
                WHERE kind = 'username' AND value_hash = sha256(convert_to(c, 'UTF8')))`

// TakenUsernames implements Identities.
func (d *Directory) TakenUsernames(ctx context.Context, candidates []string) (map[string]bool, error) {
	rows, err := d.db.Query(ctx, takenUsernamesSQL, candidates)
	if err != nil {
		return nil, fmt.Errorf("checking usernames: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("reading taken usernames: %w", err)
	}
	taken := make(map[string]bool, len(names))
	for _, n := range names {
		taken[n] = true
	}
	return taken, nil
}

// usernameTakenSQL is the single-name form for the in-transaction check.
const usernameTakenSQL = `
SELECT EXISTS (SELECT 1 FROM users WHERE lower(username) = $1)
    OR EXISTS (SELECT 1 FROM reserved_identities
                WHERE kind = 'username' AND value_hash = sha256(convert_to($1, 'UTF8')))`
