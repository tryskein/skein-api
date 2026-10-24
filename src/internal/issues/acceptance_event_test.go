package issues

// sk-303.13 / T021 — the acceptance event on every path without a
// delivery increase.
//
// Task smoke: claiming someone ELSE's unaccepted issue. This is the only
// fixture on which delivery is visible at all — on a self-claim the
// creator IS the actor, and the self-action exclusion cuts them off, so
// zero notifications fall out by construction and prove nothing.
//
// The full cardinality matrix across all paths, idempotence, and clearing
// the mark on reassignment — the owner task of row 35 (sk-303.13.2.4.9).

import (
	"testing"
)

// TestAcceptanceEvent_ClaimOfDelegatedIssue_JournalOnly (INV-5 + INV-6,
// SC-011): B claims an issue A filed and delegated.
//
//   - the acceptance stamp moves empty → filled, so exactly ONE
//     issue.accepted event is written, carrying the auto-accept reason;
//   - A is notified exactly ONCE about the whole action — through the
//     claim's status change. The acceptance event delivers nothing, so the
//     pair of events one user action produced does not become a pair of
//     notifications.
func TestAcceptanceEvent_ClaimOfDelegatedIssue_JournalOnly(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	worker := seedUser(t, pool)
	workerName := readUsername(t, pool, worker)
	pid, prefix := seedProject(t, pool, creator)

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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, worker, creator); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "delegated then claimed",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(workerName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("precondition: an issue delegated to somebody else starts unaccepted")
	}

	// Everything the creation itself delivered is out of scope here; clear
	// it so what remains was produced by the claim alone.
	if _, err := pool.Exec(t.Context(),
		`DELETE FROM notifications WHERE user_id = $1`, creator); err != nil {
		t.Fatalf("clear pre-claim notifications: %v", err)
	}

	if _, err := store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-t021-claim",
		Actor:     HumanActor(worker, workerName),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// 1. The transition is in the journal, exactly once, marked automatic.
	var accepted int
	var reason *string
	if err := pool.QueryRow(t.Context(), `
SELECT count(*), min(reason) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		dto.ID).Scan(&accepted, &reason); err != nil {
		t.Fatalf("count issue.accepted: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("issue.accepted events = %d, want exactly 1 (INV-5: no silent stamp)", accepted)
	}
	if reason == nil || *reason != "auto-accept" {
		var got string
		if reason != nil {
			got = *reason
		}
		t.Errorf("issue.accepted reason = %q, want \"auto-accept\"", got)
	}

	// 2. The acceptance event delivered nothing.
	var fromAccepted int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE e.entity_type='issue' AND e.entity_id=$1 AND e.type='issue.accepted'`,
		dto.ID).Scan(&fromAccepted); err != nil {
		t.Fatalf("count acceptance notifications: %v", err)
	}
	if fromAccepted != 0 {
		t.Errorf("the acceptance event materialised %d notification(s); automatic acceptance is journal-only", fromAccepted)
	}

	// 3. …and the creator still got exactly one notification for the whole
	//    action — the status change. More than one would be the second
	//    notification per user action INV-6 forbids; zero would mean the
	//    claim stopped notifying, which T021 did not intend either.
	rows, err := pool.Query(t.Context(), `
SELECT e.type
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1
 ORDER BY n.id`, creator)
	if err != nil {
		t.Fatalf("list creator notifications: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		types = append(types, typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(types) != 1 || types[0] != "issue.status_changed" {
		t.Errorf("creator notifications after the claim = %v, want exactly [issue.status_changed]", types)
	}
}

// TestAcceptanceEvent_SelfAssignPatch_JournalOnly (INV-5): re-pointing an
// issue at yourself with a patch stamps acceptance, and that stamp gets its
// event too — the third automatic path.
//
// The same test pins the reverse case the spec deliberately leaves alone: a
// patch that hands the issue to somebody ELSE clears the stamp and writes NO
// new acceptance event, and the historical one stays where it was (US-11).
func TestAcceptanceEvent_SelfAssignPatch_JournalOnly(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	worker := seedUser(t, pool)
	workerName := readUsername(t, pool, worker)
	third := seedUser(t, pool)
	thirdName := readUsername(t, pool, third)
	pid, prefix := seedProject(t, pool, creator)
	// INV-30 (sk-674.18 T028): an assignee resolves only to a person who may
	// work in the project — a member of it.
	addMember(t, pool, pid, worker, creator)
	addMember(t, pool, pid, third, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "unassigned", Type: "task", Priority: 2},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("precondition: an unassigned issue starts unaccepted")
	}

	// worker patches the assignee onto themselves → auto-accept.
	v := dto.Version
	patched, err := store.PatchIssue(t.Context(), dto.ID, PatchIssueInput{
		ExpectedVersion: v,
		Assignee:        ptrStr(workerName),
	}, HumanActor(worker, workerName))
	if err != nil {
		t.Fatalf("PatchIssue (self-assign): %v", err)
	}
	if patched.AcceptedAt == nil {
		t.Fatalf("precondition: self-assign on patch must stamp acceptance")
	}

	countAccepted := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
			dto.ID).Scan(&n); err != nil {
			t.Fatalf("count issue.accepted: %v", err)
		}
		return n
	}
	if got := countAccepted(); got != 1 {
		t.Fatalf("issue.accepted events after self-assign patch = %d, want 1 (INV-5)", got)
	}

	// Now hand it to a third person: the stamp is cleared, and that reverse
	// transition writes nothing — the invariant is one-directional.
	v2 := patched.Version
	reassigned, err := store.PatchIssue(t.Context(), dto.ID, PatchIssueInput{
		ExpectedVersion: v2,
		Assignee:        ptrStr(thirdName),
	}, HumanActor(worker, workerName))
	if err != nil {
		t.Fatalf("PatchIssue (reassign): %v", err)
	}
	if reassigned.AcceptedAt != nil {
		t.Fatalf("precondition: reassigning to somebody else clears the acceptance stamp")
	}
	if got := countAccepted(); got != 1 {
		t.Errorf("issue.accepted events after reassignment = %d, want still 1 — clearing the stamp neither adds an event nor deletes the historical one", got)
	}

	// Nothing about acceptance was delivered on either patch.
	var fromAccepted int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*)
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE e.entity_type='issue' AND e.entity_id=$1 AND e.type='issue.accepted'`,
		dto.ID).Scan(&fromAccepted); err != nil {
		t.Fatalf("count acceptance notifications: %v", err)
	}
	if fromAccepted != 0 {
		t.Errorf("acceptance notifications = %d, want 0 (automatic acceptance is journal-only)", fromAccepted)
	}
}
