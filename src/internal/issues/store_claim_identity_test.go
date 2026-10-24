package issues

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// sk-294 — the claim predicate must compare USER IDENTITY, not handle
// strings.
//
// `assignee` is a free-form handle column and skein_create documents three
// legal forms for it: "username, email, or user UUID — stored verbatim,
// resolved on read". Every read path resolves the handle through
// notifications.ResolveAssigneeUserID; claimInTx was the one place that did
// a raw `*old.Assignee != input.Actor.Username` compare instead. The result
// was that a task assigned by UUID (or by e-mail) could not be claimed by
// the very person it was assigned to, and the rejection blamed the wrong
// cause: `already_claimed` / branch `assignee_vs_actor`, i.e. "somebody else
// holds this", when the truth was "the handle is written in another form".
//
// The three handle namespaces cannot collide, which is why one resolver can
// serve all of them: users.username is CHECK-constrained to
// `^[a-z0-9][a-z0-9_-]{2,29}$` (max 30 chars, no '@'), so it can never be a
// 36-character UUID and never an e-mail address.

// TestClaim_AssigneeIsUUIDHandle_OwnerCanClaim is the reported defect:
// assignee holds the owner's users.id and the owner claims their own task.
func TestClaim_AssigneeIsUUIDHandle_OwnerCanClaim(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)

	store := NewStore(pool)
	uuidHandle := owner.String()
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "assigned by uuid handle",
			Type:     "task",
			Priority: 2,
			Assignee: &uuidHandle,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.Assignee == nil || *dto.Assignee != uuidHandle {
		t.Fatalf("precondition: assignee = %v, want the uuid handle %q", dto.Assignee, uuidHandle)
	}

	claimed, err := store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-sk294-uuid",
		Actor:     HumanActor(owner, ownerName),
	})
	if err != nil {
		var ac *AlreadyClaimedError
		if errors.As(err, &ac) {
			t.Fatalf("Claim refused the assignee's own task: already_claimed branch=%q (sk-294)", ac.Branch)
		}
		t.Fatalf("Claim: %v", err)
	}
	if claimed.Status != StatusInProgress {
		t.Fatalf("status = %q, want in_progress", claimed.Status)
	}
	// The handle is left VERBATIM — claim takes the lock, it does not
	// rewrite somebody's chosen handle form.
	if claimed.Assignee == nil || *claimed.Assignee != uuidHandle {
		t.Fatalf("assignee = %v, want unchanged %q", claimed.Assignee, uuidHandle)
	}
}

// TestClaim_AssigneeIsEmailHandle_OwnerCanClaim covers the second documented
// handle form. Same defect, same fix.
func TestClaim_AssigneeIsEmailHandle_OwnerCanClaim(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	pid, prefix := seedProject(t, pool, owner)

	var email string
	if err := pool.QueryRow(t.Context(),
		`SELECT email FROM users WHERE id = $1`, owner).Scan(&email); err != nil {
		t.Fatalf("reading email: %v", err)
	}

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "assigned by email handle",
			Type:     "task",
			Priority: 2,
			Assignee: &email,
		},
		owner,
		HumanActor(owner, ownerName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if _, err := store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-sk294-email",
		Actor:     HumanActor(owner, ownerName),
	}); err != nil {
		var ac *AlreadyClaimedError
		if errors.As(err, &ac) {
			t.Fatalf("Claim refused the assignee's own task: already_claimed branch=%q (sk-294)", ac.Branch)
		}
		t.Fatalf("Claim: %v", err)
	}
}

// TestClaim_AssigneeIsOtherUsersUUID_StillRefused pins the other half of the
// contract: resolving by identity must not turn the predicate into a
// rubber stamp. A task assigned to bob (by UUID) is still not alice's to
// take.
func TestClaim_AssigneeIsOtherUsersUUID_StillRefused(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, alice)
	// INV-30 (sk-674.18 T028): an assignee resolves only to a person who may
	// work in the project — a member of it.
	addMember(t, pool, pid, bob, alice)

	store := NewStore(pool)
	bobHandle := bob.String()
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "bob's task, by uuid",
			Type:     "task",
			Priority: 2,
			Assignee: &bobHandle,
		},
		alice,
		HumanActor(alice, aliceName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	_, err = store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-sk294-other",
		Actor:     HumanActor(alice, aliceName),
	})
	var ac *AlreadyClaimedError
	if !errors.As(err, &ac) {
		t.Fatalf("Claim err = %v, want AlreadyClaimedError — the task is bob's", err)
	}
	if ac.Branch != "assignee_vs_actor" {
		t.Fatalf("branch = %q, want assignee_vs_actor", ac.Branch)
	}
}

