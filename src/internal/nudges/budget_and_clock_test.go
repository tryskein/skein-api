package nudges

// budget_and_clock_test.go — sk-303.14, repair A/8. Three gates
// under three review findings, each on its own assertion.
//
//  1. TAIL STARVATION: a tick that exhausted the batch budget must continue
//     the walk from the next tick, not start again from the head of the
//     list.
//  2. THE SEGMENT IS BY THE DATABASE CLOCK: a replica whose clock ran ahead
//     must not form a second reminder for the same segment (INV-8).
//  3. THE FROZEN DEADLINE IS READ, NOT RECOMPUTED: the decision that a
//     segment has arrived is checked IN ISOLATION from the selection,
//     otherwise the assertion about it is tautological.
//
// The second round of review added a fifth: THE TIME SOURCE IS ONE. The
// "new deadline is strictly after the formation moment" guard compared
// against the START OF THE TICK, and the segment gate — against the
// database clock at write time. A whole tick fitted between them, and a
// deadline already expired on the database clock passed through that gap.
//
// The third round added a sixth and split it from the fifth: THE GATE'S
// CLOCK is the database's too. There are two defences here (the deadline
// guard in Form and the segment gate in consumeSegment), and after the
// second-round fix "the database clock" was held by one assertion on both,
// i.e. in fact only on one — the guard went red before the gate. The fifth
// gate (the guard) and the sixth (the gate) now go red on DIFFERENT
// mutations and do not substitute for each other.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"skein-api/internal/review"
)

// ---------------------------------------------------------------------------
// 1. Tail starvation
// ---------------------------------------------------------------------------

// candidatesBefore counts addressees of the category standing in the walk
// BEFORE the given one — from the worker's real selection, not from its own
// query.
//
// The database is shared and populated with foreign rows, so the number of
// steps the walk must take to reach our addressee is computed, not guessed:
// "two hundred ticks will be enough" is a disguised assumption about the
// size of the database.
func candidatesBefore(
	t *testing.T, e *env, cat review.Category, now time.Time, target uuid.UUID,
) int {
	t.Helper()
	after := uuid.Nil
	n := 0
	for range 500 {
		batch, err := e.store.SelectDue(t.Context(), cat, now, 200, after)
		if err != nil {
			t.Fatalf("selecting %s: %v", cat, err)
		}
		if len(batch) == 0 {
			t.Fatalf("addressee %s was not found in the %s selection — nothing to count from", target, cat)
		}
		for _, c := range batch {
			if c.UserID == target {
				return n
			}
			n++
		}
		after = batch[len(batch)-1].UserID
	}
	t.Fatalf("the %s selection walk did not converge", cat)
	return 0
}

