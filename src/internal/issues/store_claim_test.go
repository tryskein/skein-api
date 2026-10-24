package issues

import (
	"testing"
)

// sk-26 — Store.Claim auto-accepts on self-assign-via-claim, mirroring
// the same invariant CreateIssue (assignee=creator on insert) and
// UpdateIssue (PATCH sets assignee to caller) already enforce: any
// self-assign ⟹ accepted_at = now(). Pre-fix the free-assignee branch
// (assignee=NULL → claim by caller) set assignee=caller without
// stamping accepted_at, so a task created without assignee and then
// claimed surfaced as "assignee=me + acceptedAt=null" forever — the
// scenario behind sk-25's button-on-closed bug.

// TestClaim_SelfAssign_StampsAcceptedAt: the canonical sk-26 path. An
// issue created without an assignee starts with accepted_at=NULL. When
// the caller claims it, the free-assignee branch sets assignee=caller
// AND the new auto-accept guard stamps accepted_at + accepted_by.
func TestClaim_SelfAssign_StampsAcceptedAt(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "free-then-claimed",
			Type:     "task",
			Priority: 2,
			// No assignee → CreateIssue auto-accept doesn't fire.
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("precondition: AcceptedAt should be nil — nothing to accept yet")
	}
	if dto.Assignee != nil {
		t.Fatalf("precondition: Assignee should be nil before claim")
	}

	claimed, err := store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-sk26-self",
		Actor:     HumanActor(creator, creatorName),
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Assignee landed on caller (existing behaviour) AND acceptance
	// stamped (sk-26 new behaviour).
	if claimed.Assignee == nil || *claimed.Assignee != creatorName {
		var got string
		if claimed.Assignee != nil {
			got = *claimed.Assignee
		}
		t.Fatalf("assignee = %q, want %q", got, creatorName)
	}
	if claimed.AcceptedAt == nil {
		t.Fatalf("acceptedAt = nil after self-assign claim — sk-26 guard didn't fire")
	}
	if claimed.AcceptedBy == nil || *claimed.AcceptedBy != creator {
		var got string
		if claimed.AcceptedBy != nil {
			got = (*claimed.AcceptedBy).String()
		}
		t.Fatalf("acceptedBy = %q, want %q", got, creator.String())
	}

	// sk-303.13 / T021 — auto-accept is no longer SILENT, it is
	// JOURNAL-ONLY. Exactly one issue.accepted event records the
	// transition (INV-5: no path stamps acceptance without one), carrying
	// the auto-accept reason so an audit reader can tell "took the task"
	// from "pressed Accept" — in the issues table the two are the same
	// transition. This assertion is the inverse of the one it replaced.
	var n int
	var reason *string
	if err := pool.QueryRow(t.Context(), `
SELECT count(*), min(reason) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		dto.ID).Scan(&n, &reason); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 issue.accepted event for the claim transition, got %d", n)
	}
	if reason == nil || *reason != "auto-accept" {
		var got string
		if reason != nil {
			got = *reason
		}
		t.Errorf("issue.accepted reason = %q, want \"auto-accept\" — the journal must distinguish taking a task from pressing Accept", got)
	}

	// …and it delivers nothing: the claim already notifies the creator
	// through issue.status_changed, and a second notification for one user
	// action is what INV-6 forbids.
	var delivered int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*)
  FROM notifications nt
  JOIN events e ON e.id = nt.event_id
 WHERE e.entity_type='issue' AND e.entity_id=$1 AND e.type='issue.accepted'`,
		dto.ID).Scan(&delivered); err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	if delivered != 0 {
		t.Errorf("auto-accept materialised %d notification(s); it must be journal-only", delivered)
	}
}

// TestClaim_AdminOnBehalfOther_DoesNotStampAcceptance: admin claims a
// free issue on behalf of bob via OptionalAssignee=bob. This is
// delegation (admin lock for bob's work), not self-assign. accepted_at
// must stay NULL — bob hasn't actually acknowledged the task yet.
func TestClaim_AdminOnBehalfOther_DoesNotStampAcceptance(t *testing.T) {
	pool := newTestPool(t)
	admin := seedUser(t, pool)
	adminName := readUsername(t, pool, admin)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	pid, prefix := seedProject(t, pool, admin)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "for bob",
			Type:     "task",
			Priority: 2,
		},
		admin,
		HumanActor(admin, adminName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	claimed, err := store.Claim(t.Context(), ClaimInput{
		ID:               dto.ID,
		SessionID:        "sess-sk26-onbehalf",
		OptionalAssignee: &bobName,
		Actor:            HumanActor(admin, adminName),
	})
	if err != nil {
		t.Fatalf("Claim (on behalf): %v", err)
	}

	if claimed.Assignee == nil || *claimed.Assignee != bobName {
		t.Fatalf("assignee = %v, want %q", claimed.Assignee, bobName)
	}
	// Critical assertion: bob did not acknowledge the task. admin
	// just took the lock and reassigned. acceptance must stay NULL.
	if claimed.AcceptedAt != nil {
		t.Fatalf("acceptedAt = %v on admin-claim-on-behalf, want nil — that path is delegation, not self-assign",
			*claimed.AcceptedAt)
	}
	if claimed.AcceptedBy != nil {
		t.Fatalf("acceptedBy = %v on admin-claim-on-behalf, want nil", *claimed.AcceptedBy)
	}
}

// TestClaim_AlreadyAccepted_DoesNotReStamp: R2.2 issue (created with
// assignee=me, CreateIssue already stamped acceptance) gets claimed by
// the same user. Claim must NOT re-stamp accepted_at — the original
// acceptance timestamp is the truth (matches the explicit
// Store.Accept's idempotent contract).
func TestClaim_AlreadyAccepted_DoesNotReStamp(t *testing.T) {
	pool := newTestPool(t)
	user := seedUser(t, pool)
	userName := readUsername(t, pool, user)
	pid, prefix := seedProject(t, pool, user)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "self-assigned",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(userName), // self-assign → CreateIssue auto-accept fires
		},
		user,
		HumanActor(user, userName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt == nil {
		t.Fatalf("precondition: CreateIssue should have stamped AcceptedAt on self-assign")
	}
	originalAcceptedAt := *dto.AcceptedAt

	claimed, err := store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-sk26-noidem",
		Actor:     HumanActor(user, userName),
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if claimed.AcceptedAt == nil {
		t.Fatalf("acceptedAt = nil after claim of already-accepted issue — guard clobbered the original")
	}
	// Timestamp must be the original. Equality (not just !nil) is the
	// load-bearing assertion — the new guard is supposed to short-
	// circuit when old.AcceptedAt != nil.
	if !claimed.AcceptedAt.Equal(originalAcceptedAt) {
		t.Fatalf("acceptedAt = %v, want unchanged from original %v — Claim re-stamped a pre-accepted issue",
			*claimed.AcceptedAt, originalAcceptedAt)
	}
}
