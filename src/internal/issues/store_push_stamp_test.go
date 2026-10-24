package issues

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readPushEligibility returns the push lifecycle state and reason of the
// notification row matching (userID, eventType, issue), or ("", "") when there
// is no such row. It joins via the event so it targets the row produced for a
// specific issue event.
//
// sk-303.13 / T010 replaced the legacy push_category stamp with the row's own
// DB-backed lifecycle: «owed a push» is now push_state='pending' on a row that
// already knows its reason, decided by the write-side filter rather than by a
// second UPDATE run after the fan-out. These tests kept their subjects and
// changed their instrument.
func readPushEligibility(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, issueID, eventType string) (state string, reason string) {
	t.Helper()
	var st string
	var rs *string
	err := pool.QueryRow(t.Context(), `
SELECT n.push_state, n.reason
  FROM notifications n
  JOIN events e ON e.id = n.event_id
 WHERE n.user_id = $1
   AND e.entity_type = 'issue' AND e.entity_id = $2
   AND e.type = $3
 ORDER BY n.id DESC
 LIMIT 1`, userID, issueID, eventType).Scan(&st, &rs)
	if err != nil {
		// No row → nothing is owed (caller asserts "").
		return "", ""
	}
	if rs == nil {
		return st, ""
	}
	return st, *rs
}

// assertPushOwed asserts the recipient's row for this event is owed a push for
// the named reason.
func assertPushOwed(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, issueID, eventType, reason string) {
	t.Helper()
	state, got := readPushEligibility(t, pool, userID, issueID, eventType)
	if state != "pending" {
		t.Errorf("push_state = %q, want \"pending\" (the recipient is owed a push)", state)
	}
	if got != reason {
		t.Errorf("reason = %q, want %q", got, reason)
	}
}

// assertNoPushOwed asserts the recipient is owed nothing: either there is no
// row at all, or the row carries no push.
func assertNoPushOwed(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, issueID, eventType string) {
	t.Helper()
	state, reason := readPushEligibility(t, pool, userID, issueID, eventType)
	if state != "" && state != "none" {
		t.Errorf("push_state = %q (reason %q), want no push owed", state, reason)
	}
}

func addMember(t *testing.T, pool *pgxpool.Pool, pid, user, addedBy uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3) ON CONFLICT DO NOTHING
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1 b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, user, addedBy); err != nil {
		t.Fatalf("seed member: %v", err)
	}
}

// PatchIssue that changes the assignee leaves the NEW assignee owed a push for
// "Assigned issue", and leaves a plain watcher owed nothing — the push
// audience of that reason is the new assignee, not everyone watching.
func TestPatchIssue_StampsAssignedOnNewAssignee(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	watcher := seedUser(t, pool)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, assignee, creator)
	addMember(t, pool, pid, watcher, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "patch me", Type: "task", Priority: 2},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// A third user watches the issue so FanOut creates a row for them on
	// issue.updated — they must NOT be stamped.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO issue_watchers (issue_id, user_id, reasons, since)
VALUES ($1, $2, ARRAY['manual'], now())
ON CONFLICT (issue_id, user_id) DO NOTHING`, dto.ID, watcher); err != nil {
		t.Fatalf("seed watcher: %v", err)
	}

	if _, err := store.PatchIssue(t.Context(), dto.ID,
		PatchIssueInput{ExpectedVersion: dto.Version, Assignee: ptrStr(assigneeName)},
		HumanActor(creator, creatorName)); err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}

	assertPushOwed(t, pool, assignee, dto.ID, "issue.updated", "assignment")
	assertNoPushOwed(t, pool, watcher, dto.ID, "issue.updated")
}

// An issue created ALREADY assigned to someone other than its author leaves
// that assignee owed an "Assigned issue" push — the more specific reason
// wins over "New issue" for the person the event is personally about.
func TestCreateIssue_StampsAssignedOnAssignee(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, assignee, creator)

	// sk-303.13 / T007: the fan-out now filters at write time against the
	// recipient's reason preferences. On issue.created the assignee is reached
	// through the SUBSCRIPTION path (they auto-watch the issue), i.e. as
	// "New issue" — a reason the narrow default leaves OFF. Without the row
	// there is nothing for the stamp to land on, so this test says out loud
	// which reason it depends on instead of relying on a default it does not
	// control.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, 'issue_created', true, false)
ON CONFLICT (user_id, reason) DO UPDATE SET feed_enabled = true`, assignee); err != nil {
		t.Fatalf("enable issue_created reason: %v", err)
	}

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "assigned at birth", Type: "task", Priority: 2, Assignee: ptrStr(assigneeName)},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	assertPushOwed(t, pool, assignee, dto.ID, "issue.created", "assignment")
}

