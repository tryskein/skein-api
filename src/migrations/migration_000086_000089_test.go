package migrations

// migration_000086_000089_test.go — test matrix rows 5, 6 and 7 (US-13,
// "Data: origin of a record", the migrations 000086…000089 themselves).
// Reference — the spec, written before the migrations:
//
//   - a key created before the migration keeps what it could do: level
//     manage, the admin flag only when its owner is an instance admin, scope
//     all, no expiry; secret hash, prefix, label and last_used_at unchanged
//     (US-13 Expected / Unchanged);
//   - issues.source and comments.author_kind accept api and still refuse any
//     value outside the closed set ("Data: origin of a record");
//   - down drops exactly what its header declares — the key grant columns,
//     the scope tables, the agents columns with the REST executor rows,
//     api_idempotency — and keeps the rows of every other table; up after
//     down restores the schema and the backfill.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

// m86KeyGrantColumns are the api_keys columns 000086 adds.
var m86KeyGrantColumns = []string{"level", "is_admin", "scope_mode", "expires_at", "reissued_from_id"}

// m86KeptKeyColumns are api_keys columns of earlier migrations; down must keep them.
var m86KeptKeyColumns = []string{"id", "user_id", "key_hash", "prefix", "label", "created_at", "last_used_at", "revoked_at", "sessionless_seen_at"}

// m86NewTables are the tables 000086 and 000089 create.
var m86NewTables = []string{"api_key_scope_projects", "api_key_scope_spaces", "api_idempotency"}

// m86Key is the part of a key row that US-13 says the migration leaves alone.
type m86Key struct {
	id       uuid.UUID
	hash     string
	prefix   string
	label    string
	lastUsed time.Time
}

// seedM86Key writes a key in the form every writer before 000086 uses.
func seedM86Key(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, label string) m86Key {
	t.Helper()
	k := m86Key{
		id:       uuid.Must(uuid.NewRandom()),
		label:    label,
		lastUsed: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}
	k.hash = "hash-" + k.id.String()
	k.prefix = "ska_" + strings.ReplaceAll(k.id.String()[:13], "-", "")
	m59Exec(t, pool, "seed key "+label, `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, last_used_at)
VALUES ($1, $2, $3, $4, $5, $6)`, k.id, owner, k.hash, k.prefix, k.label, k.lastUsed)
	return k
}

// assertM86KeyIntact checks the columns US-13 lists as unchanged.
func assertM86KeyIntact(t *testing.T, pool *pgxpool.Pool, k m86Key, stage string) {
	t.Helper()
	var hash, prefix, label string
	var lastUsed *time.Time
	if err := pool.QueryRow(context.Background(), `
SELECT key_hash, prefix, label, last_used_at FROM api_keys WHERE id = $1`, k.id).
		Scan(&hash, &prefix, &label, &lastUsed); err != nil {
		t.Fatalf("%s: read key %s: %v", stage, k.label, err)
	}
	if hash != k.hash || prefix != k.prefix || label != k.label {
		t.Errorf("%s: key %s secret/prefix/label = %q/%q/%q, want %q/%q/%q",
			stage, k.label, hash, prefix, label, k.hash, k.prefix, k.label)
	}
	if lastUsed == nil || !lastUsed.Equal(k.lastUsed) {
		t.Errorf("%s: key %s last_used_at = %v, want %v", stage, k.label, lastUsed, k.lastUsed)
	}
}

// assertM86Backfill checks the US-13 grant of a migrated key.
func assertM86Backfill(t *testing.T, pool *pgxpool.Pool, k m86Key, wantAdmin bool, stage string) {
	t.Helper()
	var level *string
	var isAdmin bool
	var scope string
	var expires *time.Time
	if err := pool.QueryRow(context.Background(), `
SELECT level, is_admin, scope_mode, expires_at FROM api_keys WHERE id = $1`, k.id).
		Scan(&level, &isAdmin, &scope, &expires); err != nil {
		t.Fatalf("%s: read grant of %s: %v", stage, k.label, err)
	}
	if level == nil || *level != "manage" {
		t.Errorf("%s: key %s level = %v, want manage", stage, k.label, level)
	}
	if isAdmin != wantAdmin {
		t.Errorf("%s: key %s is_admin = %v, want %v", stage, k.label, isAdmin, wantAdmin)
	}
	if scope != "all" {
		t.Errorf("%s: key %s scope_mode = %q, want all", stage, k.label, scope)
	}
	if expires != nil {
		t.Errorf("%s: key %s expires_at = %v, want NULL", stage, k.label, *expires)
	}
}

