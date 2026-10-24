package migrations

// migration_000057_test.go — sk-579 matrix row 7 (US-5, INV-5, R7, R10).
// Reference — spec sk-579, section "Data / entities / migrations": a monitor
// gets a release session (may be empty; for a release before the rollout it is
// unknown, and that is distinguishable from empty), a token file path, the time of the last
// successful poll, a flag of completion on closing the root; the hint display marks
// (monitor x recipient session x time) are deleted together with
// the monitor; the migration is additive, without backfill. Plan (decisions "Display
// marks", "api_keys.sessionless_seen_at"): PK on the pair of marks, FK to the
// monitor with a cascade, the time of the last sessionless call on api_keys.
//
// Beyond the 000056 pattern the test holds the cascade and the NULL / '' distinction in the
// release session — the pattern does not see them.

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// m57Fixture is one user with one api key and one project — the parents a
// monitor credential needs.
type m57Fixture struct {
	user, key, project uuid.UUID
}

func seedM57Fixture(t *testing.T, pool *pgxpool.Pool, tag string) m57Fixture {
	t.Helper()
	ctx := context.Background()
	user := seedUser(t, pool, tag)
	project, _ := seedProject(t, pool, user, tag)
	key := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(ctx, `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label)
VALUES ($1, $2, 'h', $3, 'm57')`,
		key, user, "ska_"+strings.ReplaceAll(key.String()[:13], "-", "")); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	// Registered last, so it runs first: credentials hold a plain FK on the
	// project, which seedProject's cleanup would otherwise trip over.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM monitor_credentials WHERE minted_by_api_key_id = $1`, key)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, key)
	})
	return m57Fixture{user: user, key: key, project: project}
}