// TestSweep_BudgetExhaustionResumesInsteadOfRestarting — a review finding:
// starvation of the tail of the reminder queue.
//
// # What was broken
//
// sweep started EVERY tick from the nil id and was bounded by
// BatchesPerTick × BatchSize. An addressee who does not leave the selection
// and produces no reminder occupied a budget slot — and so every tick.
// Everything past the budget bound in id order went unprocessed until the
// head drained, and the head could fail to drain indefinitely.
//
// # The fixture, and why exactly this one
//
// A — an addressee with a SMALLER id who remains in the selection but never
// becomes due: no burst, interval 366 days (the contract ceiling), the base
// is half an hour ago. Half an hour is past the first-mark lower bound, so
// the selection takes them legitimately; the schedule will say "not yet"
// for another year.
// B — an addressee with a LARGER id who is due right now.
//
// The limits are one batch of one addressee: BatchSize=1, BatchesPerTick=1.
// Then every tick advances the walk by exactly one addressee, and the
// number of steps to B is counted in advance from the real selection.
//
// # What goes red under the mutation
//
// Put `after := uuid.Nil` back at the start of sweep — and the walk forever
// sticks on the first addressee who does not leave the selection (in the
// worst case on A), and B will not receive A SINGLE reminder in any number
// of ticks. Exactly that is what the review probe showed: fifty ticks, zero
// reminders for the tail.
func TestSweep_BudgetExhaustionResumesInsteadOfRestarting(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)

	// Roles are assigned IN ID ORDER: the walk goes by them, and the
	// "budget eater" must stand in front of the starving one, otherwise the
	// fixture checks the wrong order.
	u1, n1 := e.user(t)
	u2, n2 := e.user(t)
	starver, starverName := u1, n1
	starving, starvingName := u2, n2
	if starving.String() < starver.String() {
		starver, starverName, starving, starvingName = u2, n2, u1, n1
	}

	// A: remains in the selection (the base is half an hour ago — the bound
	// is passed), but does not become due for another year.
	yearMinutes := int(review.MaxInterval / time.Minute)
	zeroBurst := 0
	e.setSchedule(t, starver, &zeroBurst, &yearMinutes, &yearMinutes)
	e.seed(t, issue{suffix: "starver", status: "open", assignee: starverName,
		assigneeSetAt: ago(30 * time.Minute), createdBy: author})

	// B: due right now, factory settings.
	e.seed(t, issue{suffix: "starving", status: "open", assignee: starvingName,
		assigneeSetAt: ago(3 * time.Hour), createdBy: author})

	now := time.Now().UTC()
	if _, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, now, starver); !ok {
		t.Fatalf("precondition: the budget eater did not enter the selection — then " +
			"there is nobody to eat the budget and the test checks nothing")
	}
	steps := candidatesBefore(t, e, review.CategoryUnaccepted, now, starving)

	sc := NewScheduler(e.store, Config{BatchSize: 1, BatchesPerTick: 1})
	// Exactly as many steps as there are addressees standing before B, plus
	// their own, plus a small reserve for foreign rows that appeared
	// between the count and the run.
	budget := steps + 5
	for range budget {
		sc.tickAt(ctx, time.Now().UTC())
		if e.notifications(t, starving, review.CategoryUnaccepted) > 0 {
			break
		}
	}

	if n := e.notifications(t, starving, review.CategoryUnaccepted); n == 0 {
		t.Errorf("the tail of the walk received no reminder in %d ticks with %d addressees "+
			"in front of them. The tick restarts the walk from the head of the list, and "+
			"the head does not leave the selection — so the tail is NEVER processed",
			budget, steps)
	}
	// The reverse half: starvation was not cured by us starting to remind
	// everyone. The budget eater's deadline has not arrived, and they must
	// have no reminder (INV-11).
	if n := e.notifications(t, starver, review.CategoryUnaccepted); n != 0 {
		t.Errorf("an addressee whose deadline has not arrived was formed %d reminders — "+
			"the tail was unblocked at the cost of violating INV-11", n)
	}
}

// TestSelectDue_PinsTheFirstMarkBoundToItsActualValue — the second measure
// against starvation, pinned from BOTH sides to the actual value of the
// bound.
//
// # What was weak (finding of the second round of review A/8)
//
// The previous edition planted the assignment "a second ago" and checked
// entry into the selection at "the bound plus a minute". That only caught
// shrinking the bound below a second and stretching it above roughly
// sixty-one seconds. A plausible regression — a unit-of-measure slip
// (firstMark/time.Minute instead of /time.Second, i.e. a bound of 10
// seconds instead of 10 minutes) — stayed green, as did shifting the bound
// by a minute either way.
//
// # How it is pinned now
//
// The selection moment is given EXPLICITLY (SelectDue takes "now" as a
// parameter), the countdown base is too, and both magnitudes are chosen
// with zero nanoseconds so they survive timestamptz rounding to
// microseconds. Then the bound is checked by two points standing on
// opposite sides of its ACTUAL value:
//
//	base = now − bound + a second → NOT a candidate (the mark has not arrived);
//	base = now − bound            → a candidate (the comparison is non-strict).
//
// Each mutation of the bound falls on at least one of the points:
//
//	bound + 60 s     → the second point drops out of the selection (a lost reminder);
//	bound = 2 s      → the first point enters the selection (starvation returns);
//	<= replaced with < → the second point drops out (the addressee exactly on the mark);
//	/time.Minute     → the first point enters (a unit-of-measure slip).
//
// Both categories are checked separately: they have DIFFERENT bounds (ten
// minutes versus fifteen) and different countdown bases, and a shared check
// on one of them would declare the other covered.
func TestSelectDue_PinsTheFirstMarkBoundToItsActualValue(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, authorName := e.user(t)

	// Zero nanoseconds are required: timestamptz stores microseconds, and a
	// base with a nanosecond tail would round DOWN. Then "exactly on the
	// mark" would become "a fraction of a microsecond earlier", and a
	// "<= → <" mutation would go unnoticed.
	anchor := time.Now().UTC().Truncate(time.Second).Add(-6 * time.Hour)

	for _, cat := range review.Categories() {
		t.Run(cat.String(), func(t *testing.T) {
			bound, err := review.EarliestFirstMark(cat)
			if err != nil {
				t.Fatalf("first-mark bound: %v", err)
			}

			// A separate addressee per category: their sets are different,
			// and one person in both at once would mix the assertions.
			worker, workerName := e.user(t)
			switch cat {
			case review.CategoryUnaccepted:
				e.seed(t, issue{suffix: "bound-" + cat.String(), status: "open",
					assignee: workerName, assigneeSetAt: &anchor, createdBy: author})
			case review.CategoryUnreviewed:
				// The unreviewed addressee is the AUTHOR of the issue, and
				// the countdown base is the close moment.
				// The assignee is ANOTHER person: the unreviewed set
				// excludes issues the author did themselves.
				_ = workerName
				id := e.seed(t, issue{suffix: "bound-" + cat.String(), status: "closed",
					assignee: authorName, createdBy: worker, reviewRequired: true})
				e.forceClosedAt(t, id, anchor)
			}

			// The "the mark has not arrived yet" point: a second short of
			// the bound.
			tooEarly := anchor.Add(bound - time.Second)
			if _, ok := candidateFor(t, ctx, e.store, cat, tooEarly, worker); ok {
				t.Errorf("%s: an addressee with countdown base %s entered the selection "+
					"at moment %s — that is a second EARLIER than the first-mark bound "+
					"(%s). The bound is shrunk: the selection again drags into the batch "+
					"addressees whose mark has not arrived under any settings, and the "+
					"tail of the queue starves",
					cat, anchor, tooEarly, bound)
			}

			// The "exactly on the bound" point: the comparison is
			// non-strict, the addressee must be there.
			exactly := anchor.Add(bound)
			if _, ok := candidateFor(t, ctx, e.store, cat, exactly, worker); !ok {
				t.Errorf("%s: an addressee with countdown base %s did NOT enter the "+
					"selection at moment %s — that is exactly the first-mark bound (%s). "+
					"The bound is stretched or the comparison became strict: an addressee "+
					"who may be due by the schedule dropped out of the selection and the "+
					"reminder is lost in silence (INV-11)",
					cat, anchor, exactly, bound)
			}
		})
	}
}

