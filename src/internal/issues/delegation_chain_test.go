package issues

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/authz"
	"skein-api/internal/projects"
)

// sk-34 — delegation chain coverage.
//
// The matrix below exercises every interesting transition on the new
// previous_assignees + dismissed_by columns. Each test creates a fresh
// (pool, project, users) triple so they can run in any order under
// SKEIN_TEST_PARALLEL_PKGS=1.
//
// All tests query the DB directly (via the helpers below) rather than
// leaning on IssueDetail's new fields so a regression in scan-shape
// can't hide a regression in the actual array contents.

// readDelegationChain reads (previous_assignees, dismissed_by) directly
// from the row. Returning slices keeps the assertion sites readable.
func readDelegationChain(t *testing.T, ctx context.Context, store *Store, id string) ([]uuid.UUID, []uuid.UUID) {
	t.Helper()
	var prev, dismissed []uuid.UUID
	if err := store.pool.QueryRow(ctx,
		`SELECT previous_assignees, dismissed_by FROM issues WHERE id = $1`, id,
	).Scan(&prev, &dismissed); err != nil {
		t.Fatalf("readDelegationChain: %v", err)
	}
	return prev, dismissed
}

// containsUUID — small helper for the assertion sites.
func containsUUID(xs []uuid.UUID, x uuid.UUID) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// listDelegated runs the project-scoped ListIssues with createdBy=me
// shaped exactly the way the handler does. Returns the set of issue
// ids so the assertions can be set-equality.
func listDelegated(t *testing.T, ctx context.Context, store *Store, pid uuid.UUID, caller uuid.UUID, callerName string) map[string]struct{} {
	t.Helper()
	res, err := store.ListIssues(ctx, ListIssuesArgs{
		ProjectID:      pid,
		Caller:         &caller,
		CallerUsername: callerName,
		CreatedBy:      &caller,
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	out := make(map[string]struct{}, len(res.Items))
	for _, it := range res.Items {
		out[it.ID] = struct{}{}
	}
	return out
}

// TestDelegation_T1_ReassignAppendsOldAssignee — Alice creates → Bob
// assignee → Bob reassigns to Charlie. previous_assignees gains Bob;
// dismissed_by stays empty; Bob's Delegated list returns the issue.
func TestDelegation_T1_ReassignAppendsOldAssignee(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	charlie := seedUser(t, pool)
	charlieName := readUsername(t, pool, charlie)
	pid, prefix := seedProject(t, pool, alice)
	// The delegates work in the project: since sk-674.18 a handle of a
	// person without access is free text and never joins the chain
	// (INV-30).
	for _, u := range []uuid.UUID{bob, charlie} {
		addProjectMember(t, pool, pid, u, alice)
	}

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "delegated-chain",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(bobName),
		},
		alice,
		HumanActor(alice, aliceName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Bob reassigns to Charlie. We use Bob as the actor — the patch
	// logic uses the supplied actor for self-assign detection only;
	// the assignee diff is computed from the resolved usernames.
	patched, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{
			ExpectedVersion: dto.Version,
			Assignee:        ptrStr(charlieName),
		},
		HumanActor(bob, bobName),
	)
	if err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}
	if patched.Assignee == nil || *patched.Assignee != charlieName {
		t.Fatalf("post-patch assignee = %v, want %s", patched.Assignee, charlieName)
	}

	prev, dismissed := readDelegationChain(t, t.Context(), store, dto.ID)
	if !containsUUID(prev, bob) {
		t.Fatalf("previous_assignees = %v, want to contain bob %v", prev, bob)
	}
	if containsUUID(prev, charlie) {
		t.Fatalf("previous_assignees = %v, must NOT contain charlie %v (current assignee)", prev, charlie)
	}
	if len(dismissed) != 0 {
		t.Fatalf("dismissed_by = %v, want empty", dismissed)
	}

	// Bob's Delegated lens should include this issue.
	ids := listDelegated(t, t.Context(), store, pid, bob, bobName)
	if _, ok := ids[dto.ID]; !ok {
		t.Fatalf("Bob's Delegated lens missing %s; got %v", dto.ID, ids)
	}
}

