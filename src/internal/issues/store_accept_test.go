package issues

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestAccept_HappyPath_Assignee asserts the canonical flow: assignee
// calls Accept on a previously-unaccepted issue → accepted_at + by
// stamped, version+1, single issue.accepted event written.
func TestAccept_HappyPath_Assignee(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)

	// Create issue assigned to someone other than the creator → starts
	// with accepted_at = NULL (Phase 1 contract).
	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "to be accepted",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("precondition: AcceptedAt should be nil after assigning to other")
	}

	// Add assignee as project member so the visibility check passes
	// (Accept itself doesn't run visibility but other code paths in
	// this test suite assume membership). We're calling Accept directly
	// against the store so this is technically optional — added for
	// realism.
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, assignee, creator); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	expectedV := dto.Version
	got, err := store.Accept(t.Context(), AcceptInput{
		ID:              dto.ID,
		ExpectedVersion: &expectedV,
		Actor:           HumanActor(assignee, assigneeName),
	})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got.AcceptedAt == nil {
		t.Fatalf("AcceptedAt nil after accept; want now()")
	}
	if got.AcceptedBy == nil || *got.AcceptedBy != assignee {
		var av uuid.UUID
		if got.AcceptedBy != nil {
			av = *got.AcceptedBy
		}
		t.Fatalf("AcceptedBy = %v, want %v", av, assignee)
	}
	if got.Version != dto.Version+1 {
		t.Fatalf("version = %d, want %d", got.Version, dto.Version+1)
	}

	// Exactly one issue.accepted event.
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		dto.ID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 issue.accepted event, got %d", n)
	}
}

// TestAccept_Idempotent: second call on already-accepted issue is a
// silent no-op — no second event row, no second version bump.
func TestAccept_Idempotent(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	// Self-assign → auto-accept on create (Phase 1).
	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "self-accepted",
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
		t.Fatalf("precondition: self-assign should auto-accept")
	}
	preVersion := dto.Version
	preAcceptedAt := *dto.AcceptedAt

	// Calling Accept on this row should be a silent no-op.
	got, err := store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(creator, creatorName),
	})
	if err != nil {
		t.Fatalf("Accept (idempotent): %v", err)
	}
	if got.Version != preVersion {
		t.Fatalf("version bumped on idempotent accept: %d -> %d", preVersion, got.Version)
	}
	if got.AcceptedAt == nil || !got.AcceptedAt.Equal(preAcceptedAt) {
		t.Fatalf("AcceptedAt changed on idempotent accept: %v -> %v",
			preAcceptedAt, got.AcceptedAt)
	}

	// EXACTLY ONE issue.accepted event exists, and it is the one CreateIssue
	// wrote for the auto-accept at insert time (sk-303.13 / T021 — the
	// create path is no longer audit-quiet). The idempotent Accept added
	// NOTHING: there was no empty→filled transition to record, so the
	// second call is a no-op in the journal as well as in the row.
	//
	// Before T021 this same assertion read "0 events" because neither path
	// wrote one; the number changed for the create half, and the
	// idempotency half is what it always was.
	var n int
	var reason *string
	if err := pool.QueryRow(t.Context(), `
SELECT count(*), min(reason) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		dto.ID).Scan(&n, &reason); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 issue.accepted event (the create-time auto-accept, and nothing from the idempotent call), got %d", n)
	}
	if reason == nil || *reason != "auto-accept" {
		var got string
		if reason != nil {
			got = *reason
		}
		t.Errorf("issue.accepted reason = %q, want \"auto-accept\" — the one event here is the automatic one", got)
	}
}

// TestAccept_Forbidden_NonAssignee: a project member (not assignee, not
// admin) calling Accept gets ErrForbidden.
func TestAccept_Forbidden_NonAssignee(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	other := seedUser(t, pool)
	otherName := readUsername(t, pool, other)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "delegated",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// `other` calls Accept — not the assignee → forbidden.
	_, err = store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(other, otherName),
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}

// TestAccept_Admin_CannotAcceptForOther: only the assignee accepts — the
// project owner's rights and role=admin do not let anyone accept for
// another (US-6; the admin override went in sk-674.18 T017 and stays gone
// after sk-741 gave the admin the owner's rights). The admin is seeded with
// role='admin', so the refusal is not that of a plain user.
func TestAccept_Admin_CannotAcceptForOther(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	admin := seedAdminUser(t, pool)
	adminName := readUsername(t, pool, admin)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "admin-accept-target",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	_, err = store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(admin, adminName),
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("Accept by an admin who is not the assignee: error = %v, want ErrForbidden", err)
	}
}

// TestAccept_VersionConflict: stale ExpectedVersion → ErrVersionConflict
// with currentVersion echoed.
func TestAccept_VersionConflict(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	// INV-30 (sk-674.18 T028): an assignee resolves only to a person who may
	// work in the project — a member of it.
	addMember(t, pool, pid, assignee, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "version-target",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Bump version directly so caller's expectedVersion=current is stale.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET version=version+1, updated_at=now() WHERE id=$1`,
		dto.ID); err != nil {
		t.Fatalf("bump: %v", err)
	}

	stale := dto.Version // pre-bump value
	_, err = store.Accept(t.Context(), AcceptInput{
		ID:              dto.ID,
		ExpectedVersion: &stale,
		Actor:           HumanActor(assignee, assigneeName),
	})
	var verr *ErrVersionConflict
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want *ErrVersionConflict", err)
	}
	if verr.CurrentVersion != dto.Version+1 {
		t.Fatalf("currentVersion = %d, want %d", verr.CurrentVersion, dto.Version+1)
	}
}

