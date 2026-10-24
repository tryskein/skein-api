package nudges

// schedule_test.go — sk-303.14, task T020.
// Matrix rows: 69, 72, 79, 115, 116, plus the assertion of the absence of
// catch-up (comment in sk-303.14.2.8.2).
//
// This checks the "is it due" DECISION: the two-phase scale on live data,
// refusal to recompute an already-started segment, behaviour when the clock
// goes backwards, and the absence of replaying missed marks. The arithmetic
// of the scale itself as a pure function is checked by the earlier class
// (review/schedule_test.go); here — that the worker actually uses it and
// stores its answer.

import (
	"testing"
	"time"

	"skein-api/internal/review"
)

// approx compares moments with a tolerance: the mark in storage has
// microsecond precision, the arithmetic has nanosecond.
func approx(a, b time.Time) bool {
	d := a.Sub(b)
	return d < time.Millisecond && d > -time.Millisecond
}

// TestUnaccepted_FirstReminderNotBeforeOneBurstSegment — matrix row 72
// (INV-11, SC-007), first half.
//
// Factory scale: three reminders in the first hour, i.e. the burst segment
// is twenty minutes. An issue assigned a minute ago produces no reminder:
// the assignment notification and a reminder of it do not arrive back to
// back.
func TestUnaccepted_FirstReminderNotBeforeOneBurstSegment(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	e.seed(t, issue{suffix: "fresh", status: "open", assignee: workerName,
		assigneeSetAt: ago(time.Minute), createdBy: author})

	sc := NewScheduler(e.store, Config{})
	sc.tickAt(ctx, time.Now().UTC())

	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 0 {
		t.Errorf("row 72: a freshly assigned issue produced %d reminders — "+
			"the first must arrive no earlier than one burst segment after assignment", n)
	}
	if _, _, ok := e.rateLimit(t, worker, review.CategoryUnaccepted); ok {
		t.Errorf("row 72: the segment was consumed although no reminder was formed")
	}
}

