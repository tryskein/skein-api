package issues

// rest_holder_test.go — matrix row 43 (D-6, INV-6, edge "the executor lost
// its id").
//
// Reference — the spec, written before the code: INV-6 — a lock taken by
// the key's owner himself, from any of his keys, executors or MCP sessions,
// he releases without a role, as in the UI today; another holder's lock
// needs force_unlock; the release moves the version. FOR-PLAN of D-6 —
// events.actor_session_id of a REST executor's write is its holder string
// api:<keyId>[:<workerId>].

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/agents"
)

func TestReleaseLock_OwnerReleasesAnyOfHisHolders(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)
	store := NewStore(pool)
	ctx := context.Background()
	keyA, keyB := uuid.New(), uuid.New()

	// The owner calls from yet another executor of yet another key: the
	// one that lost the id of the executor that took the lock.
	releaser := Actor{UserID: owner, Username: ownerName, Role: "agent",
		SessionID: agents.RESTHolder(uuid.New(), "")}

	holders := []string{
		agents.RESTHolder(keyA, "worker-1"),
		agents.RESTHolder(keyB, ""),
		"mcp-session-owner",
	}
	for i, holder := range holders {
		t.Run(holder, func(t *testing.T) {
			id := fmt.Sprintf("%s-%d", prefix, 10+i)
			seedIssue(t, pool, pid, owner, id, "held by "+holder)
			if _, err := store.Claim(ctx, ClaimInput{ID: id, SessionID: holder,
				Actor: Actor{UserID: owner, Username: ownerName, Role: "agent", SessionID: holder}}); err != nil {
				t.Fatalf("claim as %s: %v", holder, err)
			}
			var claimSession *string
			if err := pool.QueryRow(ctx, `
SELECT actor_session_id FROM events
 WHERE entity_type = 'issue' AND entity_id = $1 AND type = $2
 ORDER BY id DESC LIMIT 1`, id, EventIssueClaimed).Scan(&claimSession); err != nil {
				t.Fatalf("read the claim event: %v", err)
			}
			if claimSession == nil || *claimSession != holder {
				t.Errorf("events.actor_session_id of the claim = %v, want the holder %s", claimSession, holder)
			}
			before := issueVersion(t, pool, id)

			if _, err := store.ReleaseLock(ctx, ReleaseLockInput{ID: id, Actor: releaser}); err != nil {
				t.Fatalf("the owner's release without a role: %v", err)
			}
			var lockedBy *string
			if err := pool.QueryRow(ctx, `SELECT locked_by FROM issues WHERE id = $1`, id).Scan(&lockedBy); err != nil {
				t.Fatalf("read the lock: %v", err)
			}
			if lockedBy != nil {
				t.Errorf("locked_by = %q after the owner's release, want none", *lockedBy)
			}
			if after := issueVersion(t, pool, id); after != before+1 {
				t.Errorf("version %d → %d, want %d", before, after, before+1)
			}
		})
	}

	t.Run("another user's lock needs force_unlock", func(t *testing.T) {
		other := seedUser(t, pool)
		id := prefix + "-99"
		seedIssue(t, pool, pid, owner, id, "held by another user")
		holder := agents.RESTHolder(uuid.New(), "theirs")
		if _, err := pool.Exec(ctx, `
UPDATE issues SET locked_by = $2, locked_by_user_id = $3, locked_at = now(), status = 'in_progress'
 WHERE id = $1`, id, holder, other); err != nil {
			t.Fatalf("lock as another user: %v", err)
		}
		before := issueVersion(t, pool, id)

		_, err := store.ReleaseLock(ctx, ReleaseLockInput{ID: id, Actor: releaser})
		if !errors.Is(err, ErrForceUnlockForbidden) {
			t.Fatalf("release of another user's lock = %v, want ErrForceUnlockForbidden", err)
		}
		if after := issueVersion(t, pool, id); after != before {
			t.Errorf("version %d → %d after the refusal, want unchanged", before, after)
		}
	})
}