// TestDelegation_T2_DismissExcludesFromDelegated — Bob in the chain
// calls DismissDelegated. previous_assignees still contains Bob, but
// dismissed_by also contains him; Delegated lens excludes the issue.
func TestDelegation_T2_DismissExcludesFromDelegated(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	charlie := seedUser(t, pool)
	charlieName := readUsername(t, pool, charlie)
	pid, prefix := seedProject(t, pool, alice)
	// The delegates work in the project: since sk-674.18 a handle of a
	// person without access is free text and never joins the chain
	// (INV-30).
	for _, u := range []uuid.UUID{bob, charlie} {
		addProjectMember(t, pool, pid, u, alice)
	}

	store := NewStore(pool)
	dto, _ := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "t2", Type: "task", Priority: 2, Assignee: ptrStr(bobName)},
		alice, HumanActor(alice, aliceName),
	)
	patched, _ := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: dto.Version, Assignee: ptrStr(charlieName)},
		HumanActor(bob, bobName),
	)
	_ = patched

	// Bob drops. After this the Delegated lens for Bob must be empty.
	if _, err := store.DismissDelegated(t.Context(),
		DelegationActionInput{ID: dto.ID, Actor: HumanActor(bob, bobName)},
	); err != nil {
		t.Fatalf("DismissDelegated: %v", err)
	}

	prev, dismissed := readDelegationChain(t, t.Context(), store, dto.ID)
	if !containsUUID(prev, bob) {
		t.Fatalf("previous_assignees lost Bob after dismiss: %v", prev)
	}
	if !containsUUID(dismissed, bob) {
		t.Fatalf("dismissed_by missing Bob: %v", dismissed)
	}

	ids := listDelegated(t, t.Context(), store, pid, bob, bobName)
	if _, ok := ids[dto.ID]; ok {
		t.Fatalf("dismissed issue %s still in Bob's Delegated lens", dto.ID)
	}
}

// TestDelegation_T3_ReassignToFormerClearsArrays — Charlie reassigns
// back to Bob. Bob should be removed from BOTH previous_assignees and
// dismissed_by; Charlie should appear in previous_assignees.
func TestDelegation_T3_ReassignToFormerClearsArrays(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	charlie := seedUser(t, pool)
	charlieName := readUsername(t, pool, charlie)
	pid, prefix := seedProject(t, pool, alice)
	// The delegates work in the project: since sk-674.18 a handle of a
	// person without access is free text and never joins the chain
	// (INV-30).
	for _, u := range []uuid.UUID{bob, charlie} {
		addProjectMember(t, pool, pid, u, alice)
	}

	store := NewStore(pool)
	dto, _ := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "t3", Type: "task", Priority: 2, Assignee: ptrStr(bobName)},
		alice, HumanActor(alice, aliceName),
	)

	// Bob → Charlie.
	p1, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: dto.Version, Assignee: ptrStr(charlieName)},
		HumanActor(bob, bobName),
	)
	if err != nil {
		t.Fatalf("PatchIssue (Bob→Charlie): %v", err)
	}
	// Bob dismisses.
	if _, err := store.DismissDelegated(t.Context(),
		DelegationActionInput{ID: dto.ID, Actor: HumanActor(bob, bobName)},
	); err != nil {
		t.Fatalf("DismissDelegated: %v", err)
	}
	// Charlie reassigns back to Bob. Re-read the issue post-dismiss in
	// case the dismiss bumped version (it shouldn't, but the test must
	// remain correct regardless).
	curr, err := store.GetIssue(t.Context(), dto.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: curr.Version, Assignee: ptrStr(bobName)},
		HumanActor(charlie, charlieName),
	); err != nil {
		t.Fatalf("reassign back: %v (p1.Version=%d, curr.Version=%d)", err, p1.Version, curr.Version)
	}

	prev, dismissed := readDelegationChain(t, t.Context(), store, dto.ID)
	if containsUUID(prev, bob) {
		t.Fatalf("previous_assignees still contains Bob (current assignee): %v", prev)
	}
	if !containsUUID(prev, charlie) {
		t.Fatalf("previous_assignees missing Charlie (former assignee): %v", prev)
	}
	if containsUUID(dismissed, bob) {
		t.Fatalf("dismissed_by still contains Bob after reassign-back: %v", dismissed)
	}

	// Bob is now the current assignee; Delegated lens excludes him.
	ids := listDelegated(t, t.Context(), store, pid, bob, bobName)
	if _, ok := ids[dto.ID]; ok {
		t.Fatalf("Delegated lens includes Bob's current assignment: %v", ids)
	}
}

