package search

// sk-465 — the embedding worker's batch lock must not stall writes that
// merely REFERENCE a pending row.
//
// processOne holds its batch lock for the whole embed round-trip (up to
// EmbedTimeout, 60s by default). With `FOR UPDATE` that lock conflicts with
// the FOR KEY SHARE lock every foreign-key check takes on the referenced
// row, so inserting a dependency / comment / child that points at a pending
// issue waited for the embedder — observed as a skein_create with an inline
// `discovered-from` edge cut at the proxy's 30s timeout and rolled back.
// `FOR NO KEY UPDATE` does not conflict with FOR KEY SHARE, so such inserts
// proceed while the worker embeds.
//
// The test drives the REAL processOne against a fake Ollama that holds the
// request for embedDelay and then answers 400 (not retried, so the batch
// rolls back and nothing is written to rows other tests may own). While the
// worker sits in that call it asserts:
//
//   - the target row really is held by the worker (a FOR UPDATE NOWAIT probe
//     fails) — otherwise the insert half would pass vacuously;
//   - a dependency insert referencing it gets its FOR KEY SHARE lock under a
//     short lock_timeout, i.e. it does not wait for the embedder (asserted
//     by the lock outcome, not by a stopwatch);
//   - the stated boundary: an UPDATE of the pending row itself still waits
//     (it needs FOR NO KEY UPDATE, which conflicts with the worker's lock).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

const (
	// embedDelay is how long the fake embedder holds the worker inside its
	// batch transaction — long enough for the probes below, each of which
	// gives up after lockProbe's lock_timeout.
	workerLockEmbedDelay = 3 * time.Second
	// pgLockNotAvailable is SQLSTATE 55P03 (NOWAIT / lock_timeout).
	pgLockNotAvailable = "55P03"
)

func TestWorkerBatchLock_DoesNotBlockReferencingInserts(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	owner, _ := testsupport.SeedUser(t, pool, "user")
	projID, prefix := testsupport.SeedProject(t, pool, owner, "tswlock")
	target := prefix + "-1"
	from := prefix + "-2"
	testsupport.SeedIssue(t, pool, projID, owner, target, "pending target", "open", 2)
	testsupport.SeedIssue(t, pool, projID, owner, from, "referencing side", "open", 2)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM dependencies WHERE from_id = $1 OR to_id = $1`, target)
	})

	// The worker's fetch is table-wide (ORDER BY id LIMIT n). Size the batch
	// to every pending issue so the target is in it whatever else is
	// pending in the shared test database; the 400 below means none of
	// those rows is written.
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issues WHERE embedding IS NULL`).Scan(&pending); err != nil {
		t.Fatalf("count pending issues: %v", err)
	}

	inEmbed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		select {
		case inEmbed <- struct{}{}:
		default:
		}
		time.Sleep(workerLockEmbedDelay)
		http.Error(w, "fake embedder: refusing on purpose", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	emb, err := NewEmbedder(EmbedConfig{BaseURL: srv.URL, Model: "fake", EmbedTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}

	workerDone := make(chan error, 1)
	go func() {
		_, err := processOne(ctx, pool, store, emb, issueSpec, pending)
		workerDone <- err
	}()
	select {
	case <-inEmbed:
	case err := <-workerDone:
		t.Fatalf("worker finished before reaching the embedder: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not reach the embedder within 10s")
	}

	// 1. The worker really holds the target.
	if err := lockProbe(ctx, t, pool, `SELECT id FROM issues WHERE id = $1 FOR UPDATE NOWAIT`, target); !isLockNotAvailable(err) {
		t.Fatalf("FOR UPDATE NOWAIT on %s: err = %v, want SQLSTATE %s — the worker is not holding the row, "+
			"so the insert assertion below would prove nothing", target, err, pgLockNotAvailable)
	}

	// 2. A foreign-key reference to it does not wait for the embedder: the
	// insert's FOR KEY SHARE on the target is granted within lock_timeout.
	// The probe rolls back, so nothing is left behind.
	if err := lockProbe(ctx, t, pool, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ($1, $2, 'discovered-from', '{}'::jsonb, 1, $3, now())`, from, target, owner); err != nil {
		if isLockNotAvailable(err) {
			t.Errorf("dependency insert referencing %s hit lock_timeout while the embedding worker held its batch "+
				"— the batch lock conflicts with FOR KEY SHARE again; FetchPendingBatch must lock FOR NO KEY UPDATE",
				target)
		} else {
			t.Fatalf("insert dependency %s -> %s: %v", from, target, err)
		}
	}

	// 3. Boundary: an UPDATE of the pending row itself still waits.
	if err := lockProbe(ctx, t, pool, `UPDATE issues SET title = title WHERE id = $1`, target); !isLockNotAvailable(err) {
		t.Errorf("UPDATE of worker-held %s: err = %v, want SQLSTATE %s (lock_timeout) — the documented "+
			"boundary says an update of the row still waits for the embedder", target, err, pgLockNotAvailable)
	}

	select {
	case err := <-workerDone:
		if !errors.Is(err, ErrEmbedderUnavailable) {
			t.Errorf("processOne err = %v, want ErrEmbedderUnavailable from the fake's 400", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("worker did not finish within 15s")
	}
}

// lockProbe runs one statement in its own transaction under a short
// lock_timeout, rolls it back, and returns the statement's error (nil when
// it got its lock).
func lockProbe(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) error {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback of a probe; the result is the statement's error
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	_, err = tx.Exec(ctx, sql, args...)
	return err
}

func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgLockNotAvailable
}
