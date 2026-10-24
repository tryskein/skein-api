package review

import (
	"errors"
	"math"
	"testing"
	"time"
)

// schedule_test.go — unit class of the schedule contract (sk-303.14, T002).
// Owns coverage-matrix rows 1–6:
//
//	1  two-phase scale is reproducible (INV-11)
//	2  degenerate zero burst
//	3  background from the last burst reminder, not from the base (R-5)
//	4  clock moved backwards, no flood
//	5  a fresher assignment restarts the scale (R-5)
//	6  an out-of-bounds value is rejected by the function (SC-022, SC-027)
//
// Assertions are written from the spec, not from the code: the numbers of
// the canonical example ("with three reminders and a three-hour background
// these are marks at 20, 40 and 60 minutes, then 4 hours from the base")
// are written here by hand and checked against what the package computes —
// otherwise the test would prove the implementation consistent with itself.

// base is a fixed countdown base. The moment is arbitrary, but not round:
// the schedule has no right to depend on alignment to an hour or a day.
var base = time.Date(2026, 9, 1, 13, 47, 19, 500_000_000, time.UTC)

func mustSettings(t *testing.T, burst int, unacc, unrev time.Duration) Settings {
	t.Helper()
	s := Settings{BurstCount: burst, UnacceptedInterval: unacc, UnreviewedInterval: unrev}
	if err := s.Validate(); err != nil {
		t.Fatalf("fixture settings rejected: %v", err)
	}
	return s
}

func mustMark(t *testing.T, s Settings, cat Category, b time.Time, n int) time.Time {
	t.Helper()
	got, err := s.MarkAt(cat, b, n)
	if err != nil {
		t.Fatalf("MarkAt(%s, %d): %v", cat, n, err)
	}
	return got
}

func mustNext(t *testing.T, s Settings, cat Category, b, last time.Time) time.Time {
	t.Helper()
	got, err := s.NextReminderAt(cat, b, last)
	if err != nil {
		t.Fatalf("NextReminderAt(%s, last=%v): %v", cat, last, err)
	}
	return got
}

// ---------------------------------------------------------------------------
// Row 1 — two-phase scale is reproducible, INV-11
// ---------------------------------------------------------------------------

// The spec's canonical example, written by hand: three reminders in the first
// hour, three-hour background → 20, 40, 60 minutes, then 4 hours from the
// base, then the background step.
func TestNextReminderAt_CanonicalTwoPhaseScale(t *testing.T) {
	s := mustSettings(t, 3, 3*time.Hour, DefaultUnreviewedInterval)

	want := []time.Duration{
		20 * time.Minute,
		40 * time.Minute,
		60 * time.Minute,
		4 * time.Hour, // 60 minutes of burst + 3 hours of background
		7 * time.Hour,
		10 * time.Hour,
	}

	// Walk the scale the way the worker walks it: each next reminder is
	// requested from the moment of the previous one.
	last := time.Time{}
	for i, offset := range want {
		got := mustNext(t, s, CategoryUnaccepted, base, last)
		if !got.Equal(base.Add(offset)) {
			t.Fatalf("reminder %d at %v, spec says base+%s (%v)",
				i+1, got, offset, base.Add(offset))
		}
		// The same mark, obtained by index, must coincide: the walk and
		// a direct call for the n-th mark are one and the same scale.
		if direct := mustMark(t, s, CategoryUnaccepted, base, i+1); !direct.Equal(got) {
			t.Errorf("MarkAt(%d) = %v, NextReminderAt walk = %v", i+1, direct, got)
		}
		last = got
	}
}

