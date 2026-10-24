package issues

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sk-118 (F2c) — standing transition matrix + nested-standing promote guard
// in Store.UpdateStatus, plus the Unclaim nextStatus=standing rejection.
//
// Matrix enforced (TO standing): open→✓, deferred→✓, closed→✓ (clears
// closed_at + closed_reason), in_progress→✗ (unclaim first); FROM standing:
// →open ✓ (sole demote), →in_progress ✗ (spawn a child), →deferred ✗ (demote
// to open first). R10: ANY transition INTO standing is rejected when the issue
// already has a standing parent OR standing child, in either representation
// (parent_id column OR parent-child dep edge).

// stcreate is a thin CreateIssue wrapper returning the created detail. Keeps
// each transition test to its essential arrange/act/assert.
func stcreate(t *testing.T, store *Store, pid uuid.UUID, prefix string, creator uuid.UUID, name, title string) *IssueDetail {
	t.Helper()
	dto, err := store.CreateIssue(t.Context(), pid, prefix, "counter",
		CreateIssueInput{Title: title, Type: "task", Priority: 2},
		creator, HumanActor(creator, name))
	if err != nil {
		t.Fatalf("CreateIssue %q: %v", title, err)
	}
	return dto
}

// readStatusVer reads the live status + version of an issue (after a
// setIssueStatus bypass that does not bump version).
func readStatusVer(t *testing.T, pool *pgxpool.Pool, id string) (string, int) {
	t.Helper()
	var status string
	var ver int
	if err := pool.QueryRow(t.Context(),
		`SELECT status, version FROM issues WHERE id = $1`, id).Scan(&status, &ver); err != nil {
		t.Fatalf("read status/version for %s: %v", id, err)
	}
	return status, ver
}

func TestTransition_OpenToStanding_Allowed(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "open→standing")
	got, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: iss.Version, Status: StatusStanding},
		HumanActor(creator, name))
	if err != nil {
		t.Fatalf("open→standing should be allowed, got: %v", err)
	}
	if got.Status != StatusStanding {
		t.Fatalf("status = %q, want standing", got.Status)
	}
	// Promoting a clean task must leave the lock + started_at untouched.
	if got.LockedBy != nil {
		t.Errorf("lockedBy = %v, want nil after clean promote", *got.LockedBy)
	}
	if got.StartedAt != nil {
		t.Errorf("startedAt = %v, want nil after clean promote", *got.StartedAt)
	}
}

func TestTransition_DeferredToStanding_Allowed(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "deferred→standing")
	setIssueStatus(t, pool, iss.ID, StatusDeferred)
	_, ver := readStatusVer(t, pool, iss.ID)

	got, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusStanding},
		HumanActor(creator, name))
	if err != nil {
		t.Fatalf("deferred→standing should be allowed, got: %v", err)
	}
	if got.Status != StatusStanding {
		t.Fatalf("status = %q, want standing", got.Status)
	}
}

func TestTransition_ClosedToStanding_ClearsClosedAt(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "closed→standing")
	// Close it (carries a reason → closed_reason set; closed_at stamped).
	closed, err := store.Close(t.Context(), CloseInput{
		ID:              iss.ID,
		ExpectedVersion: iss.Version,
		Reason:          "done for now",
		Actor:           HumanActor(creator, name),
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.ClosedAt == nil {
		t.Fatalf("precondition: closed_at should be set after Close")
	}

	got, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: closed.Version, Status: StatusStanding},
		HumanActor(creator, name))
	if err != nil {
		t.Fatalf("closed→standing should be allowed (reopen-as-standing), got: %v", err)
	}
	if got.Status != StatusStanding {
		t.Fatalf("status = %q, want standing", got.Status)
	}
	if got.ClosedAt != nil {
		t.Errorf("closed_at = %v, want nil after closed→standing reopen", *got.ClosedAt)
	}
	// Assert closed_reason cleared at the column level (DTO doesn't surface it).
	var reason *string
	if err := pool.QueryRow(t.Context(),
		`SELECT closed_reason FROM issues WHERE id = $1`, iss.ID).Scan(&reason); err != nil {
		t.Fatalf("read closed_reason: %v", err)
	}
	if reason != nil {
		t.Errorf("closed_reason = %q, want NULL after closed→standing reopen", *reason)
	}
}

func TestTransition_StandingToOpen_Allowed(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "standing→open")
	setIssueStatus(t, pool, iss.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, iss.ID)

	got, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusOpen},
		HumanActor(creator, name))
	if err != nil {
		t.Fatalf("standing→open should be allowed (demote), got: %v", err)
	}
	if got.Status != StatusOpen {
		t.Fatalf("status = %q, want open", got.Status)
	}
}