// TestSelectDue_AnExpiredFrozenDeadlineSurvivesAFreshCountdownBase — the
// load-bearing disjunct `rl.next_due_at IS NOT NULL OR` in HAVING, which
// had not a single guard (finding of the second round of review A/8:
// dropping the disjunct was green on the whole tree).
//
// # What this disjunct holds
//
// The first-mark lower bound answers "could the addressee have become due
// FOR THE FIRST TIME". For an addressee who has already had a reminder
// formed, the question does not apply at all: their fate is decided by the
// FROZEN deadline, and it has expired.
//
// A collision of these two rules is not invented, it is typical: the
// unaccepted countdown base is MAX(assignee_set_at), so a FRESH assignment
// takes the base forward. Then `base > now − bound` — and without the
// disjunct the row drops out of the selection, although the Go contract
// (Scheduler.due) would say "due": the deadline has expired. The reminder
// is lost in silence, and it is lost the more often the more actively the
// person is assigned issues — i.e. exactly those who need reminding.
//
// The review probe is set up here as a test: under a mutation of "remove
// the disjunct" the first assertion goes red — the addressee is not found
// in the selection.
func TestSelectDue_AnExpiredFrozenDeadlineSurvivesAFreshCountdownBase(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	// First reminder: the assignment is old, the mark has arrived.
	e.seed(t, issue{suffix: "keep-old", status: "open", assignee: workerName,
		assigneeSetAt: ago(2 * time.Hour), createdBy: author})
	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Fatalf("precondition: the first reminder was not formed (%d)", n)
	}

	dbNow, err := e.store.Now(ctx)
	if err != nil {
		t.Fatalf("database clock: %v", err)
	}
	// The segment is lived through: the frozen deadline has expired.
	e.forceNextDue(t, worker, review.CategoryUnaccepted, dbNow.Add(-time.Minute))

	// And at exactly this moment the person is assigned ONE MORE issue.
	// The countdown base — MAX(assignee_set_at) — moves forward and ends
	// up closer to "now" than the first-mark lower bound.
	fresh := dbNow.Add(-30 * time.Second)
	e.seed(t, issue{suffix: "keep-fresh", status: "open", assignee: workerName,
		assigneeSetAt: &fresh, createdBy: author})

	c, ok := candidateFor(t, ctx, e.store, review.CategoryUnaccepted, dbNow, worker)
	if !ok {
		bound, _ := review.EarliestFirstMark(review.CategoryUnaccepted)
		t.Fatalf("an addressee with an EXPIRED frozen deadline dropped out of the "+
			"selection: a fresh assignment took the countdown base (%s) inside the "+
			"first-mark lower bound (%s), and the SQL bound cut a row the Go contract "+
			"would say is \"due\". The reminder is lost in silence", fresh, bound)
	}
	if c.NextDueAt == nil {
		t.Fatalf("the candidate has no frozen deadline — the assertion checks the wrong case")
	}
	if c.NextDueAt.After(dbNow) {
		t.Fatalf("frozen deadline %s has not expired yet — the assertion checks the wrong case",
			*c.NextDueAt)
	}

	// The end-to-end half: since the addressee is in the selection and the
	// deadline has expired — a reminder is formed. Without the disjunct
	// there would be none at all.
	NewScheduler(e.store, Config{}).tickAt(ctx, dbNow)
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 2 {
		t.Errorf("after the segment expired %d reminders were formed, expected two: "+
			"the first and the second. A fresh assignment is not entitled to cancel "+
			"an already-started schedule", n)
	}
}