// INV-11: the first reminder of an unaccepted issue is not earlier than one
// burst segment after the assignment. Checked at every admitted reminder
// count, not only the factory one.
func TestNextReminderAt_FirstUnacceptedNotBeforeBurstSegment(t *testing.T) {
	for burst := 1; burst <= MaxBurstCount; burst++ {
		s := mustSettings(t, burst, MinInterval, MinInterval)
		segment := BurstWindow / time.Duration(burst)

		first := mustNext(t, s, CategoryUnaccepted, base, time.Time{})
		if first.Before(base.Add(segment)) {
			t.Errorf("burst=%d: first reminder at %v, earlier than burst segment %s (INV-11)",
				burst, first, segment)
		}
		if !first.Equal(base.Add(segment)) {
			t.Errorf("burst=%d: first reminder base+%s, burst segment %s",
				burst, first.Sub(base), segment)
		}
	}
}

// Reproducibility for ANY pair "settings, countdown base": a walk over every
// admitted reminder count, several intervals and several bases. Three
// properties are asserted at once — determinism, strict increase, and the
// walk coinciding with a direct call.
func TestSchedule_DeterministicAndMonotonicOverSettingsAndBases(t *testing.T) {
	bases := []time.Time{
		base,
		base.Add(-9000 * time.Hour), // long before
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), // exactly midnight
		time.Date(2026, 3, 29, 1, 59, 59, 999, time.FixedZone("x", 3600)),
	}
	intervals := []time.Duration{MinInterval, 47 * time.Minute, 3 * time.Hour, 12 * time.Hour}

	for _, cat := range Categories() {
		for burst := MinBurstCount; burst <= MaxBurstCount; burst++ {
			for _, iv := range intervals {
				s := mustSettings(t, burst, iv, iv)
				for _, b := range bases {
					prev := b
					last := time.Time{}
					for n := 1; n <= 12; n++ {
						got := mustNext(t, s, cat, b, last)

						// Determinism: same input — same answer.
						again := mustNext(t, s, cat, b, last)
						if !again.Equal(got) {
							t.Fatalf("cat=%s burst=%d iv=%s n=%d: %v then %v",
								cat, burst, iv, n, got, again)
						}
						// Strict increase: the scale never stands still
						// and never goes backwards.
						if !got.After(prev) {
							t.Fatalf("cat=%s burst=%d iv=%s n=%d: %v not after %v",
								cat, burst, iv, n, got, prev)
						}
						// The walk equals a direct call.
						if direct := mustMark(t, s, cat, b, n); !direct.Equal(got) {
							t.Fatalf("cat=%s burst=%d iv=%s: MarkAt(%d)=%v, walk=%v",
								cat, burst, iv, n, direct, got)
						}
						prev, last = got, got
					}
				}
			}
		}
	}
}

// Burst segments are equal, the last one lands exactly on the hour boundary,
// and at a ceiling of six reminders the cadence is no more than one per ten
// minutes — the very bound the ceiling was chosen for.
func TestMarkAt_BurstSegmentsEqualAndEndOnWindow(t *testing.T) {
	for burst := 1; burst <= MaxBurstCount; burst++ {
		s := mustSettings(t, burst, DefaultUnacceptedInterval, DefaultUnreviewedInterval)

		prev := base
		for k := 1; k <= burst; k++ {
			mark := mustMark(t, s, CategoryUnaccepted, base, k)
			if gap := mark.Sub(prev); gap != BurstWindow/time.Duration(burst) {
				t.Errorf("burst=%d: segment %d is %s, want %s",
					burst, k, gap, BurstWindow/time.Duration(burst))
			}
			prev = mark
		}
		if last := mustMark(t, s, CategoryUnaccepted, base, burst); !last.Equal(base.Add(BurstWindow)) {
			t.Errorf("burst=%d: last burst mark %v, want the hour boundary %v",
				burst, last, base.Add(BurstWindow))
		}
	}

	// Ceiling: six reminders — exactly once per ten minutes, not more often.
	s := mustSettings(t, MaxBurstCount, DefaultUnacceptedInterval, DefaultUnreviewedInterval)
	if gap := mustMark(t, s, CategoryUnaccepted, base, 1).Sub(base); gap < 10*time.Minute {
		t.Errorf("burst ceiling gives cadence %s, more often than once per 10 minutes", gap)
	}
}

// ---------------------------------------------------------------------------
// Row 2 — degenerate zero burst
// ---------------------------------------------------------------------------

