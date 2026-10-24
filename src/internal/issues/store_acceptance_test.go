package issues

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestCreate_AcceptedAt_SelfAssign asserts that creating an issue with
// the assignee resolving to the creator (case-insensitive username
// match) auto-seeds accepted_at = now() and accepted_by = creator at
// INSERT time. Phase 1 ui-redesign — Q-4 = a (symmetric with patch).
func TestCreate_AcceptedAt_SelfAssign(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "self-assigned task",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(creatorName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt == nil {
		t.Fatalf("AcceptedAt nil; want now()")
	}
	if dto.AcceptedBy == nil || *dto.AcceptedBy != creator {
		got := uuid.Nil
		if dto.AcceptedBy != nil {
			got = *dto.AcceptedBy
		}
		t.Fatalf("AcceptedBy = %v, want %v", got, creator)
	}
	if dto.AcceptedByUsername == nil || *dto.AcceptedByUsername != creatorName {
		got := ""
		if dto.AcceptedByUsername != nil {
			got = *dto.AcceptedByUsername
		}
		t.Fatalf("AcceptedByUsername = %q, want %q", got, creatorName)
	}
}

// TestCreate_AcceptedAt_OtherAssign asserts that assigning to someone
// other than the creator leaves accepted_at = NULL.
func TestCreate_AcceptedAt_OtherAssign(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	other := seedUser(t, pool)
	otherName := readUsername(t, pool, other)
	pid, prefix := seedProject(t, pool, creator)

	creatorName := readUsername(t, pool, creator)
	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "delegated task",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(otherName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("AcceptedAt = %v, want nil (assignee != creator)", dto.AcceptedAt)
	}
	if dto.AcceptedBy != nil {
		t.Fatalf("AcceptedBy = %v, want nil", dto.AcceptedBy)
	}
}

// TestCreate_AcceptedAt_NoAssignee — creating without an assignee
// produces accepted_at = nil. Sanity check.
func TestCreate_AcceptedAt_NoAssignee(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "unassigned task",
			Type:     "task",
			Priority: 2,
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("AcceptedAt = %v, want nil", dto.AcceptedAt)
	}
}

// TestPatch_AcceptedAt_ReassignToOther asserts that reassigning from
// user A to user B resets accepted_at and accepted_by to NULL — the
// new assignee has not yet acknowledged the task.
func TestPatch_AcceptedAt_ReassignToOther(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	other := seedUser(t, pool)
	otherName := readUsername(t, pool, other)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	// Create with self-assign so accepted_at is non-null.
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "reassign target",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(creatorName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt == nil {
		t.Fatalf("precondition: AcceptedAt should be set after self-assign")
	}

	// Now patch the assignee to someone else.
	patched, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{
			ExpectedVersion: dto.Version,
			Assignee:        ptrStr(otherName),
		},
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}
	// Make sure assignee actually changed, otherwise the precondition
	// for the assertion below is wrong.
	if patched.Assignee == nil || *patched.Assignee != otherName {
		t.Fatalf("patched assignee not what we set: got %v want %s", patched.Assignee, otherName)
	}
	if patched.AcceptedAt != nil {
		t.Fatalf("AcceptedAt = %v, want nil after reassign", patched.AcceptedAt)
	}
	if patched.AcceptedBy != nil {
		t.Fatalf("AcceptedBy = %v, want nil after reassign", patched.AcceptedBy)
	}
}

// TestPatch_AcceptedAt_ReassignToSelf asserts that reassigning a task
// to oneself auto-accepts it on patch, mirroring the create path
// (Q-4 = a, symmetric).
func TestPatch_AcceptedAt_ReassignToSelf(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	other := seedUser(t, pool)
	otherName := readUsername(t, pool, other)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	// Create assigned to someone else → accepted_at = nil.
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "to be claimed",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(otherName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("precondition: AcceptedAt should be nil")
	}

	// Patch the assignee to the creator.
	patched, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{
			ExpectedVersion: dto.Version,
			Assignee:        ptrStr(creatorName),
		},
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}
	if patched.AcceptedAt == nil {
		t.Fatalf("AcceptedAt nil after self-reassign; want now()")
	}
	if patched.AcceptedBy == nil || *patched.AcceptedBy != creator {
		got := uuid.Nil
		if patched.AcceptedBy != nil {
			got = *patched.AcceptedBy
		}
		t.Fatalf("AcceptedBy = %v, want %v", got, creator)
	}
}

// TestPatch_AcceptedAt_Unassign asserts that clearing the assignee
// (assignee = "") resets acceptance state to NULL (Q-3 = a).
func TestPatch_AcceptedAt_Unassign(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "self-assigned then unassigned",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(creatorName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt == nil {
		t.Fatalf("precondition: AcceptedAt should be set after self-assign")
	}

	patched, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{
			ExpectedVersion: dto.Version,
			Assignee:        ptrStr(""),
		},
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}
	if patched.AcceptedAt != nil {
		t.Fatalf("AcceptedAt = %v, want nil after unassign", patched.AcceptedAt)
	}
	if patched.AcceptedBy != nil {
		t.Fatalf("AcceptedBy = %v, want nil after unassign", patched.AcceptedBy)
	}
}

