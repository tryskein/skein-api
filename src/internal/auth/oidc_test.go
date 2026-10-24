package auth

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// testDatabaseURL — same env var the rest of the integration suite uses.
// Tests that need a real DB skip when it's not set so `go test ./...`
// without a running stack still passes (matches the convention in
// internal/handler/auth_test.go).
const testDatabaseURL = "TEST_DATABASE_URL"

// newOIDCTestPool dials TEST_DATABASE_URL, pings, and registers cleanup.
// Mirrors handler/auth_test.go::newAuthTestPool so the two suites have
// identical skip/timeout semantics.
func newOIDCTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// setupProvisioner wires a Provisioner against the test pool with the
// supplied autoProvision flag and bootstrap-admin email. The defaultRole
// is hard-coded to "user" — bootstrap behaviour is the only branch that
// changes the role under autoProvision=true.
func setupProvisioner(t *testing.T, autoProvision bool, bootstrap string) *Provisioner {
	t.Helper()
	pool := newOIDCTestPool(t)
	return NewProvisioner(pool, autoProvision, "user", bootstrap)
}

// insertExistingUser seeds a Keycloak-only row (password_hash = NULL,
// keycloak_sub set) so we can exercise the match-and-update path. The
// row is hard-deleted by t.Cleanup to keep the schema clean between
// runs of `go test ./...` against a shared local DB.
func insertExistingUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email, sub string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	// Username MUST match users_username CHECK '^[a-z0-9][a-z0-9_-]{2,29}$';
	// strip hyphens out of the uuid prefix to be safe.
	uname := "kc" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, keycloak_sub, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', NULL, $5, now(), '{}'::jsonb)
`, id, strings.ToLower(email), uname, uname, sub)
	if err != nil {
		t.Fatalf("insert existing user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// cleanupBySub registers a hard-delete by keycloak_sub so a successful
// auto-provision INSERT doesn't pollute the shared DB.
func cleanupBySub(t *testing.T, pool *pgxpool.Pool, sub string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE keycloak_sub = $1`, sub)
	})
}

// uniqueSub returns a Keycloak-like UUID string. Using uuid.New rather
// than a static value so parallel test invocations don't trip the unique
// index on keycloak_sub.
func uniqueSub() string {
	return uuid.Must(uuid.NewRandom()).String()
}

// uniqueUsername returns a username matching the project's check
// constraint (^[a-z0-9][a-z0-9_-]{2,29}$). Avoids collisions across
// parallel test runs sharing a DB.
func uniqueUsername(prefix string) string {
	suffix := strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:10], "-", "")
	return prefix + suffix
}

// TestProvisioner_FirstLoginCreatesUser verifies the §8.5.12 happy path:
// autoProvision=true + unknown sub → INSERT, role=defaultRole,
// password_hash IS NULL, last_login_at set.
func TestProvisioner_FirstLoginCreatesUser(t *testing.T) {
	p := setupProvisioner(t, true, "")
	sub := uniqueSub()
	username := uniqueUsername("alice")
	email := username + "@example.com"
	cleanupBySub(t, p.pool, sub)

	ctx := context.Background()
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: sub, Email: email, Username: username, Name: "Alice Example",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !res.Provisioned {
		t.Fatal("expected Provisioned=true on first login")
	}
	if res.Role != "user" {
		t.Fatalf("Role = %q, want user", res.Role)
	}
	if res.Username != username {
		t.Fatalf("Username = %q, want %q", res.Username, username)
	}

	// Validate the row state directly: password_hash IS NULL,
	// last_login_at populated.
	var (
		hasPwdHash  bool
		hasLastLog  bool
		storedRole  string
		storedSub   string
		storedEmail string
	)
	err = p.pool.QueryRow(ctx, `
SELECT password_hash IS NOT NULL, last_login_at IS NOT NULL, role, keycloak_sub, email
  FROM users WHERE id = $1
`, res.UserID).Scan(&hasPwdHash, &hasLastLog, &storedRole, &storedSub, &storedEmail)
	if err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if hasPwdHash {
		t.Error("password_hash must be NULL for SSO-provisioned user")
	}
	if !hasLastLog {
		t.Error("last_login_at must be set on first login")
	}
	if storedRole != "user" {
		t.Errorf("stored role = %q, want user", storedRole)
	}
	if storedSub != sub {
		t.Errorf("stored keycloak_sub = %q, want %q", storedSub, sub)
	}
	if storedEmail != email {
		t.Errorf("stored email = %q, want %q", storedEmail, email)
	}
}