func TestNextReminderAt_ZeroBurst_BackgroundFromBase(t *testing.T) {
	s := mustSettings(t, 0, 3*time.Hour, DefaultUnreviewedInterval)

	last := time.Time{}
	for n := 1; n <= 4; n++ {
		got := mustNext(t, s, CategoryUnaccepted, base, last)
		want := base.Add(time.Duration(n) * 3 * time.Hour)
		if !got.Equal(want) {
			t.Fatalf("zero burst, reminder %d: %v, want %v", n, got, want)
		}
		last = got
	}

	// The property of a zero burst is not "the first hour is empty": a
	// legal setting "no burst, interval 15 minutes" gives, by the spec
	// itself, a first mark at the fifteenth minute. The property is that
	// the first hour is NOT SPLIT: the first mark is exactly one interval
	// later, whatever that interval is. Checked at an interval shorter
	// than an hour — the case where a claim about an empty first hour
	// would be false.
	short := mustSettings(t, 0, MinInterval, DefaultUnreviewedInterval)
	first := mustNext(t, short, CategoryUnaccepted, base, time.Time{})
	if !first.Equal(base.Add(MinInterval)) {
		t.Errorf("with zero burst first mark %v, want base+%s", first, MinInterval)
	}
}

// Unreviewed goes without a burst ALWAYS (SC-013) — even when the number of
// reminders in the first hour is set non-zero. This is a property of the
// category, not a setting that can be switched on.
func TestUnreviewed_NeverBursts(t *testing.T) {
	s := mustSettings(t, MaxBurstCount, DefaultUnacceptedInterval, 12*time.Hour)

	burst, err := s.BurstCountFor(CategoryUnreviewed)
	if err != nil {
		t.Fatalf("BurstCountFor: %v", err)
	}
	if burst != 0 {
		t.Errorf("unreviewed got a burst of %d reminders (SC-013)", burst)
	}

	last := time.Time{}
	for n := 1; n <= 3; n++ {
		got := mustNext(t, s, CategoryUnreviewed, base, last)
		want := base.Add(time.Duration(n) * 12 * time.Hour)
		if !got.Equal(want) {
			t.Fatalf("unreviewed, reminder %d: %v, want %v", n, got, want)
		}
		last = got
	}
}

// ---------------------------------------------------------------------------
// Row 3 — background from the last burst reminder, not from the base (R-5)
// ---------------------------------------------------------------------------

func TestNextReminderAt_BackgroundCountsFromLastBurstMark(t *testing.T) {
	cases := []struct {
		burst    int
		interval time.Duration
	}{
		{3, 3 * time.Hour},
		{1, MinInterval},
		{MaxBurstCount, 90 * time.Minute},
	}

	for _, tc := range cases {
		s := mustSettings(t, tc.burst, tc.interval, DefaultUnreviewedInterval)

		lastBurst := mustMark(t, s, CategoryUnaccepted, base, tc.burst)
		firstBackground := mustMark(t, s, CategoryUnaccepted, base, tc.burst+1)

		wantFromBurst := lastBurst.Add(tc.interval)
		if !firstBackground.Equal(wantFromBurst) {
			t.Errorf("burst=%d iv=%s: first background %v, want %v (interval after the last burst reminder)",
				tc.burst, tc.interval, firstBackground, wantFromBurst)
		}

		// Explicit negative claim: NOT from the base. Without it the
		// test above would also pass on an implementation that counts
		// the background from the base, when the hour happens to be a
		// multiple of the interval.
		wrongFromBase := base.Add(tc.interval)
		if firstBackground.Equal(wrongFromBase) {
			t.Errorf("burst=%d iv=%s: background counted from the base (%v), must be from the last burst reminder",
				tc.burst, tc.interval, wrongFromBase)
		}
		if firstBackground.Sub(base) != BurstWindow+tc.interval {
			t.Errorf("burst=%d iv=%s: first background at base+%s, spec requires base+%s",
				tc.burst, tc.interval, firstBackground.Sub(base), BurstWindow+tc.interval)
		}
	}
}

