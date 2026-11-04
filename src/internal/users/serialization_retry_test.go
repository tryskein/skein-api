package users

// sk-731 — a serialization abort of a user mutation is a concurrency
// conflict, not a business refusal. Deactivate and UpdateUser used to map
// ANY SQLSTATE 40001 to ErrLastAdmin («would leave zero active admins»), so
// two legal changes racing each other — two admins of three deactivated (or
// demoted) at once, one admin stays — answered the loser with a false
// last_admin_cannot_demote. They now retry the transaction like every
// membership primitive (spaces.WithRetry, K-7): the retry re-reads the
// admin count, so the loser either applies (an admin is left) or meets
// the real ErrLastAdmin in its own recheck.
//
// The race is made deterministic, not left to scheduling: a holder
// transaction takes the events table in EXCLUSIVE mode, so both mutations
// do their reads and their users UPDATE and then wait on their event
// INSERT. Only when both wait is the holder released: each has read the
// row the other wrote, and SERIALIZABLE aborts one of them (write skew).
// The loser's next BEGIN waits for the peer mutation to commit. Without
// that second barrier, it can exhaust all three attempts while the winning
// commit is still pending, which is allowed by the bounded-retry contract.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type userMutationRaceKey struct{}

type userMutationRaceParticipant struct {
	begins        atomic.Int32
	committed     chan struct{}
	peerCommitted <-chan struct{}
}

type userMutationRaceTracer struct {
	serializationFailures atomic.Int32
}

func (tr *userMutationRaceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	p, _ := ctx.Value(userMutationRaceKey{}).(*userMutationRaceParticipant)
	if p != nil && strings.HasPrefix(data.SQL, "begin") && p.begins.Add(1) > 1 {
		// This runs before the new transaction's first statement and after
		// the failed attempt has rolled back, so it holds no loser locks
		// and cannot capture a snapshot that predates the peer's commit.
		select {
		case <-p.peerCommitted:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (tr *userMutationRaceTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(userMutationRaceKey{}) == nil {
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(data.Err, &pgErr) && pgErr.Code == "40001" {
		tr.serializationFailures.Add(1)
	}
}

// raceBehindEventsLock runs a and b concurrently while the events table is
// held, then permits a retry only after the peer mutation commits. The
// observer pool never carries the worker tracer or a participant context.
func raceBehindEventsLock(t *testing.T, pool *pgxpool.Pool, a, b func(context.Context, *Store) error) (error, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tracer := &userMutationRaceTracer{}
	cfg := pool.Config()
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = tracer
	workers, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("worker pool: %v", err)
	}
	defer func() {
		cancel()
		workers.Close()
	}()
	store := newTestStore(t, workers)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	var wg sync.WaitGroup
	defer func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		// The holder may already be rolled back; release its lock before
		// joining workers on every failure path.
		_ = holder.Rollback(cleanupCtx)
		wg.Wait()
	}()
	pid := holder.Conn().PgConn().PID()
	if _, err := holder.Exec(ctx, `LOCK TABLE events IN EXCLUSIVE MODE`); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	pa := &userMutationRaceParticipant{committed: make(chan struct{})}
	pb := &userMutationRaceParticipant{committed: make(chan struct{})}
	pa.peerCommitted, pb.peerCommitted = pb.committed, pa.committed
	errA, errB := make(chan error, 1), make(chan error, 1)
	run := func(p *userMutationRaceParticipant, mutate func(context.Context, *Store) error, result chan<- error) {
		defer wg.Done()
		err := mutate(context.WithValue(ctx, userMutationRaceKey{}, p), store)
		if err == nil {
			close(p.committed)
		}
		result <- err
	}
	wg.Add(2)
	go run(pa, a, errA)
	go run(pb, b, errB)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE wait_event_type = 'Lock' AND $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting); err != nil {
			t.Fatalf("count waiters: %v", err)
		}
		if waiting >= 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("%d of the 2 mutations wait behind the events lock; want both", waiting)
		}
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("holder release: %v", err)
	}
	var ea, eb error
	select {
	case ea = <-errA:
	case <-ctx.Done():
		t.Fatalf("first mutation did not finish: %v", ctx.Err())
	}
	select {
	case eb = <-errB:
	case <-ctx.Done():
		t.Fatalf("second mutation did not finish: %v", ctx.Err())
	}
	if tracer.serializationFailures.Load() == 0 {
		t.Fatal("concurrent mutations did not produce a real serialization failure")
	}
	return ea, eb
}

// threeActiveAdmins leaves exactly three active admins — the seeded ones —
// and restores the others on cleanup.
func threeActiveAdmins(t *testing.T, pool *pgxpool.Pool) [3]struct {
	id   uuid.UUID
	name string
} {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE role='admin' AND deactivated_at IS NULL`); err != nil {
		t.Fatalf("squash existing admins: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE users SET deactivated_at = NULL WHERE role='admin'`)
	})
	var out [3]struct {
		id   uuid.UUID
		name string
	}
	for i := range out {
		out[i].id, out[i].name = seedUserDirect(t, pool, "admin")
	}
	return out
}

func TestUserMutations_SerializationConflictIsRetriedNotLastAdmin(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	requireSerialPackages(t)
	ctx := context.Background()
	demote := "user"

	cases := []struct {
		name   string
		mutate func(context.Context, *Store, Actor, uuid.UUID) error
	}{
		{"deactivate", func(ctx context.Context, store *Store, actor Actor, target uuid.UUID) error {
			return store.Deactivate(ctx, target, actor)
		}},
		{"demote", func(ctx context.Context, store *Store, actor Actor, target uuid.UUID) error {
			_, err := store.UpdateUser(ctx, target, UpdateUserInput{Role: &demote}, actor)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admins := threeActiveAdmins(t, pool)
			actor := adminActor(admins[0].id, admins[0].name)

			errB, errC := raceBehindEventsLock(t, pool,
				func(ctx context.Context, store *Store) error { return tc.mutate(ctx, store, actor, admins[1].id) },
				func(ctx context.Context, store *Store) error { return tc.mutate(ctx, store, actor, admins[2].id) },
			)
			if errB != nil || errC != nil {
				t.Fatalf("two of three admins %s at once: got %v / %v, want both to apply (one admin stays)", tc.name, errB, errC)
			}
			if n, err := store.CountActiveAdmins(ctx); err != nil || n != 1 {
				t.Fatalf("active admins after the race = %d (err %v), want 1", n, err)
			}
		})
	}
}
