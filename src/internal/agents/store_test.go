package agents

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
)

// The stale-lock reaper judges an agent alive by agents.last_seen_at
// (sk-243). That only works if the stamp is rewritten on CONTINUING
// activity, not just stamped once at first contact — so the property the
// reaper depends on is pinned here, next to the code that provides it,
// rather than assumed from the caller's side.

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "ag" + strings.ReplaceAll(id.String()[:18], "-", "")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'fake-bcrypt-hash-for-test', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func lastSeen(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, session string) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT last_seen_at FROM agents WHERE user_id = $1 AND session_id = $2`,
		userID, session).Scan(&ts); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	return ts
}

func TestUpsert_InsertsThenBumpsLastSeen(t *testing.T) {
	pool := newTestPool(t)
	user := seedUser(t, pool)
	const session = "mcp-session-upsert"
	ctx := context.Background()

	if err := Upsert(ctx, pool, user, session, LastSeenGranularity); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	first := lastSeen(t, pool, user, session)

	// Back-date past the 60-second write throttle to stand in for "the
	// agent has been working for a while", then contact again.
	if _, err := pool.Exec(ctx, `
UPDATE agents SET last_seen_at = now() - interval '10 minutes'
 WHERE user_id = $1 AND session_id = $2`, user, session); err != nil {
		t.Fatalf("back-date: %v", err)
	}
	backdated := lastSeen(t, pool, user, session)

	if err := Upsert(ctx, pool, user, session, LastSeenGranularity); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	bumped := lastSeen(t, pool, user, session)
	if !bumped.After(backdated) {
		t.Fatalf("last_seen_at was not bumped on continuing activity: %v (was %v) — "+
			"the stale-lock reaper's liveness signal would be dead on arrival", bumped, backdated)
	}
	if bumped.Before(first) {
		t.Fatalf("last_seen_at went backwards: %v < %v", bumped, first)
	}

	// Within the throttle window the stamp is deliberately left alone —
	// documented behaviour, and the reason a liveness window below ~2
	// minutes is meaningless.
	held := lastSeen(t, pool, user, session)
	if err := Upsert(ctx, pool, user, session, LastSeenGranularity); err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if got := lastSeen(t, pool, user, session); !got.Equal(held) {
		t.Errorf("last_seen_at rewritten inside the 60s throttle: %v -> %v", held, got)
	}

	_, _ = pool.Exec(ctx, `DELETE FROM agents WHERE user_id = $1`, user)
}

// An empty session id is the initialize request, which has no id yet.
func TestUpsert_EmptySessionIsNoOp(t *testing.T) {
	pool := newTestPool(t)
	user := seedUser(t, pool)
	if err := Upsert(context.Background(), pool, user, "", LastSeenGranularity); err != nil {
		t.Fatalf("Upsert with empty session: %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("agents rows = %d, want 0", n)
	}
}

// TestTouchRESTExecutor_RegistersAndThrottles — matrix row 42 (D-6).
// Reference — the plan's decision D-6: a REST executor is registered in
// agents with its key and worker id, and its activity is extended with
// the same 60-second throttle as an MCP session: a touch inside the window
// does not move last_seen_at, a touch after it does. "After the window" is
// reached by moving the stamp back in the database, not by waiting.
func TestTouchRESTExecutor_RegistersAndThrottles(t *testing.T) {
	pool := newTestPool(t)
	user := seedUser(t, pool)
	ctx := context.Background()
	k, err := apikeys.New(pool).Create(ctx, apikeys.NewKey{
		UserID: user, Label: "rest executor",
		Prefix: "ska_t016" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8], Hash: "fake-bcrypt-hash-for-test",
		Grant: apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}},
	}, apikeys.TTLPolicy{Default: 24 * time.Hour, Max: 48 * time.Hour})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, k.ID) })

	for _, worker := range []string{"", "worker-1"} {
		t.Run("worker="+worker, func(t *testing.T) {
			holder, err := TouchRESTExecutor(ctx, pool, user, k.ID, worker, LastSeenGranularity)
			if err != nil {
				t.Fatalf("first touch: %v", err)
			}
			want := "api:" + k.ID.String()
			if worker != "" {
				want += ":" + worker
			}
			if holder != want {
				t.Fatalf("holder = %q, want %q", holder, want)
			}
			var keyID *uuid.UUID
			var workerID *string
			if err := pool.QueryRow(ctx,
				`SELECT api_key_id, worker_id FROM agents WHERE user_id = $1 AND session_id = $2`,
				user, holder).Scan(&keyID, &workerID); err != nil {
				t.Fatalf("read the executor row: %v", err)
			}
			if keyID == nil || *keyID != k.ID {
				t.Errorf("api_key_id = %v, want %s", keyID, k.ID)
			}
			if (worker == "" && workerID != nil) || (worker != "" && (workerID == nil || *workerID != worker)) {
				t.Errorf("worker_id = %v, want %q", workerID, worker)
			}

			first := lastSeen(t, pool, user, holder)
			if _, err := TouchRESTExecutor(ctx, pool, user, k.ID, worker, LastSeenGranularity); err != nil {
				t.Fatalf("touch inside the window: %v", err)
			}
			if got := lastSeen(t, pool, user, holder); !got.Equal(first) {
				t.Errorf("last_seen_at moved inside the 60 s window: %v → %v", first, got)
			}

			if _, err := pool.Exec(ctx, `
UPDATE agents SET last_seen_at = last_seen_at - interval '61 seconds'
 WHERE user_id = $1 AND session_id = $2`, user, holder); err != nil {
				t.Fatalf("move the stamp back: %v", err)
			}
			backdated := lastSeen(t, pool, user, holder)
			if _, err := TouchRESTExecutor(ctx, pool, user, k.ID, worker, LastSeenGranularity); err != nil {
				t.Fatalf("touch after the window: %v", err)
			}
			if got := lastSeen(t, pool, user, holder); !got.After(backdated) {
				t.Errorf("last_seen_at not moved after the window: %v (was %v)", got, backdated)
			}
		})
	}
}