// TestProvisioner_KnownSubUpdatesLastLogin verifies the §8.5.12
// match-and-update path: an existing row keyed on sub gets last_login_at
// bumped and email/name re-synced. Provisioned must be false.
func TestProvisioner_KnownSubUpdatesLastLogin(t *testing.T) {
	p := setupProvisioner(t, true, "")
	sub := uniqueSub()
	ctx := context.Background()

	// Seed with the original email, then call Provision with a different
	// email + name to prove the sync semantics.
	origEmail := uniqueUsername("orig") + "@example.com"
	existingID := insertExistingUser(t, ctx, p.pool, origEmail, sub)

	newEmail := uniqueUsername("new") + "@example.com"
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: sub, Email: newEmail, Username: "ignored", Name: "Updated Name",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Provisioned {
		t.Fatal("expected Provisioned=false on known sub")
	}
	if res.UserID != existingID {
		t.Fatalf("UserID = %v, want %v (must update existing row, not insert)", res.UserID, existingID)
	}
	if res.Email != newEmail {
		t.Fatalf("Email = %q, want %q (must be synced from claims)", res.Email, newEmail)
	}

	// Verify in DB: name was updated, last_login_at set.
	var (
		gotName    string
		gotLastLog *time.Time
		gotEmail   string
	)
	err = p.pool.QueryRow(ctx, `
SELECT name, last_login_at, email FROM users WHERE id = $1
`, existingID).Scan(&gotName, &gotLastLog, &gotEmail)
	if err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if gotName != "Updated Name" {
		t.Errorf("stored name = %q, want Updated Name", gotName)
	}
	if gotLastLog == nil {
		t.Error("last_login_at must be set after match-and-update")
	}
	if gotEmail != newEmail {
		t.Errorf("stored email = %q, want %q", gotEmail, newEmail)
	}
}

// TestProvisioner_BootstrapAdminEmailGetsAdminRole verifies the
// §8.5.12 elevation path: when claim email matches bootstrap-admin-email
// case-insensitively, the auto-provisioned user gets role=admin.
func TestProvisioner_BootstrapAdminEmailGetsAdminRole(t *testing.T) {
	bootstrap := "ADMIN-" + uniqueUsername("") + "@example.com"
	p := setupProvisioner(t, true, bootstrap)

	sub := uniqueSub()
	cleanupBySub(t, p.pool, sub)
	ctx := context.Background()

	// Pass the email with mixed case to prove case-insensitive match.
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: sub, Email: strings.ToUpper(bootstrap), Username: uniqueUsername("admin"), Name: "First Admin",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Role != "admin" {
		t.Fatalf("Role = %q, want admin", res.Role)
	}
	if !res.Provisioned {
		t.Fatal("expected Provisioned=true")
	}
}

// TestProvisioner_UsernameConflictGetsSuffix verifies the §8.5.12
// suffix-retry path: when the requested username collides with an
// existing row, Provision appends a numeric suffix.
func TestProvisioner_UsernameConflictGetsSuffix(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	// Pre-seed an existing user with the desired base username.
	base := uniqueUsername("john")
	preSub := uniqueSub()
	preEmail := base + "@example.com"
	insertExistingUser(t, ctx, p.pool, preEmail, preSub)
	// insertExistingUser builds its own username — we need the same
	// base. Insert a second row explicitly with base as username:
	otherID := uuid.Must(uuid.NewRandom())
	_, err := p.pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, keycloak_sub, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', NULL, $5, now(), '{}'::jsonb)
`, otherID, "other-"+base+"@example.com", base, base, uniqueSub())
	if err != nil {
		t.Fatalf("seed conflicting user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, otherID)
	})

	// Now Provision a NEW sub asking for the same username.
	newSub := uniqueSub()
	cleanupBySub(t, p.pool, newSub)
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: newSub, Email: "newuser-" + base + "@example.com", Username: base, Name: "John Two",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Username == base {
		t.Fatalf("Username = %q; want suffixed variant (base was taken)", res.Username)
	}
	want := base + "2"
	if res.Username != want {
		t.Fatalf("Username = %q, want %q", res.Username, want)
	}
}

// TestProvisioner_WithoutAutoProvisionReturnsErrUserNotProvisioned
// verifies §8.5.12: when autoProvision=false and the sub has no row,
// Provision returns ErrUserNotProvisioned (which the handler translates
// to 403 user_not_provisioned).
func TestProvisioner_WithoutAutoProvisionReturnsErrUserNotProvisioned(t *testing.T) {
	p := setupProvisioner(t, false, "")
	ctx := context.Background()

	sub := uniqueSub()
	_, err := p.Provision(ctx, ProvisionInput{
		Sub: sub, Email: "ghost@example.com", Username: uniqueUsername("ghost"), Name: "Ghost",
	})
	if !errors.Is(err, ErrUserNotProvisioned) {
		t.Fatalf("err = %v, want ErrUserNotProvisioned", err)
	}

	// And no row should have been inserted.
	var count int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE keycloak_sub = $1`, sub).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("found %d rows for unknown sub; want 0 (no INSERT path)", count)
	}
}