// TestUnaccepted_TwoPhaseScheduleMarks — matrix row 72, second half.
//
// BOTH phases are checked on live state, and specifically the frozen
// deadline the worker wrote:
//
//	base − 25 minutes → the first burst mark (20 min) has already passed,
//	                    the segment is consumed, the next deadline is the
//	                    SECOND burst mark (40 min from the base);
//	base − 61 minutes → the burst is exhausted, the next deadline is the
//	                    background interval AFTER THE LAST burst mark, i.e.
//	                    4 hours from the base at the factory three repeats
//	                    and a three-hour background.
//
// The second half is material: a background counted from the base, not from
// the end of the burst, would give 3 hours and look "almost right".
func TestUnaccepted_TwoPhaseScheduleMarks(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	base := ago(25 * time.Minute)
	e.seed(t, issue{suffix: "burst", status: "open", assignee: workerName,
		assigneeSetAt: base, createdBy: author})

	sc := NewScheduler(e.store, Config{})
	now := time.Now().UTC()
	sc.tickAt(ctx, now)

	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Fatalf("row 72: expected one reminder, formed %d", n)
	}
	_, next, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("row 72: the segment was not consumed")
	}
	wantSecondMark := base.Add(40 * time.Minute)
	if !approx(next, wantSecondMark) {
		t.Errorf("row 72: next deadline %s, expected the second burst mark %s "+
			"(base %s, factory three repeats in the first hour)", next, wantSecondMark, base)
	}

	// Second phase: the same addressee, the base shifted past the burst
	// edge.
	lateBase := time.Now().UTC().Add(-61 * time.Minute)
	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET assignee_set_at = $2 WHERE id = $1`, e.prefix+"-burst", lateBase); err != nil {
		t.Fatalf("shifting the base: %v", err)
	}
	// The deadline is shifted into the past BY AN HOUR AND A HALF, not by
	// a minute, and that is not decoration: with a one-minute shift "add
	// the interval to the previous deadline" gives almost the same answer
	// as an honest calculation, and the assertion stops distinguishing
	// implementations. An hour and a half splits them by an hour and a
	// half.
	e.forceNextDue(t, worker, review.CategoryUnaccepted, time.Now().UTC().Add(-90*time.Minute))

	now2 := time.Now().UTC()
	sc.tickAt(ctx, now2)
	_, next2, _ := e.rateLimit(t, worker, review.CategoryUnaccepted)
	wantBackground := lateBase.Add(review.BurstWindow + review.DefaultUnacceptedInterval)
	if !approx(next2, wantBackground) {
		t.Errorf("row 72: first background deadline %s, expected the background interval "+
			"AFTER the last burst mark — %s (base %s). Counting the background from the "+
			"base would give %s and look almost right", next2, wantBackground, lateBase,
			lateBase.Add(review.DefaultUnacceptedInterval))
	}
}

// TestUnreviewed_HasNoBurst — matrix row 79 (SC-013).
//
// Unreviewed has no burst: the first reminder arrives after the BACKGROUND
// INTERVAL from the appearance of the result, not after a burst segment.
// Both sides are checked: half an hour after close — silence (with a burst
// there would be a reminder here), thirteen hours later — a reminder, and
// the next deadline is exactly one interval further.
func TestUnreviewed_HasNoBurst(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	_, workerName := e.user(t)

	id := e.seed(t, issue{suffix: "unrev", status: "closed", assignee: workerName,
		createdBy: author, reviewRequired: true})
	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET closed_at = $2 WHERE id = $1`, id, time.Now().UTC().Add(-30*time.Minute)); err != nil {
		t.Fatalf("shifting the close moment: %v", err)
	}

	sc := NewScheduler(e.store, Config{})
	sc.tickAt(ctx, time.Now().UTC())
	if n := e.notifications(t, author, review.CategoryUnreviewed); n != 0 {
		t.Errorf("row 79: a result closed half an hour ago produced %d reminders — "+
			"that is a sign of a burst the category must not have", n)
	}

	closedAt := time.Now().UTC().Add(-13 * time.Hour)
	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET closed_at = $2 WHERE id = $1`, id, closedAt); err != nil {
		t.Fatalf("shifting the close moment: %v", err)
	}
	sc.tickAt(ctx, time.Now().UTC())
	if n := e.notifications(t, author, review.CategoryUnreviewed); n != 1 {
		t.Fatalf("row 79: after the background interval expected one reminder, formed %d", n)
	}
	_, next, ok := e.rateLimit(t, author, review.CategoryUnreviewed)
	if !ok {
		t.Fatalf("row 79: the segment was not consumed")
	}
	want := closedAt.Add(2 * review.DefaultUnreviewedInterval)
	if !approx(next, want) {
		t.Errorf("row 79: next deadline %s, expected the second mark of the uniform grid %s "+
			"(base %s, interval %s)", next, want, closedAt, review.DefaultUnreviewedInterval)
	}
}

// TestUnaccepted_SeveralIssuesGiveOneReminderWithTheCount — matrix row 69
// (SC-008).
//
// One reminder, not one per issue, and in its payload — ALL issues of the
// set: the count the person will see is computed by the path from the
// payload after the re-check, so a payload truncated at selection would
// undercount it.
func TestUnaccepted_SeveralIssuesGiveOneReminderWithTheCount(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	base := ago(25 * time.Minute)
	for _, s := range []string{"m1", "m2", "m3"} {
		e.seed(t, issue{suffix: s, status: "open", assignee: workerName,
			assigneeSetAt: base, createdBy: author})
	}

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())

	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Fatalf("row 69: three unaccepted issues gave %d notifications, expected one", n)
	}
	var payload []byte
	if err := e.pool.QueryRow(ctx, `