// TestDelegation_T4_ReassignClearsDismissAndReappendsBob — after T3,
// if Bob reassigns the task to Dave he should re-appear in
// previous_assignees AND must NOT be in dismissed_by (cleared on the
// reassign-back step in T3 lineage). Delegated for Bob includes it.
func TestDelegation_T4_ReassignClearsDismissAndReappendsBob(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	charlie := seedUser(t, pool)
	charlieName := readUsername(t, pool, charlie)
	dave := seedUser(t, pool)
	daveName := readUsername(t, pool, dave)
	pid, prefix := seedProject(t, pool, alice)
	// The delegates work in the project: since sk-674.18 a handle of a
	// person without access is free text and never joins the chain
	// (INV-30).
	for _, u := range []uuid.UUID{bob, charlie, dave} {
		addProjectMember(t, pool, pid, u, alice)
	}

	store := NewStore(pool)
	dto, _ := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "t4", Type: "task", Priority: 2, Assignee: ptrStr(bobName)},
		alice, HumanActor(alice, aliceName),
	)
	// Bob → Charlie.
	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: dto.Version, Assignee: ptrStr(charlieName)},
		HumanActor(bob, bobName),
	); err != nil {
		t.Fatalf("Bob→Charlie: %v", err)
	}
	// Bob dismisses.
	if _, err := store.DismissDelegated(t.Context(),
		DelegationActionInput{ID: dto.ID, Actor: HumanActor(bob, bobName)},
	); err != nil {
		t.Fatalf("DismissDelegated: %v", err)
	}
	// Re-read pre-each-PatchIssue so we never go stale.
	curr, _ := store.GetIssue(t.Context(), dto.ID)
	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: curr.Version, Assignee: ptrStr(bobName)},
		HumanActor(charlie, charlieName),
	); err != nil {
		t.Fatalf("Charlie→Bob: %v", err)
	}
	curr, _ = store.GetIssue(t.Context(), dto.ID)
	// Bob → Dave (should re-append Bob to previous_assignees, dismissed_by stays clean).
	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: curr.Version, Assignee: ptrStr(daveName)},
		HumanActor(bob, bobName),
	); err != nil {
		t.Fatalf("reassign Bob→Dave: %v", err)
	}

	prev, dismissed := readDelegationChain(t, t.Context(), store, dto.ID)
	if !containsUUID(prev, bob) {
		t.Fatalf("previous_assignees missing Bob after Bob→Dave: %v", prev)
	}
	if containsUUID(dismissed, bob) {
		t.Fatalf("dismissed_by still contains Bob after reassign-back+reassign-out cycle: %v", dismissed)
	}

	ids := listDelegated(t, t.Context(), store, pid, bob, bobName)
	if _, ok := ids[dto.ID]; !ok {
		t.Fatalf("Delegated lens missing %s for Bob after re-entering the chain: %v", dto.ID, ids)
	}
}

// TestDelegation_T5_CreatorAlwaysInDelegated — Alice creates → assigns
// Bob → Bob dismisses (irrelevant for Alice). Alice's Delegated lens
// MUST still include the issue regardless of dismissed_by content.
// Alice is the creator; she has no opt-out path. (dismissed_by is the
// chain-member opt-out, not a creator opt-out.)
func TestDelegation_T5_CreatorAlwaysInDelegated(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	pid, prefix := seedProject(t, pool, alice)

	store := NewStore(pool)
	dto, _ := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "t5", Type: "task", Priority: 2, Assignee: ptrStr(bobName)},
		alice, HumanActor(alice, aliceName),
	)
	// Even if Alice somehow ended up in dismissed_by (she can't via
	// the public API since she's the creator, not a chain member —
	// but the predicate must still surface the issue for her), the
	// created_by branch of the OR-chain wins.
	_, err := store.pool.Exec(t.Context(),
		`UPDATE issues SET dismissed_by = array_append(dismissed_by, $2::uuid) WHERE id = $1`,
		dto.ID, alice,
	)
	if err != nil {
		t.Fatalf("manual dismissed_by inject: %v", err)
	}

	ids := listDelegated(t, t.Context(), store, pid, alice, aliceName)
	if _, ok := ids[dto.ID]; !ok {
		t.Fatalf("Alice (creator) missing from Delegated despite dismissed_by injection: %v", ids)
	}
}

// TestDelegation_T6_CurrentAssigneeExcluded — Charlie reassigns to
// Alice (the creator). Alice's Delegated lens MUST NOT include the
// issue because she's now the current assignee (the lens hides tasks
// you're currently working on — that's the On-me lens's job).
func TestDelegation_T6_CurrentAssigneeExcluded(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	charlie := seedUser(t, pool)
	charlieName := readUsername(t, pool, charlie)
	pid, prefix := seedProject(t, pool, alice)

	store := NewStore(pool)
	dto, _ := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "t6", Type: "task", Priority: 2, Assignee: ptrStr(bobName)},
		alice, HumanActor(alice, aliceName),
	)
	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: dto.Version, Assignee: ptrStr(charlieName)},
		HumanActor(bob, bobName),
	); err != nil {
		t.Fatalf("Bob→Charlie: %v", err)
	}
	curr, _ := store.GetIssue(t.Context(), dto.ID)
	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: curr.Version, Assignee: ptrStr(aliceName)},
		HumanActor(charlie, charlieName),
	); err != nil {
		t.Fatalf("reassign Charlie→Alice: %v", err)
	}

	ids := listDelegated(t, t.Context(), store, pid, alice, aliceName)
	if _, ok := ids[dto.ID]; ok {
		t.Fatalf("Delegated lens for current-assignee Alice contains %s; lens should hide it", dto.ID)
	}
}

