package eventbus

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const testDatabaseURL = "TEST_DATABASE_URL"

// newTestPool mirrors the pattern used across other internal packages —
// when TEST_DATABASE_URL is unset the test is skipped, when set we open a
// pool and register Close() on cleanup.
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
		t.Fatalf("connect %s: %v", testDatabaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping %s: %v", testDatabaseURL, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedActor inserts a fresh users row + a project so the events FK
// (actor_user_id → users) resolves and the project_id column carries a
// real uuid. Returns (userID, projectID, username); registers t.Cleanup
// to wipe both rows (events cascade-deleted via the project FK chain).
func seedActor(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	uid := uuid.Must(uuid.NewRandom())
	uname := "ebt" + uid.String()[:10]
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		uid, uname+"@x.test", uname, uname); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	pid := uuid.Must(uuid.NewRandom())
	pname := "ebproj" + pid.String()[:10]
	pprefix := "ebp" + pid.String()[:6]
	seedSpaceID := testsupport.TestSpace(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, id_prefix, default_id_mode, description,
                      git_repos, archived_at, created_by, created_at, updated_at, metadata, space_id)
VALUES ($1, $2, $3, 'counter', '', '[]'::jsonb, NULL, $4, now(), now(), '{}'::jsonb, $5)`,
		pid, pname, pprefix, uid, seedSpaceID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE project_id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, uid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})
	return uid, pid, uname
}

// TestWriteHybridUpdateEvents_ParentAndChildren verifies the row layout:
// one parent row with payload.changes carrying the full diff and N child
// rows each carrying field / old_data / new_data plus payload.parentEventId
// pointing at the parent's id.
func TestWriteHybridUpdateEvents_ParentAndChildren(t *testing.T) {
	pool := newTestPool(t)
	uid, pid, uname := seedActor(t, pool)
	ctx := context.Background()

	changes := []FieldChange{
		{Field: "title", OldValue: "old", NewValue: "new"},
		{Field: "priority", OldValue: 1, NewValue: 5},
		{Field: "assignee", OldValue: "", NewValue: "alice"},
	}

	var parentID int64
	var returnedPayload []byte
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		id, payload, err := WriteHybridUpdateEvents(
			ctx, tx,
			"issue.updated", "issue.field_changed",
			"issue", "test-1", &pid,
			Actor{UserID: uid, Username: uname, Role: "human"},
			changes,
			"force-overridden",
		)
		if err != nil {
			return err
		}
		parentID = id
		returnedPayload = payload
		return nil
	}); err != nil {
		t.Fatalf("WriteHybridUpdateEvents: %v", err)
	}
	if parentID == 0 {
		t.Fatal("parent id should be non-zero")
	}
	if len(returnedPayload) == 0 {
		t.Fatal("returned parent payload should be non-empty")
	}

	// Verify parent row.
	var (
		parentType    string
		parentField   *string
		parentOldData []byte
		parentNewData []byte
		parentReason  *string
		parentPayload []byte
	)
	if err := pool.QueryRow(ctx, `
SELECT type, field, old_data, new_data, reason, payload
  FROM events WHERE id = $1`, parentID).Scan(
		&parentType, &parentField, &parentOldData, &parentNewData, &parentReason, &parentPayload,
	); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if parentType != "issue.updated" {
		t.Fatalf("parent.type = %q, want issue.updated", parentType)
	}
	if parentField != nil {
		t.Fatalf("parent.field = %v, want NULL", parentField)
	}
	if parentOldData != nil {
		t.Fatalf("parent.old_data = %s, want NULL", string(parentOldData))
	}
	if parentNewData != nil {
		t.Fatalf("parent.new_data = %s, want NULL", string(parentNewData))
	}
	if parentReason == nil || *parentReason != "force-overridden" {
		t.Fatalf("parent.reason = %v, want force-overridden", parentReason)
	}
	var pp struct {
		Changes []struct {
			Field    string `json:"field"`
			OldValue any    `json:"oldValue"`
			NewValue any    `json:"newValue"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(parentPayload, &pp); err != nil {
		t.Fatalf("parse parent payload: %v", err)
	}
	if len(pp.Changes) != 3 {
		t.Fatalf("payload.changes len = %d, want 3", len(pp.Changes))
	}
	if pp.Changes[0].Field != "title" || pp.Changes[1].Field != "priority" || pp.Changes[2].Field != "assignee" {
		t.Fatalf("payload.changes order wrong: %+v", pp.Changes)
	}
	// W1 fix: the helper returns the bytes it sent into the INSERT —
	// callers (e.g. notifications.FanOut) can reuse them as the parent
	// payload without re-marshalling. PostgreSQL canonicalises jsonb on
	// disk (sorted keys, no whitespace) so byte-for-byte equality with
	// `parentPayload` (read back from the DB) is not guaranteed; assert
	// SEMANTIC equivalence instead.
	var rp struct {
		Changes []struct {
			Field    string `json:"field"`
			OldValue any    `json:"oldValue"`
			NewValue any    `json:"newValue"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(returnedPayload, &rp); err != nil {
		t.Fatalf("parse returned payload: %v", err)
	}
	if len(rp.Changes) != len(pp.Changes) {
		t.Fatalf("returned payload changes len = %d, stored = %d", len(rp.Changes), len(pp.Changes))
	}
	for i := range rp.Changes {
		if rp.Changes[i].Field != pp.Changes[i].Field {
			t.Fatalf("changes[%d].field: returned=%q stored=%q", i, rp.Changes[i].Field, pp.Changes[i].Field)
		}
	}

	// Verify children: 3 rows, all field_changed, each with payload.parentEventId.
	rows, err := pool.Query(ctx, `
SELECT field, old_data, new_data, payload
  FROM events
 WHERE entity_type='issue' AND entity_id='test-1' AND type='issue.field_changed'
 ORDER BY id`)
	if err != nil {
		t.Fatalf("read children: %v", err)
	}
	defer rows.Close()
	var n int
	wantFields := []string{"title", "priority", "assignee"}
	for rows.Next() {
		var (
			field   *string
			oldData []byte
			newData []byte
			payload []byte
		)
		if err := rows.Scan(&field, &oldData, &newData, &payload); err != nil {
			t.Fatalf("scan child: %v", err)
		}
		if field == nil || *field != wantFields[n] {
			t.Fatalf("child[%d].field = %v, want %s", n, field, wantFields[n])
		}
		if len(oldData) == 0 || len(newData) == 0 {
			t.Fatalf("child[%d] missing old/new data", n)
		}
		var p struct {
			ParentEventID int64 `json:"parentEventId"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("parse child[%d] payload: %v", n, err)
		}
		if p.ParentEventID != parentID {
			t.Fatalf("child[%d].parentEventId = %d, want %d", n, p.ParentEventID, parentID)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("got %d child rows, want 3", n)
	}
}

// TestWriteHybridUpdateEvents_NoChangesNoOp verifies that an empty
// changes slice writes nothing and returns parentEventID=0.
func TestWriteHybridUpdateEvents_NoChangesNoOp(t *testing.T) {
	pool := newTestPool(t)
	uid, pid, uname := seedActor(t, pool)
	ctx := context.Background()

	var parentID int64
	var returnedPayload []byte
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		id, payload, err := WriteHybridUpdateEvents(
			ctx, tx,
			"issue.updated", "issue.field_changed",
			"issue", "noop-1", &pid,
			Actor{UserID: uid, Username: uname, Role: "human"},
			nil, // no changes
			"",
		)
		parentID = id
		returnedPayload = payload
		return err
	}); err != nil {
		t.Fatalf("WriteHybridUpdateEvents: %v", err)
	}
	if parentID != 0 {
		t.Fatalf("parent id = %d, want 0", parentID)
	}
	if returnedPayload != nil {
		t.Fatalf("returned payload = %s, want nil on no-op", string(returnedPayload))
	}
	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id='noop-1'`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("got %d rows, want 0", n)
	}
}

// TestWriteHybridUpdateEvents_NilProjectIDForUserEntity verifies the
// users.* path: projectID may be nil and the parent row is written with
// project_id=NULL.
func TestWriteHybridUpdateEvents_NilProjectIDForUserEntity(t *testing.T) {
	pool := newTestPool(t)
	uid, _, uname := seedActor(t, pool)
	ctx := context.Background()

	target := uuid.Must(uuid.NewRandom())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE entity_type='user' AND entity_id=$1`, target.String())
	})

	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, _, err := WriteHybridUpdateEvents(
			ctx, tx,
			"user.updated", "user.field_changed",
			"user", target.String(), nil,
			Actor{UserID: uid, Username: uname, Role: "human"},
			[]FieldChange{{Field: "name", OldValue: "Old", NewValue: "New"}},
			"",
		)
		return err
	}); err != nil {
		t.Fatalf("WriteHybridUpdateEvents: %v", err)
	}

	var pid *uuid.UUID
	if err := pool.QueryRow(ctx, `
SELECT project_id FROM events
 WHERE entity_type='user' AND entity_id=$1 AND type='user.updated'`, target.String()).Scan(&pid); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if pid != nil {
		t.Fatalf("project_id = %v, want NULL", pid)
	}
}