// ---------------------------------------------------------------------------
// 2. The segment is by the database clock
// ---------------------------------------------------------------------------

// TestForm_ALiveSegmentIsNotConsumedASecondTime — WHETHER there is a
// segment gate at all: a claim on a pair whose segment is still live must
// leave empty-handed.
//
// # What was broken
//
// consumeSegment compared reminder_rate_limit.next_due_at with a "now"
// computed by time.Now() of THE PROCESS doing the tick. INV-8 is at the
// same time promised with SEVERAL instances running at once — i.e. the
// promise rested on an unwritten assumption of replica clock agreement. A
// replica that had run ahead by more than a segment (three hours at factory
// settings, fifteen minutes at the minimum setting) formed a second
// reminder for the same segment.
//
// The review probe is reproduced here verbatim: the first formation sets
// the deadline three hours ahead, the second must REFUSE, because the
// segment is still live.
//
// ⚠ CORRECTED PROMISE (repair A/8, third round). This used to say: "A
// mutation of 'return the comparison to the caller's moment' (WHERE
// next_due_at <= $now) goes red on exactly this assertion" — and the
// assertion was named
// TestForm_SegmentIsHeldByTheDatabaseClockNotTheCaller. Both words turned
// out to be untrue, and they were devalued by the same commit that added
// them: it removed the caller's moment from Form's signature, and "a
// replica with a runaway clock" became inexpressible except by the
// DEADLINE. And the deadline is caught not by the gate but by the guard.
// Measured separately: a mutation of "the gate takes the moment as a
// parameter, the scheduler passes it" is green on the whole package if the
// moment is substituted into this call mechanically.
//
// WHAT THE ASSERTION ACTUALLY HOLDS — measured by mutations:
//
//	the gate is dropped (the WHERE condition removed)      → RED: "delivery rows 2";
//	the guard is dropped (the inFuture check removed)      → green: the deadline is already in the future;
//	the gate takes the moment as a parameter               → green (see above).
//
// I.e. it holds the EXISTENCE of the gate, not the source of its clock.
// The source of the clock is held by
// TestTick_AReplicaWhoseClockRanAheadCannotReopenALiveSegment, set up for
// this separately: a runaway replica can be expressed only where its "now"
// is still in the signature — on the tick.
func TestForm_ALiveSegmentIsNotConsumedASecondTime(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	worker, c := e.unacceptedCandidate(t, "dbclock")

	now := time.Now().UTC()
	first, err := e.store.Form(ctx, review.CategoryUnaccepted, c, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("first formation: %v", err)
	}
	if !first.Formed {
		t.Fatalf("precondition: the first formation did not happen — there is nothing to check the second against")
	}

	// A second claim on the same pair. The segment has not expired on the
	// database clock, so the gate must refuse it. The deadline is computed
	// with "a replica's clock that ran five hours ahead" — but here they
	// express only THE DEADLINE ITSELF: Form has no caller moment, and the
	// replica's clock does not enter the decision from anywhere.
	ahead := now.Add(5 * time.Hour)
	second, err := e.store.Form(ctx, review.CategoryUnaccepted, c, ahead.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("second formation: %v", err)
	}
	if second.Formed {
		t.Errorf("a live segment was consumed a second time — there is no gate at all " +
			"that refuses a claim on an unexpired deadline, and INV-8 (\"at most one " +
			"reminder per segment\", promised PRECISELY with several instances) is held " +
			"by nothing")
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("delivery rows %d, expected one — for one segment the person received "+
			"more than one reminder", n)
	}
}