// TestDelegation_T7_ProjectCounterReflectsDismiss — project's
// myOutboxOpenCount drops when a chain-member dismisses. Verifies the
// counter SQL in list_search.go / store.go honors the new predicate.
func TestDelegation_T7_ProjectCounterReflectsDismiss(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	charlie := seedUser(t, pool)
	charlieName := readUsername(t, pool, charlie)
	pid, prefix := seedProject(t, pool, alice)

	// Add Bob as a project member so his counter query sees the project.
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, bob, alice); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	store := NewStore(pool)
	dto, _ := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "t7-counter", Type: "task", Priority: 2, Assignee: ptrStr(bobName)},
		alice, HumanActor(alice, aliceName),
	)
	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: dto.Version, Assignee: ptrStr(charlieName)},
		HumanActor(bob, bobName),
	); err != nil {
		t.Fatalf("reassign Bob→Charlie: %v", err)
	}

	// The counter as the product serves it: projects.Store.ListProjects,
	// read as Bob (a project member).
	if n := projectOutboxOpenCount(t, pool, pid, bob); n != 1 {
		t.Fatalf("counter before dismiss = %d, want 1", n)
	}

	if _, err := store.DismissDelegated(t.Context(),
		DelegationActionInput{ID: dto.ID, Actor: HumanActor(bob, bobName)},
	); err != nil {
		t.Fatalf("DismissDelegated: %v", err)
	}

	if n := projectOutboxOpenCount(t, pool, pid, bob); n != 0 {
		t.Fatalf("counter after dismiss = %d, want 0", n)
	}
}

// TestDelegation_T8_CreatorWithNilAssigneeNotInDelegated — sk-37
// (regression-guard for the demo-5 narrowing of the Delegated lens).
//
// Alice creates an issue with NO assignee. She is the creator. The
// Delegated lens MUST exclude this issue from her Outbox view: a task
// with no assignee isn't actually delegated to anyone — it's just
// unowned. Pre-demo-5 the predicate was
// `(assignee IS NULL OR assignee != me)` which leaked unowned tasks
// into the creator's Outbox; demo-5 narrowed it to
// `assignee IS NOT NULL AND assignee != me`. This test pins that
// semantic so the next refactor can't quietly re-introduce the leak.
//
// Both surfaces must agree (list ↔ counter) — if they diverge the
// project-row badge desyncs from the actual Outbox list and the user
// sees a "12 open" pill above an empty list (or vice versa).
//
// Surfaces exercised:
//   - ListIssues with CreatedBy=Alice (project-scoped Delegated list)
//   - The project's myOutboxOpenCount as projects.Store.ListProjects
//     returns it
func TestDelegation_T8_CreatorWithNilAssigneeNotInDelegated(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	pid, prefix := seedProject(t, pool, alice)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "t8-no-assignee",
			Type:     "task",
			Priority: 2,
			// Assignee intentionally nil — exercises the creator-with-
			// no-assignee branch of the Delegated predicate. Post-demo-5
			// this branch must NOT admit the issue into Delegated.
		},
		alice, HumanActor(alice, aliceName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// List surface: Alice's Delegated list MUST NOT include the issue.
	// If it does, the demo-5 narrowing has been reverted somewhere in
	// the ListIssues / ListByUser predicate chain.
	ids := listDelegated(t, t.Context(), store, pid, alice, aliceName)
	if _, ok := ids[dto.ID]; ok {
		t.Fatalf("creator-with-nil-assignee leaked into Delegated list (demo-5 regression): %v", ids)
	}

	// Counter surface: the project's myOutboxOpenCount as ListProjects
	// serves it to Alice. It must be 0 for an unassigned creator-issue; a
	// non-zero result means the counter and the list have drifted apart.
	if n := projectOutboxOpenCount(t, pool, pid, alice); n != 0 {
		t.Fatalf("my_outbox_open_count for creator-with-nil-assignee = %d, want 0 (demo-5: unassigned issues are not Delegated)", n)
	}
}

