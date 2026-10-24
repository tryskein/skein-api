package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// sk-285.5.5 — GET /api/v1/users/directory became a bounded, searchable
// listing. This file covers the two halves separately:
//
//   - `limit` parsing / validation — pure UNIT, no database. The handler
//     validates before it touches the store, so a handler built over a nil
//     store is enough to prove the rejection path, and proving it that way
//     also proves the rejection happens BEFORE any query is issued.
//   - search + paging semantics — INTEGRATION against the real schema
//     (TEST_DATABASE_URL); see the harness note in CLAUDE.md.

// ---------------- unit: limit parsing / validation ----------------

// newDirectoryUnitHandler builds a handler whose store and pool are nil.
// Any code path that reaches the database panics, which is the point: the
// tests below assert on responses that must be produced without one.
func newDirectoryUnitHandler() *UsersHandler {
	return NewUsersHandler(nil, nil, nil)
}

// TestListDirectory_LimitRejectedBeforeStore — UNIT. A non-integer or
// out-of-range `limit` is a 400 `invalid_limit`, not a silent clamp: a
// client that asks for 500 rows and receives 100 with no signal cannot
// tell the difference between "that's everyone" and "that's the cap". The
// nil store guarantees the rejection precedes the query.
func TestListDirectory_LimitRejectedBeforeStore(t *testing.T) {
	h := newDirectoryUnitHandler()
	callerID := uuid.Must(uuid.NewRandom())

	cases := []struct {
		name  string
		query string
	}{
		{"not a number", "limit=abc"},
		{"empty-ish whitespace", "limit=%20"},
		{"float", "limit=1.5"},
		{"zero", "limit=0"},
		{"negative", "limit=-1"},
		{"one above the ceiling", fmt.Sprintf("limit=%d", users.DirectoryMaxLimit+1)},
		{"absurd", "limit=1000000"},
		{"overflows int64", "limit=99999999999999999999"},
		{"leading plus is still not accepted as a page size we asked for", "limit=+0"},
		{"hex", "limit=0x10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := callListDirectory(t, h, callerID, "user", tc.query)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", res.StatusCode, string(body))
			}
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatalf("decode body: %v (body=%s)", err, string(body))
			}
			if got, _ := raw["error"].(string); got != "invalid_limit" {
				t.Errorf("error code = %q, want %q (body=%s)", got, "invalid_limit", string(body))
			}
		})
	}
}

// TestListDirectory_OverLongSearchRejectedBeforeStore — UNIT, sk-285.5.5
// review F6. `q` reaches a LIKE pattern that is evaluated per row, on a
// query whose short-term branch is deliberately not index-served, and any
// authenticated caller can send it. An over-long term is refused in the
// same shape as an out-of-range `limit` (400 + a stable error code), never
// truncated: results for a term the caller did not send are worse than a
// refusal. The nil store proves the rejection precedes the query.
func TestListDirectory_OverLongSearchRejectedBeforeStore(t *testing.T) {
	h := newDirectoryUnitHandler()
	callerID := uuid.Must(uuid.NewRandom())

	cases := []struct {
		name string
		q    string
	}{
		{"one rune over the ceiling", strings.Repeat("a", users.DirectorySearchMaxRunes+1)},
		{"kilobytes", strings.Repeat("a", 40_000)},
		{"multibyte over the ceiling", strings.Repeat("\u0449", users.DirectorySearchMaxRunes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := callListDirectory(t, h, callerID, "user", "q="+url.QueryEscape(tc.q))
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", res.StatusCode, string(body))
			}
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatalf("decode body: %v (body=%s)", err, string(body))
			}
			if got, _ := raw["error"].(string); got != "invalid_search" {
				t.Errorf("error code = %q, want %q (body=%s)", got, "invalid_search", string(body))
			}
		})
	}
}