// ---------------------------------------------------------------------------
// 3. The frozen deadline is read, not recomputed
// ---------------------------------------------------------------------------

// TestDue_FrozenDeadlineIsReadNotRecomputed — the second support of the
// segment rule, checked IN ISOLATION from the selection.
//
// # Why separately from the selection
//
// The `if c.NextDueAt != nil` branch in Scheduler.due is supplied with a
// comment that the rule "would survive any rewrite of the selection". The
// review showed that the comment was held by nothing: a `return true, nil`
// mutation was green, because SelectDue had already selected the rows with
// the same condition and the same "now", and on live data the branch is
// always true. An assertion about it must call it directly, with a
// candidate the selection would not have returned.
//
// Both sides AND independence from settings are checked: a deadline in the
// future — not due, a deadline in the past — due, and settings under which
// the mark would recompute completely differently do not change the answer.
// The last is "a setting change takes effect from the next segment": a
// started segment is not cut when the interval is shortened and not
// stretched when it is lengthened.
func TestDue_FrozenDeadlineIsReadNotRecomputed(t *testing.T) {
	sc := NewScheduler(nil, Config{})
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := now.Add(-10 * time.Hour)

	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	// The settings are deliberately DIFFERENT and both far from the frozen
	// deadlines below: a recompute with current ones would give a different
	// answer on at least one of them.
	shortest := review.Settings{BurstCount: review.MaxBurstCount,
		UnacceptedInterval: review.MinInterval, UnreviewedInterval: review.MinInterval}
	longest := review.Settings{BurstCount: 0,
		UnacceptedInterval: review.MaxInterval, UnreviewedInterval: review.MaxInterval}

	for _, s := range []review.Settings{shortest, longest, review.Defaults()} {
		for _, cat := range review.Categories() {
			notYet := Candidate{UserID: uuid.New(), Base: base, NextDueAt: &future}
			if due, err := sc.due(cat, notYet, s, now); err != nil {
				t.Fatalf("%s: the due decision refused: %v", cat, err)
			} else if due {
				t.Errorf("%s: frozen deadline %s has not arrived yet (now %s), and the "+
					"decision says \"due\". The frozen-deadline branch does not read it, "+
					"it answers affirmatively always — on live data this is invisible, "+
					"because the selection already selected the rows with the same condition",
					cat, future, now)
			}

			overdue := Candidate{UserID: uuid.New(), Base: base, NextDueAt: &past}
			if due, err := sc.due(cat, overdue, s, now); err != nil {
				t.Fatalf("%s: the due decision refused: %v", cat, err)
			} else if !due {
				t.Errorf("%s: frozen deadline %s has expired (now %s), and the decision "+
					"says \"not due\" — so the deadline is recomputed with current "+
					"settings, and a started segment is stretched after the fact", cat, past, now)
			}

			// Exactly on the mark — due: the comparison is non-strict, as
			// in review.IsDue.
			exactly := Candidate{UserID: uuid.New(), Base: base, NextDueAt: &now}
			if due, err := sc.due(cat, exactly, s, now); err != nil {
				t.Fatalf("%s: the due decision refused: %v", cat, err)
			} else if !due {
				t.Errorf("%s: on the mark itself %s the decision says \"not due\" — the "+
					"segment bound has drifted from review.IsDue", cat, now)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Form's guard: the new deadline is strictly after the formation moment
// ---------------------------------------------------------------------------

// TestForm_RefusesADeadlineThatIsNotInTheFuture — a guard that had no
// assertion of its own (finding of review A/8).
//
// # What it does and why it is bad without it
//
// Form refuses to write a deadline that does not exceed the formation
// moment: such a deadline means an IMMEDIATELY expired segment, i.e. a
// reminder every tick. A mutation of "drop the guard" was green on the
// whole tree.
//
// # Why the assertion lives separately from the catch-up class
//
// Because it is about SOMETHING ELSE. The catch-up class
// (TestUnaccepted_MissedSegmentsAreNotReplayed) asserts that the schedule
// does not replay missed marks; this guard is the last defence in case the
// schedule still returned the past. While the assertions about them were
// piled into one test, the guard went red EARLIER, and the catch-up class
// failed on "the reminder was not formed at all", not on "catch-up". They
// are split.
//
// Two past deadlines of different age are checked: a certainly past one (an
// hour ago) and one taken from the process clock EXACTLY BEFORE the call —
// the one that lags only by the time of opening the transaction.
//
// ⚠ CORRECTED WORDING (repair A/8, third round). The second case was named
// here the "strictly later, not not-earlier" bound. There is no bound in
// it: while Form opens the transaction, the database clock moves forward,
// so such a deadline by the time of the guard lies STRICTLY in the past,
// and a non-strict comparison refuses it exactly as a strict one does.
// Measured: a mutation of `$1 > now()` → `>= now()` is green on the whole
// package. From the outside the bound is unreachable anyway — the guard's
// moment is transaction_timestamp() of its own transaction, and from the
// outside it does not exist yet. It is held by
// TestDeadlineIsAhead_TheBoundaryIsStrictlyLater, which takes the moment
// FROM INSIDE the transaction; here remains what this case actually
// checks: a lag of the time of opening the transaction is already caught
// by the guard.
func TestForm_RefusesADeadlineThatIsNotInTheFuture(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	worker, c := e.unacceptedCandidate(t, "guard")

	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		next time.Time
	}{
		{"deadline an hour in the past", now.Add(-time.Hour)},
		{"deadline lags by the time of opening the transaction", now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.store.Form(ctx, review.CategoryUnaccepted, c, tc.next)
			if err == nil {
				t.Errorf("%s: formation passed without a refusal (Formed=%v). A segment "+
					"expired at the moment of opening means a reminder EVERY tick",
					tc.name, res.Formed)
			}
			if res.Formed {
				t.Errorf("%s: the segment was consumed", tc.name)
			}
		})
	}

	// Nothing written: neither a delivery row nor segment state.
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 0 {
		t.Errorf("the guard's refusal left %d delivery rows — the transaction was not rolled back", n)
	}
	if _, _, ok := e.rateLimit(t, worker, review.CategoryUnaccepted); ok {
		t.Error("the guard's refusal left rate-limit state — the segment " +
			"was consumed by a refusing formation")
	}
}

// ---------------------------------------------------------------------------
// 5. The time source is one: the deadline guard and the segment gate measure
//    with one and the same
// ---------------------------------------------------------------------------

// TestTick_AStaleTickMomentCannotOpenAnAlreadyExpiredSegment — finding of
// the second round of review A/8: two time sources in one contract.
//
// # What was broken
//
// The segment gate compared against the DATABASE clock (`next_due_at <=
// now()`), and the guard in Form ("the new deadline is strictly after the
// formation moment") — against the moment the caller passed in, i.e.
// against the database clock as read AT THE START OF THE TICK. The
// schedule computes the new deadline from the same start of the tick and
// returns it in (0, interval]; if the interval is shorter than what the
// tick has already spent, by the time of the write the deadline LIES IN
// THE PAST on the database clock. The guard let it through, the row was
// written, and the next tick's gate saw the segment as immediately expired
// — "two reminders for one segment", exactly what the guard exists to
// stop.
//
// # How this is reproduced here
//
// The tick moment is taken twenty minutes into the past at a fifteen-
// minute interval (the minimum setting, burst off). The schedule computes
// from it a deadline that is certainly in the past on the database clock:
// the last mark of the grid following "now minus twenty minutes" sits no
// more than fifteen from it. Twenty minutes is not an arbitrary magnitude:
// it is GREATER than the interval, so the gap opens deterministically, not
// with the "tickPeriod/interval" probability a live scheduler would catch
// it with.
//
// # Why the assertion counts notifications, not looks at a refusal
//
// Because Form's refusal is a method, not a promise. The promise is INV-8:
// for one segment a person receives at most one reminder. Under a mutation
// of "return the caller's moment to the guard" the first tick forms a
// reminder with an already-expired deadline, the second forms a second,
// and it is the count that goes red — i.e. the broken promise, not a
// vanished precondition.
func TestTick_AStaleTickMomentCannotOpenAnAlreadyExpiredSegment(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	// Burst off, the interval is the minimum. Then the background marks go
	// exactly every fifteen minutes from the countdown base, and the
	// arithmetic below does not depend on a single built-in default.
	e.setSchedule(t, worker, intp(0), intp(review.MinIntervalMinutes), nil)
	e.seed(t, issue{suffix: "stale", status: "open", assignee: workerName,
		assigneeSetAt: ago(2 * time.Hour), createdBy: author})

	dbNow, err := e.store.Now(ctx)
	if err != nil {
		t.Fatalf("database clock: %v", err)
	}
	stale := dbNow.Add(-20 * time.Minute)

	// A tick from a STALE moment: the schedule yields a deadline that lies
	// in the past on the database clock.
	NewScheduler(e.store, Config{}).tickAt(ctx, stale)
	// A tick from a fresh moment: if the previous one wrote an expired
	// segment, this one will form a second reminder from it.
	NewScheduler(e.store, Config{}).tickAt(ctx, dbNow)

	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("for one segment %d reminders were formed, expected one (INV-8). "+
			"The deadline guard and the segment gate measure with DIFFERENT clocks: "+
			"the guard let through a deadline already expired on the database clock, "+
			"and the next tick found the segment open", n)
	}

	// A direct consequence of the same property, as a separate assertion:
	// the written deadline must lie in the future on the database clock. A
	// segment expired at the moment of opening is a reminder every tick.
	_, next, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("there is no rate-limit state — there was nothing to write")
	}
	after, err := e.store.Now(ctx)
	if err != nil {
		t.Fatalf("database clock: %v", err)
	}
	if !next.After(after) {
		t.Errorf("written deadline %s is not after the database clock %s — the segment "+
			"was opened already expired", next, after)
	}
}