// TestDelegation_T9_NonChainMemberCannotDismiss — sk-34 review Go-W2.
// A project member who is neither the creator nor a past assignee
// (a "bystander") must NOT be allowed to call DismissDelegated.
// Allowing it would let any project member scribble onto dismissed_by
// on arbitrary issues and silently install sticky-unwatch rows
// they have no business owning.
//
// Expected: ValidationError with Code = "not_in_chain".
func TestDelegation_T9_NonChainMemberCannotDismiss(t *testing.T) {
	pool := newTestPool(t)
	alice := seedUser(t, pool)
	aliceName := readUsername(t, pool, alice)
	bob := seedUser(t, pool)
	bobName := readUsername(t, pool, bob)
	bystander := seedUser(t, pool)
	bystanderName := readUsername(t, pool, bystander)
	pid, prefix := seedProject(t, pool, alice)

	// bystander is a project member with visibility but is NOT in the
	// chain — he didn't create the issue and was never the assignee.
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, bystander, alice); err != nil {
		t.Fatalf("seed bystander member: %v", err)
	}

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "t9", Type: "task", Priority: 2, Assignee: ptrStr(bobName)},
		alice, HumanActor(alice, aliceName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	_, err = store.DismissDelegated(t.Context(),
		DelegationActionInput{ID: dto.ID, Actor: HumanActor(bystander, bystanderName)},
	)
	if err == nil {
		t.Fatalf("DismissDelegated by bystander succeeded; expected not_in_chain validation error")
	}
	ve, ok := IsValidationError(err)
	if !ok {
		t.Fatalf("DismissDelegated returned %T (%v); expected *ValidationError", err, err)
	}
	if ve.Code != "not_in_chain" {
		t.Fatalf("DismissDelegated returned code=%q; want not_in_chain", ve.Code)
	}

	// Defensive: bystander must NOT be in dismissed_by AND there must
	// be no issue_watchers row for him (sticky-unwatch must not have
	// been installed by a rejected call).
	var dismissedHasBystander bool
	if err := pool.QueryRow(t.Context(),
		`SELECT $2::uuid = ANY(dismissed_by) FROM issues WHERE id = $1`,
		dto.ID, bystander,
	).Scan(&dismissedHasBystander); err != nil {
		t.Fatalf("dismissed_by readback: %v", err)
	}
	if dismissedHasBystander {
		t.Fatalf("bystander leaked into dismissed_by despite rejection")
	}
	var watcherRows int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		dto.ID, bystander,
	).Scan(&watcherRows); err != nil {
		t.Fatalf("watcher readback: %v", err)
	}
	if watcherRows != 0 {
		t.Fatalf("bystander leaked into issue_watchers despite rejection (rows=%d)", watcherRows)
	}

	// Bob (in chain via current assignee → previous_assignees on next
	// reassign, but right now he's the CURRENT assignee) — the
	// chain-membership check should reject him too because he's not yet
	// in previous_assignees and he's not the creator. The Delegated
	// affordance is only meaningful for past chain members; a current
	// assignee uses Accept/On-me, not Drop. This is a regression guard
	// to make sure the "current assignee" branch doesn't slip past the
	// chain check (created_by check rejects, previous_assignees
	// membership rejects because Bob isn't there yet).
	_, err = store.DismissDelegated(t.Context(),
		DelegationActionInput{ID: dto.ID, Actor: HumanActor(bob, bobName)},
	)
	if err == nil {
		t.Fatalf("DismissDelegated by current-assignee Bob succeeded; expected not_in_chain")
	}
	ve, ok = IsValidationError(err)
	if !ok || ve.Code != "not_in_chain" {
		t.Fatalf("DismissDelegated by current-assignee returned %v; want not_in_chain", err)
	}
}

// projectOutboxOpenCount reads the project's myOutboxOpenCount the way the
// product serves it — projects.Store.ListProjects for the caller — rather
// than through a copy of its SQL, so a change to the real counter shows up
// here.
func projectOutboxOpenCount(t *testing.T, pool *pgxpool.Pool, pid, caller uuid.UUID) int {
	t.Helper()
	res, err := projects.NewStore(pool).ListProjects(
		authz.WithUnrestricted(context.Background()),
		projects.ListProjectsArgs{CallerID: caller},
	)
	if err != nil {
		t.Fatalf("ListProjects as %s: %v", caller, err)
	}
	for i := range res.Items {
		if res.Items[i].ID == pid {
			return res.Items[i].MyOutboxOpenCount
		}
	}
	t.Fatalf("project %s not in ListProjects for %s", pid, caller)
	return 0
}