func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == constraint
}

func m86Text(t *testing.T, pool *pgxpool.Pool, stage, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	return s
}

func TestMigration000086_000089(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := m59Migrator(t)
	if err := m.Migrate(85); err != nil {
		t.Fatalf("Migrate(85): %v", err)
	}

	// Keys created before the migration, by an instance admin and by a
	// regular user.
	admin, _ := testsupport.SeedUser(t, pool, "admin")
	regular := seedUser(t, pool, "m86")
	adminKey := seedM86Key(t, pool, admin, "m86 admin")
	regularKey := seedM86Key(t, pool, regular, "m86 regular")

	m59Up(t, m, "first up")

	t.Run("row 5: a migrated key keeps its owner's rights", func(t *testing.T) {
		assertM86Backfill(t, pool, adminKey, true, "admin owner")
		assertM86Backfill(t, pool, regularKey, false, "regular owner")
		assertM86KeyIntact(t, pool, adminKey, "admin owner")
		assertM86KeyIntact(t, pool, regularKey, "regular owner")
	})

	t.Run("000086 CHECK: at least one permission, admin only with scope all", func(t *testing.T) {
		// Review C2-05: the constraints that hold the grant rules for a
		// writer that bypasses the store.
		insert := func(level any, isAdmin bool, scope string) error {
			id := uuid.New()
			_, err := pool.Exec(ctx, `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, level, is_admin, scope_mode)
VALUES ($1, $2, 'h', $3, 'm86 check', $4, $5, $6)`,
				id, regular, "ska_"+strings.ReplaceAll(id.String()[:13], "-", ""), level, isAdmin, scope)
			return err
		}
		if err := insert(nil, false, "all"); !isCheckViolation(err, "api_keys_has_permission") {
			t.Errorf("no level and no admin: err %v, want api_keys_has_permission", err)
		}
		if err := insert(nil, true, "projects"); !isCheckViolation(err, "api_keys_admin_scope_all") {
			t.Errorf("admin with scope projects: err %v, want api_keys_admin_scope_all", err)
		}
		if err := insert("read", true, "spaces"); !isCheckViolation(err, "api_keys_admin_scope_all") {
			t.Errorf("admin with scope spaces: err %v, want api_keys_admin_scope_all", err)
		}
		if err := insert(nil, true, "all"); err != nil {
			t.Errorf("admin only, scope all: %v, want accepted", err)
		}
		if err := insert("write", false, "projects"); err != nil {
			t.Errorf("level write, scope projects: %v, want accepted", err)
		}
	})

	project, prefix := seedProject(t, pool, regular, "m86")
	issueID := prefix + "-1"
	seedIssue(t, pool, issueID, project, regular, "")

	t.Run("row 6: the origin value api is accepted, others are not", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE issues SET source = 'api' WHERE id = $1`, issueID); err != nil {
			t.Errorf("issues.source = api: %v, want accepted", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE issues SET source = 'agent' WHERE id = $1`, issueID); !isCheckViolation(err, "issues_source_check") {
			t.Errorf("issues.source = agent: err %v, want issues_source_check", err)
		}
		comment := func(kind string) error {
			_, err := pool.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_type, author_kind, body)
VALUES ($1, $2, 'm86', 'agent', $3, 'm86')`, uuid.New(), issueID, kind)
			return err
		}
		if err := comment("api"); err != nil {
			t.Errorf("comments.author_kind = api: %v, want accepted", err)
		}
		if err := comment("agent"); !isCheckViolation(err, "comments_author_kind_check") {
			t.Errorf("comments.author_kind = agent: err %v, want comments_author_kind_check", err)
		}
	})

	t.Run("row 7: down drops what it declares, up after down restores", func(t *testing.T) {
		// Rows in every object the migrations add, and rows of other tables
		// that the rollback must keep.
		space := testsupport.SeedSpace(t, pool, regular)
		m59Exec(t, pool, "scope project", `INSERT INTO api_key_scope_projects (api_key_id, project_id) VALUES ($1, $2)`, regularKey.id, project)
		m59Exec(t, pool, "scope space", `INSERT INTO api_key_scope_spaces (api_key_id, space_id) VALUES ($1, $2)`, regularKey.id, space)
		mcpSession := "m86-mcp-" + uuid.NewString()[:8]
		restSession := "api:" + regularKey.id.String() + ":w1"
		m59Exec(t, pool, "mcp agent", `INSERT INTO agents (user_id, session_id) VALUES ($1, $2)`, regular, mcpSession)
		m59Exec(t, pool, "rest agent", `
INSERT INTO agents (user_id, session_id, api_key_id, worker_id) VALUES ($1, $2, $3, 'w1')`, regular, restSession, regularKey.id)
		m59Exec(t, pool, "idempotency", `
INSERT INTO api_idempotency (api_key_id, idempotency_key, operation_id, target, body_fingerprint,
                             force, state, marker_token, in_progress_until, expires_at)
VALUES ($1, 'k1', 'createIssue', '', '\x00'::bytea, false, 'in_progress', $2,
        now() + interval '1 minute', now() + interval '24 hours')`, regularKey.id, uuid.New())
		var commentID uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT id FROM comments WHERE issue_id = $1 AND author_kind = 'api'`, issueID).Scan(&commentID); err != nil {
			t.Fatalf("the api comment of row 6: %v", err)
		}

		if err := m.Migrate(85); err != nil {
			t.Fatalf("Migrate(85): %v", err)
		}
		for _, c := range m86KeyGrantColumns {
			if columnExists(t, pool, "api_keys", c) {
				t.Errorf("after down: api_keys.%s remained", c)
			}
		}
		for _, c := range m86KeptKeyColumns {
			if !columnExists(t, pool, "api_keys", c) {
				t.Errorf("after down: api_keys.%s was dropped", c)
			}
		}
		for _, tbl := range m86NewTables {
			if tableExists(t, pool, tbl) {
				t.Errorf("after down: table %s remained", tbl)
			}
		}
		for _, c := range []string{"api_key_id", "worker_id"} {
			if columnExists(t, pool, "agents", c) {
				t.Errorf("after down: agents.%s remained", c)
			}
		}
		assertM86KeyIntact(t, pool, adminKey, "after down")
		assertM86KeyIntact(t, pool, regularKey, "after down")
		if n := m59Count(t, pool, "mcp agent after down",
			`SELECT count(*) FROM agents WHERE user_id = $1 AND session_id = $2`, regular, mcpSession); n != 1 {
			t.Errorf("after down: MCP session rows = %d, want 1", n)
		}
		if n := m59Count(t, pool, "rest agent after down",
			`SELECT count(*) FROM agents WHERE user_id = $1 AND session_id = $2`, regular, restSession); n != 0 {
			t.Errorf("after down: REST executor rows = %d, want 0 (declared by 000088 down)", n)
		}
		if got := m86Text(t, pool, "issue after down", `SELECT source FROM issues WHERE id = $1`, issueID); got != "agent_directed" {
			t.Errorf("after down: issue source = %q, want agent_directed", got)
		}
		if got := m86Text(t, pool, "comment after down", `SELECT author_kind FROM comments WHERE id = $1`, commentID); got != "agent_directed" {
			t.Errorf("after down: comment author_kind = %q, want agent_directed", got)
		}
		if _, err := pool.Exec(ctx, `UPDATE issues SET source = 'api' WHERE id = $1`, issueID); !isCheckViolation(err, "issues_source_check") {
			t.Errorf("after down: issues.source = api: err %v, want issues_source_check", err)
		}

		m59Up(t, m, "up after down")
		for _, c := range m86KeyGrantColumns {
			if !columnExists(t, pool, "api_keys", c) {
				t.Errorf("after re-up: api_keys.%s missing", c)
			}
		}
		for _, tbl := range m86NewTables {
			if !tableExists(t, pool, tbl) {
				t.Errorf("after re-up: table %s missing", tbl)
			}
		}
		for _, c := range []string{"api_key_id", "worker_id"} {
			if !columnExists(t, pool, "agents", c) {
				t.Errorf("after re-up: agents.%s missing", c)
			}
		}
		assertM86Backfill(t, pool, adminKey, true, "after re-up")
		assertM86Backfill(t, pool, regularKey, false, "after re-up")
		assertM86KeyIntact(t, pool, regularKey, "after re-up")
		if _, err := pool.Exec(ctx, `UPDATE issues SET source = 'api' WHERE id = $1`, issueID); err != nil {
			t.Errorf("after re-up: issues.source = api: %v, want accepted", err)
		}
	})
}