// TestTransition_StandingToDeferred_Rejected — standing → deferred is REJECTED
// (spec §11 / integration-tests-architecture §6 invariant 9): a standing task
// is a perpetual obligation, not a parkable unit of work. The only demote is
// standing → open. The reject leg must leave status + lock + version untouched
// and carry the cannot_defer_standing code.
func TestTransition_StandingToDeferred_Rejected(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "standing→deferred")
	setIssueStatus(t, pool, iss.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, iss.ID)

	_, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusDeferred},
		HumanActor(creator, name))
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "cannot_defer_standing" {
		t.Fatalf("err = %v, want validation code cannot_defer_standing", err)
	}
	// Reject path must leave status + version intact (no bump).
	status, gotVer := readStatusVer(t, pool, iss.ID)
	if status != StatusStanding {
		t.Errorf("status = %q after rejected standing→deferred, want standing", status)
	}
	if gotVer != ver {
		t.Errorf("version = %d after rejected standing→deferred, want %d (no bump)", gotVer, ver)
	}
	// No lock should have been taken on the reject path.
	var lockedBy *string
	if err := pool.QueryRow(t.Context(),
		`SELECT locked_by FROM issues WHERE id = $1`, iss.ID).Scan(&lockedBy); err != nil {
		t.Fatalf("read locked_by: %v", err)
	}
	if lockedBy != nil {
		t.Errorf("locked_by = %q after rejected standing→deferred, want NULL (no lock taken)", *lockedBy)
	}
}

func TestTransition_InProgressLockedToStanding_Rejected(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "in_progress(locked)→standing")
	// Claim → status in_progress + lock held by this session.
	claimed, err := store.Claim(t.Context(), ClaimInput{
		ID: iss.ID, SessionID: "sess-f2c-lock", Actor: HumanActor(creator, name),
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.Status != StatusInProgress || claimed.LockedBy == nil {
		t.Fatalf("precondition: want locked in_progress, got status=%q locked=%v", claimed.Status, claimed.LockedBy)
	}

	// Promote while holding the lock (force=false). Must be rejected with the
	// unclaim-first directive — and must NOT touch the lock.
	_, err = store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{
			ExpectedVersion: claimed.Version,
			Status:          StatusStanding,
			SessionID:       "sess-f2c-lock",
		}, HumanActor(creator, name))
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "cannot_promote_locked_to_standing" {
		t.Fatalf("err = %v, want validation code cannot_promote_locked_to_standing", err)
	}
	// Reject path must leave status + lock + version intact.
	status, ver := readStatusVer(t, pool, iss.ID)
	if status != StatusInProgress {
		t.Errorf("status = %q after rejected promote, want in_progress (unchanged)", status)
	}
	if ver != claimed.Version {
		t.Errorf("version = %d after rejected promote, want %d (no bump)", ver, claimed.Version)
	}
	var lockedBy *string
	if err := pool.QueryRow(t.Context(),
		`SELECT locked_by FROM issues WHERE id = $1`, iss.ID).Scan(&lockedBy); err != nil {
		t.Fatalf("read locked_by: %v", err)
	}
	if lockedBy == nil {
		t.Errorf("locked_by = NULL after rejected promote, want lock retained")
	}
}

func TestTransition_StandingToInProgress_Rejected(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "standing→in_progress")
	setIssueStatus(t, pool, iss.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, iss.ID)

	_, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusInProgress},
		HumanActor(creator, name))
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "cannot_work_standing_directly" {
		t.Fatalf("err = %v, want validation code cannot_work_standing_directly", err)
	}
	// Unchanged.
	if status, _ := readStatusVer(t, pool, iss.ID); status != StatusStanding {
		t.Errorf("status = %q after rejected standing→in_progress, want standing", status)
	}
}

// TestTransition_StandingToDeferred_HappyControl is the happy-path control: a
// plain open→deferred transition on a non-standing issue is wholly unaffected
// by the F2c matrix.
func TestTransition_OpenToDeferred_HappyControl(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "open→deferred control")
	got, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: iss.Version, Status: StatusDeferred},
		HumanActor(creator, name))
	if err != nil {
		t.Fatalf("open→deferred (control) should be allowed, got: %v", err)
	}
	if got.Status != StatusDeferred {
		t.Fatalf("status = %q, want deferred", got.Status)
	}
}

// --- R10: nested-standing promote guard, both directions, both reps ---

