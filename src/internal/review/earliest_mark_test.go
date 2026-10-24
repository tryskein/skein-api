package review

// earliest_mark_test.go — sk-303.14, repair A/8.
//
// EarliestFirstMark is a LOWER BOUND on the first mark, used by the reminder
// worker's selection so it does not drag into the batch addressees whose
// first mark has not arrived under any settings. The bound must be safe in
// one direction: it MUST NOT cut off an addressee whose mark has arrived.
//
// The bound is derived from the same constants as the schedule, and
// "derived" is exactly the claim that must be held when the constants
// change.
//
// ⚠ CORRECTED CLAIM (repair A/8, second round). This used to say the
// property is proved by "enumerating the ENTIRE settings space". That was
// untrue, and it is proved: the burst count was enumerated fully (7 of 7),
// but for the interval FOUR NAMED POINTS were taken from 527 026 admitted
// minutes — that is a grid, not an enumeration. A mutation that breaks the
// first mark ONLY at interval == 20m (a legal value, but off the grid) left
// both ./internal/review and ./internal/nudges green. The property was held
// not by the test, but by the current arithmetic's monotonicity in the
// interval — that is, by exactly what was supposed to be checked.
//
// What stands here now, verbatim:
//
//   - FULL enumeration of the reachable space: every admitted burst count
//     (0..MaxBurstCount) × EVERY admitted interval minute
//     (MinIntervalMinutes..MaxIntervalMinutes, 527 026 values) × both
//     categories. Reachable — because the interval is stored and transported
//     as WHOLE MINUTES (ValidateIntervalMinutes, ResolveMinutes, the schema's
//     int4 column), and a fractional minute does not occur in settings by
//     any path.
//
//   - PSEUDORANDOM SAMPLE with a pinned seed over the rest of the space:
//     Settings.Validate() accepts any duration in [MinInterval, MaxInterval],
//     not only whole minutes, and a Settings built in code may carry a
//     fractional one. Enumerating nanoseconds is impossible (3.16e16
//     values), so this is a sample — and the seed is pinned so a run is
//     reproducible: a red test must go red on the next run too.
//
//   - CROSS check of field independence at named points: a category's mark
//     must not depend on the NEIGHBOURING category's interval.
//
// The wording is narrowed to the truth on purpose: lying in a test's name
// and header is not allowed, and "enumeration" at four points is a lie that
// reads as a guarantee.

import (
	"math/rand"
	"testing"
	"time"
)

// checkFirstMark is one application of the carrying property.
//
// The schedule's first mark must lie NO EARLIER than the bound. The reverse
// would mean the worker's selection cuts off an addressee who is due — that
// is, it silently loses a reminder.
func checkFirstMark(t *testing.T, cat Category, bound time.Duration, base time.Time, s Settings) {
	t.Helper()
	mark, err := s.MarkAt(cat, base, 1)
	if err != nil {
		t.Fatalf("%s (burst=%d, unaccepted=%s, unreviewed=%s): first mark not "+
			"computed: %v", cat, s.BurstCount, s.UnacceptedInterval, s.UnreviewedInterval, err)
	}
	if mark.Before(base.Add(bound)) {
		t.Fatalf("%s (burst=%d, unaccepted=%s, unreviewed=%s): first mark %s EARLIER "+
			"than bound %s. The worker's selection cuts off by this bound, so an "+
			"addressee who is due drops out of it and the reminder vanishes silently (INV-11)",
			cat, s.BurstCount, s.UnacceptedInterval, s.UnreviewedInterval,
			mark.Sub(base), bound)
	}
}

// boundFor is the category's bound plus a check that it actually cuts
// anything off.
func boundFor(t *testing.T, cat Category) time.Duration {
	t.Helper()
	bound, err := EarliestFirstMark(cat)
	if err != nil {
		t.Fatalf("%s: bound not computed: %v", cat, err)
	}
	if bound <= 0 {
		t.Fatalf("%s: bound %s is not positive — then it cuts off nothing and "+
			"the selection again drags not-yet-due addressees into the batch", cat, bound)
	}
	return bound
}

