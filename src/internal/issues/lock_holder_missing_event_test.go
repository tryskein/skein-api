package issues

import (
	"context"
	"testing"
)

// sk-421.1 T002 (US-15, SC-005, DRAFT ADR-1 "Downsides"), test-plan row 20:
// when the record of the claim that holds the lock is gone (purged journal),
// the issue detail still loads and simply has no lock holder's name — no
// error, and no fallback to the assignee or to the session registry.
func TestLockHolderName_MissingClaimEventDropsTheFieldNotTheCard(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	var ownerName string
	if err := pool.QueryRow(context.Background(),
		`SELECT username FROM users WHERE id = $1`, owner).Scan(&ownerName); err != nil {
		t.Fatalf("read username: %v", err)
	}
	pid, prefix := seedProject(t, pool, owner)
	id := prefix + "-1"
	seedIssue(t, pool, pid, owner, id, "held")
	store := NewStore(pool)
	ctx := callerCtx(t, owner, "user")

	if _, err := store.Claim(ctx, ClaimInput{
		ID:        id,
		SessionID: "sess-row20",
		Actor:     HumanActor(owner, ownerName),
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	before, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue before deleting the record: %v", err)
	}
	if before.LockedByUsername == nil || *before.LockedByUsername != ownerName {
		t.Fatalf("before deleting the record: lockedByUsername = %v, want %q", before.LockedByUsername, ownerName)
	}

	ct, err := pool.Exec(context.Background(), `
DELETE FROM events WHERE entity_type = 'issue' AND entity_id = $1 AND type = 'issue.claimed'`, id)
	if err != nil {
		t.Fatalf("delete claim record: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("deleted %d issue.claimed records, want 1", ct.RowsAffected())
	}

	after, err := store.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue without the claim record: %v — the card must not fail", err)
	}
	if after.LockedBy == nil || *after.LockedBy != "sess-row20" || after.LockedAt == nil {
		t.Fatalf("lock changed: lockedBy=%v lockedAt=%v — fixture must keep the lock itself", after.LockedBy, after.LockedAt)
	}
	if after.LockedByUsername != nil {
		t.Errorf("lockedByUsername = %q without the claim record, want the key absent "+
			"(no fallback to assignee or session registry)", *after.LockedByUsername)
	}
}