// TestForm_RefusalIsMeasuredByTheDatabaseClockNotTheCaller — the same
// finding, addressed at Form.
//
// The assertion is narrow and therefore strong: a deadline that lies in the
// past on the DATABASE clock is refused regardless of which "now" it was
// computed with. Form has no caller moment at all, so it now has nothing
// to let such a deadline through with; a mutation of "the guard is dropped"
// goes red here, and not on a precondition — the precondition (the
// addressee is in the selection) is checked before the call.
func TestForm_RefusalIsMeasuredByTheDatabaseClockNotTheCaller(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	worker, c := e.unacceptedCandidate(t, "onesource")

	dbNow, err := e.store.Now(ctx)
	if err != nil {
		t.Fatalf("database clock: %v", err)
	}

	// A deadline an hour in the past on the database clock. Exactly such a
	// deadline is what the previous edition wrote when the caller's moment
	// lagged more strongly.
	res, err := e.store.Form(ctx, review.CategoryUnaccepted, c, dbNow.Add(-time.Hour))
	if err == nil {
		t.Errorf("a deadline an hour in the past on the database clock was accepted "+
			"(Formed=%v): the guard measures with a different clock from the segment "+
			"gate, and the segment it opened expires at the moment of opening", res.Formed)
	}
	if res.Formed {
		t.Error("the segment was consumed by a deadline that has already expired on the database clock")
	}
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 0 {
		t.Errorf("the refusal left %d delivery rows — the transaction was not rolled back", n)
	}
	if _, _, ok := e.rateLimit(t, worker, review.CategoryUnaccepted); ok {
		t.Error("the refusal left rate-limit state")
	}
}

