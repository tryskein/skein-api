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

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// raceBehindEventsLock runs a and b concurrently while the events table is
// held, releases it once both wait behind it, and returns their errors.
func raceBehindEventsLock(t *testing.T, pool *pgxpool.Pool, a, b func() error) (error, error) {
	t.Helper()
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var pid int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE events IN EXCLUSIVE MODE`); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	errA, errB := make(chan error, 1), make(chan error, 1)
	go func() { errA <- a() }()
	go func() { errB <- b() }()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE wait_event_type = 'Lock' AND $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting); err != nil {
			t.Fatalf("count waiters: %v", err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of the 2 mutations wait behind the events lock; want both", waiting)
		}
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("holder release: %v", err)
	}
	return <-errA, <-errB
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
		mutate func(actor Actor, target uuid.UUID) error
	}{
		{"deactivate", func(actor Actor, target uuid.UUID) error {
			return store.Deactivate(ctx, target, actor)
		}},
		{"demote", func(actor Actor, target uuid.UUID) error {
			_, err := store.UpdateUser(ctx, target, UpdateUserInput{Role: &demote}, actor)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admins := threeActiveAdmins(t, pool)
			actor := adminActor(admins[0].id, admins[0].name)

			errB, errC := raceBehindEventsLock(t, pool,
				func() error { return tc.mutate(actor, admins[1].id) },
				func() error { return tc.mutate(actor, admins[2].id) },
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
