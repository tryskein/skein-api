package nudges

// selection_test.go — sk-303.14, task T020.
// Matrix rows: 73, 74, 75, 76 (the "from selection" half), 77, 78, plus the
// negative assertion about a row without a countdown base (an earlier
// decision, recorded as a comment in sk-303.14.2.8.1).
//
// This checks MEMBERSHIP in the unaccepted set through the worker's eyes:
// which issues land in its selection and which addressees appear because of
// them. The schedule and segment consumption are neighbouring files.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/review"
)

// issueVisibleToAnyone — whether the issue lands in anyone's selection.
//
// Needed where the assertion sounds "there is NOBODY to remind": checking
// one addressee is not enough, because the question is exactly that there
// is no addressee.
func issueVisibleToAnyone(
	t *testing.T, ctx context.Context, s *Store, cat review.Category, now time.Time, issueID string,
) bool {
	t.Helper()
	after := uuid.Nil
	for range 100 {
		batch, err := s.SelectDue(ctx, cat, now, 50, after)
		if err != nil {
			t.Fatalf("selecting %s: %v", cat, err)
		}
		if len(batch) == 0 {
			return false
		}
		for _, c := range batch {
			if slices.Contains(c.IssueIDs, issueID) {
				return true
			}
		}
		after = batch[len(batch)-1].UserID
	}
	t.Fatalf("selecting %s: the walk did not converge in 100 batches", cat)
	return false
}

// TestUnaccepted_StatusBreakdownRowByRow — matrix row 73 (SC-009) and row
// 74 (SC-024).
//
// The status-breakdown table from the spec is confirmed ROW BY ROW: one
// addressee has all six states at once, and each is checked by its own
// assertion. One shared set comparison would be worse here: it would say
// "something does not add up" without naming which table row broke.
func TestUnaccepted_StatusBreakdownRowByRow(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)
	base := ago(2 * time.Hour)

	open := e.seed(t, issue{suffix: "open", status: "open", assignee: workerName,
		assigneeSetAt: base, createdBy: author})
	deferred := e.seed(t, issue{suffix: "deferred", status: "deferred", assignee: workerName,
		assigneeSetAt: base, createdBy: author})
	inProgress := e.seed(t, issue{suffix: "inprog", status: "in_progress", assignee: workerName,
		assigneeSetAt: base, createdBy: author})
	accepted := e.seed(t, issue{suffix: "accepted", status: "in_progress", assignee: workerName,
		assigneeSetAt: base, createdBy: author, accepted: true})
	closed := e.seed(t, issue{suffix: "closed", status: "closed", assignee: workerName,
		assigneeSetAt: base, createdBy: author})
	standing := e.seed(t, issue{suffix: "standing", status: "standing", assignee: workerName,
		assigneeSetAt: base, createdBy: author})

	now := time.Now().UTC()
	c, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, worker)
	if !ok {
		t.Fatalf("an addressee with unaccepted issues did not enter the selection at all")
	}

	// Each table row is its own assertion with its own message.
	for _, want := range []struct {
		id   string
		note string
	}{
		{open, "open, no acceptance → remind"},
		{deferred, "deferred, no acceptance → remind: the issue remains hanging on the person"},
		{inProgress, "in progress, no acceptance → remind: the state is reachable besides claiming"},
	} {
		if !slices.Contains(c.IssueIDs, want.id) {
			t.Errorf("row 73 (%s): issue %s is missing from the selection; selection = %v",
				want.note, want.id, c.IssueIDs)
		}
	}
	for _, bad := range []struct {
		id   string
		note string
	}{
		{accepted, "accepted by any path → do not remind"},
		{closed, "closed → do not remind"},
		{standing, "standing → do not remind: it is not waiting for acceptance and never completes"},
	} {
		if slices.Contains(c.IssueIDs, bad.id) {
			t.Errorf("row 73 (%s): issue %s entered the selection; selection = %v",
				bad.note, bad.id, c.IssueIDs)
		}
	}
}

