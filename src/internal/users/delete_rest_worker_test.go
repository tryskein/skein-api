package users

// delete_rest_worker_test.go — matrix row 48 (D-6, class of agents readers).
//
// Reference — the plan's decision D-6: deleting a user treats the agents
// rows of his REST executors as his sessions — they go with the account,
// like the rows of his MCP sessions.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/agents"
	"skein-api/internal/apikeys"
)

func TestDeleteAccount_RemovesTheRESTExecutorRows(t *testing.T) {
	pool := newTestPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()
	operator, operatorName := seedUserDirect(t, pool, "admin")
	victim, victimName := seedUserDirect(t, pool, "user")
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM events WHERE actor_user_id = $1 OR (entity_type = 'user' AND entity_id = $2::text)`, operator, victim.String())
		_, _ = pool.Exec(c, `
DELETE FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, victimName+"@test.local", victimName)
	})
	k, err := apikeys.New(pool).Create(ctx, apikeys.NewKey{
		UserID: victim, Label: "rest executor",
		Prefix: "ska_t016" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8], Hash: "fake-bcrypt-hash-for-test",
		Grant: apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}},
	}, apikeys.TTLPolicy{Default: 24 * time.Hour, Max: 48 * time.Hour})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, k.ID) })
	for _, worker := range []string{"", "worker-1"} {
		if _, err := agents.TouchRESTExecutor(ctx, pool, victim, k.ID, worker, agents.LastSeenGranularity); err != nil {
			t.Fatalf("register the REST executor %q: %v", worker, err)
		}
	}
	countRows := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM agents WHERE user_id = $1 AND session_id LIKE 'api:%'`, victim).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := countRows(); n != 2 {
		t.Fatalf("REST executor rows before the deletion = %d, want 2", n)
	}

	if err := store.DeleteAccount(ctx, victim, "", Actor{UserID: operator, Username: operatorName, Role: "human"}); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if n := countRows(); n != 0 {
		t.Errorf("REST executor rows after the deletion = %d, want 0", n)
	}
}