SELECT ev.payload FROM events ev
  JOIN notifications n ON n.event_id = ev.id
 WHERE n.user_id = $1 AND n.reason = $2`,
		worker, reasonOf(t, review.CategoryUnaccepted)).Scan(&payload); err != nil {
		t.Fatalf("reading the payload: %v", err)
	}
	got, err := parsePayloadIDs(payload)
	if err != nil {
		t.Fatalf("parsing the payload: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("row 69: the payload has %d issues (%v), expected 3 — the count the "+
			"person will see is computed from this list", len(got), got)
	}
}

// TestUnaccepted_SettingChangeAppliesFromTheNextSegment — matrix row 115.
//
// Both sides of the rule, and both on one state:
//
//  1. an ALREADY STARTED segment is not recomputed — neither when the
//     interval is shortened nor when it is lengthened. That is exactly why
//     the deadline is stored, not derived from the last reminder's moment:
//     a derivation would take current settings;
//  2. the NEXT segment is counted with the new settings.
func TestUnaccepted_SettingChangeAppliesFromTheNextSegment(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	// The base is past the burst edge — the segment is immediately
	// background, and the rule is checked on the background interval, which
	// is what the person configures.
	base := ago(61 * time.Minute)
	e.seed(t, issue{suffix: "settings", status: "open", assignee: workerName,
		assigneeSetAt: base, createdBy: author})

	sc := NewScheduler(e.store, Config{})
	sc.tickAt(ctx, time.Now().UTC())
	_, frozen, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("precondition: the segment was not consumed")
	}

	for _, tc := range []struct {
		name    string
		minutes int
	}{
		{"shortening", 60},
		{"lengthening", 720},
	} {
		e.setSchedule(t, worker, nil, intp(tc.minutes), nil)
		sc.tickAt(ctx, time.Now().UTC())
		_, after, _ := e.rateLimit(t, worker, review.CategoryUnaccepted)
		if !approx(after, frozen) {
			t.Errorf("row 115 (%s the interval to %d min): the started segment was "+
				"recomputed — the deadline moved from %s to %s", tc.name, tc.minutes, frozen, after)
		}
	}

	// The segment is lived through: the next one must be counted with the
	// LAST setting (720 minutes), not the factory one.
	e.forceNextDue(t, worker, review.CategoryUnaccepted, time.Now().UTC().Add(-time.Minute))
	now := time.Now().UTC()
	sc.tickAt(ctx, now)
	_, next, _ := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !next.After(now.Add(11 * time.Hour)) {
		t.Errorf("row 115: the next segment was not counted with the new setting — "+
			"deadline %s, and at a 720-minute interval it must sit almost 12 hours from %s", next, now)
	}
}

// TestUnaccepted_ClockGoingBackwardsExpiresNothing — matrix row 116.
//
// An assertion about the PREDICATE IN THE STORE, not about the scale as a
// function: the scale is checked by row 4 of the earlier class. Here —
// that a written deadline is not considered expired at a time earlier than
// it, and no flood of reminders occurs.
func TestUnaccepted_ClockGoingBackwardsExpiresNothing(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	base := ago(61 * time.Minute)
	e.seed(t, issue{suffix: "clock", status: "open", assignee: workerName,
		assigneeSetAt: base, createdBy: author})

	sc := NewScheduler(e.store, Config{})
	sc.tickAt(ctx, time.Now().UTC())
	_, frozen, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("precondition: the segment was not consumed")
	}

	// The clock went back a day: the moment is earlier than the written
	// deadline.
	past := frozen.Add(-24 * time.Hour)
	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, past, worker); ok {
		t.Errorf("row 116: at a time earlier than the deadline (%s < %s) the addressee "+
			"entered the selection again", past, frozen)
	}

	// And even a direct attempt to consume the segment writes nothing: the
	// deadline written in the pair's state has not expired on the DATABASE
	// clock.
	//
	// The new deadline here is certainly future (a day ahead), and that is
	// not carelessness but the address of the assertion: Form no longer has
	// a caller moment at all (repair A/8, second round), so "the clock went
	// backwards" is checked at the only place the clock still participates
	// — the segment gate. A past deadline would hit the catch-up guard and
	// take the assertion into someone else's refusal.
	res, err := e.store.Form(ctx, review.CategoryUnaccepted,
		Candidate{UserID: worker, Base: base.UTC(), IssueIDs: []string{e.prefix + "-clock"}},
		time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("row 116: formation returned an error: %v", err)
	}
	if res.Formed {
		t.Errorf("row 116: the segment was consumed with an unexpired written deadline")
	}

	// Several ticks in a row in the past — not a single extra notification.
	for range 3 {
		sc.tickAt(ctx, past)
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("row 116: after the clock went backwards reminders became %d — that is "+
			"the flood that must not happen", n)
	}
	_, afterNext, _ := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !approx(afterNext, frozen) {
		t.Errorf("row 116: the written deadline moved from %s to %s", frozen, afterNext)
	}
}

// TestUnaccepted_MissedSegmentsAreNotReplayed — the assertion of the
// absence of catch-up (comment in sk-303.14.2.8.2).
//
// A worker that slept through several segments forms ONE reminder, not a
// series, and the next deadline ends up STRICTLY IN THE FUTURE. That is
// the defence the scale skips missed marks for: a replay would hand the
// restored worker a volley for every slept-through segment — exactly the
// hammering the whole rate-limit table exists to prevent.
//
// Row 116 checks something else: there time goes BACKWARDS, here — the
// worker was absent while it went forwards.
func TestUnaccepted_MissedSegmentsAreNotReplayed(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	// The base is thirty hours ago: at a three-hour background that is a
	// dozen marks.
	base := ago(30 * time.Hour)
	e.seed(t, issue{suffix: "asleep", status: "open", assignee: workerName,
		assigneeSetAt: base, createdBy: author})

	sc := NewScheduler(e.store, Config{})
	sc.tickAt(ctx, time.Now().UTC())
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Fatalf("precondition: the first tick formed %d reminders instead of one", n)
	}

	// HERE IT IS, the slept-through state: the pair's deadline is written
	// a day ago. Distinguishing catch-up from an honest calculation is
	// possible ONLY on an existing state row — an addressee without one
	// has no deadline to "shift" from at all, and both implementations
	// answer the same.
	e.forceNextDue(t, worker, review.CategoryUnaccepted, time.Now().UTC().Add(-24*time.Hour))

	now := time.Now().UTC()
	sc.tickAt(ctx, now)

	// THE ORDER OF ASSERTIONS HERE IS SIGNIFICANT (repair A/8).
	//
	// First comes the assertion ABOUT CATCH-UP — about the written
	// deadline itself, not about the reminder count. Previously the
	// counter stood first, and that masked the cause: under a catch-up
	// mutation the schedule returns a PAST mark, Form's guard ("the new
	// deadline is strictly after the formation moment") refuses, no
	// reminder is formed at all — and the test failed on "added 1
	// reminders", i.e. went red for "the reminder was not formed", not
	// for "catch-up happened". The assertion must go red for ITS OWN
	// reason.
	//
	// The written deadline catches both forms of catch-up: if Form's
	// guard is in place — the state row stayed with a past deadline; if
	// the guard is removed — the row was updated with a past deadline.
	// In both cases the deadline is not in the future.
	_, next, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("catch-up: the state row disappeared")
	}
	if !next.After(now) {
		t.Fatalf("catch-up: written deadline %s is not after the formation moment %s — "+
			"the schedule replays missed marks, and every next tick issues one more "+
			"reminder, for every slept-through segment", next, now)
	}

	// And only now — the consequence: for all slept-through segments
	// exactly one reminder was added.
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 2 {
		t.Fatalf("catch-up: for a day of slept-through segments %d reminders were added, "+
			"expected one (2 in total)", n)
	}

	// A second tick at the same moment adds nothing — the observable
	// consequence of a strictly future deadline.
	sc.tickAt(ctx, now)
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 2 {
		t.Errorf("catch-up: the second tick brought the reminder count to %d — a volley "+
			"for slept-through segments", n)
	}
}