// Explicit acceptance leaves the CREATOR owed a push: the person who filed the
// task is the one waiting for the acknowledgement.
func TestAccept_StampsAcceptedOnCreator(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, assignee, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "accept me", Type: "task", Priority: 2, Assignee: ptrStr(assigneeName)},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	v := dto.Version
	if _, err := store.Accept(t.Context(), AcceptInput{
		ID:              dto.ID,
		ExpectedVersion: &v,
		Actor:           HumanActor(assignee, assigneeName),
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	assertPushOwed(t, pool, creator, dto.ID, "issue.accepted", "accepted")
}

// delegatedIssue creates an issue assigned to someone OTHER than the creator
// (created_by = creator, assignee = assignee) and returns its detail. Helper
// for the delegated_status stamping tests.
func delegatedIssue(t *testing.T, store *Store, pool *pgxpool.Pool, pid uuid.UUID, prefix string, creator uuid.UUID, creatorName, assigneeName string) *IssueDetail {
	t.Helper()
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "delegated", Type: "task", Priority: 2, Assignee: ptrStr(assigneeName)},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	return dto
}

// Claim by a delegate (assignee ≠ creator) leaves the CREATOR owed a
// delegated-status push on the status_changed row (open → in_progress).
func TestClaim_StampsDelegatedStatusOnCreator(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, assignee, creator)

	store := NewStore(pool)
	dto := delegatedIssue(t, store, pool, pid, prefix, creator, creatorName, assigneeName)

	if _, err := store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-claim",
		Actor:     HumanActor(assignee, assigneeName),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	assertPushOwed(t, pool, creator, dto.ID, "issue.status_changed", "delegated_status")
}

// Unclaim with a real status transition (in_progress → open) by a delegate
// leaves the creator owed a delegated-status push.
func TestUnclaim_StampsDelegatedStatusOnCreator(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, assignee, creator)

	store := NewStore(pool)
	dto := delegatedIssue(t, store, pool, pid, prefix, creator, creatorName, assigneeName)

	if _, err := store.Claim(t.Context(), ClaimInput{
		ID:        dto.ID,
		SessionID: "sess-unclaim",
		Actor:     HumanActor(assignee, assigneeName),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Give up → open: status actually changes, so status_changed fires.
	if _, err := store.Unclaim(t.Context(), UnclaimInput{
		ID:         dto.ID,
		SessionID:  "sess-unclaim",
		NextStatus: "open",
		Reason:     "giving up",
		Actor:      HumanActor(assignee, assigneeName),
	}); err != nil {
		t.Fatalf("Unclaim: %v", err)
	}

	assertPushOwed(t, pool, creator, dto.ID, "issue.status_changed", "delegated_status")
}

// UpdateStatus by a delegate leaves the creator owed a delegated-status push.
func TestUpdateStatus_StampsDelegatedStatusOnCreator(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, assignee, creator)

	store := NewStore(pool)
	dto := delegatedIssue(t, store, pool, pid, prefix, creator, creatorName, assigneeName)

	if _, err := store.UpdateStatus(t.Context(), dto.ID,
		UpdateStatusInput{ExpectedVersion: dto.Version, Status: "in_progress"},
		HumanActor(assignee, assigneeName)); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	assertPushOwed(t, pool, creator, dto.ID, "issue.status_changed", "delegated_status")
}

// Close by a delegate leaves the creator owed a delegated-status push.
func TestClose_StampsDelegatedStatusOnCreator(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, assignee, creator)

	store := NewStore(pool)
	dto := delegatedIssue(t, store, pool, pid, prefix, creator, creatorName, assigneeName)

	if _, err := store.Close(t.Context(), CloseInput{
		ID:              dto.ID,
		ExpectedVersion: dto.Version,
		Reason:          "done",
		Actor:           HumanActor(assignee, assigneeName),
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assertPushOwed(t, pool, creator, dto.ID, "issue.status_changed", "delegated_status")
}

// When the CREATOR themselves changes status (self-action) they are the actor,
// excluded from the fan-out entirely — no row, nothing owed.
func TestUpdateStatus_NoStampWhenCreatorIsActor(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	// Issue assigned to the creator (not delegated) — creator acts on own task.
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "self task", Type: "task", Priority: 2, Assignee: ptrStr(creatorName)},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if _, err := store.UpdateStatus(t.Context(), dto.ID,
		UpdateStatusInput{ExpectedVersion: dto.Version, Status: "in_progress"},
		HumanActor(creator, creatorName)); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	assertNoPushOwed(t, pool, creator, dto.ID, "issue.status_changed")
}

// When the creator SELF-ASSIGNED the task (created_by == assignee) and a
// DIFFERENT member changes its status, the creator gets a status_changed row
// (they're a watcher, not the actor) — but it must NOT be owed a push: a
// self-assigned task isn't "delegated". Guards the assignee≠creator predicate
// of the delegating-creator audience.
func TestUpdateStatus_NoStampWhenCreatorIsAssignee(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	other := seedUser(t, pool)
	otherName := readUsername(t, pool, other)
	pid, prefix := seedProject(t, pool, creator)
	addMember(t, pool, pid, other, creator)

	store := NewStore(pool)
	// Self-assigned: created_by == assignee == creator.
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: "self assigned", Type: "task", Priority: 2, Assignee: ptrStr(creatorName)},
		creator, HumanActor(creator, creatorName))
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// A DIFFERENT member moves it → creator (watcher, not actor) gets a row.
	if _, err := store.UpdateStatus(t.Context(), dto.ID,
		UpdateStatusInput{ExpectedVersion: dto.Version, Status: "in_progress"},
		HumanActor(other, otherName)); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	assertNoPushOwed(t, pool, creator, dto.ID, "issue.status_changed")
}