// TestProvisioner_WithoutAutoProvisionUpdatesKnownSub verifies that the
// match-only branch still updates last_login_at + email + name when the
// sub IS known — only INSERT is gated by autoProvision.
func TestProvisioner_WithoutAutoProvisionUpdatesKnownSub(t *testing.T) {
	p := setupProvisioner(t, false, "")
	ctx := context.Background()

	sub := uniqueSub()
	origEmail := uniqueUsername("orig") + "@example.com"
	id := insertExistingUser(t, ctx, p.pool, origEmail, sub)

	newEmail := uniqueUsername("new") + "@example.com"
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: sub, Email: newEmail, Username: "ignored", Name: "Updated",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.UserID != id {
		t.Fatalf("UserID = %v, want %v", res.UserID, id)
	}
	if res.Provisioned {
		t.Fatal("expected Provisioned=false on UPDATE branch")
	}
	if res.Email != newEmail {
		t.Fatalf("email not synced: got %q want %q", res.Email, newEmail)
	}
}

// TestProvisioner_MissingClaimsReturnsErr verifies §8.5.12: empty
// REQUIRED claims (email + username) trigger ErrMissingClaims, which
// the handler maps to 422 missing_claims.
//
// Sub is OPTIONAL in the trust-headers org pattern — its absence MUST
// NOT raise ErrMissingClaims (covered by the rest of the suite which
// calls Provision with Sub: "").
//
// Name is also OPTIONAL — see ProvisionInput doc. Empty name on INSERT
// falls back to username (verified by TestProvisioner_EmptyNameFallsBackToUsername);
// empty name on UPDATE keeps the existing display name (verified by
// TestProvisioner_EmptyNamePreservesExistingOnUpdate). Empty Name is
// therefore intentionally absent from this missing-claims table.
func TestProvisioner_MissingClaimsReturnsErr(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	// ErrMissingClaims fires only when BOTH a usable email is absent OR a
	// username cannot be resolved at all. Per the trust-headers pivot, an
	// empty/whitespace `Username` is NOT a missing claim on its own:
	// deriveUsername() falls back to the email local-part, so
	// {Email: "a@b", Username: ""} resolves to username "a" and
	// provisions successfully. The username-required branch is
	// only reachable when the username is empty AND the email has no
	// usable local-part to derive from (no "@", so nothing before it).
	cases := []struct {
		name string
		in   ProvisionInput
	}{
		{"empty email", ProvisionInput{Sub: "s", Email: "", Username: "u", Name: "n"}},
		{"whitespace email", ProvisionInput{Sub: "s", Email: "  ", Username: "u", Name: "n"}},
		// Username empty AND email has no local-part to derive from →
		// deriveUsername returns "" → ErrMissingClaims.
		{"empty username, no-local-part email", ProvisionInput{Sub: "s", Email: "noatsign", Username: "", Name: "n"}},
		{"whitespace username, no-local-part email", ProvisionInput{Sub: "s", Email: "noatsign", Username: "  ", Name: "n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.Provision(ctx, c.in)
			if !errors.Is(err, ErrMissingClaims) {
				t.Fatalf("err = %v, want ErrMissingClaims", err)
			}
		})
	}
}

// TestProvisioner_EmptySubIsAllowed verifies the trust-headers org
// pattern: oauth2-proxy forwards email/user/name but NOT the Keycloak
// `sub` claim, so the provisioner must accept Sub: "" and use email as
// the primary match/insert key. The inserted row carries a synthetic
// `trust-headers:<email>` sub to satisfy the users_auth_method CHECK
// constraint without a migration.
func TestProvisioner_EmptySubIsAllowed(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	username := uniqueUsername("trust")
	email := username + "@example.com"

	res, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: username, Name: "Trust Headers User",
	})
	if err != nil {
		t.Fatalf("Provision with empty Sub: %v", err)
	}
	// Cleanup by id since there's no real sub to filter on.
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, res.UserID)
	})

	if !res.Provisioned {
		t.Fatal("expected Provisioned=true on first email-based login")
	}
	if res.Email != email {
		t.Fatalf("Email = %q, want %q", res.Email, email)
	}
	if res.Role != "user" {
		t.Fatalf("Role = %q, want user", res.Role)
	}

	// Verify the synthetic sub satisfies the CHECK without colliding
	// with any real Keycloak sub.
	var storedSub string
	if err := p.pool.QueryRow(ctx,
		`SELECT keycloak_sub FROM users WHERE id = $1`, res.UserID,
	).Scan(&storedSub); err != nil {
		t.Fatalf("read keycloak_sub: %v", err)
	}
	if !strings.HasPrefix(storedSub, "trust-headers:") {
		t.Errorf("keycloak_sub = %q, want prefix \"trust-headers:\"", storedSub)
	}
}