// TestListDirectory_SearchAtCeilingReachesStore — UNIT. The boundary is
// INCLUSIVE: a term of exactly DirectorySearchMaxRunes runes is accepted,
// so the validator has to get past it and hit the (nil) store. The panic
// that follows is the proof — the alternative reading, "400 for everything
// long-ish", would leave the ceiling untested from below.
func TestListDirectory_SearchAtCeilingReachesStore(t *testing.T) {
	h := newDirectoryUnitHandler()
	callerID := uuid.Must(uuid.NewRandom())
	defer func() {
		if recover() == nil {
			t.Errorf("a term of exactly %d runes was rejected before the store; the ceiling must be inclusive",
				users.DirectorySearchMaxRunes)
		}
	}()
	_, _ = callListDirectory(t, h, callerID, "user",
		"q="+strings.Repeat("a", users.DirectorySearchMaxRunes))
}

// ---------------- integration: search + paging ----------------

// newDirectoryHandler wires a real users.Store over the test pool.
func newDirectoryHandler(t *testing.T, pool *pgxpool.Pool) *UsersHandler {
	t.Helper()
	return NewUsersHandler(pool,
		users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil),
		projects.NewStore(pool))
}

// directoryToken returns a random lowercase token usable as a username
// fragment (the users CHECK allows ^[a-z0-9][a-z0-9_-]{2,29}$). Every
// fixture in a test shares one token so a `q=` search isolates that test's
// rows from everything else in the shared test database.
func directoryToken() string {
	return "d" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String(), "-", "")[:12]
}