// ---------------------------------------------------------------------------
// 6. The segment gate measures with the DATABASE clock, not the caller's
//    moment
// ---------------------------------------------------------------------------

// TestTick_AReplicaWhoseClockRanAheadCannotReopenALiveSegment — an
// assertion under the segment GATE, separately from the deadline guard.
//
// # Why a separate assertion
//
// There are two defences here, and they are different:
//
//	the guard in Form       — "the new deadline is strictly after the formation moment";
//	the consumeSegment gate — "the previous deadline has already expired ON THE DATABASE CLOCK".
//
// Each must be held by ITS OWN assertion. While "the database clock" was
// checked only through Form
// (TestForm_ALiveSegmentIsNotConsumedASecondTime below), a runaway replica
// clock was expressed by the deadline alone — and the deadline is caught
// by the guard, not the gate. One assertion covered both defences, i.e. in
// fact only one: a mutation of "the gate takes the moment as a parameter,
// the scheduler passes it" was green on the whole package.
//
// # Why through the scheduler, not through Form
//
// There is NO caller moment in Form's signature (see the form.go header),
// so there is nothing to express "a replica with a runaway clock" by a
// Form call with — a parameter for that does not exist. It does exist on
// the tick: tickAt takes "now", and that "now" is the replica's view of
// time. A mutation of the gate pulls the moment from exactly there — so
// the assertion must stand there too.
//
// # How it is reproduced
//
// A healthy instance ticks from the database clock and closes the segment
// with a deadline no further than fifteen minutes (the interval is the
// minimum, burst off). Then a replica whose clock ran an HOUR ahead ticks
// — certainly more than a segment. Its deadline, computed by the schedule
// from its own "now", lies in the future on the database clock too, so
// Form's guard lets it through: it is the GATE that must refuse, seeing
// that the segment is still live on the database clock. If it lets it
// through — the person will receive a second reminder for one segment,
// i.e. INV-8 will again rest on replica clock agreement.
func TestTick_AReplicaWhoseClockRanAheadCannotReopenALiveSegment(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	e.setSchedule(t, worker, intp(0), intp(review.MinIntervalMinutes), nil)
	e.seed(t, issue{suffix: "runahead", status: "open", assignee: workerName,
		assigneeSetAt: ago(2 * time.Hour), createdBy: author})

	dbNow, err := e.store.Now(ctx)
	if err != nil {
		t.Fatalf("database clock: %v", err)
	}

	// A healthy instance: the segment is opened, the deadline has gone
	// into the future.
	NewScheduler(e.store, Config{}).tickAt(ctx, dbNow)
	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Fatalf("precondition: the healthy instance formed %d reminders, "+
			"expected one — there is nothing to check the replica against", n)
	}
	_, opened, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("precondition: the segment is not opened — the gate has nothing to guard")
	}
	if !opened.After(dbNow) {
		t.Fatalf("precondition: opened deadline %s is not after the database clock %s — "+
			"the segment expired at the moment of opening, and the second claim will be "+
			"refused not by the gate", opened, dbNow)
	}

	// A REPLICA WHOSE CLOCK RAN AN HOUR AHEAD. An hour is more than a
	// segment (fifteen minutes), so on its "now" the segment looks long
	// expired; on the database clock it is live.
	ahead := dbNow.Add(time.Hour)
	NewScheduler(e.store, Config{}).tickAt(ctx, ahead)

	if n := e.notifications(t, worker, review.CategoryUnaccepted); n != 1 {
		t.Errorf("for one segment %d reminders were formed, expected one (INV-8). "+
			"The segment gate compared the previous deadline with the REPLICA's \"now\", "+
			"not with the database clock: an instance whose clock ran ahead by more than "+
			"a segment declares a neighbour's live segment expired", n)
	}
	_, after, ok := e.rateLimit(t, worker, review.CategoryUnaccepted)
	if !ok {
		t.Fatalf("the rate-limit state disappeared")
	}
	if !after.Equal(opened) {
		t.Errorf("the frozen deadline was rewritten from %s to %s — the replica consumed "+
			"a live segment and opened a new one", opened, after)
	}
}