// insertM57Credential inserts a credential through the columns of 000030 only
// — the form every pre-000057 writer uses.
func insertM57Credential(t *testing.T, pool *pgxpool.Pool, f m57Fixture) uuid.UUID {
	t.Helper()
	digest := make([]byte, 32)
	if _, err := rand.Read(digest); err != nil {
		t.Fatalf("digest: %v", err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
INSERT INTO monitor_credentials
    (token_digest, user_id, minted_by_api_key_id, audience, capability,
     project_id, scope_issue_id, scope_subtree, event_types,
     absolute_deadline, expires_at)
VALUES ($1, $2, $3, 'notifications:poll', 'read', $4, 'm57-1', false,
        ARRAY['comment.created'], now() + interval '12 hours', now() + interval '6 hours')
RETURNING id`,
		digest, f.user, f.key, f.project).Scan(&id); err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	return id
}

// m57NewColumns are the monitor_credentials columns 000057 adds.
var m57NewColumns = []string{"issued_session_id", "token_file_path", "last_poll_succeeded_at", "root_closed_at"}

// assertM57Down checks that everything 000057 adds is gone.
func assertM57Down(t *testing.T, pool *pgxpool.Pool, stage string) {
	t.Helper()
	for _, c := range m57NewColumns {
		if columnExists(t, pool, "monitor_credentials", c) {
			t.Errorf("%s: monitor_credentials.%s remained", stage, c)
		}
	}
	if tableExists(t, pool, "monitor_stall_notices") {
		t.Errorf("%s: table monitor_stall_notices remained", stage)
	}
	if columnExists(t, pool, "api_keys", "sessionless_seen_at") {
		t.Errorf("%s: api_keys.sessionless_seen_at remained", stage)
	}
}

// assertCredentialIntact checks a credential row by its 000030 columns.
func assertCredentialIntact(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, f m57Fixture, stage string) {
	t.Helper()
	var (
		key   uuid.UUID
		scope string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT minted_by_api_key_id, scope_issue_id FROM monitor_credentials WHERE id = $1`, id).
		Scan(&key, &scope); err != nil {
		t.Fatalf("%s: credential %s is unreadable: %v", stage, id, err)
	}
	if key != f.key || scope != "m57-1" {
		t.Errorf("%s: credential changed: key=%s scope=%q", stage, key, scope)
	}
}

// assertNewColumnsNull checks that a credential written before 000057 reads
// NULL in every new column — no backfill, and in particular no empty-string session.
func assertNewColumnsNull(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, stage string) {
	t.Helper()
	var (
		session, path       *string
		lastPoll, rootClose *time.Time
	)
	if err := pool.QueryRow(context.Background(), `
SELECT issued_session_id, token_file_path, last_poll_succeeded_at, root_closed_at
  FROM monitor_credentials WHERE id = $1`, id).
		Scan(&session, &path, &lastPoll, &rootClose); err != nil {
		t.Fatalf("%s: read new columns: %v", stage, err)
	}
	if session != nil || path != nil || lastPoll != nil || rootClose != nil {
		t.Errorf("%s: pre-000057 credential has non-NULL new columns: session=%v path=%v lastPoll=%v rootClosed=%v",
			stage, session, path, lastPoll, rootClose)
	}
}

// assertM57Up checks the data properties of the 000057 schema.
func assertM57Up(t *testing.T, pool *pgxpool.Pool, f m57Fixture, stage string) {
	t.Helper()
	ctx := context.Background()

	// Issuing session: NULL (issued before rollout) and '' (client without
	// Mcp-Session-Id) are both storable and read back distinct.
	unknown := insertM57Credential(t, pool, f)
	sessionless := insertM57Credential(t, pool, f)
	if _, err := pool.Exec(ctx, `
UPDATE monitor_credentials
   SET issued_session_id = '', token_file_path = '/tmp/skein-monitor/m57.token',
       last_poll_succeeded_at = now(), root_closed_at = now()
 WHERE id = $1`, sessionless); err != nil {
		t.Fatalf("%s: write new columns: %v", stage, err)
	}
	var (
		gotUnknown, gotEmpty *string
		path                 string
		lastPoll, rootClosed *time.Time
	)
	if err := pool.QueryRow(ctx,
		`SELECT issued_session_id FROM monitor_credentials WHERE id = $1`, unknown).Scan(&gotUnknown); err != nil {
		t.Fatalf("%s: read unknown session: %v", stage, err)
	}
	if err := pool.QueryRow(ctx, `
SELECT issued_session_id, token_file_path, last_poll_succeeded_at, root_closed_at
  FROM monitor_credentials WHERE id = $1`, sessionless).
		Scan(&gotEmpty, &path, &lastPoll, &rootClosed); err != nil {
		t.Fatalf("%s: read sessionless credential: %v", stage, err)
	}
	if gotUnknown != nil {
		t.Errorf("%s: credential written without a session reads %q, want NULL (unknown)", stage, *gotUnknown)
	}
	if gotEmpty == nil || *gotEmpty != "" {
		t.Errorf("%s: empty issuing session reads %v, want ''", stage, gotEmpty)
	}
	if path != "/tmp/skein-monitor/m57.token" || lastPoll == nil || rootClosed == nil {
		t.Errorf("%s: stored path=%q lastPoll=%v rootClosed=%v", stage, path, lastPoll, rootClosed)
	}

	// Show marks: one row per (monitor, recipient session).
	if _, err := pool.Exec(ctx,
		`INSERT INTO monitor_stall_notices (monitor_id, recipient_session_id) VALUES ($1, 'sess-a')`,
		sessionless); err != nil {
		t.Fatalf("%s: first show mark: %v", stage, err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO monitor_stall_notices (monitor_id, recipient_session_id) VALUES ($1, 'sess-a')`,
		sessionless)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("%s: second show mark for the same pair: want SQLSTATE 23505, got %v", stage, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO monitor_stall_notices (monitor_id, recipient_session_id) VALUES ($1, 'sess-b'), ($1, '')`,
		sessionless); err != nil {
		t.Errorf("%s: other recipient sessions of the same monitor were refused: %v", stage, err)
	}

	// Deleting the monitor deletes its show marks.
	if _, err := pool.Exec(ctx, `DELETE FROM monitor_credentials WHERE id = $1`, sessionless); err != nil {
		t.Fatalf("%s: delete credential: %v", stage, err)
	}
	var left int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM monitor_stall_notices WHERE monitor_id = $1`, sessionless).Scan(&left); err != nil {
		t.Fatalf("%s: count show marks: %v", stage, err)
	}
	if left != 0 {
		t.Errorf("%s: %d show marks outlived their monitor", stage, left)
	}

	// api_keys.sessionless_seen_at is stored.
	var seen *time.Time
	if err := pool.QueryRow(ctx,
		`UPDATE api_keys SET sessionless_seen_at = now() WHERE id = $1 RETURNING sessionless_seen_at`,
		f.key).Scan(&seen); err != nil {
		t.Fatalf("%s: write sessionless_seen_at: %v", stage, err)
	}
	if seen == nil {
		t.Errorf("%s: api_keys.sessionless_seen_at reads NULL after a write", stage)
	}
}

func TestMigration000057_UpDownUpRoundTrip(t *testing.T) {
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run", testDatabaseURL)
	}
	pool := testPool(t)
	f := seedM57Fixture(t, pool, "m57")
	assertM57Up(t, pool, f, "before rollback")

	// A credential that lives through the rollback.
	keeper := insertM57Credential(t, pool, f)

	m, err := migrate.New("file://.", pgxMigrateURL(url))
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	defer func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("restore Up after down: %v", err)
		}
	}()

	// down: only the new objects go; the credential row stays.
	if err := m.Migrate(56); err != nil {
		t.Fatalf("Migrate(56) (000057 down): %v", err)
	}
	assertM57Down(t, pool, "after rollback")
	assertCredentialIntact(t, pool, keeper, f, "after rollback")

	// A credential written on the 000056 schema, before 000057 exists.
	legacy := insertM57Credential(t, pool, f)

	// up: existing credentials survive with NULL in every new column.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("Up after down: %v", err)
	}
	assertCredentialIntact(t, pool, legacy, f, "upgrade")
	assertNewColumnsNull(t, pool, legacy, "upgrade")
	assertCredentialIntact(t, pool, keeper, f, "re-apply")
	assertNewColumnsNull(t, pool, keeper, "re-apply")
	assertM57Up(t, pool, f, "after re-apply")

	// down again: the new objects go once more, the credentials stay.
	if err := m.Migrate(56); err != nil {
		t.Fatalf("second Migrate(56) (000057 down): %v", err)
	}
	assertM57Down(t, pool, "after second rollback")
	assertCredentialIntact(t, pool, keeper, f, "after second rollback")
	assertCredentialIntact(t, pool, legacy, f, "after second rollback")
}