// TestEarliestFirstMark_NeverExceedsTheRealFirstMark_EveryReachableSetting —
// FULL enumeration of the reachable settings space.
//
// The reachable space is discrete and small: burst count 0..6, interval —
// whole minutes from MinIntervalMinutes to MaxIntervalMinutes. A fractional
// minute does not occur in settings by any path — not through REST
// (ValidateIntervalMinutes), not through storage (the int4 minutes column).
// So this is an ENUMERATION, not a grid, and the word stands as a fact.
//
// Both intervals are set to THE SAME value at each step: the property is
// checked against THIS category's interval, and independence from the other
// is closed by the cross check below. A double enumeration would be
// 527 026² steps and would never finish.
func TestEarliestFirstMark_NeverExceedsTheRealFirstMark_EveryReachableSetting(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for _, cat := range Categories() {
		bound := boundFor(t, cat)
		for burst := MinBurstCount; burst <= MaxBurstCount; burst++ {
			for m := MinIntervalMinutes; m <= MaxIntervalMinutes; m++ {
				d := time.Duration(m) * time.Minute
				checkFirstMark(t, cat, bound, base, Settings{BurstCount: burst,
					UnacceptedInterval: d, UnreviewedInterval: d})
			}
		}
	}
}

// TestEarliestFirstMark_NeverExceedsTheRealFirstMark_SubMinuteDurations —
// the rest of the space, which cannot be enumerated.
//
// Settings.Validate() accepts ANY duration in [MinInterval, MaxInterval],
// not only whole minutes: a setting assembled in code may carry a
// fractional one. There are 3.16e16 nanoseconds — enumeration is
// impossible, so a sample. The seed is pinned: a run must be reproducible,
// otherwise a red test cannot be repeated and dissecting a find turns into
// guesswork.
func TestEarliestFirstMark_NeverExceedsTheRealFirstMark_SubMinuteDurations(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	const (
		seed    = 20260901
		samples = 200_000
	)
	rng := rand.New(rand.NewSource(seed))
	span := int64(MaxInterval - MinInterval)

	for _, cat := range Categories() {
		bound := boundFor(t, cat)
		for range samples {
			d := MinInterval + time.Duration(rng.Int63n(span+1))
			burst := MinBurstCount + rng.Intn(MaxBurstCount-MinBurstCount+1)
			checkFirstMark(t, cat, bound, base, Settings{BurstCount: burst,
				UnacceptedInterval: d, UnreviewedInterval: d})
		}
	}
}