// ---------------------------------------------------------------------------
// Row 4 — clock moved backwards: a segment does not expire early, no flood
// ---------------------------------------------------------------------------

func TestIsDue_ClockMovedBackwards(t *testing.T) {
	s := mustSettings(t, 3, 3*time.Hour, DefaultUnreviewedInterval)
	firstMark := base.Add(20 * time.Minute)

	// Before the deadline — not due, including a nanosecond before the mark.
	for _, now := range []time.Time{base, base.Add(19 * time.Minute), firstMark.Add(-time.Nanosecond)} {
		due, err := s.IsDue(CategoryUnaccepted, base, time.Time{}, now)
		if err != nil {
			t.Fatalf("IsDue: %v", err)
		}
		if due {
			t.Errorf("segment declared expired at %v, mark is only at %v", now, firstMark)
		}
	}

	// Exactly on the mark — due.
	due, err := s.IsDue(CategoryUnaccepted, base, time.Time{}, firstMark)
	if err != nil {
		t.Fatalf("IsDue: %v", err)
	}
	if !due {
		t.Errorf("on the mark itself %v the segment is not considered arrived", firstMark)
	}

	// A reminder was formed on the mark, after which the clock went
	// backwards. No earlier moment has the right to become "due" again.
	for _, back := range []time.Duration{-time.Nanosecond, -5 * time.Minute, -2 * time.Hour, -30 * 24 * time.Hour} {
		now := firstMark.Add(back)
		due, err := s.IsDue(CategoryUnaccepted, base, firstMark, now)
		if err != nil {
			t.Fatalf("IsDue: %v", err)
		}
		if due {
			t.Errorf("clock moved %s backwards (to %v) — segment declared expired again", back, now)
		}
	}
}

// No flood: a worker that slept through many segments gets ONE next mark
// ahead, not a queue of every missed one.
func TestNextReminderAt_MissedMarksAreSkippedNotReplayed(t *testing.T) {
	s := mustSettings(t, 3, 3*time.Hour, DefaultUnreviewedInterval)

	// Last reminder — the first burst mark. Then the worker was silent
	// for a day.
	last := base.Add(20 * time.Minute)
	slept := base.Add(24 * time.Hour)

	next := mustNext(t, s, CategoryUnaccepted, base, last)
	if !next.Equal(base.Add(40 * time.Minute)) {
		t.Fatalf("next mark %v, want base+40m", next)
	}

	// Form exactly one reminder "now" and ask for the next: it must lie
	// AHEAD of the current moment, otherwise the worker on the same tick
	// will emit a second, a third and so on — that is the flood.
	after := mustNext(t, s, CategoryUnaccepted, base, slept)
	if !after.After(slept) {
		t.Errorf("after a reminder at %v the next mark %v is not in the future — the worker will emit a batch",
			slept, after)
	}
	// And it is one mark, not a tail of missed ones: the distance to it
	// is no more than one background interval.
	if gap := after.Sub(slept); gap > 3*time.Hour {
		t.Errorf("next mark in %s — the scale is replaying missed segments", gap)
	}

	due, err := s.IsDue(CategoryUnaccepted, base, slept, slept)
	if err != nil {
		t.Fatalf("IsDue: %v", err)
	}
	if due {
		t.Errorf("immediately after a formed reminder the segment is «due» again — that is a flood")
	}
}

// ---------------------------------------------------------------------------
// Row 5 — a fresher assignment restarts the scale (R-5)
// ---------------------------------------------------------------------------

