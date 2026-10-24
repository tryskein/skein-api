package monitorcreds

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const testDatabaseURL = "TEST_DATABASE_URL"

// newTestPool returns a pool against TEST_DATABASE_URL, or skips the test
// silently when it's unset (same bootstrap as internal/reminders tests).
func newTestPool(t *testing.T) *pgxpool.Pool {
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

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "mcu" + strings.ReplaceAll(id.String()[:18], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func seedProject(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	suffix := strings.ReplaceAll(id.String()[:13], "-", "")
	name := "mctest_" + suffix
	prefix := "mc" + suffix[:6]
	ctx := context.Background()
	seedSpaceID := testsupport.TestSpace(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at,
                      created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		id, name, prefix, owner, seedSpaceID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
	})
	return id
}

// seedAPIKey inserts an active api_keys row owned by userID. The prefix is
// derived from the id to satisfy the partial UNIQUE(prefix) WHERE active.
// Returns the api_keys.id. The monitor_credentials FK is ON DELETE CASCADE,
// so dropping this key (cleanup) removes any credential minted by it.
func seedAPIKey(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	prefix := "ska_" + strings.ReplaceAll(id.String()[:12], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at)
VALUES ($1, $2, 'fake-hash-for-test', $3, 'monitorcreds test', now())`,
		id, userID, prefix)
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return id
}

// fixture wires a user + project + api key and a Store ready to mint.
// Registration order (user → project → apikey) makes t.Cleanup LIFO safe:
// the api key is deleted first (CASCADE clears monitor_credentials), then
// the project (no more FK refs), then the user.
type fixture struct {
	store     *Store
	pool      *pgxpool.Pool
	userID    uuid.UUID
	projectID uuid.UUID
	apiKeyID  uuid.UUID
}

// testWindow is the fixture store's renewal window (sk-579 matrix row 8): a
// legal non-default value, so a store that kept 10m or the 6h default would
// be caught.
const testWindow = 7 * time.Hour

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := newTestPool(t)
	u := seedUser(t, pool)
	p := seedProject(t, pool, u)
	k := seedAPIKey(t, pool, u)
	return fixture{store: New(pool, testWindow), pool: pool, userID: u, projectID: p, apiKeyID: k}
}

func (f fixture) mintParams() MintParams {
	return MintParams{
		UserID:           f.userID,
		MintedByAPIKeyID: f.apiKeyID,
		Audience:         "notifications:poll",
		Capability:       "read",
		ProjectID:        f.projectID,
		ScopeIssueID:     "mc-1",
		ScopeSubtree:     true,
		EventTypes:       []string{"comment.created"},
	}
}

const testAudience = "notifications:poll"

// expiresAt reads the persisted expires_at for a credential (by digest of
// the plaintext) — the renew slid it server-side, so we verify against the
// row, not the returned Claims (which carry no timestamps).
func expiresAt(t *testing.T, pool *pgxpool.Pool, plaintext string) time.Time {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	var ts time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT expires_at FROM monitor_credentials WHERE token_digest = $1`, digest[:]).Scan(&ts)
	if err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	return ts
}

func TestMintAndValidateHappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	plaintext, cred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(plaintext, TokenTag) {
		t.Fatalf("plaintext %q missing %q prefix", plaintext, TokenTag)
	}
	// skm_ + 43 base64url chars (32 bytes, no padding) = 47.
	if len(plaintext) != len(TokenTag)+43 {
		t.Fatalf("plaintext length = %d, want %d", len(plaintext), len(TokenTag)+43)
	}
	// One now() stamps all three: absolute_deadline = created + 12h and
	// expires_at = created + the store's window, exactly (INV-1).
	if d := cred.AbsoluteDeadline.Sub(cred.CreatedAt); d != 12*time.Hour {
		t.Fatalf("absolute_deadline - created = %s, want 12h", d)
	}
	if d := cred.ExpiresAt.Sub(cred.CreatedAt); d != testWindow {
		t.Fatalf("expires_at - created = %s, want the store window %s", d, testWindow)
	}

	claims, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience)
	if err != nil {
		t.Fatalf("ValidateAndRenew: %v", err)
	}
	if claims.UserID != f.userID {
		t.Fatalf("claims.UserID = %s, want %s", claims.UserID, f.userID)
	}
	if claims.ProjectID != f.projectID {
		t.Fatalf("claims.ProjectID = %s, want %s", claims.ProjectID, f.projectID)
	}
	if claims.ScopeIssueID != "mc-1" {
		t.Fatalf("claims.ScopeIssueID = %q, want mc-1", claims.ScopeIssueID)
	}
	if !claims.ScopeSubtree {
		t.Fatalf("claims.ScopeSubtree = false, want true")
	}
	if len(claims.EventTypes) != 1 || claims.EventTypes[0] != "comment.created" {
		t.Fatalf("claims.EventTypes = %v, want [comment.created]", claims.EventTypes)
	}
	// CreatedAt is the server-owned not-before floor the poll endpoint uses.
	// It must echo the credential's immutable mint time (the RETURNING
	// created_at), normalised to UTC.
	if claims.CreatedAt.IsZero() {
		t.Fatalf("claims.CreatedAt is zero, want the credential mint time")
	}
	if claims.CreatedAt.Location() != time.UTC {
		t.Fatalf("claims.CreatedAt location = %v, want UTC", claims.CreatedAt.Location())
	}
	if d := claims.CreatedAt.Sub(cred.CreatedAt); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("claims.CreatedAt %v != credential created_at %v (delta %s)", claims.CreatedAt, cred.CreatedAt, d)
	}
}

func TestValidateAndRenewSlidesExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Pull expires_at back to a near-future instant so a successful renew
	// (now + window) visibly advances it. Must stay in the future so the
	// now() < expires_at predicate still matches.
	digest := sha256.Sum256([]byte(plaintext))
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET expires_at = now() + interval '1 minute' WHERE token_digest = $1`,
		digest[:]); err != nil {
		t.Fatalf("rewind expires_at: %v", err)
	}
	before := expiresAt(t, f.pool, plaintext)

	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); err != nil {
		t.Fatalf("ValidateAndRenew: %v", err)
	}
	after := expiresAt(t, f.pool, plaintext)
	if !after.After(before) {
		t.Fatalf("expires_at did not advance: before=%s after=%s", before, after)
	}
	// The renew stamps last_used_at and expires_at from one now(): the slide
	// is exactly the store window (INV-1).
	var expires, lastUsed time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT expires_at, last_used_at FROM monitor_credentials WHERE token_digest = $1`,
		digest[:]).Scan(&expires, &lastUsed); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if d := expires.Sub(lastUsed); d != testWindow {
		t.Fatalf("expires_at - last_used_at = %s, want the store window %s", d, testWindow)
	}
}

// TestMintWithFullWindowIsCappedAtDeadline — a 12h window at mint gives
// expires_at = absolute_deadline exactly (LEAST on the mint path too).
func TestMintWithFullWindowIsCappedAtDeadline(t *testing.T) {
	f := newFixture(t)
	store := New(f.pool, MaxLifetime)
	_, cred, err := store.Mint(context.Background(), f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !cred.ExpiresAt.Equal(cred.AbsoluteDeadline) {
		t.Fatalf("expires_at %s != absolute_deadline %s with a 12h window", cred.ExpiresAt, cred.AbsoluteDeadline)
	}
}

func TestValidateAndRenewCapsAtAbsoluteDeadline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Force absolute_deadline to 2 minutes out: a renew wanting now + window
	// must be capped to the deadline (LEAST). Keep expires_at < deadline so
	// the token is still live.
	digest := sha256.Sum256([]byte(plaintext))
	if _, err := f.pool.Exec(ctx, `
UPDATE monitor_credentials
   SET absolute_deadline = now() + interval '2 minutes',
       expires_at        = now() + interval '1 minute'
 WHERE token_digest = $1`, digest[:]); err != nil {
		t.Fatalf("force deadline: %v", err)
	}

	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); err != nil {
		t.Fatalf("ValidateAndRenew: %v", err)
	}

	var expires, deadline time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT expires_at, absolute_deadline FROM monitor_credentials WHERE token_digest = $1`,
		digest[:]).Scan(&expires, &deadline); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if expires.After(deadline) {
		t.Fatalf("expires_at %s exceeds absolute_deadline %s (cap broken)", expires, deadline)
	}
	// The cap pins expires_at TO the deadline (now + window > deadline).
	if !expires.Equal(deadline) {
		t.Fatalf("expires_at not pinned to deadline: deadline-expires = %s", deadline.Sub(expires))
	}
}

func TestValidateAndRenewRejectsWrongAudience(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := f.store.ValidateAndRenew(ctx, plaintext, "some:other-audience"); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew wrong audience = %v, want ErrInvalidMonitorToken", err)
	}
}

func TestValidateAndRenewRejectsUnknownToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.store.ValidateAndRenew(ctx, "skm_does-not-exist", testAudience); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew unknown token = %v, want ErrInvalidMonitorToken", err)
	}
}

func TestValidateAndRenewRejectsExpired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	digest := sha256.Sum256([]byte(plaintext))
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET expires_at = now() - interval '1 second' WHERE token_digest = $1`,
		digest[:]); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew expired = %v, want ErrInvalidMonitorToken", err)
	}
}