func TestPromote_ToStanding_WithStandingParent_Rejected_ParentIDColumn(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	parent := stcreate(t, store, pid, prefix, creator, name, "standing parent (col)")
	setIssueStatus(t, pool, parent.ID, StatusStanding)
	child := stcreate(t, store, pid, prefix, creator, name, "child (col)")
	// Legacy representation: issues.parent_id column points at the parent.
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET parent_id = $1 WHERE id = $2`, parent.ID, child.ID); err != nil {
		t.Fatalf("set parent_id: %v", err)
	}
	_, ver := readStatusVer(t, pool, child.ID)

	_, err := store.UpdateStatus(t.Context(), child.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusStanding},
		HumanActor(creator, name))
	assertNestedStandingRejected(t, err)
}

func TestPromote_ToStanding_WithStandingParent_Rejected_DepEdge(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	parent := stcreate(t, store, pid, prefix, creator, name, "standing parent (edge)")
	setIssueStatus(t, pool, parent.ID, StatusStanding)
	child := stcreate(t, store, pid, prefix, creator, name, "child (edge)")
	// Canonical representation: parent-child dep edge from child → parent.
	seedDep(t, pool, child.ID, parent.ID, "parent-child", creator)
	_, ver := readStatusVer(t, pool, child.ID)

	_, err := store.UpdateStatus(t.Context(), child.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusStanding},
		HumanActor(creator, name))
	assertNestedStandingRejected(t, err)
}

func TestPromote_ToStanding_WithStandingChild_Rejected_ParentIDColumn(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	parent := stcreate(t, store, pid, prefix, creator, name, "parent (col, child standing)")
	child := stcreate(t, store, pid, prefix, creator, name, "standing child (col)")
	setIssueStatus(t, pool, child.ID, StatusStanding)
	if _, err := pool.Exec(t.Context(),
		`UPDATE issues SET parent_id = $1 WHERE id = $2`, parent.ID, child.ID); err != nil {
		t.Fatalf("set parent_id: %v", err)
	}
	_, ver := readStatusVer(t, pool, parent.ID)

	_, err := store.UpdateStatus(t.Context(), parent.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusStanding},
		HumanActor(creator, name))
	assertNestedStandingRejected(t, err)
}

func TestPromote_ToStanding_WithStandingChild_Rejected_DepEdge(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	parent := stcreate(t, store, pid, prefix, creator, name, "parent (edge, child standing)")
	child := stcreate(t, store, pid, prefix, creator, name, "standing child (edge)")
	setIssueStatus(t, pool, child.ID, StatusStanding)
	seedDep(t, pool, child.ID, parent.ID, "parent-child", creator)
	_, ver := readStatusVer(t, pool, parent.ID)

	_, err := store.UpdateStatus(t.Context(), parent.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusStanding},
		HumanActor(creator, name))
	assertNestedStandingRejected(t, err)
}

func assertNestedStandingRejected(t *testing.T, err error) {
	t.Helper()
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "nested_standing_forbidden" {
		t.Fatalf("err = %v, want validation code nested_standing_forbidden", err)
	}
}

// TestDemote_StandingToOpen_Claimable verifies the demote round-trip: after
// standing→open the issue is claimable again (status outside the standing
// lifecycle, no lingering lock).
func TestDemote_StandingToOpen_Claimable(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "demote then claim")
	setIssueStatus(t, pool, iss.ID, StatusStanding)
	_, ver := readStatusVer(t, pool, iss.ID)

	demoted, err := store.UpdateStatus(t.Context(), iss.ID,
		UpdateStatusInput{ExpectedVersion: ver, Status: StatusOpen},
		HumanActor(creator, name))
	if err != nil {
		t.Fatalf("standing→open demote: %v", err)
	}
	if demoted.Status != StatusOpen {
		t.Fatalf("status = %q, want open", demoted.Status)
	}

	claimed, err := store.Claim(t.Context(), ClaimInput{
		ID: iss.ID, SessionID: "sess-f2c-demote", Actor: HumanActor(creator, name),
	})
	if err != nil {
		t.Fatalf("Claim after demote should succeed, got: %v", err)
	}
	if claimed.Status != StatusInProgress {
		t.Fatalf("status = %q after claim, want in_progress", claimed.Status)
	}
}

// TestUnclaim_NextStatusStanding_Rejected — Unclaim only releases the lock
// into {in_progress, open, deferred}; standing is rejected (mirrors the
// nextStatus=closed rejection). Closes the F2a finding where adding standing
// to the shared ValidateStatus set let direct store callers reach it.
func TestUnclaim_NextStatusStanding_Rejected(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "unclaim→standing")
	if _, err := store.Claim(t.Context(), ClaimInput{
		ID: iss.ID, SessionID: "sess-f2c-unclaim", Actor: HumanActor(creator, name),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	_, err := store.Unclaim(t.Context(), UnclaimInput{
		ID:         iss.ID,
		SessionID:  "sess-f2c-unclaim",
		NextStatus: StatusStanding,
		Reason:     "should be rejected",
		Actor:      HumanActor(creator, name),
	})
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "next_status_standing" {
		t.Fatalf("err = %v, want validation code next_status_standing", err)
	}
	// Lock + status untouched by the rejected unclaim.
	if status, _ := readStatusVer(t, pool, iss.ID); status != StatusInProgress {
		t.Errorf("status = %q after rejected unclaim, want in_progress", status)
	}
	var lockedBy *string
	if err := pool.QueryRow(t.Context(),
		`SELECT locked_by FROM issues WHERE id = $1`, iss.ID).Scan(&lockedBy); err != nil {
		t.Fatalf("read locked_by: %v", err)
	}
	if lockedBy == nil {
		t.Errorf("lock released by rejected unclaim — must be retained")
	}
}