func TestNextReminderAt_FresherAssignmentRestartsScale(t *testing.T) {
	s := mustSettings(t, 3, 3*time.Hour, DefaultUnreviewedInterval)

	// The old schedule had already gone into the background.
	oldLast := base.Add(4 * time.Hour)
	if got := mustNext(t, s, CategoryUnaccepted, base, oldLast); !got.Equal(base.Add(7 * time.Hour)) {
		t.Fatalf("precondition: old scale background %v, want base+7h", got)
	}

	// Reassignment: the new countdown base is later than the last
	// reminder of the old one.
	newBase := base.Add(5 * time.Hour)
	got := mustNext(t, s, CategoryUnaccepted, newBase, oldLast)

	if !got.Equal(newBase.Add(20 * time.Minute)) {
		t.Errorf("after reassignment next reminder %v, want a new burst newBase+20m (%v)",
			got, newBase.Add(20*time.Minute))
	}
	if got.Equal(base.Add(7 * time.Hour)) {
		t.Errorf("after reassignment the scale continued the OLD background (%v) instead of a new burst", got)
	}

	// The full new scale is indistinguishable from a scale from a clean
	// slate.
	last := oldLast
	for n := 1; n <= 4; n++ {
		mark := mustNext(t, s, CategoryUnaccepted, newBase, last)
		fresh := mustMark(t, s, CategoryUnaccepted, newBase, n)
		if !mark.Equal(fresh) {
			t.Fatalf("mark %d after restart %v, from a clean slate %v", n, mark, fresh)
		}
		last = mark
	}

	// A reminder formed ALREADY AFTER the reassignment does not restart
	// the scale: it belongs to the new schedule and simply moves us along
	// it. A consumed segment is not handed back (INV-8 — already outside
	// this function, but the scale has no right to return it).
	insideNew := newBase.Add(25 * time.Minute)
	if got := mustNext(t, s, CategoryUnaccepted, newBase, insideNew); !got.Equal(newBase.Add(40 * time.Minute)) {
		t.Errorf("a reminder inside the new scale gave %v, want the next mark newBase+40m", got)
	}
}

// ---------------------------------------------------------------------------
// Row 6 — an out-of-bounds value is rejected, not rewritten (SC-022, SC-027)
// ---------------------------------------------------------------------------

func TestValidation_OutOfBoundsRejectedNotClamped(t *testing.T) {
	tooBig := MaxBurstCount + 1
	negative := -1
	tooShort := MinInterval - time.Second
	tooLong := MaxInterval + time.Second
	zero := time.Duration(0)

	cases := []struct {
		name     string
		override Override
		wantErr  error
		param    string
	}{
		{"burst above ceiling", Override{BurstCount: &tooBig}, ErrBurstCountOutOfRange, "burstCount"},
		{"burst negative", Override{BurstCount: &negative}, ErrBurstCountOutOfRange, "burstCount"},
		{"unaccepted interval below minimum", Override{UnacceptedInterval: &tooShort}, ErrIntervalTooShort, "unacceptedIntervalMinutes"},
		{"unaccepted interval zero", Override{UnacceptedInterval: &zero}, ErrIntervalTooShort, "unacceptedIntervalMinutes"},
		{"unreviewed interval below minimum", Override{UnreviewedInterval: &tooShort}, ErrIntervalTooShort, "unreviewedIntervalMinutes"},
		{"unaccepted interval above ceiling", Override{UnacceptedInterval: &tooLong}, ErrIntervalTooLong, "unacceptedIntervalMinutes"},
		{"unreviewed interval above ceiling", Override{UnreviewedInterval: &tooLong}, ErrIntervalTooLong, "unreviewedIntervalMinutes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.override)
			if err == nil {
				t.Fatalf("value accepted and silently turned into %+v", got)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error %v, want %v", err, tc.wantErr)
			}
			// The refusal must name the parameter — otherwise the
			// settings form cannot show which field was not accepted.
			var invalid *InvalidSettingError
			if !errors.As(err, &invalid) {
				t.Fatalf("error %T does not carry a parameter name", err)
			}
			if invalid.Param != tc.param {
				t.Errorf("refusal named parameter %q, want %q", invalid.Param, tc.param)
			}
			// And nothing was "fixed": settings were not returned
			// clamped.
			if got != (Settings{}) {
				t.Errorf("settings %+v returned together with the refusal — value silently rewritten", got)
			}
		})
	}
}