// TestProvisioner_EmptySubMatchesByEmail verifies the email-based
// re-login path: a row created by the trust-headers flow (synthetic
// keycloak_sub) is matched by LOWER(email) on subsequent logins, even
// though there's no real Keycloak sub to compare on.
func TestProvisioner_EmptySubMatchesByEmail(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	username := uniqueUsername("rematch")
	email := username + "@example.com"

	first, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: username, Name: "Original Name",
	})
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, first.UserID)
	})
	if !first.Provisioned {
		t.Fatal("expected first call to insert")
	}

	// Second login: same email, no sub, updated display name.
	second, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: "ignored-on-update", Name: "Renamed",
	})
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if second.UserID != first.UserID {
		t.Fatalf("UserID changed across logins: %v → %v (must reuse row)", first.UserID, second.UserID)
	}
	if second.Provisioned {
		t.Fatal("expected Provisioned=false on second login")
	}

	var gotName string
	if err := p.pool.QueryRow(ctx,
		`SELECT name FROM users WHERE id = $1`, first.UserID,
	).Scan(&gotName); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if gotName != "Renamed" {
		t.Errorf("stored name = %q, want Renamed (sync from claims must happen on email-match)", gotName)
	}
}

// TestProvisioner_EmptySubAutoProvisionOffMatchesExisting verifies that
// the trust-headers email-match path works even when autoProvision is
// disabled — known users must keep logging in; only unknown users hit
// the gate. (autoProvision=false + unknown email → ErrUserNotProvisioned
// covered by TestProvisioner_WithoutAutoProvisionReturnsErrUserNotProvisioned.)
func TestProvisioner_EmptySubAutoProvisionOffMatchesExisting(t *testing.T) {
	p := setupProvisioner(t, false, "")
	ctx := context.Background()

	// Seed a row via the legacy sub-bearing path so it exists.
	sub := uniqueSub()
	email := uniqueUsername("seed") + "@example.com"
	id := insertExistingUser(t, ctx, p.pool, email, sub)

	// Now provision via trust-headers (no sub), email matches.
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: "ignored", Name: "From Trust Headers",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.UserID != id {
		t.Fatalf("UserID = %v, want %v (must match by email when sub is empty)", res.UserID, id)
	}
	if res.Provisioned {
		t.Fatal("expected Provisioned=false on email-match")
	}
}

