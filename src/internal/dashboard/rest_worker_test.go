package dashboard

// rest_worker_test.go — matrix row 45 (D-6, class of agents readers).
//
// Reference — the plan's decision D-6: every reader of agents classifies the
// row of a REST executor explicitly; the activity dashboard counts it as an
// agent, as `api` counts as agent_directed (INV-4).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
)

func TestAgents_CountsARESTExecutorAsAnAgent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	user, username := seedUser(t, pool)
	k, err := apikeys.New(pool).Create(ctx, apikeys.NewKey{
		UserID: user, Label: "rest worker",
		Prefix: "ska_t016" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8], Hash: "fake-bcrypt-hash-for-test",
		Grant: apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}},
	}, apikeys.TTLPolicy{Default: 24 * time.Hour, Max: 48 * time.Hour})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE user_id = $1`, user)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, k.ID)
	})
	// One REST executor of the key, seen two minutes ago, and nothing else.
	if _, err := pool.Exec(ctx, `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at, api_key_id, worker_id)
VALUES ($1, $2, now() - interval '2 minutes', now() - interval '2 minutes', $3, 'worker-1')`,
		user, "api:"+k.ID.String()+":worker-1", k.ID); err != nil {
		t.Fatalf("seed the REST executor row: %v", err)
	}

	// The reach is the user's own project, so the 50-row cap of the list
	// cannot push the user out behind other tests' agents.
	pid := seedProject(t, pool, user)
	res, err := New(pool).Agents(ctx, map[uuid.UUID]struct{}{pid: {}}, false)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	for _, r := range res.Items {
		if r.Username == username {
			if r.Live != 1 || r.AllTime != 1 {
				t.Errorf("%s: live=%d allTime=%d, want 1/1 — the REST executor is an agent", username, r.Live, r.AllTime)
			}
			return
		}
	}
	t.Errorf("%s with a live REST executor is absent from the agents list", username)
}