// TestUnaccepted_StandingAloneProducesNoAddressee — matrix row 74 (SC-024)
// as a separate assertion.
//
// The difference from the previous: the addressee has NO other unaccepted
// issues. A standing task not excluded from the set would produce an
// addressee here — i.e. an eternal reminder for an issue that never
// completes. The row-by-row breakdown would not catch this: there the
// addressee exists because of other issues.
func TestUnaccepted_StandingAloneProducesNoAddressee(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	e.seed(t, issue{suffix: "standing-only", status: "standing", assignee: workerName,
		assigneeSetAt: ago(2 * time.Hour), createdBy: author})

	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted,
		time.Now().UTC(), worker); ok {
		t.Errorf("row 74: the addressee of a single STANDING issue entered the selection — " +
			"an eternal reminder for a never-completing issue is pure noise")
	}
}

// TestUnaccepted_UnresolvableAssigneeAddressesNobody — matrix row 75.
//
// The assignee is stored as text, so a string that does not resolve to a
// user (a typo, a deleted account) is a data state, not an impossibility.
// There is nobody to address, and the issue lands in NOBODY'S selection.
func TestUnaccepted_UnresolvableAssigneeAddressesNobody(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)

	id := e.seed(t, issue{suffix: "ghost", status: "open",
		assignee:      "no-such-user-" + uuid.NewString()[:8],
		assigneeSetAt: ago(2 * time.Hour), createdBy: author})

	if issueVisibleToAnyone(t, ctx, e.store, review.CategoryUnaccepted, time.Now().UTC(), id) {
		t.Errorf("row 75: issue %s with an unresolvable assignee entered someone's selection", id)
	}
}

// TestUnaccepted_DeactivatedAddresseeIsExcluded — matrix row 76.
//
// Both halves of the row are closed by ONE fragment, and that is material:
// reminder selection and the per-element re-check at delivery
// (outbox.digestElements) substitute ONE AND THE SAME unaccepted predicate
// (INV-3). So this checks that a deactivated addressee's issue is visible
// to NOBODY — neither as their own selection nor as someone else's; the
// delivery outcome for an already-formed row is checked by
// formation_test.go.
func TestUnaccepted_DeactivatedAddresseeIsExcluded(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	id := e.seed(t, issue{suffix: "deact", status: "open", assignee: workerName,
		assigneeSetAt: ago(2 * time.Hour), createdBy: author})

	now := time.Now().UTC()
	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, worker); !ok {
		t.Fatalf("precondition: the active addressee did not enter the selection")
	}

	if _, err := e.pool.Exec(ctx,
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, worker); err != nil {
		t.Fatalf("deactivation: %v", err)
	}

	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, worker); ok {
		t.Errorf("row 76: the deactivated addressee remained in the selection")
	}
	if issueVisibleToAnyone(t, ctx, e.store, review.CategoryUnaccepted, now, id) {
		t.Errorf("row 76: issue %s of a deactivated addressee is visible in someone else's selection", id)
	}
}