func TestValidateAndRenewRejectsPastAbsoluteDeadline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	digest := sha256.Sum256([]byte(plaintext))
	// expires_at still in the future but absolute_deadline already passed:
	// the now() < absolute_deadline predicate must reject it.
	if _, err := f.pool.Exec(ctx, `
UPDATE monitor_credentials
   SET absolute_deadline = now() - interval '1 second',
       expires_at        = now() + interval '5 minutes'
 WHERE token_digest = $1`, digest[:]); err != nil {
		t.Fatalf("force past deadline: %v", err)
	}
	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew past-deadline = %v, want ErrInvalidMonitorToken", err)
	}
}

func TestValidateAndRenewRejectsRevoked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plaintext, cred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := f.store.RevokeOwned(ctx, cred.ID, f.userID); err != nil {
		t.Fatalf("RevokeOwned: %v", err)
	}
	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew revoked = %v, want ErrInvalidMonitorToken", err)
	}
}

// TestRevokeOwned — sk-579 matrix row 10 (US-6, INV-11): the owner (user,
// not key) stops a monitor; a repeat is a success that leaves revoked_at
// alone; another user's credential, a random id and a row purged by Cleanup
// all give the same ErrNotFound.
func TestRevokeOwned(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	revokedAt := func(id uuid.UUID) *time.Time {
		var ts *time.Time
		if err := f.pool.QueryRow(ctx, `SELECT revoked_at FROM monitor_credentials WHERE id = $1`, id).Scan(&ts); err != nil {
			t.Fatalf("read revoked_at: %v", err)
		}
		return ts
	}

	_, own, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	revoked, err := f.store.RevokeOwned(ctx, own.ID, f.userID)
	if err != nil || !revoked {
		t.Fatalf("RevokeOwned own = (%v, %v), want (true, nil)", revoked, err)
	}
	first := revokedAt(own.ID)
	if first == nil {
		t.Fatalf("revoked_at is NULL after a stop")
	}
	revoked, err = f.store.RevokeOwned(ctx, own.ID, f.userID)
	if err != nil || revoked {
		t.Fatalf("repeat RevokeOwned = (%v, %v), want (false, nil)", revoked, err)
	}
	if again := revokedAt(own.ID); again == nil || !again.Equal(*first) {
		t.Fatalf("a repeat stop rewrote revoked_at: %v -> %v", first, again)
	}

	// The owner is the user: a monitor minted by another key of the same
	// user is stopped too.
	otherKey := seedAPIKey(t, f.pool, f.userID)
	params := f.mintParams()
	params.MintedByAPIKeyID = otherKey
	_, viaOtherKey, err := f.store.Mint(ctx, params)
	if err != nil {
		t.Fatalf("Mint via another key: %v", err)
	}
	if revoked, err := f.store.RevokeOwned(ctx, viaOtherKey.ID, f.userID); err != nil || !revoked {
		t.Fatalf("RevokeOwned of a monitor minted by another key of the same user = (%v, %v)", revoked, err)
	}

	// Another user's monitor, a random id and a purged row: one error.
	_, foreign, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint foreign: %v", err)
	}
	stranger := seedUser(t, f.pool)
	_, purged, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint purged: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET revoked_at = now() - interval '48 hours' WHERE id = $1`, purged.ID); err != nil {
		t.Fatalf("age out: %v", err)
	}
	if _, err := f.store.Cleanup(ctx, 24*time.Hour); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for name, call := range map[string]func() error{
		"another user's monitor": func() error { _, err := f.store.RevokeOwned(ctx, foreign.ID, stranger); return err },
		"random id":              func() error { _, err := f.store.RevokeOwned(ctx, uuid.New(), f.userID); return err },
		"purged by Cleanup":      func() error { _, err := f.store.RevokeOwned(ctx, purged.ID, f.userID); return err },
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: RevokeOwned = %v, want ErrNotFound", name, err)
		}
	}
	if revokedAt(foreign.ID) != nil {
		t.Errorf("another user revoked this user's monitor")
	}
}

func TestValidateAndRenewRejectsRevokedMintingKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Soft-revoke the minting api key — the inline join (ak.revoked_at IS
	// NULL) must drop the credential without touching the credential row.
	if _, err := f.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, f.apiKeyID); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}
	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew revoked-key = %v, want ErrInvalidMonitorToken", err)
	}
}

func TestValidateAndRenewRejectsDeactivatedUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Deactivate the owner — the inline join (u.deactivated_at IS NULL) must
	// drop the credential.
	if _, err := f.pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, f.userID); err != nil {
		t.Fatalf("deactivate user: %v", err)
	}
	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew deactivated-user = %v, want ErrInvalidMonitorToken", err)
	}
}

func TestValidateAndRenewRejectsOwnerMismatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A second user + their own api key. Craft a credential whose user_id is
	// f.userID but minted_by_api_key_id belongs to user2 — the
	// mc.user_id = ak.user_id defence predicate must reject it. (This state
	// can't arise via Mint; we insert it directly.)
	user2 := seedUser(t, f.pool)
	key2 := seedAPIKey(t, f.pool, user2)

	plaintext := "skm_owner-mismatch-fixture-token-value-12345"
	digest := sha256.Sum256([]byte(plaintext))
	if _, err := f.pool.Exec(ctx, `