// TestDeadlineIsAhead_TheBoundaryIsStrictlyLater — the deadline guard's
// bound, taken FROM INSIDE the transaction.
//
// # Why from inside
//
// The guard asks `$1 > now()`, and `now()` is transaction_timestamp(): the
// moment is fixed at the START of the transaction. From the outside such a
// moment cannot be obtained: a deadline taken from the process clock
// before the Form call is already strictly in the past by the time of the
// guard, so it is refused by a non-strict comparison exactly as by a
// strict one — a `>` → `>=` mutation is green on it (measured). The bound
// is reachable only where the transaction's moment is KNOWN, i.e. inside
// it itself.
//
// Both sides are checked: a deadline EQUAL to the formation moment is not
// future (otherwise the segment is opened already expired, i.e. a reminder
// every tick); a deadline a microsecond later is future (a microsecond is
// timestamptz's resolution, i.e. the smallest distinguishable step).
func TestDeadlineIsAhead_TheBoundaryIsStrictlyLater(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("opening the transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var txNow time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txNow); err != nil {
		t.Fatalf("the transaction's moment: %v", err)
	}
	txNow = txNow.UTC()

	seen, inFuture, err := deadlineIsAhead(ctx, tx, txNow)
	if err != nil {
		t.Fatalf("the guard refused to answer: %v", err)
	}
	if !seen.Equal(txNow) {
		t.Fatalf("precondition: the guard sees moment %s, and the transaction opened at %s — "+
			"now() has stopped being transaction_timestamp(), and the bound is taken in "+
			"the wrong place",
			seen, txNow)
	}
	if inFuture {
		t.Errorf("a deadline EQUAL to the formation moment was recognised as future — " +
			"the comparison became \"not earlier\" instead of \"strictly later\", and " +
			"the segment is opened expired at the moment of opening, i.e. a reminder " +
			"every tick")
	}

	if _, inFuture, err := deadlineIsAhead(ctx, tx, txNow.Add(time.Microsecond)); err != nil {
		t.Fatalf("the guard refused to answer: %v", err)
	} else if !inFuture {
		t.Errorf("a deadline a microsecond after the formation moment was recognised as " +
			"not future — the comparison is stricter than \"strictly later\", and the " +
			"smallest distinguishable segment has become unreachable")
	}
}