// TestFirstMark_UsesTheIntervalOfItsOwnCategory — cross check of the
// fields.
//
// The enumeration above sets BOTH intervals to one value, so a swap of one
// field for the other it cannot catch in principle. Here the fields are
// SPLIT, and the claim is no longer about the bound but about the mark's
// VALUE — otherwise it is tautological: swapped intervals both sit no
// lower than MinInterval, so they do not violate the bound and are not
// caught by a "no earlier than the bound" check at all.
//
// A mutation "swap the branches of Settings.IntervalFor" goes red here.
func TestFirstMark_UsesTheIntervalOfItsOwnCategory(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// No burst — then the first mark IS the category's interval, and a
	// field swap is visible as a value mismatch, not as "still no earlier
	// than the bound".
	for _, unacc := range []time.Duration{MinInterval, DefaultUnacceptedInterval, MaxInterval} {
		for _, unrev := range []time.Duration{MinInterval, DefaultUnreviewedInterval, MaxInterval} {
			s := Settings{BurstCount: 0, UnacceptedInterval: unacc, UnreviewedInterval: unrev}
			// The expected value is taken FROM THE FIELDS, not from
			// s.IntervalFor: asking the same function we suspect would
			// give a tautology — swapping its branches would swap the
			// expectation too.
			want := map[Category]time.Duration{
				CategoryUnaccepted: unacc,
				CategoryUnreviewed: unrev,
			}
			for _, cat := range Categories() {
				want, ok := want[cat]
				if !ok {
					t.Fatalf("%s: category not considered by this gate — a new "+
						"third one must be considered, not accepted silently", cat)
				}
				mark, err := s.MarkAt(cat, base, 1)
				if err != nil {
					t.Fatalf("%s: first mark not computed: %v", cat, err)
				}
				if got := mark.Sub(base); got != want {
					t.Errorf("%s (unaccepted=%s, unreviewed=%s): first mark after %s, "+
						"but THIS category's interval is %s. The mark is computed from "+
						"the other field — one category's setting moves the other's schedule",
						cat, unacc, unrev, got, want)
				}
			}
		}
	}

	// And with a burst, the unaccepted first mark is a fraction of the
	// burst window and does not depend on the intervals AT ALL. The reverse
	// would mean a background setting leaking into the burst.
	for burst := 1; burst <= MaxBurstCount; burst++ {
		want := time.Duration(int64(BurstWindow) / int64(burst))
		for _, unacc := range []time.Duration{MinInterval, MaxInterval} {
			s := Settings{BurstCount: burst, UnacceptedInterval: unacc,
				UnreviewedInterval: MinInterval}
			mark, err := s.MarkAt(CategoryUnaccepted, base, 1)
			if err != nil {
				t.Fatalf("burst=%d: first mark not computed: %v", burst, err)
			}
			if got := mark.Sub(base); got != want {
				t.Errorf("burst=%d, unaccepted=%s: burst first mark after %s, "+
					"want %s (burst-window fraction). The background interval leaked "+
					"into the burst", burst, unacc, got, want)
			}
		}
	}
}

// TestEarliestFirstMark_IsTightEnoughToBeUseful — the other side.
//
// The bound must be NO GREATER than the earliest reachable first mark: a
// too-generous bound is safe but useless — it would delay reminders to
// addressees with a frequent burst. The claim pins it exactly to the
// reachable minimum, so "just in case add a day" will go red.
func TestEarliestFirstMark_IsTightEnoughToBeUseful(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		cat  Category
		best Settings
	}{
		// The most frequent burst: six reminders in the first hour →
		// first mark after ten minutes.
		{CategoryUnaccepted, Settings{BurstCount: MaxBurstCount,
			UnacceptedInterval: MinInterval, UnreviewedInterval: MinInterval}},
		// The category never has a burst, so the earliest is the interval
		// floor.
		{CategoryUnreviewed, Settings{BurstCount: MaxBurstCount,
			UnacceptedInterval: MinInterval, UnreviewedInterval: MinInterval}},
	} {
		bound, err := EarliestFirstMark(tc.cat)
		if err != nil {
			t.Fatalf("%s: bound not computed: %v", tc.cat, err)
		}
		mark, err := tc.best.MarkAt(tc.cat, base, 1)
		if err != nil {
			t.Fatalf("%s: first mark not computed: %v", tc.cat, err)
		}
		if got := mark.Sub(base); got != bound {
			t.Errorf("%s: earliest reachable first mark %s, bound %s. "+
				"The bound must coincide with the reachable minimum: greater — "+
				"delayed reminders, lesser — it stops being derivable from the contract",
				tc.cat, got, bound)
		}
	}
}

// TestEarliestFirstMark_RejectsUnknownCategory — closedness of the category
// set.
func TestEarliestFirstMark_RejectsUnknownCategory(t *testing.T) {
	if _, err := EarliestFirstMark(Category("weekly_digest")); err == nil {
		t.Error("unknown category got a bound instead of a refusal — the selection " +
			"would have built a condition on a made-up number")
	}
}
