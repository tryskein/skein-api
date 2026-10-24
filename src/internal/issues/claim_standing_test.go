package issues

import (
	"strings"
	"testing"
)

// sk-118 (F2d) — Store.Claim rejects a standing issue with the actionable
// `cannot_claim_standing` ValidationError. Standing tasks are never worked
// directly; the directive tells the caller to spawn a child subtask and claim
// that instead. The reject path must be wholly side-effect-free: no status
// flip, no locked_by, no started_at, no version bump, no event — mirroring the
// closed-claim guard. This is the exact regression codex asked for in the F2c
// review (the bug: Store.Claim only rejected closed, so skein_claim could move
// a standing issue to in_progress).

// TestClaim_StandingRejected: claiming a standing issue surfaces
// ErrCannotClaimStanding with the directive message.
func TestClaim_StandingRejected(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "standing-claim-rejected")
	setIssueStatus(t, pool, iss.ID, StatusStanding)

	_, err := store.Claim(t.Context(), ClaimInput{
		ID:        iss.ID,
		SessionID: "sess-f2d-reject",
		Actor:     HumanActor(creator, name),
	})
	ve, ok := IsValidationError(err)
	if !ok || ve.Code != "cannot_claim_standing" {
		t.Fatalf("err = %v, want validation code cannot_claim_standing", err)
	}
	// Directive must point the caller at the spawn-a-child workaround.
	for _, want := range []string{"spawn", "child", "parentId"} {
		if !strings.Contains(ve.Message, want) {
			t.Errorf("message = %q, want it to mention %q", ve.Message, want)
		}
	}
}

// TestClaim_StandingRejected_NoPartialClaim is the codex-requested regression:
// set an issue standing, call Claim, and assert status / locked_by / version /
// started_at are ALL unchanged (no partial claim, no event).
func TestClaim_StandingRejected_NoPartialClaim(t *testing.T) {
	pool := newTestPool(t)
	creator := seedUser(t, pool)
	name := readUsername(t, pool, creator)
	pid, prefix := seedProject(t, pool, creator)
	store := NewStore(pool)

	iss := stcreate(t, store, pid, prefix, creator, name, "standing-no-partial")
	setIssueStatus(t, pool, iss.ID, StatusStanding)
	// setIssueStatus bypasses the version bump, so the live version is the
	// create-time version. Capture the full pre-claim row.
	wantStatus, wantVer := readStatusVer(t, pool, iss.ID)
	if wantStatus != StatusStanding {
		t.Fatalf("precondition: status = %q, want standing", wantStatus)
	}

	_, err := store.Claim(t.Context(), ClaimInput{
		ID:        iss.ID,
		SessionID: "sess-f2d-nopartial",
		Actor:     HumanActor(creator, name),
	})
	if _, ok := IsValidationError(err); !ok {
		t.Fatalf("Claim err = %v, want ErrCannotClaimStanding ValidationError", err)
	}

	// status + version unchanged.
	gotStatus, gotVer := readStatusVer(t, pool, iss.ID)
	if gotStatus != wantStatus {
		t.Errorf("status = %q after rejected claim, want %q (unchanged)", gotStatus, wantStatus)
	}
	if gotVer != wantVer {
		t.Errorf("version = %d after rejected claim, want %d (no bump)", gotVer, wantVer)
	}

	// locked_by + started_at must both remain NULL — no partial claim.
	var lockedBy *string
	var startedAt *string // scanned as text; only nil-ness matters here
	if err := pool.QueryRow(t.Context(),
		`SELECT locked_by, started_at::text FROM issues WHERE id = $1`, iss.ID).
		Scan(&lockedBy, &startedAt); err != nil {
		t.Fatalf("read locked_by/started_at: %v", err)
	}
	if lockedBy != nil {
		t.Errorf("locked_by = %v after rejected claim, want NULL", *lockedBy)
	}
	if startedAt != nil {
		t.Errorf("started_at = %v after rejected claim, want NULL", *startedAt)
	}

	// No issue.claimed (or status_changed) event for the rejected claim.
	var events int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM events
 WHERE entity_type='issue' AND entity_id=$1 AND actor_session_id=$2`,
		iss.ID, "sess-f2d-nopartial").Scan(&events); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if events != 0 {
		t.Errorf("expected zero events for rejected standing claim, got %d", events)
	}
}