// TestUnaccepted_AcceptanceStopsReminders — matrix row 77 (SC-010).
//
// "Took into work" is not the name of a command, it is an observable
// result: the appearance of an acceptance mark. So it is the mark that is
// checked, not a status change.
func TestUnaccepted_AcceptanceStopsReminders(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	id := e.seed(t, issue{suffix: "accept-later", status: "open", assignee: workerName,
		assigneeSetAt: ago(2 * time.Hour), createdBy: author})

	now := time.Now().UTC()
	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, worker); !ok {
		t.Fatalf("precondition: the unaccepted issue did not produce an addressee")
	}

	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET accepted_at = now(), accepted_by = $2 WHERE id = $1`,
		id, worker); err != nil {
		t.Fatalf("stamping acceptance: %v", err)
	}

	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, worker); ok {
		t.Errorf("row 77: after the acceptance mark appeared the addressee remained in the selection")
	}
}

// TestUnaccepted_ReassignmentMovesRemindersWithAFreshBase — matrix row 78.
//
// Reassignment must do three things at once: stop reminders for the
// previous assignee, start them for the new one, and give the new one a
// FRESH countdown base — for them this is a fresh assignment with its own
// burst (R-5). The third part is checked separately and on purpose: a
// transfer without a fresh base would look correct on the first two
// assertions, and in fact would put the previous person's schedule onto
// the new assignee.
func TestUnaccepted_ReassignmentMovesRemindersWithAFreshBase(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	oldWorker, oldName := e.user(t)
	newWorker, newName := e.user(t)

	oldBase := ago(2 * time.Hour)
	id := e.seed(t, issue{suffix: "reassign", status: "open", assignee: oldName,
		assigneeSetAt: oldBase, createdBy: author})

	now := time.Now().UTC()
	before, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, oldWorker)
	if !ok {
		t.Fatalf("precondition: the previous assignee did not produce an addressee")
	}
	// Comparison with a tolerance: the mark in the database has microsecond
	// precision, time.Time has nanosecond, and exact equality would catch
	// storage rounding, not a base substitution.
	if d := before.Base.Sub(oldBase.UTC()); d > time.Millisecond || d < -time.Millisecond {
		t.Fatalf("precondition: previous assignee's base %s, expected %s",
			before.Base, oldBase.UTC())
	}

	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET assignee = $2 WHERE id = $1`, id, newName); err != nil {
		t.Fatalf("reassignment: %v", err)
	}

	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, oldWorker); ok {
		t.Errorf("row 78: reminders remained with the previous assignee")
	}
	// The moment is SHIFTED FORWARD, and that is not a convenience.
	//
	// Repair A/8 moved the first-mark lower bound into the selection: an
	// addressee whose first mark has not arrived under any settings no
	// longer lands in the batch — otherwise they would occupy a budget slot
	// every tick and starve the tail of the list. Reassignment stamps a
	// fresh base, i.e. "right now", and immediately after it the new
	// assignee is legitimately NOT a candidate (INV-11: the first reminder
	// is not earlier than one burst segment). This test's assertion is
	// about a FRESH BASE, not about instant entry into the selection, so
	// we ask at a moment when the bound is certainly passed.
	after, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted,
		time.Now().UTC().Add(2*time.Hour), newWorker)
	if !ok {
		t.Fatalf("row 78: the new assignee did not become an addressee")
	}
	if !after.Base.After(before.Base) {
		t.Errorf("row 78: the new assignee's base %s is not fresher than the previous %s — "+
			"reassignment did not restart the schedule, and the new person will get "+
			"someone else's burst", after.Base, before.Base)
	}
}

// TestUnaccepted_RowWithoutScheduleBaseRemindsNobody — the negative
// assertion of the earlier decision (comment in sk-303.14.2.8.1).
//
// Issues assigned BEFORE the mechanic shipped have no countdown base and
// nowhere for one to come from: the migration chose NULL as the only
// option that does not introduce a fictional fact, and reported the number
// of such rows with the issues_assigned_without_base counter. The schedule
// refuses without a base, so such a row does not enter the selection —
// i.e. the historical archive will not flood into reminders on the day of
// the release.
//
// The assertion is separate, not a side effect of another test: it CONFIRMS
// an accepted decision, and hidden inside someone else's check it would
// stay silent on a change of decision.
func TestUnaccepted_RowWithoutScheduleBaseRemindsNobody(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	id := e.seed(t, issue{suffix: "nobase", status: "open", assignee: workerName,
		assigneeSetAt: nil, createdBy: author})

	// The trigger stamps a base on EVERY write path, so the historical
	// state is reproduced by lifting the stamp after insert — otherwise
	// such a state cannot be obtained by product means at all.
	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET assignee_set_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("lifting the countdown base: %v", err)
	}
	var base *time.Time
	if err := e.pool.QueryRow(ctx,
		`SELECT assignee_set_at FROM issues WHERE id = $1`, id).Scan(&base); err != nil {
		t.Fatalf("reading the base: %v", err)
	}
	if base != nil {
		t.Fatalf("precondition: the countdown base was not lifted (%s) — nothing to check", base)
	}

	now := time.Now().UTC()
	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, worker); ok {
		t.Errorf("the addressee of an issue WITHOUT a countdown base entered the selection: " +
			"there is nothing to count a schedule for it from, and the reminder would be " +
			"counted from a made-up moment")
	}
	if issueVisibleToAnyone(t, ctx, e.store, review.CategoryUnaccepted, now, id) {
		t.Errorf("issue %s without a countdown base is visible in the selection", id)
	}
}