// TestClaim_AssigneeIsFreeFormLabel_BehaviourUnchanged: a handle that
// resolves to nobody ("the docs team") keeps the pre-fix, string-compare
// behaviour — an unresolvable label is not evidence of identity in either
// direction, so it must neither open the task up nor start refusing the
// caller whose username IS that literal string.
func TestClaim_AssigneeIsFreeFormLabel_BehaviourUnchanged(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	pid, prefix := seedProject(t, pool, alice)

	store := NewStore(pool)
	label := "the docs team"
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "owned by a label",
			Type:     "task",
			Priority: 2,
			Assignee: &label,
		},
		alice,
		HumanActor(alice, aliceName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	_, err = store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-sk294-label",
		Actor:     HumanActor(alice, aliceName),
	})
	var ac *AlreadyClaimedError
	if !errors.As(err, &ac) {
		t.Fatalf("Claim err = %v, want AlreadyClaimedError (label ≠ alice)", err)
	}
	if ac.Branch != "assignee_vs_actor" {
		t.Fatalf("branch = %q, want assignee_vs_actor", ac.Branch)
	}
}

// TestClaim_OnBehalfOverrideMatchesByIdentity: the admin claim-on-behalf
// branch carried the same string compare. Handing it bob's USERNAME while
// the row already carries bob's UUID is the same person, so it must not be
// rejected as assignee_vs_override.
func TestClaim_OnBehalfOverrideMatchesByIdentity(t *testing.T) {
	pool := newTestPool(t)
	admin := seedUser(t, pool)
	adminName := readUsername(t, pool, admin)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	pid, prefix := seedProject(t, pool, admin)
	// INV-30 (sk-674.18 T028): an assignee resolves only to a person who may
	// work in the project — a member of it.
	addMember(t, pool, pid, bob, admin)

	store := NewStore(pool)
	bobUUID := bob.String()
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "bob by uuid, reclaimed by username",
			Type:     "task",
			Priority: 2,
			Assignee: &bobUUID,
		},
		admin,
		HumanActor(admin, adminName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	claimed, err := store.Claim(t.Context(), ClaimInput{
		ID:               dto.ID,
		SessionID:        "sess-sk294-override",
		OptionalAssignee: &bobName,
		Actor:            HumanActor(admin, adminName),
	})
	if err != nil {
		var ac *AlreadyClaimedError
		if errors.As(err, &ac) {
			t.Fatalf("claim-on-behalf refused for the same user: branch=%q (sk-294)", ac.Branch)
		}
		t.Fatalf("Claim: %v", err)
	}
	if claimed.Assignee == nil || *claimed.Assignee != bobName {
		t.Fatalf("assignee = %v, want %q (override applies)", claimed.Assignee, bobName)
	}
}

// TestAccept_AssigneeIsUUIDHandle_OwnerCanAccept is the neighbouring
// surface: Store.Accept already resolved through ResolveAssigneeUserID, so
// it was only as good as the resolver. With a username-only resolver a
// UUID-assigned task was un-acceptable by its own assignee (ErrForbidden).
func TestAccept_AssigneeIsUUIDHandle_OwnerCanAccept(t *testing.T) {
	pool := newTestPool(t)
	owner := seedUser(t, pool)
	ownerName := readUsername(t, pool, owner)
	other := seedUser(t, pool)
	otherName := readUsername(t, pool, other)
	pid, prefix := seedProject(t, pool, other)
	// INV-30 (sk-674.18 T028): an assignee resolves only to a person who may
	// work in the project — a member of it.
	addMember(t, pool, pid, owner, other)

	store := NewStore(pool)
	uuidHandle := owner.String()
	// Created by somebody else so CreateIssue's auto-accept-on-self-assign
	// does not pre-stamp acceptance.
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "accept by uuid handle",
			Type:     "task",
			Priority: 2,
			Assignee: &uuidHandle,
		},
		other,
		HumanActor(other, otherName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("precondition: acceptedAt should be nil")
	}

	accepted, err := store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(owner, ownerName),
	})
	if err != nil {
		t.Fatalf("Accept by the (uuid-handled) assignee: %v", err)
	}
	if accepted.AcceptedBy == nil || *accepted.AcceptedBy != owner {
		var got uuid.UUID
		if accepted.AcceptedBy != nil {
			got = *accepted.AcceptedBy
		}
		t.Fatalf("acceptedBy = %v, want %v", got, owner)
	}
}
