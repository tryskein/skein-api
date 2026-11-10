package lockreap

// rest_holder_test.go — matrix row 41 (US-5, D-6, SC-026).
//
// Reference — the spec, written before the code (US-5): the activity of an
// executor is any request of the key with its X-Skein-Worker-Id; the lock of
// a silent executor goes at the same thresholds as MCP; the activity of one
// executor keeps no lock of another; the key itself (no header) is active
// only on requests of the key without the header; the lock of an issue that
// left the key's scope and the lock of a key that lost `write` are not kept
// by any activity and go at the threshold. The key rule does not depend on
// the channel (lead's decision on row 49): an MCP session that called with a
// key that may no longer write loses its lock the same way.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/agents"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// seedGrantKey stores an API key of owner with the given level and scope.
func seedGrantKey(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, level apikeys.Level, scope apikeys.Scope) uuid.UUID {
	t.Helper()
	_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	k, err := apikeys.New(pool).Create(context.Background(), apikeys.NewKey{
		UserID: owner, Label: "rest holder", Prefix: prefix, Hash: hash,
		Grant: apikeys.Grant{Level: level, Scope: scope},
	}, testsupport.KeyTTLPolicy)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, k.ID)
	})
	return k.ID
}

// seedKeyedAgent records a holder row that carries the key it called with,
// last seen `ago` in the past. workerID is "" for an MCP session or for the
// key itself.
func seedKeyedAgent(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, sessionID string, keyID uuid.UUID, workerID string, ago time.Duration) {
	t.Helper()
	var worker any
	if workerID != "" {
		worker = workerID
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at, api_key_id, worker_id)
VALUES ($1, $2, now() - $3::interval, now() - $3::interval, $4, $5)`,
		owner, sessionID, ago.String(), keyID, worker); err != nil {
		t.Fatalf("seed agent %s: %v", sessionID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agents WHERE user_id = $1 AND session_id = $2`, owner, sessionID)
	})
}