// seedDirectoryUser inserts one active user with an explicit username and
// display name, cleaned up at the end of the test.
func seedDirectoryUser(t *testing.T, pool *pgxpool.Pool, username, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'unused', now(), '{}'::jsonb)`,
		id, username+"@test.local", username, name); err != nil {
		t.Fatalf("seed directory user %q: %v", username, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// directoryViewer seeds a viewer who shares one regular space with exactly
// the given users: by INV-9 (sk-674.18 T027) the directory shows him those
// people and himself — the universe of a test is bounded by a space, as the
// production rule bounds it. The viewer's username carries no directory
// token, so a token search never matches him.
func directoryViewer(t *testing.T, pool *pgxpool.Pool, ids []uuid.UUID) uuid.UUID {
	t.Helper()
	viewer := seedDirectoryUser(t, pool, "dv"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""), "Directory Viewer")
	space := testsupport.SeedSpace(t, pool, viewer)
	for _, id := range ids {
		testsupport.AddSpaceMember(t, pool, space, id)
	}
	return viewer
}

// TestListDirectory_SearchMatchesUsernameAndName — INTEGRATION. `q` is a
// case-insensitive substring match over BOTH username and display name,
// and a row matching neither stays out.
func TestListDirectory_SearchMatchesUsernameAndName(t *testing.T) {
	pool := newUsersTestPool(t)
	h := newDirectoryHandler(t, pool)

	tok := directoryToken()
	byUsername := tok + "user"
	// byName's USERNAME deliberately carries no trace of the token — the
	// only thing that can pull it into the result is the display-name arm
	// of the search.
	byName := directoryToken() + "nm"
	unrelated := directoryToken() + "other"

	ids := []uuid.UUID{
		seedDirectoryUser(t, pool, byUsername, "Someone Else"),
		seedDirectoryUser(t, pool, byName, "Contains "+strings.ToUpper(tok)+" In Name"),
		seedDirectoryUser(t, pool, unrelated, "Unrelated Person"),
	}
	// INV-9 (sk-674.18 T027, row 206): the caller sees the fixtures through
	// a common space — a viewer whose username carries no token.
	callerID := directoryViewer(t, pool, ids)

	// Lowercase term, and one of the two fixtures carries the token
	// UPPERCASED in its display name — proves the match is case-insensitive
	// on both columns.
	res, body := callListDirectory(t, h, callerID, "user", "q="+tok)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", res.StatusCode, string(body))
	}
	got := directoryUsernames(t, body)
	if _, ok := got[byUsername]; !ok {
		t.Errorf("username match %q missing (body=%s)", byUsername, string(body))
	}
	if _, ok := got[byName]; !ok {
		t.Errorf("display-name match %q missing (body=%s)", byName, string(body))
	}
	if _, ok := got[unrelated]; ok {
		t.Errorf("non-matching user %q leaked into a filtered page", unrelated)
	}

	// The same term uppercased must return the same set.
	_, body = callListDirectory(t, h, callerID, "user", "q="+strings.ToUpper(tok))
	upper := directoryUsernames(t, body)
	if _, ok := upper[byUsername]; !ok {
		t.Errorf("uppercase search lost the username match %q (body=%s)", byUsername, string(body))
	}
	if _, ok := upper[byName]; !ok {
		t.Errorf("uppercase search lost the display-name match %q (body=%s)", byName, string(body))
	}
}

// TestListDirectory_LimitAndHasMore — INTEGRATION. The page is cut at
// `limit` and `hasMore` reports whether the cut hid anything. This is the
// completeness signal the endpoint gained together with the bound: a
// truncated page that claims to be complete is the failure mode being
// tested against.
func TestListDirectory_LimitAndHasMore(t *testing.T) {
	pool := newUsersTestPool(t)
	h := newDirectoryHandler(t, pool)

	tok := directoryToken()
	const seeded = 5
	var ids []uuid.UUID
	for i := 0; i < seeded; i++ {
		ids = append(ids, seedDirectoryUser(t, pool, fmt.Sprintf("%s%02d", tok, i), fmt.Sprintf("Person %02d", i)))
	}
	// INV-9 (sk-674.18 T027, row 206): the caller sees the fixtures through
	// a common space — a viewer whose username carries no token.
	callerID := directoryViewer(t, pool, ids)

	_, body := callListDirectory(t, h, callerID, "user", "q="+tok+"&limit=2")
	page := decodeDirectory(t, body)
	if len(page.Items) != 2 {
		t.Fatalf("limit=2 returned %d items, want 2 (body=%s)", len(page.Items), string(body))
	}
	if !page.HasMore {
		t.Errorf("limit=2 over %d matches: hasMore = false, want true (body=%s)", seeded, string(body))
	}

	_, body = callListDirectory(t, h, callerID, "user", "q="+tok+"&limit="+fmt.Sprint(seeded))
	page = decodeDirectory(t, body)
	if len(page.Items) != seeded {
		t.Fatalf("limit=%d returned %d items, want %d (body=%s)",
			seeded, len(page.Items), seeded, string(body))
	}
	if page.HasMore {
		t.Errorf("limit=%d over exactly %d matches: hasMore = true, want false (body=%s)",
			seeded, seeded, string(body))
	}

	// Boundary values of the validator are ACCEPTED (the rejection test
	// above only proves what is refused). 1 and the ceiling must both 200.
	for _, q := range []string{"limit=1", fmt.Sprintf("limit=%d", users.DirectoryMaxLimit)} {
		res, body := callListDirectory(t, h, callerID, "user", "q="+tok+"&"+q)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (body=%s)", q, res.StatusCode, string(body))
		}
	}
}

// TestListDirectory_DefaultLimitApplies — INTEGRATION. With NO limit
// parameter the page is capped at users.DirectoryDefaultLimit and says so.
// This is the regression guard for the actual defect: before sk-285.5.5 a
// parameterless call returned every active user (tens of thousands of rows,
// a multi-megabyte body).
func TestListDirectory_DefaultLimitApplies(t *testing.T) {
	pool := newUsersTestPool(t)
	h := newDirectoryHandler(t, pool)

	tok := directoryToken()
	over := users.DirectoryDefaultLimit + 1
	var ids []uuid.UUID
	for i := 0; i < over; i++ {
		ids = append(ids, seedDirectoryUser(t, pool, fmt.Sprintf("%s%03d", tok, i), fmt.Sprintf("Person %03d", i)))
	}
	// INV-9 (sk-674.18 T027, row 206): the caller sees the fixtures through
	// a common space — a viewer whose username carries no token.
	callerID := directoryViewer(t, pool, ids)

	_, body := callListDirectory(t, h, callerID, "user", "q="+tok)
	page := decodeDirectory(t, body)
	if len(page.Items) != users.DirectoryDefaultLimit {
		t.Fatalf("no limit param over %d matches returned %d items, want the default %d (body length=%d)",
			over, len(page.Items), users.DirectoryDefaultLimit, len(body))
	}
	if !page.HasMore {
		t.Errorf("no limit param over %d matches: hasMore = false, want true", over)
	}
}

// TestListDirectory_ExactUsernameRanksFirst — INTEGRATION. With the result
// set cut at `limit`, a purely alphabetical order can push the exact match
// off the page: "a<tok>" sorts before "<tok>". The rank puts the exact
// username first regardless.
func TestListDirectory_ExactUsernameRanksFirst(t *testing.T) {
	pool := newUsersTestPool(t)
	h := newDirectoryHandler(t, pool)

	tok := directoryToken()
	ids := []uuid.UUID{
		seedDirectoryUser(t, pool, "a"+tok, "Alphabetically First"),
		seedDirectoryUser(t, pool, tok, "The Exact Match"),
		seedDirectoryUser(t, pool, tok+"z", "Prefix Match"),
	}
	// INV-9 (sk-674.18 T027, row 206): the caller sees the fixtures through
	// a common space — a viewer whose username carries no token.
	callerID := directoryViewer(t, pool, ids)

	// limit=1 — the exact match is the ONLY row that may come back.
	_, body := callListDirectory(t, h, callerID, "user", "q="+tok+"&limit=1")
	page := decodeDirectory(t, body)
	if len(page.Items) != 1 {
		t.Fatalf("limit=1 returned %d items (body=%s)", len(page.Items), string(body))
	}
	if page.Items[0].Username != tok {
		t.Errorf("top hit = %q, want the exact match %q (body=%s)",
			page.Items[0].Username, tok, string(body))
	}
	if !page.HasMore {
		t.Errorf("limit=1 over 3 matches: hasMore = false, want true")
	}

	// Full page: exact, then the username-prefix hit, then the rest.
	_, body = callListDirectory(t, h, callerID, "user", "q="+tok)
	page = decodeDirectory(t, body)
	if len(page.Items) < 3 {
		t.Fatalf("expected all 3 fixtures, got %d (body=%s)", len(page.Items), string(body))
	}
	want := []string{tok, tok + "z", "a" + tok}
	for i, w := range want {
		if page.Items[i].Username != w {
			t.Fatalf("rank position %d = %q, want %q (order=%v)",
				i, page.Items[i].Username, w, usernameSlice(page.Items))
		}
	}
}

// TestListDirectory_SearchWildcardsAreLiteral — INTEGRATION. `%` and `_`
// typed into the search box match THEMSELVES. Without escaping, `%` would
// degrade the search to "everyone" — i.e. silently restore the unbounded
// behaviour this change removed, one keystroke at a time.
func TestListDirectory_SearchWildcardsAreLiteral(t *testing.T) {
	pool := newUsersTestPool(t)
	h := newDirectoryHandler(t, pool)

	tok := directoryToken()
	// Usernames cannot hold '%' (CHECK); display names can.
	ids := []uuid.UUID{
		seedDirectoryUser(t, pool, tok+"plain", "Plain "+tok),
		seedDirectoryUser(t, pool, tok+"pct", "Percent 100% "+tok),
		// '_' IS legal in a username, so the underscore arm can assert on both.
		seedDirectoryUser(t, pool, tok+"_us", "Underscored "+tok),
	}
	// INV-9 (sk-674.18 T027, row 206): the caller sees the fixtures through
	// a common space — a viewer whose username carries no token.
	callerID := directoryViewer(t, pool, ids)

	// A bare '%' must not match the plain fixture.
	_, body := callListDirectory(t, h, callerID, "user", "q=%25")
	for _, it := range decodeDirectory(t, body).Items {
		if it.Username == tok+"plain" {
			t.Fatalf("q=%% matched a row with no literal percent (%q) — wildcard leaked", it.Username)
		}
	}

	// '%' as a literal finds only the fixture that really contains one.
	_, body = callListDirectory(t, h, callerID, "user", "q=100%25")
	got := directoryUsernames(t, body)
	if _, ok := got[tok+"pct"]; !ok {
		t.Errorf("literal '100%%' search missed %q (body=%s)", tok+"pct", string(body))
	}
	if _, ok := got[tok+"plain"]; ok {
		t.Errorf("literal '100%%' search matched %q", tok+"plain")
	}

	// '_' is a single-character wildcard in LIKE; escaped it matches only
	// the underscored username.
	_, body = callListDirectory(t, h, callerID, "user", "q="+tok+"_")
	got = directoryUsernames(t, body)
	if _, ok := got[tok+"_us"]; !ok {
		t.Errorf("literal underscore search missed %q (body=%s)", tok+"_us", string(body))
	}
	if _, ok := got[tok+"plain"]; ok {
		t.Errorf("underscore behaved as a wildcard: matched %q", tok+"plain")
	}
}

// TestListDirectory_ScopedRespectsLimitAndSearch — INTEGRATION, sk-163 S6
// path. A directory narrowed to the people a viewer may see (INV-9, one
// rule for every caller since sk-674.18 T027) must be bounded and
// searchable too; otherwise a viewer of a large space keeps exactly the
// unbounded payload sk-285.5.5 took away.
func TestListDirectory_ScopedRespectsLimitAndSearch(t *testing.T) {
	pool := newUsersTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)

	tok := directoryToken()
	ids := make([]uuid.UUID, 0, 4)
	for i := 0; i < 4; i++ {
		ids = append(ids, seedDirectoryUser(t,
			pool, fmt.Sprintf("%s%02d", tok, i), fmt.Sprintf("Scoped %02d", i)))
	}
	outsider := seedDirectoryUser(t, pool, tok+"out", "Outside The Scope")
	viewer := directoryViewer(t, pool, ids)

	ctx := context.Background()

	// Limit applies.
	res, err := store.ListDirectory(ctx, viewer, users.DirectoryInput{Limit: 2})
	if err != nil {
		t.Fatalf("scoped directory: %v", err)
	}
	if len(res.Items) != 2 || !res.HasMore {
		t.Errorf("scoped limit=2 over 4 allowed: items=%d hasMore=%v, want 2/true",
			len(res.Items), res.HasMore)
	}

	// Search applies — and never reaches outside the allowed set.
	res, err = store.ListDirectory(ctx, viewer, users.DirectoryInput{Search: tok})
	if err != nil {
		t.Fatalf("scoped directory search: %v", err)
	}
	if len(res.Items) != len(ids) {
		t.Errorf("scoped search over the token returned %d items, want %d: %v",
			len(res.Items), len(ids), usernameSlice(res.Items))
	}
	for _, it := range res.Items {
		if it.ID == outsider {
			t.Errorf("scoped search leaked a user outside the allowed set (%q)", it.Username)
		}
	}

	// A viewer who shares no space with anyone sees himself only, not the
	// whole directory.
	alone := seedDirectoryUser(t, pool, "dv"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""), "Alone")
	res, err = store.ListDirectory(ctx, alone, users.DirectoryInput{})
	if err != nil {
		t.Fatalf("directory of a viewer without spaces: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != alone || res.HasMore {
		t.Errorf("a viewer without spaces: items=%v hasMore=%v, want himself only", usernameSlice(res.Items), res.HasMore)
	}
}

func usernameSlice(items []users.UserDirectoryEntry) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Username)
	}
	return out
}

// TestListDirectory_ShortTermMatchesByPrefix — INTEGRATION, sk-285.5.5
// review F5. A term shorter than users.DirectoryTrigramMinLen matches by
// PREFIX; from that length up it matches by SUBSTRING. The two arms are
// asserted against the SAME fixtures so the contrast is the assertion: the
// row that only contains the term in the middle is absent from the short
// search and present in the long one.
//
// Exercised through the store with a viewer who shares one space with the
// fixtures (directoryViewer) rather than the REST handler on purpose. A one-
// or two-character search over a shared test database matches an unbounded
// number of unrelated rows, and the page is cut at `limit` — the fixtures
// could legitimately fall off it. The viewer's space bounds the universe to
// this test's own rows (INV-9).
func TestListDirectory_ShortTermMatchesByPrefix(t *testing.T) {
	pool := newUsersTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)

	tok := directoryToken()
	probe := tok[:2]  // 2 runes → prefix branch
	longer := tok[:6] // 6 runes → substring branch

	// Starts with the probe (and with `longer`) in its USERNAME.
	prefixHit := tok + "aa"
	// Contains both terms, but not at position 0 — the discriminator.
	middleHit := "z" + tok + "bb"
	// Username shares nothing; its DISPLAY NAME starts with the probe.
	nameHit := directoryToken() + "nm"

	ids := []uuid.UUID{
		seedDirectoryUser(t, pool, prefixHit, "Prefix Match"),
		seedDirectoryUser(t, pool, middleHit, "Middle Match"),
		seedDirectoryUser(t, pool, nameHit, tok+" Person"),
	}

	viewer := directoryViewer(t, pool, ids)
	ctx := context.Background()

	short, err := store.ListDirectory(ctx, viewer, users.DirectoryInput{Search: probe})
	if err != nil {
		t.Fatalf("short-term directory search: %v", err)
	}
	shortSet := map[string]bool{}
	for _, it := range short.Items {
		shortSet[it.Username] = true
	}
	if !shortSet[prefixHit] {
		t.Errorf("short term %q missed the username-prefix row %q (got %v)",
			probe, prefixHit, usernameSlice(short.Items))
	}
	if !shortSet[nameHit] {
		t.Errorf("short term %q missed the display-name-prefix row %q (got %v)",
			probe, nameHit, usernameSlice(short.Items))
	}
	if shortSet[middleHit] {
		t.Errorf("short term %q matched %q, which only CONTAINS it — short terms must be prefix-only "+
			"(that is the whole point: a mid-string match cannot be served by an index here)",
			probe, middleHit)
	}

	long, err := store.ListDirectory(ctx, viewer, users.DirectoryInput{Search: longer})
	if err != nil {
		t.Fatalf("substring directory search: %v", err)
	}
	longSet := map[string]bool{}
	for _, it := range long.Items {
		longSet[it.Username] = true
	}
	for _, want := range []string{prefixHit, middleHit, nameHit} {
		if !longSet[want] {
			t.Errorf("term %q (>= %d runes) missed %q — from the threshold up the match is SUBSTRING (got %v)",
				longer, users.DirectoryTrigramMinLen, want, usernameSlice(long.Items))
		}
	}
}

// TestListDirectory_ShortTermRanksUsernameHitsFirst — INTEGRATION. The
// prefix branch keeps the ranking INTENT of the substring branch: username
// matches ahead of display-name-only matches, username ASC inside each
// bucket. With the page cut at `limit`, an unranked prefix search can drop
// the row the reader is typing towards. (There is no exact-username tier
// here and there cannot be one: a username is at least three characters
// and this branch only runs below that.)
func TestListDirectory_ShortTermRanksUsernameHitsFirst(t *testing.T) {
	pool := newUsersTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)

	tok := directoryToken()
	probe := tok[:2]
	// Two username-prefix hits, plus one row reachable ONLY through its
	// display name. The name-only row must rank last however its username
	// sorts — hence a username starting with 'a', which sorts before the
	// token-prefixed pair.
	nameOnly := "a" + directoryToken()[1:] + "nm"
	ids := []uuid.UUID{
		seedDirectoryUser(t, pool, tok+"zz", "Later"),
		seedDirectoryUser(t, pool, tok+"aa", "Earlier"),
		seedDirectoryUser(t, pool, nameOnly, tok+" By Name"),
	}

	res, err := store.ListDirectory(context.Background(), directoryViewer(t, pool, ids),
		users.DirectoryInput{Search: probe})
	if err != nil {
		t.Fatalf("short-term directory search: %v", err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("expected all three fixtures, got %v", usernameSlice(res.Items))
	}
	want := []string{tok + "aa", tok + "zz", nameOnly}
	for i, w := range want {
		if res.Items[i].Username != w {
			t.Fatalf("prefix branch order = %v, want %v", usernameSlice(res.Items), want)
		}
	}
}