// TestPatch_AcceptedAt_CaseOnlyAssigneeEdit_PreservesAcceptance pins
// the W1 fix: a patch that changes ONLY the case of the assignee
// string ("Admin" → "admin", or in this test the actual username
// uppercased vs lowercased) must NOT flip the assigneeChanged gate.
//
// Pre-fix behaviour (raw string compare in PatchIssue):
//   - ResolveAssigneeUserID is case-insensitive on users.username, so
//     both "Alice" and "alice" resolve to the same user.
//   - But the assigneeChanged flag was computed by raw `oldVal !=
//     *input.Assignee`, so a case-only edit flipped it to true.
//   - That caused (a) the CASE-gated UPDATE to either reset or
//     re-stamp accepted_at/accepted_by, and (b) the IssuePatchFields
//     Assignee diff to emit a spurious issue.field_changed audit row.
//
// Post-fix: both sides are resolved through ResolveAssigneeUserID and
// compared by users.id; the Assignee diff also uses strings.EqualFold.
// "Admin" vs "admin" lands the same UUID + the same lower-case
// canonical form, so neither path fires.
func TestPatch_AcceptedAt_CaseOnlyAssigneeEdit_PreservesAcceptance(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "case-only assignee patch",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(creatorName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt == nil {
		t.Fatalf("precondition: AcceptedAt should be set after self-assign")
	}
	preAcceptedAt := *dto.AcceptedAt
	preVersion := dto.Version

	// Patch the assignee to the SAME user but with the case flipped.
	// strings.ToUpper round-trips harmlessly when seedUser-derived
	// usernames are already lower-case (the seed builds them as
	// "tu" + lowercase hex), so this is a strict case-only edit.
	upperName := strings.ToUpper(creatorName)
	if upperName == creatorName {
		t.Fatalf("precondition: creator username %q should differ from its uppercase form", creatorName)
	}
	patched, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{
			ExpectedVersion: dto.Version,
			Assignee:        ptrStr(upperName),
		},
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}

	// Acceptance must NOT have churned.
	if patched.AcceptedAt == nil {
		t.Fatalf("AcceptedAt = nil; want %v (unchanged on case-only edit)", preAcceptedAt)
	}
	if !patched.AcceptedAt.Equal(preAcceptedAt) {
		t.Fatalf("AcceptedAt = %v, want %v (unchanged on case-only edit)",
			*patched.AcceptedAt, preAcceptedAt)
	}
	if patched.AcceptedBy == nil || *patched.AcceptedBy != creator {
		got := uuid.Nil
		if patched.AcceptedBy != nil {
			got = *patched.AcceptedBy
		}
		t.Fatalf("AcceptedBy = %v, want %v (unchanged on case-only edit)", got, creator)
	}

	// No issue.field_changed audit row should have been written for the
	// spurious assignee diff. Also, no issue.updated parent — since
	// no other field changed either, the patch is a complete no-op
	// from the audit perspective.
	var nFieldChanged int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1
   AND type='issue.field_changed'
   AND payload->>'field' = 'assignee'`,
		dto.ID).Scan(&nFieldChanged); err != nil {
		t.Fatalf("count field_changed: %v", err)
	}
	if nFieldChanged != 0 {
		t.Fatalf("expected 0 issue.field_changed[assignee] events, got %d", nFieldChanged)
	}

	// Version must not have bumped — UPDATE still ran (it's
	// unconditional on the row), but the row's effective state is
	// unchanged. Note: the current PatchIssue UPDATE always bumps
	// version regardless of whether any column actually changed,
	// because the UPDATE landed (see ct.RowsAffected check). So we
	// allow version to bump by 1 — what we MUST NOT allow is acceptance
	// state churn or an audit row, both pinned above.
	if patched.Version != preVersion+1 {
		t.Fatalf("version = %d, want %d", patched.Version, preVersion+1)
	}
}

// TestPatch_AcceptedAt_PriorityOnly_PreservesAcceptance asserts that a
// patch which doesn't touch assignee leaves acceptance state alone.
// CASE-gated columns in the UPDATE statement must keep accepted_at /
// accepted_by exactly as they were (Q-6 = b).
func TestPatch_AcceptedAt_PriorityOnly_PreservesAcceptance(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "priority bump",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(creatorName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt == nil {
		t.Fatalf("precondition: AcceptedAt should be set after self-assign")
	}
	preAcceptedAt := *dto.AcceptedAt

	newPrio := 0
	patched, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{
			ExpectedVersion: dto.Version,
			Priority:        &newPrio,
		},
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}
	if patched.AcceptedAt == nil {
		t.Fatalf("AcceptedAt = nil; want %v (unchanged after priority-only patch)", preAcceptedAt)
	}
	if !patched.AcceptedAt.Equal(preAcceptedAt) {
		t.Fatalf("AcceptedAt = %v, want %v (unchanged after priority-only patch)",
			*patched.AcceptedAt, preAcceptedAt)
	}
	if patched.AcceptedBy == nil || *patched.AcceptedBy != creator {
		got := uuid.Nil
		if patched.AcceptedBy != nil {
			got = *patched.AcceptedBy
		}
		t.Fatalf("AcceptedBy = %v, want %v (unchanged after priority-only patch)", got, creator)
	}
}
