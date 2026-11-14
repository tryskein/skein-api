package idempotency_test

// store_test.go — row 78 (D-8 rule 2, «Data: Idempotency»): the late outcome
// of a first execution whose marker a repeat took over does not overwrite
// the repeat's marker, and the operation of the first execution rolls back;
// a row past its 24 h retention is deleted by the cleanup, and a repeat
// after it runs again. Time is moved by writing the row, never by waiting.

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/apikeys"
	"skein-api/internal/idempotency"
	"skein-api/internal/publicapi"
	"skein-api/internal/testsupport"
)

func TestStore_LateOutcomeAndCleanup(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	store := idempotency.NewStore(pool)

	identity := func(w world) idempotency.Identity {
		sum := sha256.Sum256([]byte(`{"text":"one"}`))
		return idempotency.Identity{KeyID: w.keyID, Key: w.idemKeys, OperationID: opAddNote,
			Target: notesPath(w.issueA), Fingerprint: sum[:]}
	}
	state := func(t *testing.T, w world) (token uuid.UUID, st string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
SELECT marker_token, state FROM api_idempotency WHERE api_key_id = $1 AND idempotency_key = $2`,
			w.keyID, w.idemKeys).Scan(&token, &st); err != nil {
			t.Fatalf("read the row: %v", err)
		}
		return token, st
	}

	t.Run("late outcome after a take-over rolls back and leaves the marker", func(t *testing.T) {
		w := seedWorld(t, pool, apikeys.LevelWrite)
		first, err := store.Begin(ctx, identity(w), time.Minute)
		if err != nil || first.Verdict != idempotency.Execute {
			t.Fatalf("first: verdict %v, err %v", first.Verdict, err)
		}
		exec(t, pool, "end the first marker", `
UPDATE api_idempotency SET in_progress_until = now() - interval '1 second'
WHERE api_key_id = $1 AND idempotency_key = $2`, w.keyID, w.idemKeys)
		repeat, err := store.Begin(ctx, identity(w), time.Minute)
		if err != nil || repeat.Verdict != idempotency.Execute {
			t.Fatalf("repeat after the marker ended: verdict %v, err %v; want Execute", repeat.Verdict, err)
		}

		outcome := idempotency.Outcome{Status: 201, ID: w.issueA, Version: 2,
			Refs: idempotency.Refs{Objects: []publicapi.ObjectRef{{Kind: publicapi.ObjectIssue, ID: w.issueA}}}}
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
INSERT INTO comments (id, issue_id, author, author_type, body) VALUES ($1, $2, 'late', 'agent', 'late')`,
				uuid.New(), w.issueA); err != nil {
				return err
			}
			return idempotency.Record(ctx, tx, w.keyID, w.idemKeys, first.Token, outcome)
		})
		if !errors.Is(err, idempotency.ErrMarkerLost) {
			t.Fatalf("late record: %v, want ErrMarkerLost", err)
		}
		if n := notes(t, pool, w.issueA); n != 0 {
			t.Errorf("%d notes after the late execution, want 0 (rolled back)", n)
		}
		if token, st := state(t, w); token != repeat.Token || st != "in_progress" {
			t.Errorf("row: token %s state %s, want the repeat's %s in_progress", token, st, repeat.Token)
		}

		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return idempotency.Record(ctx, tx, w.keyID, w.idemKeys, repeat.Token, outcome)
		})
		if err != nil {
			t.Fatalf("the repeat's record: %v", err)
		}
		if _, st := state(t, w); st != "completed" {
			t.Errorf("state %s after the repeat's record, want completed", st)
		}
	})

	t.Run("cleanup deletes a row past its retention, a repeat runs again", func(t *testing.T) {
		w := seedWorld(t, pool, apikeys.LevelWrite)
		live := seedWorld(t, pool, apikeys.LevelWrite)
		for _, x := range []world{w, live} {
			d, err := store.Begin(ctx, identity(x), time.Minute)
			if err != nil || d.Verdict != idempotency.Execute {
				t.Fatalf("begin: verdict %v, err %v", d.Verdict, err)
			}
			if err := store.Finish(ctx, x.keyID, x.idemKeys, d.Token,
				idempotency.Response{Status: 201, ContentType: "application/json", Body: []byte(`{"id":"x"}`)},
				true, idempotency.Refs{}); err != nil {
				t.Fatalf("finish: %v", err)
			}
		}
		exec(t, pool, "age the row past its retention", `
UPDATE api_idempotency SET expires_at = now() - interval '1 second', created_at = now() - interval '25 hours'
WHERE api_key_id = $1 AND idempotency_key = $2`, w.keyID, w.idemKeys)

		deleted, err := store.DeleteExpired(ctx)
		if err != nil {
			t.Fatalf("DeleteExpired: %v", err)
		}
		if deleted < 1 {
			t.Errorf("deleted %d rows, want at least the aged one", deleted)
		}
		var left int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_idempotency WHERE api_key_id = $1`, w.keyID).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Errorf("%d rows of the aged key left", left)
		}
		if d, err := store.Begin(ctx, identity(live), time.Minute); err != nil || d.Verdict != idempotency.Replay {
			t.Errorf("the row inside its retention: verdict %v, err %v; want Replay", d.Verdict, err)
		}
		if d, err := store.Begin(ctx, identity(w), time.Minute); err != nil || d.Verdict != idempotency.Execute {
			t.Errorf("repeat after the cleanup: verdict %v, err %v; want Execute", d.Verdict, err)
		}
	})
}