// TestAccept_NoExpectedVersion_Skips: ExpectedVersion=nil disables the
// optimistic-concurrency check entirely. Accept proceeds even when the
// row was bumped under us.
func TestAccept_NoExpectedVersion_Skips(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)
	// INV-30 (sk-674.18 T028): an assignee resolves only to a person who may
	// work in the project — a member of it.
	addMember(t, pool, pid, assignee, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "no-version",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET version=version+5, updated_at=now() WHERE id=$1`,
		dto.ID); err != nil {
		t.Fatalf("bump: %v", err)
	}

	// nil ExpectedVersion → no check.
	got, err := store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(assignee, assigneeName),
	})
	if err != nil {
		t.Fatalf("Accept (nil expectedVersion): %v", err)
	}
	if got.AcceptedAt == nil {
		t.Fatalf("AcceptedAt nil after accept")
	}
}

// TestAccept_IssueNotFound: missing id → ErrNotFound.
func TestAccept_IssueNotFound(t *testing.T) {
	pool := newTestPool(t)
	user := seedUser(t, pool)
	uname := readUsername(t, pool, user)

	store := NewStore(pool)
	_, err := store.Accept(t.Context(), AcceptInput{
		ID:    "nope-9999",
		Actor: HumanActor(user, uname),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestAccept_NoAssignee_Forbidden: an issue with no assignee at all —
// non-admin caller can't accept (would otherwise be a free-for-all).
func TestAccept_NoAssignee_Forbidden(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "unassigned",
			Type:     "task",
			Priority: 2,
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	_, err = store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(creator, creatorName),
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden (no assignee, non-admin)", err)
	}
}

// TestAccept_FanOut_NotifiesCreator: assignee accepts → creator
// receives a notification row keyed off the issue.accepted event.
func TestAccept_FanOut_NotifiesCreator(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)

	// CreateIssue auto-watches the creator on the issue (R1.6 contract),
	// so they're in the issue_watchers pool when the event fires.
	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "notify-creator",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	// Add the assignee as a project member so the FanOut membership
	// recheck doesn't drop them (admin-bypass would also work but
	// straightforward membership keeps the test self-contained).
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, assignee, creator); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	if _, err := store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(assignee, assigneeName),
	}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// Look up the event id for issue.accepted, then count notification
	// rows pointing at it for the creator.
	var eventID int64
	if err := pool.QueryRow(t.Context(), `
SELECT id FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'
 ORDER BY id DESC LIMIT 1`,
		dto.ID).Scan(&eventID); err != nil {
		t.Fatalf("read event id: %v", err)
	}

	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM notifications
 WHERE event_id=$1 AND user_id=$2 AND type='issue.accepted'`,
		eventID, creator).Scan(&n); err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 notification for creator, got %d", n)
	}
}