// TestProvisioner_ConcurrentFirstLoginNoRace fires N parallel Provision
// calls for the same fresh email and asserts: (a) all succeed without
// 5xx, (b) all return the same UserID, (c) exactly one reports
// Provisioned=true (the INSERT winner; the rest land on the
// ON CONFLICT branch as logical UPDATEs).
//
// Regression test for the email-race Critical from the 2026-05-18 Go
// review: prior to the ON CONFLICT (lower(email)) UPSERT, two
// concurrent first-login requests collided on idx_users_email_lower
// and one of them surfaced 500 to a legitimate user.
func TestProvisioner_ConcurrentFirstLoginNoRace(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()
	username := uniqueUsername("race")
	email := username + "@example.com"
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM users WHERE LOWER(email) = $1`, email)
	})

	const goroutines = 8
	var (
		wg            sync.WaitGroup
		mu            sync.Mutex
		results       []*ProvisionResult
		errs          []error
		provisionedCt int
	)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Provision(ctx, ProvisionInput{
				Sub: "", Email: email, Username: username, Name: "Race Tester",
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, res)
			if res.Provisioned {
				provisionedCt++
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("got %d errors out of %d goroutines; first: %v", len(errs), goroutines, errs[0])
	}
	if len(results) != goroutines {
		t.Fatalf("got %d results, want %d", len(results), goroutines)
	}
	firstID := results[0].UserID
	for i, r := range results {
		if r.UserID != firstID {
			t.Fatalf("result[%d].UserID = %v, want %v (all concurrent calls must converge on one row)", i, r.UserID, firstID)
		}
	}
	if provisionedCt != 1 {
		t.Fatalf("Provisioned=true count = %d, want 1 (exactly one goroutine wins the INSERT)", provisionedCt)
	}
}

// TestProvisioner_AutoLinkLocalPasswordUser verifies the trust-headers
// org-pattern's most security-sensitive path: an existing local-password
// user (password_hash IS NOT NULL, keycloak_sub IS NULL) is silently
// linked to their SSO identity when the inbound email matches. The
// original password_hash MUST be preserved so legacy login keeps
// working for this user.
//
// Regression test for W-1 from the 2026-05-18 Go review.
func TestProvisioner_AutoLinkLocalPasswordUser(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	// Seed a local-password row directly (password_hash non-NULL,
	// keycloak_sub NULL — the pattern for users created by the legacy
	// POST /api/v1/auth/login flow before SSO was switched on).
	username := uniqueUsername("local")
	email := username + "@example.com"
	id := uuid.Must(uuid.NewRandom())
	originalHash := "$2a$04$abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmno"
	_, err := p.pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, keycloak_sub, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', $5, NULL, NOW(), '{}'::jsonb)
`, id, email, username, "Local User", originalHash)
	if err != nil {
		t.Fatalf("seed local user: %v", err)
	}
	t.Cleanup(func() { _, _ = p.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })

	// SSO first-login with the matching email; sub empty (trust-headers).
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: "ignored", Name: "Different Display",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Provisioned {
		t.Fatal("expected Provisioned=false (matched local user, did not INSERT)")
	}
	if res.UserID != id {
		t.Fatalf("UserID = %v, want %v (must match existing local row)", res.UserID, id)
	}

	// The auto-link semantics in detail:
	//   1. password_hash MUST be preserved — legacy login still works.
	//   2. keycloak_sub backfilled to the synthetic trust-headers token.
	//   3. name = "Different Display" overrides "Local User" (non-empty inbound name).
	var (
		gotHash *string
		gotSub  *string
		gotName string
	)
	err = p.pool.QueryRow(ctx, `SELECT password_hash, keycloak_sub, name FROM users WHERE id = $1`, id).
		Scan(&gotHash, &gotSub, &gotName)
	if err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if gotHash == nil || *gotHash != originalHash {
		t.Errorf("password_hash = %v, want preserved %q (legacy login must keep working)", gotHash, originalHash)
	}
	if gotSub == nil || *gotSub != trustHeaderSubPrefix+email {
		t.Errorf("keycloak_sub = %v, want %q", gotSub, trustHeaderSubPrefix+email)
	}
	if gotName != "Different Display" {
		t.Errorf("name = %q, want %q (non-empty inbound name should override)", gotName, "Different Display")
	}
}

