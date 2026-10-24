package outbox

// supersede_lease_test.go — sk-303.14, repair A/8.
//
// THE RULE "A LIVE LEASE IS NOT TOUCHED" — the half of the supersede contract
// that had no assertion.
//
// SupersedeUndeliveredTx finishes rows in two states with the supersede
// outcome, and both are safe by the delivery machine's construction: pending
// (the transport has not taken it yet) and leased with an EXPIRED lease (the
// row has been abandoned; the expired-lease return would put it back without
// us). A row with a LIVE lease is being sent by the delivery runner right now,
// and its outcome is the runner's decision: capturing such a row would give
// two authors of one outcome.
//
// Review showed the rule was unprotected: a mutation "claim leased without
// checking push_leased_until <= now()" stayed green. The other direction was
// covered (an `AND false` condition gave three reds), i.e. "we supersede
// enough" was checked, but not "we do not supersede more than needed".
//
// The assertion here is TWO-SIDED and therefore not tautological: one fixture
// holds all three states, and each must get its own fate.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedLeaseFixtureRow puts a delivery row in the given machine state.
func seedLeaseFixtureRow(
	t *testing.T, pool *pgxpool.Pool, user uuid.UUID, reason, state string, leasedFor time.Duration,
) int64 {
	t.Helper()
	ctx := context.Background()

	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, actor, actor_type, entity_type, entity_id,
                    project_id, payload, published_at)
VALUES (now(), 'nudge.unaccepted_digest', 'system', 'system', 'user', $1,
        NULL, '{"issueIds":["lease-1"]}'::jsonb, now())
RETURNING id`, user.String()).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, eventID) })

	var leasedUntil any
	if state == "leased" {
		leasedUntil = time.Now().UTC().Add(leasedFor)
	}

	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (
    user_id, event_id, type, project_id, reason, feed_channel,
    push_state, push_leased_until, push_next_attempt_at, push_deadline_at)
VALUES ($1, $2, 'nudge.unaccepted_digest', NULL, $3, false,
        $4, $5::timestamptz, now(), now() + interval '1 hour')
RETURNING id`, user, eventID, reason, state, leasedUntil).Scan(&id); err != nil {
		t.Fatalf("seed notification (%s): %v", state, err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM notifications WHERE id = $1`, id) })
	return id
}

// leaseRowState reads the row's state and outcome.
func leaseRowState(t *testing.T, pool *pgxpool.Pool, id int64) (string, *string) {
	t.Helper()
	var state string
	var outcome *string
	if err := pool.QueryRow(context.Background(),
		`SELECT push_state, push_outcome FROM notifications WHERE id = $1`, id).
		Scan(&state, &outcome); err != nil {
		t.Fatalf("read notification %d: %v", id, err)
	}
	return state, outcome
}

// TestSupersede_LeavesALiveLeaseAloneAndTakesTheRest.
func TestSupersede_LeavesALiveLeaseAloneAndTakesTheRest(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	user := seedUser(t, pool)
	const reason = "unaccepted_reminder"

	pendingID := seedLeaseFixtureRow(t, pool, user, reason, "pending", 0)
	expiredID := seedLeaseFixtureRow(t, pool, user, reason, "leased", -time.Minute)
	liveID := seedLeaseFixtureRow(t, pool, user, reason, "leased", time.Hour)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	n, err := SupersedeUndeliveredTx(ctx, tx, user, reason)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("supersede: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n != 2 {
		t.Errorf("superseded %d rows, want 2 (pending and with an expired lease). "+
			"Three would mean a LIVE lease was captured, one — that an abandoned row "+
			"stayed and will reach the person as an extra notification", n)
	}

	for _, tc := range []struct {
		name string
		id   int64
	}{
		{"pending row", pendingID},
		{"row with an expired lease", expiredID},
	} {
		state, outcome := leaseRowState(t, pool, tc.id)
		if state != "final" || outcome == nil || *outcome != OutcomeSuperseded {
			got := "<nil>"
			if outcome != nil {
				got = *outcome
			}
			t.Errorf("%s: state %q, outcome %q; want final/%s — otherwise the person "+
				"will get a pile of notifications for every segment of transport downtime",
				tc.name, state, got, OutcomeSuperseded)
		}
	}

	// HERE IT IS, the unprotected half.
	state, outcome := leaseRowState(t, pool, liveID)
	if state != "leased" || outcome != nil {
		got := "<nil>"
		if outcome != nil {
			got = *outcome
		}
		t.Errorf("row with a LIVE lease: state %q, outcome %q; want leased with no "+
			"outcome. The delivery runner is sending it right now, and its outcome is "+
			"the runner's — capturing it gives two authors of one outcome: one send will be "+
			"counted twice or not counted at all", state, got)
	}
}