// TestAccept_CannotAcceptClosed: Accept on a closed issue returns
// ErrCannotAcceptClosed without mutating the row — no version bump, no
// accepted_at stamp, no event, no notification (sk-25). Mirrors
// TestClaim_CannotClaimClosed's contract for the "ack work" verb.
func TestAccept_CannotAcceptClosed(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "to be closed before accept",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if dto.AcceptedAt != nil {
		t.Fatalf("precondition: AcceptedAt should be nil after assigning to other")
	}

	// Close the issue directly via UPDATE — we're testing Accept's
	// guard, not Close itself, so we skip the full Close ritual
	// (lock, events, notifications).
	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET status='closed', closed_at=now(), closed_reason='test setup',
                  version=version+1, updated_at=now()
 WHERE id=$1`, dto.ID); err != nil {
		t.Fatalf("close issue (setup): %v", err)
	}
	closedVersion := dto.Version + 1

	// Assignee tries to Accept. Expect ErrCannotAcceptClosed.
	_, err = store.Accept(t.Context(), AcceptInput{
		ID:              dto.ID,
		ExpectedVersion: &closedVersion,
		Actor:           HumanActor(assignee, assigneeName),
	})
	if !errors.Is(err, ErrCannotAcceptClosed) {
		t.Fatalf("error = %v, want ErrCannotAcceptClosed", err)
	}

	// Row was NOT mutated: version stays at closedVersion,
	// accepted_at stays NULL, no issue.accepted event was written.
	var version int
	var acceptedAt *string
	if err := pool.QueryRow(t.Context(),
		`SELECT version, accepted_at::text FROM issues WHERE id=$1`, dto.ID,
	).Scan(&version, &acceptedAt); err != nil {
		t.Fatalf("re-read row: %v", err)
	}
	if version != closedVersion {
		t.Fatalf("version = %d, want %d (no bump after rejected Accept)",
			version, closedVersion)
	}
	if acceptedAt != nil {
		t.Fatalf("accepted_at = %v, want NULL (no stamp after rejected Accept)",
			*acceptedAt)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND type='issue.accepted'`,
		dto.ID).Scan(&n); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 issue.accepted events, got %d", n)
	}
}

// TestAccept_CannotAcceptClosed_PrecedesPermissionGate: the closed
// guard fires BEFORE the permission gate, so a non-assignee asking to
// accept a closed row gets the precise reason (cannot_accept_closed),
// not a misleading ErrForbidden. Documents the ordering invariant.
func TestAccept_CannotAcceptClosed_PrecedesPermissionGate(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	creatorName := readUsername(t, pool, creator)
	assignee := seedUser(t, pool)
	assigneeName := readUsername(t, pool, assignee)
	other := seedUser(t, pool)
	otherName := readUsername(t, pool, other)
	pid, prefix := seedProject(t, pool, creator)

	store := NewStore(pool)
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{
			Title:    "closed and foreign",
			Type:     "task",
			Priority: 2,
			Assignee: ptrStr(assigneeName),
		},
		creator,
		HumanActor(creator, creatorName),
	)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET status='closed', closed_at=now(), closed_reason='test setup',
                  version=version+1, updated_at=now()
 WHERE id=$1`, dto.ID); err != nil {
		t.Fatalf("close issue (setup): %v", err)
	}

	// `other` is neither admin nor assignee. If the gates ran in the
	// wrong order this would return ErrForbidden — we assert the
	// status guard wins.
	_, err = store.Accept(t.Context(), AcceptInput{
		ID:    dto.ID,
		Actor: HumanActor(other, otherName),
	})
	if !errors.Is(err, ErrCannotAcceptClosed) {
		t.Fatalf("error = %v, want ErrCannotAcceptClosed (must precede ErrForbidden)", err)
	}
}