func TestReaper_RESTHolders(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, owner)
	writeAll := apikeys.Scope{Mode: apikeys.ScopeAll}
	issue := func(holder string) string {
		id := prefix + "-" + uuid.NewString()[:8]
		seedLockedIssue(t, pool, pid, owner, id, holder, 2*time.Hour)
		return id
	}
	kept := func(t *testing.T, id, holder string) {
		t.Helper()
		if got := lockedBy(t, pool, id); got == nil || *got != holder {
			t.Errorf("lock of %s = %v, want kept by %s", id, got, holder)
		}
	}
	released := func(t *testing.T, id string) {
		t.Helper()
		if got := lockedBy(t, pool, id); got != nil {
			t.Errorf("lock of %s held by %q, want released", id, *got)
		}
	}

	t.Run("the activity of worker A keeps A's lock and not B's", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite, writeAll)
		a, b := agents.RESTHolder(key, "worker-a"), agents.RESTHolder(key, "worker-b")
		seedKeyedAgent(t, pool, owner, a, key, "worker-a", time.Minute)
		seedKeyedAgent(t, pool, owner, b, key, "worker-b", 2*time.Hour)
		idA, idB := issue(a), issue(b)

		newReaper(pool).Tick(context.Background())

		kept(t, idA, a)
		released(t, idB)
	})

	t.Run("the key itself lives only on requests without a worker id", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite, writeAll)
		self, worker := agents.RESTHolder(key, ""), agents.RESTHolder(key, "busy")
		// The key's own row is silent; a worker of the same key is busy.
		seedKeyedAgent(t, pool, owner, self, key, "", 2*time.Hour)
		seedKeyedAgent(t, pool, owner, worker, key, "busy", time.Minute)
		id := issue(self)

		newReaper(pool).Tick(context.Background())

		released(t, id)
	})

	t.Run("the key itself keeps its lock while it calls without a worker id", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite, writeAll)
		self := agents.RESTHolder(key, "")
		seedKeyedAgent(t, pool, owner, self, key, "", time.Minute)
		id := issue(self)

		newReaper(pool).Tick(context.Background())

		kept(t, id, self)
	})

	t.Run("a key that lost write keeps no lock however active", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelRead, writeAll)
		holder := agents.RESTHolder(key, "reader")
		seedKeyedAgent(t, pool, owner, holder, key, "reader", time.Minute)
		id := issue(holder)

		newReaper(pool).Tick(context.Background())

		released(t, id)
	})

	t.Run("an issue outside the key's scope keeps no lock however active", func(t *testing.T) {
		otherPID, _ := seedProject(t, pool, owner)
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite,
			apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{otherPID}})
		holder := agents.RESTHolder(key, "elsewhere")
		seedKeyedAgent(t, pool, owner, holder, key, "elsewhere", time.Minute)
		id := issue(holder)

		newReaper(pool).Tick(context.Background())

		released(t, id)
	})

	t.Run("an MCP session whose key lost write keeps no lock however active", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelRead, writeAll)
		const session = "mcp-session-reader-key"
		seedKeyedAgent(t, pool, owner, session, key, "", time.Minute)
		id := issue(session)

		newReaper(pool).Tick(context.Background())

		released(t, id)
	})

	// Review C5-05: a revoked key, an expired key, and a key scoped by spaces.
	t.Run("a revoked key keeps no lock however active", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite, writeAll)
		holder := agents.RESTHolder(key, "revoked")
		seedKeyedAgent(t, pool, owner, holder, key, "revoked", time.Minute)
		if _, err := pool.Exec(context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, key); err != nil {
			t.Fatalf("revoke the key: %v", err)
		}
		id := issue(holder)

		newReaper(pool).Tick(context.Background())

		released(t, id)
	})

	t.Run("an expired key keeps no lock however active", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite, writeAll)
		holder := agents.RESTHolder(key, "expired")
		seedKeyedAgent(t, pool, owner, holder, key, "expired", time.Minute)
		if _, err := pool.Exec(context.Background(),
			`UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, key); err != nil {
			t.Fatalf("expire the key: %v", err)
		}
		id := issue(holder)

		newReaper(pool).Tick(context.Background())

		released(t, id)
	})

	var issueSpace uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT space_id FROM projects WHERE id = $1`, pid).Scan(&issueSpace); err != nil {
		t.Fatalf("read the project's space: %v", err)
	}
	t.Run("a key scoped to the issue's space keeps the lock of an active holder", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite,
			apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{issueSpace}})
		holder := agents.RESTHolder(key, "in-space")
		seedKeyedAgent(t, pool, owner, holder, key, "in-space", time.Minute)
		id := issue(holder)

		newReaper(pool).Tick(context.Background())

		kept(t, id, holder)
	})

	t.Run("a key scoped to another space keeps no lock however active", func(t *testing.T) {
		other := testsupport.SeedSpace(t, pool, owner)
		key := seedGrantKey(t, pool, owner, apikeys.LevelWrite,
			apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{other}})
		holder := agents.RESTHolder(key, "out-of-space")
		seedKeyedAgent(t, pool, owner, holder, key, "out-of-space", time.Minute)
		id := issue(holder)

		newReaper(pool).Tick(context.Background())

		released(t, id)
	})

	t.Run("a lock younger than the threshold stays whatever the key", func(t *testing.T) {
		key := seedGrantKey(t, pool, owner, apikeys.LevelRead, writeAll)
		holder := agents.RESTHolder(key, "fresh")
		seedKeyedAgent(t, pool, owner, holder, key, "fresh", time.Minute)
		id := prefix + "-" + uuid.NewString()[:8]
		seedLockedIssue(t, pool, pid, owner, id, holder, time.Minute)

		newReaper(pool).Tick(context.Background())

		kept(t, id, holder)
	})
}