INSERT INTO monitor_credentials (
    id, token_digest, user_id, minted_by_api_key_id, audience, capability,
    project_id, scope_issue_id, scope_subtree, event_types,
    created_at, absolute_deadline, expires_at
) VALUES (
    gen_random_uuid(), $1, $2, $3, $4, 'read',
    $5, 'mc-1', false, ARRAY['comment.created'],
    now(), now() + interval '12 hours', now() + interval '10 minutes'
)`, digest[:], f.userID, key2, testAudience, f.projectID); err != nil {
		t.Fatalf("insert mismatched credential: %v", err)
	}

	if _, err := f.store.ValidateAndRenew(ctx, plaintext, testAudience); !errors.Is(err, ErrInvalidMonitorToken) {
		t.Fatalf("ValidateAndRenew owner-mismatch = %v, want ErrInvalidMonitorToken", err)
	}
}

func TestCleanupDeletesDeadRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 1) Live credential — must survive.
	livePlain, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint live: %v", err)
	}

	// 2) Past absolute deadline — must be deleted regardless of retention.
	deadPlain, deadCred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint dead: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET absolute_deadline = now() - interval '1 hour' WHERE id = $1`,
		deadCred.ID); err != nil {
		t.Fatalf("age out deadline: %v", err)
	}

	// 3) Revoked older than retention — must be deleted.
	oldRevPlain, oldRevCred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint old-revoked: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE monitor_credentials SET revoked_at = now() - interval '48 hours' WHERE id = $1`,
		oldRevCred.ID); err != nil {
		t.Fatalf("age out revoked: %v", err)
	}

	// 4) Revoked recently (within retention) — must survive.
	recentRevPlain, recentRevCred, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint recent-revoked: %v", err)
	}
	if _, err := f.store.RevokeOwned(ctx, recentRevCred.ID, f.userID); err != nil {
		t.Fatalf("RevokeOwned recent: %v", err)
	}

	deleted, err := f.store.Cleanup(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("Cleanup deleted %d rows, want >= 2 (dead-deadline + old-revoked)", deleted)
	}

	if !exists(t, f.pool, livePlain) {
		t.Fatalf("live credential was deleted")
	}
	if exists(t, f.pool, deadPlain) {
		t.Fatalf("past-deadline credential survived cleanup")
	}
	if exists(t, f.pool, oldRevPlain) {
		t.Fatalf("old-revoked credential survived cleanup")
	}
	if !exists(t, f.pool, recentRevPlain) {
		t.Fatalf("recently-revoked credential (within retention) was deleted")
	}
}

// TestCredentialCascadesOnAPIKeyDelete pins the ON DELETE CASCADE from
// monitor_credentials.minted_by_api_key_id → api_keys(id) (G7): hard-deleting
// the minting api key must take the dependent credential row with it. This is
// the third revocation route (alongside explicit Revoke and the renew-time
// ak.revoked_at / u.deactivated_at joins) and the only one that physically
// removes the row.
func TestCredentialCascadesOnAPIKeyDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	plaintext, _, err := f.store.Mint(ctx, f.mintParams())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !exists(t, f.pool, plaintext) {
		t.Fatalf("credential missing right after mint")
	}

	// Hard-delete the minting api key (not a soft revoke). The seedAPIKey
	// cleanup's later DELETE on the same id becomes a harmless no-op.
	if _, err := f.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, f.apiKeyID); err != nil {
		t.Fatalf("delete minting api key: %v", err)
	}

	if exists(t, f.pool, plaintext) {
		t.Fatalf("credential survived deletion of its minting api key — ON DELETE CASCADE missing")
	}
}

func exists(t *testing.T, pool *pgxpool.Pool, plaintext string) bool {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM monitor_credentials WHERE token_digest = $1`, digest[:]).Scan(&n); err != nil {
		t.Fatalf("exists query: %v", err)
	}
	return n > 0
}
