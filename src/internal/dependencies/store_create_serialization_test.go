package dependencies

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCreate_SerializationFailureAfterInsert_IsRetryable is the sk-647.2
// regression: a SERIALIZABLE abort raised by a statement of Create OTHER than
// the dependency INSERT must come back as ErrSerializationRetry, the
// retryable signal — not as a raw wrapped *pgconn.PgError that every surface
// renders as internal_error. In production it came from the event insert:
// `inserting event dependency.created: … (SQLSTATE 40001)`.
//
// The abort is built, not hoped for. A concurrent SERIALIZABLE writer W forms
// a read/write cycle with Create:
//
//   - W reads the whole events table (index scans off → a relation-level
//     SIREAD lock), so Create's later event insert is a write W has read;
//   - W inserts an edge to → other, which Create's cycle walk from `to` reads
//     while it is uncommitted, so W wrote something Create read;
//   - W holds `from` FOR UPDATE, so Create's dependency INSERT waits on the
//     FK's KEY SHARE lock (reparentWaitForBlocked is the positive control),
//     after Create's snapshot and its reads, before its event insert;
//   - W commits (it only locked `from`, so the INSERT then proceeds without a
//     conflict of its own), and Create's event insert closes the cycle with a
//     transaction that has already committed: Postgres aborts Create there.
//
// Nothing must be written: the edge, its event, none of it.
func TestCreate_SerializationFailureAfterInsert_IsRetryable(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	uid := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, uid)
	from, to, other := prefix+"-1", prefix+"-2", prefix+"-3"
	seedOpenIssue(t, pool, pid, uid, from, "from")
	seedOpenIssue(t, pool, pid, uid, to, "to")
	seedOpenIssue(t, pool, pid, uid, other, "other")

	appName := "sk6472-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	opStore := NewStore(reparentTaggedPool(t, appName))

	w, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	for _, stmt := range []string{
		`SET LOCAL enable_indexscan = off`,
		`SET LOCAL enable_bitmapscan = off`,
		`SET LOCAL enable_indexonlyscan = off`,
	} {
		if _, err := w.Exec(ctx, stmt); err != nil {
			t.Fatalf("writer %q: %v", stmt, err)
		}
	}
	var n int
	if err := w.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("writer reads events: %v", err)
	}
	if _, err := w.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'blocks', '{}'::jsonb, 1, $3, now())`, to, other, uid); err != nil {
		t.Fatalf("writer inserts %s -> %s: %v", to, other, err)
	}
	if _, err := w.Exec(ctx, `SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, from); err != nil {
		t.Fatalf("writer locks %s: %v", from, err)
	}

	resCh := make(chan error, 1)
	go func() {
		_, err := opStore.Create(ctx, from, CreateDepInput{ToID: to, Type: DepBlocks},
			CallerAccess{UserID: uid, Role: "user"}, HumanActor(uid, "tester"))
		resCh <- err
	}()
	if !reparentWaitForBlocked(t, pool, appName, 0) {
		t.Fatalf("Create never blocked on the writer's lock of %s — the conflict was not built", from)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}

	select {
	case err = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("Create did not return within 30s")
	}
	if !errors.Is(err, ErrSerializationRetry) {
		t.Fatalf("Create(%s -> %s) after a committed SERIALIZABLE conflict = %v, want ErrSerializationRetry", from, to, err)
	}

	var edges, events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM dependencies WHERE from_id = $1 AND to_id = $2`, from, to).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE type = $1 AND entity_id = $2`,
		EventDependencyCreated, dependencyEntityID(from, to, DepBlocks)).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if edges != 0 || events != 0 {
		t.Fatalf("after the abort: %d edges, %d dependency.created events for %s, want 0 and 0", edges, events, from)
	}
}