// Boundary values INSIDE the admitted range are accepted: the ceiling and
// the floor must not be cut off together with what is beyond them.
func TestValidation_BoundaryValuesAccepted(t *testing.T) {
	for _, burst := range []int{MinBurstCount, MaxBurstCount} {
		b := burst
		if _, err := Resolve(Override{BurstCount: &b}); err != nil {
			t.Errorf("burst %d on the bound rejected: %v", burst, err)
		}
	}
	for _, exact := range []time.Duration{MinInterval, MaxInterval} {
		d := exact
		s, err := Resolve(Override{UnacceptedInterval: &d, UnreviewedInterval: &d})
		if err != nil {
			t.Fatalf("interval exactly %s rejected: %v", exact, err)
		}
		if s.UnacceptedInterval != exact || s.UnreviewedInterval != exact {
			t.Errorf("accepted intervals changed: %+v", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Interval ceiling — not taste, but the limit of a mark's representability
// ---------------------------------------------------------------------------

// The maximum admitted interval must yield a scale that INCREASES. This is
// exactly where the unbounded version broke: the interval passed the check,
// and multiplication by the mark number overflowed int64 and returned a
// moment EARLIER than the base — "permanently overdue" instead of "very
// rare".
func TestMarkAt_MaximumIntervalStaysMonotonic(t *testing.T) {
	s := mustSettings(t, 0, MaxInterval, MaxInterval)

	prev := base
	for n := 1; n <= 100; n++ {
		got := mustMark(t, s, CategoryUnaccepted, base, n)
		if !got.After(prev) {
			t.Fatalf("mark %d at %v not after previous %v — scale does not increase", n, got, prev)
		}
		if want := base.Add(time.Duration(n) * MaxInterval); !got.Equal(want) {
			t.Fatalf("mark %d at %v, want %v", n, got, want)
		}
		prev = got
	}
}

// A mark number at which the offset stops being representable must arrive as
// a REFUSAL. Silent overflow here is indistinguishable from "due right now",
// and the worker would send reminders on every tick.
func TestMarkAt_IndexBeyondRepresentableRangeRefused(t *testing.T) {
	s := mustSettings(t, 0, MaxInterval, MaxInterval)

	// First unrepresentable number: the one after what still fits.
	limit := int(time.Duration(math.MaxInt64) / MaxInterval)
	if _, err := s.MarkAt(CategoryUnaccepted, base, limit); err != nil {
		t.Fatalf("mark %d is still representable, but was rejected: %v", limit, err)
	}
	for _, n := range []int{limit + 1, 1_000_000, math.MaxInt32} {
		got, err := s.MarkAt(CategoryUnaccepted, base, n)
		if !errors.Is(err, ErrScheduleOutOfRange) {
			t.Errorf("mark %d gave (%v, %v), want ErrScheduleOutOfRange", n, got, err)
		}
		if !got.IsZero() {
			t.Errorf("a moment %v returned together with the refusal", got)
		}
	}

	// Same on a scale with a burst: the background is counted from the
	// hour edge, and overflow lives in the same arithmetic.
	withBurst := mustSettings(t, MaxBurstCount, MaxInterval, MaxInterval)
	if _, err := withBurst.MarkAt(CategoryUnaccepted, base, math.MaxInt32); !errors.Is(err, ErrScheduleOutOfRange) {
		t.Errorf("background after a burst at a huge number gave %v, want ErrScheduleOutOfRange", err)
	}
}

// Empty → factory, and the factory is what is written in the spec.
func TestResolve_EmptyTakesFactoryDefaults(t *testing.T) {
	s, err := Resolve(Override{})
	if err != nil {
		t.Fatalf("empty settings set rejected: %v", err)
	}
	if s != Defaults() {
		t.Errorf("empty gave %+v, factory %+v", s, Defaults())
	}
	if s.BurstCount != 3 {
		t.Errorf("factory first-hour reminder count %d, spec says 3", s.BurstCount)
	}
	if s.UnacceptedInterval != 3*time.Hour {
		t.Errorf("factory unaccepted interval %s, spec says 3 hours", s.UnacceptedInterval)
	}
	if s.UnreviewedInterval != 12*time.Hour {
		t.Errorf("factory unreviewed interval %s, spec says 12 hours", s.UnreviewedInterval)
	}

	// Partial set: what is supplied is taken, the rest stays factory.
	burst := 0
	partial, err := Resolve(Override{BurstCount: &burst})
	if err != nil {
		t.Fatalf("partial set rejected: %v", err)
	}
	if partial.BurstCount != 0 {
		t.Errorf("explicit zero burst replaced with %d — «empty» and «zero» collapsed", partial.BurstCount)
	}
	if partial.UnacceptedInterval != DefaultUnacceptedInterval ||
		partial.UnreviewedInterval != DefaultUnreviewedInterval {
		t.Errorf("unset intervals did not take factory: %+v", partial)
	}
}

// Bounds in minutes are the same numbers as in durations: schema 000044 and
// the settings contract take them from here, the units must not drift.
func TestBoundsMirrorsInMinutes(t *testing.T) {
	if MinIntervalMinutes != 15 {
		t.Errorf("interval floor %d minutes, spec says 15", MinIntervalMinutes)
	}
	if DefaultUnacceptedIntervalMinutes != 180 {
		t.Errorf("factory unaccepted interval %d minutes, spec says 3 hours", DefaultUnacceptedIntervalMinutes)
	}
	if DefaultUnreviewedIntervalMinutes != 720 {
		t.Errorf("factory unreviewed interval %d minutes, spec says 12 hours", DefaultUnreviewedIntervalMinutes)
	}
	if MaxIntervalMinutes != 366*24*60 {
		t.Errorf("interval ceiling %d minutes, contract says 366 days", MaxIntervalMinutes)
	}
	if time.Duration(MinIntervalMinutes)*time.Minute != MinInterval {
		t.Errorf("minutes mirror drifted from the duration")
	}
	if time.Duration(MaxIntervalMinutes)*time.Minute != MaxInterval {
		t.Errorf("ceiling minutes mirror drifted from the duration")
	}
	// The ceiling must sit well below the representability limit —
	// otherwise it does not protect against the overflow it exists for.
	if MaxInterval >= time.Duration(math.MaxInt64)/2 {
		t.Errorf("ceiling %s leaves no headroom to the time.Duration limit", MaxInterval)
	}
}

// ---------------------------------------------------------------------------
// Degenerate-input refusals
// ---------------------------------------------------------------------------

func TestSchedule_DegenerateInputRejected(t *testing.T) {
	s := Defaults()

	if _, err := s.NextReminderAt(CategoryUnaccepted, time.Time{}, time.Time{}); !errors.Is(err, ErrNoBase) {
		t.Errorf("empty countdown base gave %v, want ErrNoBase", err)
	}
	if _, err := s.MarkAt(CategoryUnaccepted, time.Time{}, 1); !errors.Is(err, ErrNoBase) {
		t.Errorf("MarkAt with empty base gave %v, want ErrNoBase", err)
	}
	if _, err := s.MarkAt(CategoryUnaccepted, base, 0); !errors.Is(err, ErrNonPositiveIndex) {
		t.Errorf("zero mark number gave %v, want ErrNonPositiveIndex", err)
	}
	if _, err := s.NextReminderAt(Category("digest"), base, time.Time{}); !errors.Is(err, ErrUnknownCategory) {
		t.Errorf("unknown category gave %v, want ErrUnknownCategory", err)
	}
	if Category("digest").Valid() {
		t.Error("unknown category declared admitted")
	}

	// Corrupt settings do not compute a scale silently: the refusal
	// arrives on the computation path too, not only on save.
	broken := Settings{BurstCount: 99, UnacceptedInterval: time.Minute, UnreviewedInterval: time.Minute}
	if _, err := broken.NextReminderAt(CategoryUnaccepted, base, time.Time{}); err == nil {
		t.Error("scale computed from out-of-bounds settings")
	}
}