// TestProvisioner_EmptyNamePreservesExistingOnUpdate verifies the
// COALESCE(NULLIF($name,”), name) semantics on the UPDATE path.
// Today's trust-headers oauth2-proxy config does NOT forward a name
// claim, so skein-web sends an empty `name` to skein-api. The
// implementation must therefore keep whatever display name the user
// has (either previously synced or edited via profile UI).
//
// Regression test for W-1 from the 2026-05-18 .NET review: prior to
// this fix the row's `name` was overwritten with the inbound value
// on every SSO login, silently reverting any profile-edited display.
func TestProvisioner_EmptyNamePreservesExistingOnUpdate(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	username := uniqueUsername("preserve")
	email := username + "@example.com"
	originalName := "Carefully Edited Profile Name"
	id := uuid.Must(uuid.NewRandom())
	_, err := p.pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, keycloak_sub, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', NULL, $5, NOW(), '{}'::jsonb)
`, id, email, username, originalName, trustHeaderSubPrefix+email)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = p.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })

	// Empty Name in input — must keep originalName.
	res, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: "irrelevant", Name: "",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.UserID != id {
		t.Fatalf("UserID = %v, want %v", res.UserID, id)
	}

	var got string
	if err := p.pool.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != originalName {
		t.Errorf("name = %q, want %q (empty inbound name must preserve existing display name)", got, originalName)
	}
}

// TestProvisioner_EmptyNameFallsBackToUsernameOnInsert verifies the
// INSERT path: when the inbound name claim is empty (trust-headers
// reality) the synthesised display name is the user's `username`. We
// want a non-empty value in the column — users.name has no NOT NULL
// constraint but the UI renders "" as a confusing blank.
func TestProvisioner_EmptyNameFallsBackToUsernameOnInsert(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()
	username := uniqueUsername("noname")
	email := username + "@example.com"
	cleanupBySub(t, p.pool, trustHeaderSubPrefix+email)

	res, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: username, Name: "",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !res.Provisioned {
		t.Fatal("expected Provisioned=true on INSERT")
	}

	var got string
	if err := p.pool.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, res.UserID).Scan(&got); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != username {
		t.Errorf("name = %q, want %q (empty inbound name must fall back to username on INSERT)", got, username)
	}
}

// TestProvisioner_BootstrapAdminViaEmptySubEmailMatch verifies the
// elevation path that production overwhelmingly hits under the
// trust-headers pivot: empty Sub + inbound email matching the
// bootstrap-admin email → role=admin on the auto-provisioned row.
//
// Regression test for W-4 from the 2026-05-18 Go review: the
// pre-existing bootstrap test exercised the Sub-supplied (R5.x) path
// which is rarely the prod hot path.
func TestProvisioner_BootstrapAdminViaEmptySubEmailMatch(t *testing.T) {
	bootstrap := "admin-" + uniqueUsername("") + "@example.com"
	p := setupProvisioner(t, true, bootstrap)
	ctx := context.Background()
	username := uniqueUsername("boot")
	cleanupBySub(t, p.pool, trustHeaderSubPrefix+bootstrap)

	res, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: bootstrap, Username: username, Name: "Boot Admin",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Role != "admin" {
		t.Fatalf("Role = %q, want admin (bootstrap email match elevation)", res.Role)
	}
	if !res.Provisioned {
		t.Fatal("expected Provisioned=true on first login")
	}

	// Verify the row carries the synthetic trust-headers sub —
	// distinguishable from real Keycloak UUIDs, see trustHeaderSubPrefix.
	var gotSub string
	if err := p.pool.QueryRow(ctx, `SELECT keycloak_sub FROM users WHERE id = $1`, res.UserID).Scan(&gotSub); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.HasPrefix(gotSub, trustHeaderSubPrefix) {
		t.Errorf("keycloak_sub = %q, want prefix %q", gotSub, trustHeaderSubPrefix)
	}
}

// seedExternalUser inserts a LOCAL external collaborator row (sk-163 S5):
// is_external=true, password_hash set (local password), keycloak_sub as
// supplied (NULL for the email-match case, a real sub for the sub-match
// case). Hard-deleted on cleanup. Returns the row id.
func seedExternalUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string, keycloakSub *string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := uniqueUsername("ext")
	// A throwaway bcrypt-shaped string — the anti-link guard never verifies
	// it; it only satisfies the users_auth_method CHECK (>=1 of
	// password_hash/keycloak_sub non-NULL) for the keycloak_sub-NULL case.
	hash := "$2a$04$abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmno"
	_, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, keycloak_sub, is_external, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', $5, $6, true, NOW(), '{}'::jsonb)
`, id, strings.ToLower(email), uname, "External User", hash, keycloakSub)
	if err != nil {
		t.Fatalf("seed external user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestProvisioner_ExternalEmailMatchRejected — forward anti-link, email path
// (sk-163 S5). An SSO login whose email matches an EXTERNAL local account
// must be REFUSED with ErrExternalAccountConflict, and the external row must
// be left untouched: keycloak_sub stays NULL (no silent link) and
// last_login_at stays NULL.
func TestProvisioner_ExternalEmailMatchRejected(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	username := uniqueUsername("extmail")
	email := username + "@customer.com"
	id := seedExternalUser(t, ctx, p.pool, email, nil) // keycloak_sub NULL

	res, err := p.Provision(ctx, ProvisionInput{
		Sub: "", Email: email, Username: "ignored", Name: "Attacker Display",
	})
	if !errors.Is(err, ErrExternalAccountConflict) {
		t.Fatalf("Provision err = %v, want ErrExternalAccountConflict", err)
	}
	if res != nil {
		t.Fatalf("expected nil result on refusal, got %+v", res)
	}

	// The external row MUST NOT have been linked or mutated.
	var (
		gotSub      *string
		gotName     string
		gotLastSeen *time.Time
		gotExternal bool
	)
	if err := p.pool.QueryRow(ctx, `
SELECT keycloak_sub, name, last_login_at, is_external FROM users WHERE id = $1`, id).
		Scan(&gotSub, &gotName, &gotLastSeen, &gotExternal); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if gotSub != nil {
		t.Errorf("keycloak_sub = %q, want NULL (external row must not be auto-linked)", *gotSub)
	}
	if gotName != "External User" {
		t.Errorf("name = %q, want unchanged %q (refused login must not sync display)", gotName, "External User")
	}
	if gotLastSeen != nil {
		t.Errorf("last_login_at = %v, want NULL (refused login must not stamp last_login)", gotLastSeen)
	}
	if !gotExternal {
		t.Error("is_external flipped to false — must stay true")
	}
}

// TestProvisioner_ExternalSubMatchRejected — forward anti-link, sub path
// (sk-163 S5). Critical edge case: if an external row ever carries a real
// keycloak_sub (import / manual SQL / bug), Provision tries the sub-match
// FIRST, so the email-only guard would miss it. updateBySub must detect
// is_external and refuse, leaving the row untouched.
func TestProvisioner_ExternalSubMatchRejected(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	sub := "11111111-2222-3333-4444-" + strings.ReplaceAll(uuid.Must(uuid.NewRandom()).String()[:12], "-", "")
	username := uniqueUsername("extsub")
	email := username + "@customer.com"
	id := seedExternalUser(t, ctx, p.pool, email, &sub) // keycloak_sub = real sub

	res, err := p.Provision(ctx, ProvisionInput{
		Sub: sub, Email: email, Username: "ignored", Name: "Attacker Display",
	})
	if !errors.Is(err, ErrExternalAccountConflict) {
		t.Fatalf("Provision err = %v, want ErrExternalAccountConflict", err)
	}
	if res != nil {
		t.Fatalf("expected nil result on refusal, got %+v", res)
	}

	// Row untouched: name unchanged, last_login_at still NULL, sub unchanged.
	var (
		gotSub      string
		gotName     string
		gotLastSeen *time.Time
	)
	if err := p.pool.QueryRow(ctx, `
SELECT keycloak_sub, name, last_login_at FROM users WHERE id = $1`, id).
		Scan(&gotSub, &gotName, &gotLastSeen); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if gotSub != sub {
		t.Errorf("keycloak_sub = %q, want unchanged %q", gotSub, sub)
	}
	if gotName != "External User" {
		t.Errorf("name = %q, want unchanged %q (refused sub-match must not sync display)", gotName, "External User")
	}
	if gotLastSeen != nil {
		t.Errorf("last_login_at = %v, want NULL (refused login must not stamp last_login)", gotLastSeen)
	}
}

// TestProvisioner_InsertNewExternalCollision_BackstopOnly records the decision
// (sk-163.16 #1) NOT to build a concurrency harness for the insertNew
// ON CONFLICT external-collision branch (oidc.go ~533), and instead pins the
// backstop that branch depends on.
//
// That branch is single-threaded UNREACHABLE: updateByEmail / updateBySub
// already refuse an external email before insertNew runs, so insertNew is
// reached only when no row existed at read time. The branch then fires ONLY on
// a TOCTOU race — an EXTERNAL row committed by a concurrent request between
// that read and this INSERT. The `DO UPDATE ... WHERE NOT users.is_external`
// matches zero rows, RETURNING yields pgx.ErrNoRows, and Provision surfaces
// ErrExternalAccountConflict without touching the external row. A deterministic
// unit test would need an injected mid-flight barrier (query hook / advisory
// lock) inserting the external row between the read and the INSERT — high
// effort, low value, because the branch is already covered by:
//
//	(a) the lower(email) UNIQUE index (idx_users_email_lower, migration
//	    000001): two concurrent INSERTs for the same email cannot both commit —
//	    one wins, the loser deterministically lands on the ON CONFLICT path that
//	    triggers this branch; and
//	(b) code inspection + the explicit sentinel + annotation at oidc.go:~533.
//
// So instead of the harness this test asserts backstop (a) is actually present
// in the schema. If the UNIQUE index is ever dropped, the race branch loses its
// serialization guarantee and this test fails — a cheap, durable guard on the
// invariant the untested branch leans on.
func TestProvisioner_InsertNewExternalCollision_BackstopOnly(t *testing.T) {
	p := setupProvisioner(t, true, "")
	ctx := context.Background()

	var n int
	if err := p.pool.QueryRow(ctx, `
SELECT count(*) FROM pg_indexes
 WHERE tablename  = 'users'
   AND indexname  = 'idx_users_email_lower'
   AND indexdef ILIKE '%unique%'
   AND indexdef ILIKE '%lower(email)%'`).Scan(&n); err != nil {
		t.Fatalf("query pg_indexes for backstop: %v", err)
	}
	if n != 1 {
		t.Fatalf("backstop missing: expected exactly 1 UNIQUE lower(email) index named "+
			"idx_users_email_lower, found %d — the insertNew external-collision branch "+
			"(oidc.go ~533) relies on it to serialize concurrent same-email inserts", n)
	}
}

// TestProvisioner_SSOWriteVsSerializableAssignment_AssigneeIDAtCommit is the
// regression of sk-622.21 R25 (found in review): INV-15 says issues.assignee_user_id equals the resolution of the
// assignee text by the current users directory at every instant after
// commit. The race, reproduced on a fully migrated database:
//
//	A: BEGIN SERIALIZABLE; SELECT users by the e-mail (snapshot: no such user)
//	B: an SSO login creates the account / moves an account to that e-mail
//	A: assign the issue to that e-mail; COMMIT
//
// With B autocommitted in READ COMMITTED both sides committed and the row
// kept assignee_user_id = NULL until the hourly AssigneeResyncRunner pass.
// With B SERIALIZABLE (Provisioner.provisionTx) SSI sees the rw-cycle and
// aborts A with 40001; A's replay — what every SERIALIZABLE assignment path
// does — resolves against B's commit. The assertion is the state read right
// after both commits, with no worker involved.
//
// B is the production path (Provisioner.Provision); A is a raw transaction
// on its own connection standing for the assignment, because its snapshot
// must be taken BEFORE B — the ordering is the call order of this single
// goroutine (B does not block on A: A has only read), so no sleeps.
func TestProvisioner_SSOWriteVsSerializableAssignment_AssigneeIDAtCommit(t *testing.T) {
	cases := []struct {
		name string
		// seed prepares the directory before A's snapshot and returns the
		// ProvisionInput B logs in with.
		seed func(t *testing.T, ctx context.Context, p *Provisioner, email string) ProvisionInput
	}{
		{
			name: "account created at SSO login (insertNew)",
			seed: func(t *testing.T, ctx context.Context, p *Provisioner, email string) ProvisionInput {
				username := uniqueUsername("rcins")
				sub := uniqueSub()
				cleanupBySub(t, p.pool, sub)
				return ProvisionInput{Sub: sub, Email: email, Username: username}
			},
		},
		{
			name: "e-mail changed at SSO login (updateBySub)",
			seed: func(t *testing.T, ctx context.Context, p *Provisioner, email string) ProvisionInput {
				sub := uniqueSub()
				insertExistingUser(t, ctx, p.pool, uniqueUsername("rcold")+"@example.com", sub)
				return ProvisionInput{Sub: sub, Email: email, Username: uniqueUsername("rcupd")}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := setupProvisioner(t, true, "")
			ctx := context.Background()
			owner, _ := testsupport.SeedUser(t, p.pool, "user")
			projectID, prefix := testsupport.SeedProject(t, p.pool, owner, "rcsso")
			issueID := testsupport.SeedIssue(t, p.pool, projectID, owner, prefix+"-1", "assignee race", "open", 2)
			email := uniqueUsername("rcnew") + "@example.com"
			in := tc.seed(t, ctx, p, email)

			connA, err := p.pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire A: %v", err)
			}
			defer connA.Release()

			// A, first attempt: snapshot before B.
			txA, err := connA.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatalf("A begin: %v", err)
			}
			var seen int
			if err := txA.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email) = $1`, email).Scan(&seen); err != nil {
				_ = txA.Rollback(ctx)
				t.Fatalf("A snapshot read: %v", err)
			}
			if seen != 0 {
				_ = txA.Rollback(ctx)
				t.Fatalf("A snapshot sees %d users with %s, want 0 (precondition)", seen, email)
			}

			// B: the production SSO path, committed while A is open.
			res, err := p.Provision(ctx, in)
			if err != nil {
				_ = txA.Rollback(ctx)
				t.Fatalf("B Provision: %v", err)
			}

			// A: assign on the old snapshot, commit. Then replay on a
			// class-40 abort, as the SERIALIZABLE assignment paths do.
			assign := func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE issues SET assignee = $2 WHERE id = $1`, issueID, email); err != nil {
					return err
				}
				return tx.Commit(ctx)
			}
			aborted := 0
			err = assign(txA)
			if err != nil {
				_ = txA.Rollback(ctx)
			}
			for err != nil {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "40P01") {
					t.Fatalf("A assignment failed with a non-retryable error: %v", err)
				}
				aborted++
				if aborted > 3 {
					t.Fatalf("A assignment still aborting after %d replays: %v", aborted, err)
				}
				tx, berr := connA.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
				if berr != nil {
					t.Fatalf("A replay begin: %v", berr)
				}
				if err = assign(tx); err != nil {
					_ = tx.Rollback(ctx)
				}
			}
			t.Logf("A aborted %d time(s) before committing", aborted)

			// Right after both commits, no worker: INV-15.
			var (
				gotID      *uuid.UUID
				resolution *uuid.UUID
			)
			if err := p.pool.QueryRow(ctx, `
SELECT assignee_user_id, resolve_assignee_user_id(assignee) FROM issues WHERE id = $1`, issueID).Scan(&gotID, &resolution); err != nil {
				t.Fatalf("read issue: %v", err)
			}
			if resolution == nil || *resolution != res.UserID {
				t.Fatalf("resolution of %s = %v, want the SSO account %s (precondition)", email, resolution, res.UserID)
			}
			if gotID == nil || *gotID != res.UserID {
				t.Fatalf("assignee_user_id = %v right after both commits, want %s (INV-15 at commit)", gotID, res.UserID)
			}
		})
	}
}
