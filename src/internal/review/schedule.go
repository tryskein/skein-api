// Package review holds the "author review and repeat reminders"
// domain (sk-303.14). This file is its schedule contract: the pure,
// I/O-free arithmetic that decides WHEN the next repeat reminder for a person
// is due, plus the validator for the user-facing settings that shape it.
//
// Nothing here touches a database, an HTTP request, the clock or the
// environment. Every function is a total function of its arguments — which is
// the point: the same triple (settings, base, last) must always name the same
// moment, on any replica, at any tick, so that two workers racing over the same
// segment compute the same deadline and one of them loses the conditional
// update instead of both sending (INV-8).
//
// The two categories are deliberately NOT symmetric:
//
//   - Unaccepted (US-4) gets a two-phase schedule — a burst inside the first
//     hour, then a steady background interval. An assigned task the person has
//     not acknowledged is the case where "notice this" is urgent.
//   - Unreviewed (US-5) gets the background interval only, never a burst
//     (SC-013): a closed result nobody has read is not urgent, and bursting on
//     it is the noise people unsubscribe from.
package review

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// ---------------------------------------------------------------------------
// Factory values, units and bounds — the single source for the whole feature.
// ---------------------------------------------------------------------------
//
// The migration's CHECK constraints (000044) and the settings endpoint's 422
// (T010) both derive their bounds from the constants below; nothing else is
// allowed to hardcode them, or the schema and the domain drift apart in
// silence.
//
// Bounds are chosen from consequence, not taste (spec, "Schedule contract"):
// more often than once per 10 minutes stops being "notice this task" and
// becomes the hammering people mute wholesale, taking the whole feature with
// it; an unread result is not urgent, so twice a day is enough not to lose it
// and rare enough not to annoy. The interval CEILING is there for a different
// reason — see MaxInterval: past it the arithmetic stops being representable.
const (
	// BurstWindow — the span the burst reminders are spread over. The first
	// burst mark sits at BurstWindow/N, the last one exactly at BurstWindow.
	BurstWindow = time.Hour

	// MinBurstCount / MaxBurstCount — inclusive bounds for the number of
	// reminders inside the first hour. 0 means "no burst at all"; 6 is the
	// hard ceiling that keeps the cadence at no more than one per 10 minutes.
	MinBurstCount = 0
	MaxBurstCount = 6

	// DefaultBurstCount — built-in default used when the user left the setting
	// empty ("Empty → take factory").
	DefaultBurstCount = 3

	// MinInterval — floor for both background intervals.
	MinInterval = 15 * time.Minute

	// MaxInterval — ceiling for both background intervals: 366 days, a full
	// year plus the leap day.
	//
	// The ceiling is arithmetic, not taste. Marks are interval*index, and
	// time.Duration is an int64 of nanoseconds that runs out at ~292 years.
	// Without a ceiling a perfectly "valid" large interval makes that
	// multiplication WRAP: the offset comes out NEGATIVE and the mark lands
	// BEFORE the base — a reminder permanently overdue, which is the exact
	// opposite of what the person asked for by setting a long interval. The
	// storage side is no protection either: the column is int4 and admits
	// 2_147_483_647 minutes, roughly fourteen times past the wrap.
	//
	// 366 days is chosen where the two consequences meet: it is far below the
	// wrap (it leaves ~291 marks of headroom before scaleInterval refuses),
	// and it is past every honest cadence — a person who wants silence has
	// the mechanic's own off switch (INV-6), not an interval so long the
	// reminder never comes.
	MaxInterval = 366 * 24 * time.Hour

	// DefaultUnacceptedInterval / DefaultUnreviewedInterval — factory values
	// for the background phase of each category.
	DefaultUnacceptedInterval = 3 * time.Hour
	DefaultUnreviewedInterval = 12 * time.Hour
)

// Storage-unit mirrors of the bounds above. Intervals are persisted and
// transported as whole minutes (a duration string in a CHECK constraint is not
// a thing), so the schema and the REST contract need the same numbers in that
// unit. They are derived, not retyped, so the two units cannot disagree.
const (
	MinIntervalMinutes               = int(MinInterval / time.Minute)
	MaxIntervalMinutes               = int(MaxInterval / time.Minute)
	DefaultUnacceptedIntervalMinutes = int(DefaultUnacceptedInterval / time.Minute)
	DefaultUnreviewedIntervalMinutes = int(DefaultUnreviewedInterval / time.Minute)
)

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

// Category names one of the two reminder streams. It is also the value stored
// in the rate-limit state ("per user and category") and carried on the
// notification reason, so the string form is part of the contract.
type Category string

// categories is the closed set, in declaration order. Filled ONLY by
// registerCategory; no hand edits.
//
// ⚠ Why this is a registry, not an enum with a handwritten Categories()
// (repair A/8). Previously the constant declarations, the Categories() list
// and the Valid() predicate were THREE handwritten copies of one set. A third
// category added as a constant would silently miss both the worker's sweep
// (Scheduler.tickAt takes the set from Categories) and the admissibility
// check — that is, a category would be created that never reminds anyone, and
// nothing would say so. A leftover of wave A/1 that became dangerous the
// moment Categories() gained a production consumer.
//
// Now declaring a category and inserting it into the set is one action, and
// Categories() and Valid() are derived from the set, not written next to it.
var categories []Category

// registerCategory declares a category and at the same moment inserts it
// into the closed set.
func registerCategory(name string) Category {
	c := Category(name)
	if name == "" {
		panic("review: category must have a name")
	}
	if slices.Contains(categories, c) {
		panic("review: category registered twice: " + name)
	}
	categories = append(categories, c)
	return c
}

var (
	// CategoryUnaccepted — an assigned, unaccepted task; reminded to the
	// ASSIGNEE. Two-phase schedule (burst + background).
	CategoryUnaccepted = registerCategory("unaccepted")

	// CategoryUnreviewed — a closed result the author has not marked as read;
	// reminded to the AUTHOR. Background only, no burst (SC-013).
	CategoryUnreviewed = registerCategory("unreviewed")
)

// Categories returns every category, in a stable order. Callers that must
// iterate the closed set (workers, metrics labels, settings forms) take it
// from here instead of writing their own list that later goes stale.
//
// A copy: the set is shared by every caller, and a caller that sorted or
// truncated the slice in place would quietly change what the background worker
// sweeps.
func Categories() []Category {
	return slices.Clone(categories)
}

// Valid reports whether c belongs to the closed set. Derived from the same
// registry as Categories, so the two cannot disagree.
func (c Category) Valid() bool {
	return slices.Contains(categories, c)
}

// String makes Category printable in log attributes and errors.
func (c Category) String() string { return string(c) }

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Sentinels for the failure modes of this package. The settings ones surface
// as 422 (business error) at the API boundary; ErrNoBase and ErrUnknownCategory
// are programming errors on the caller's side and surface as 500.
var (
	// ErrBurstCountOutOfRange — the burst count is outside MinBurstCount..MaxBurstCount.
	ErrBurstCountOutOfRange = errors.New("burst count out of range")

	// ErrIntervalTooShort — a background interval is below MinInterval.
	ErrIntervalTooShort = errors.New("background interval below the minimum")

	// ErrIntervalTooLong — a background interval is above MaxInterval.
	ErrIntervalTooLong = errors.New("background interval above the maximum")

	// ErrScheduleOutOfRange — the requested mark lies beyond what
	// time.Duration can represent. Refusing loudly is the whole point: the
	// alternative is int64 wraparound, which returns a NEGATIVE offset and a
	// mark before the base without any sign that anything went wrong.
	ErrScheduleOutOfRange = errors.New("schedule mark out of representable range")

	// ErrNoBase — the schedule was asked for a moment without a base to count
	// from. There is no sane fallback: counting from time.Time{} would put
	// every mark in year 1 and make everything permanently due.
	ErrNoBase = errors.New("schedule base is not set")

	// ErrUnknownCategory — a Category value outside the closed set.
	ErrUnknownCategory = errors.New("unknown reminder category")

	// ErrNonPositiveIndex — MarkAt was asked for the 0th or a negative mark.
	// Marks are 1-based; index 0 would silently mean "the base itself".
	ErrNonPositiveIndex = errors.New("reminder index must be >= 1")
)

// InvalidSettingError says WHICH setting was rejected and why, so the settings
// endpoint can name the offending field in its 422 body instead of returning
// an opaque "validation failed". Unwraps to the sentinel above, so
// errors.Is(err, ErrIntervalTooShort) works on it.
type InvalidSettingError struct {
	// Param is the wire name of the offending setting, e.g. "burstCount".
	Param string
	// Detail is a human-readable explanation including the observed value.
	Detail string

	err error
}

func (e *InvalidSettingError) Error() string {
	return fmt.Sprintf("invalid schedule setting %s: %s", e.Param, e.Detail)
}

func (e *InvalidSettingError) Unwrap() error { return e.err }

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// Settings is a RESOLVED schedule configuration: every field carries an
// effective value, never "unset". Turn user input into one with Resolve, which
// applies factory defaults to the empty fields and rejects the out-of-range
// ones.
type Settings struct {
	// BurstCount — reminders inside the first hour for CategoryUnaccepted.
	// 0 disables the burst entirely.
	BurstCount int

	// UnacceptedInterval — background interval for CategoryUnaccepted.
	UnacceptedInterval time.Duration

	// UnreviewedInterval — background interval for CategoryUnreviewed.
	UnreviewedInterval time.Duration
}

// Defaults returns the factory settings.
func Defaults() Settings {
	return Settings{
		BurstCount:         DefaultBurstCount,
		UnacceptedInterval: DefaultUnacceptedInterval,
		UnreviewedInterval: DefaultUnreviewedInterval,
	}
}

// Override carries what the user actually supplied. A nil field means "empty"
// — the user has no opinion and the built-in default applies. It is NOT a way to
// say "zero": *0 for BurstCount is a legal, meaningful value ("no burst"), and
// the pointer is what keeps it distinguishable from "not set".
type Override struct {
	BurstCount         *int
	UnacceptedInterval *time.Duration
	UnreviewedInterval *time.Duration
}

// Resolve turns user input into effective Settings: supplied values are
// validated, empty ones fall back to the factory.
//
// An out-of-range value is REJECTED, never clamped (SC-022, SC-027). Silently
// rewriting 90 into 6 would leave the person looking at a form that agreed
// with them and a system that did something else.
func Resolve(o Override) (Settings, error) {
	s := Defaults()

	if o.BurstCount != nil {
		if err := ValidateBurstCount(*o.BurstCount); err != nil {
			return Settings{}, err
		}
		s.BurstCount = *o.BurstCount
	}
	if o.UnacceptedInterval != nil {
		if err := ValidateInterval("unacceptedIntervalMinutes", *o.UnacceptedInterval); err != nil {
			return Settings{}, err
		}
		s.UnacceptedInterval = *o.UnacceptedInterval
	}
	if o.UnreviewedInterval != nil {
		if err := ValidateInterval("unreviewedIntervalMinutes", *o.UnreviewedInterval); err != nil {
			return Settings{}, err
		}
		s.UnreviewedInterval = *o.UnreviewedInterval
	}
	return s, nil
}

// ValidateBurstCount rejects a burst count outside MinBurstCount..MaxBurstCount.
func ValidateBurstCount(n int) error {
	if n < MinBurstCount || n > MaxBurstCount {
		return &InvalidSettingError{
			Param: "burstCount",
			Detail: fmt.Sprintf("got %d, want %d..%d",
				n, MinBurstCount, MaxBurstCount),
			err: ErrBurstCountOutOfRange,
		}
	}
	return nil
}

// ValidateInterval rejects a background interval outside
// MinInterval..MaxInterval. param names the field for the caller's error
// payload.
//
// Both ends are checked here and nowhere else: this is the single gate every
// path goes through — Resolve on the write side, Settings.Validate on the read
// side, and the mark arithmetic before it multiplies anything.
func ValidateInterval(param string, d time.Duration) error {
	if d < MinInterval {
		return &InvalidSettingError{
			Param:  param,
			Detail: fmt.Sprintf("got %s, want at least %s", d, MinInterval),
			err:    ErrIntervalTooShort,
		}
	}
	if d > MaxInterval {
		return &InvalidSettingError{
			Param:  param,
			Detail: fmt.Sprintf("got %s, want at most %s", d, MaxInterval),
			err:    ErrIntervalTooLong,
		}
	}
	return nil
}

// ValidateIntervalMinutes is the same bound check as ValidateInterval, but in
// the STORAGE AND TRANSPORT UNIT: whole minutes. The bounds are the same
// numbers — MinIntervalMinutes / MaxIntervalMinutes are derived by division,
// not retyped — so a third instance of the bounds does not appear here.
//
// A separate function, not "multiply by time.Minute and call
// ValidateInterval", for an arithmetic reason: conversion to time.Duration
// multiplies by 6e10, and a value that arrived over the wire (int64, the
// client is free to send anything) OVERFLOWS on that multiplication and
// arrives negative. A negative duration is below MinInterval, so the refusal
// would still happen, but for a false reason ("interval too short" on a
// gigantic number) and with a Detail that is meaningless to a person. The
// check in minutes refuses for the real reason and before any multiplication.
func ValidateIntervalMinutes(param string, minutes int) error {
	if minutes < MinIntervalMinutes {
		return &InvalidSettingError{
			Param:  param,
			Detail: fmt.Sprintf("got %d minutes, want at least %d", minutes, MinIntervalMinutes),
			err:    ErrIntervalTooShort,
		}
	}
	if minutes > MaxIntervalMinutes {
		return &InvalidSettingError{
			Param:  param,
			Detail: fmt.Sprintf("got %d minutes, want at most %d", minutes, MaxIntervalMinutes),
			err:    ErrIntervalTooLong,
		}
	}
	return nil
}

// ResolveMinutes is Resolve for the stored and transport form of settings:
// three optional values, intervals in whole minutes. nil means "empty" and
// yields the built-in default; 0 for the reminder count is a legal setting
// "no burst" and does not collapse with "empty" (that distinction is why the
// schema columns are declared NULLABLE, not "0 = unset").
//
// This is the only conversion from the storage unit into the domain unit:
// neither the settings handler nor the store multiplies minutes by
// time.Minute itself, and neither knows the bounds. A value outside the
// bounds is REJECTED (SC-022, SC-027) — the error unwraps to
// *InvalidSettingError and names the field.
func ResolveMinutes(burstCount, unacceptedMinutes, unreviewedMinutes *int) (Settings, error) {
	var o Override
	if burstCount != nil {
		o.BurstCount = burstCount
	}
	if unacceptedMinutes != nil {
		if err := ValidateIntervalMinutes("unacceptedIntervalMinutes", *unacceptedMinutes); err != nil {
			return Settings{}, err
		}
		d := time.Duration(*unacceptedMinutes) * time.Minute
		o.UnacceptedInterval = &d
	}
	if unreviewedMinutes != nil {
		if err := ValidateIntervalMinutes("unreviewedIntervalMinutes", *unreviewedMinutes); err != nil {
			return Settings{}, err
		}
		d := time.Duration(*unreviewedMinutes) * time.Minute
		o.UnreviewedInterval = &d
	}
	return Resolve(o)
}

// Validate checks an already-assembled Settings value. Used on the read path
// (settings loaded from storage) and as the guard inside the mark arithmetic,
// so a corrupt row cannot produce a nonsense schedule.
func (s Settings) Validate() error {
	if err := ValidateBurstCount(s.BurstCount); err != nil {
		return err
	}
	if err := ValidateInterval("unacceptedIntervalMinutes", s.UnacceptedInterval); err != nil {
		return err
	}
	return ValidateInterval("unreviewedIntervalMinutes", s.UnreviewedInterval)
}

// BurstCountFor returns the burst count that applies to the category: the
// configured one for unaccepted, and always 0 for unreviewed — SC-013 makes
// the absence of a burst a property of the category, not a setting the user
// could switch on by raising BurstCount.
func (s Settings) BurstCountFor(cat Category) (int, error) {
	switch cat {
	case CategoryUnaccepted:
		return s.BurstCount, nil
	case CategoryUnreviewed:
		return 0, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownCategory, string(cat))
	}
}

// IntervalFor returns the background interval that applies to the category.
func (s Settings) IntervalFor(cat Category) (time.Duration, error) {
	switch cat {
	case CategoryUnaccepted:
		return s.UnacceptedInterval, nil
	case CategoryUnreviewed:
		return s.UnreviewedInterval, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownCategory, string(cat))
	}
}

// ---------------------------------------------------------------------------
// The schedule itself
// ---------------------------------------------------------------------------

// maxDuration — the largest value time.Duration can hold (~292 years).
const maxDuration = time.Duration(math.MaxInt64)

// scaleInterval returns interval*steps, refusing rather than wrapping.
//
// This guard is the second half of the MaxInterval bound and cannot be dropped
// in favour of it: bounding the interval alone still leaves the INDEX free, and
// a person whose task sat unaccepted long enough — or a corrupt "last" that
// puts the counter far out — reaches the same int64 wrap from the other side.
// A wrapped product is not a large number, it is a NEGATIVE one, and it would
// come back as a mark before the base: due immediately, forever. So the check
// happens BEFORE the multiplication (division cannot overflow), and the caller
// gets ErrScheduleOutOfRange instead of a plausible-looking wrong moment.
func scaleInterval(interval time.Duration, steps int64) (time.Duration, error) {
	if interval <= 0 {
		return 0, fmt.Errorf("%w: non-positive interval %s", ErrScheduleOutOfRange, interval)
	}
	if steps < 0 {
		return 0, fmt.Errorf("%w: negative step count %d", ErrScheduleOutOfRange, steps)
	}
	if steps > int64(maxDuration/interval) {
		return 0, fmt.Errorf("%w: %d steps of %s exceed the representable span %s",
			ErrScheduleOutOfRange, steps, interval, maxDuration)
	}
	return interval * time.Duration(steps), nil
}

// offsetAfterBurst returns BurstWindow + interval*steps with the same refusal
// on overflow, including the final addition.
func offsetAfterBurst(interval time.Duration, steps int64) (time.Duration, error) {
	off, err := scaleInterval(interval, steps)
	if err != nil {
		return 0, err
	}
	if off > maxDuration-BurstWindow {
		return 0, fmt.Errorf("%w: %s past the burst window exceeds the representable span %s",
			ErrScheduleOutOfRange, off, maxDuration)
	}
	return BurstWindow + off, nil
}

// MarkAt returns the moment of the n-th reminder (1-based) of the category,
// counted from base.
//
// The shape of the sequence, straight from the "Schedule contract":
//
//	burst  (n <= B, only when B > 0):  base + BurstWindow*n/B
//	                                   → equal segments, the last one landing
//	                                     exactly on base + BurstWindow
//	background after a burst (B > 0):  base + BurstWindow + interval*(n-B)
//	                                   → the background phase counts from the
//	                                     LAST BURST MARK, not from base
//	background with no burst (B == 0): base + interval*n
//
// Worked example from the spec: B=3, interval=3h → 20m, 40m, 60m, then 4h from
// base. That "4h" is the whole reason the background offset is BurstWindow +
// interval and not interval alone.
//
// The burst offset is computed as BurstWindow*n/B in one expression instead of
// accumulating a per-step segment: for B in 1..6 the division is exact
// (3600s/6 = 600s), and the single expression keeps it exact even if the
// window ever stops dividing evenly — the marks stay monotonic and the last
// one stays exactly on the window edge.
func (s Settings) MarkAt(cat Category, base time.Time, n int) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	if base.IsZero() {
		return time.Time{}, ErrNoBase
	}
	if n < 1 {
		return time.Time{}, fmt.Errorf("%w: got %d", ErrNonPositiveIndex, n)
	}
	burst, err := s.BurstCountFor(cat)
	if err != nil {
		return time.Time{}, err
	}
	interval, err := s.IntervalFor(cat)
	if err != nil {
		return time.Time{}, err
	}

	if burst > 0 {
		if n <= burst {
			// n <= burst <= MaxBurstCount, so BurstWindow*n is at most six
			// hours: this product cannot overflow and needs no guard.
			return base.Add(time.Duration(int64(BurstWindow) * int64(n) / int64(burst))), nil
		}
		off, err := offsetAfterBurst(interval, int64(n-burst))
		if err != nil {
			return time.Time{}, err
		}
		return base.Add(off), nil
	}
	off, err := scaleInterval(interval, int64(n))
	if err != nil {
		return time.Time{}, err
	}
	return base.Add(off), nil
}

// NextReminderAt returns the moment of the next reminder for the category.
//
//   - base is the countdown base: the moment the task became this person's
//     (R-5). A fresher assignment means a fresher base, and passing it here is
//     what restarts the schedule from zero — new base, new burst.
//   - last is the moment of the most recent reminder ALREADY FORMED for this
//     (person, category) pair. Zero means none yet, and the answer is the very
//     first mark — which is why the first unaccepted reminder can never land
//     earlier than one burst segment after the assignment (INV-11).
//
// A last that PRECEDES base is treated as "none yet": it belongs to the
// schedule that the fresher assignment replaced, and letting it survive the
// restart would skip the new burst. Rate limiting is a separate concern — the
// already-consumed segment is not handed back by this function, it is held by
// the rate-limit state (INV-8).
//
// The answer is the first mark STRICTLY AFTER last, computed in closed form
// rather than by walking the sequence: a worker that was down for a day must
// not iterate a day's worth of marks, and — more importantly — must not
// replay them. Missed marks are skipped, not queued; that is what keeps a
// restored worker from firing a burst of reminders for every segment it slept
// through.
func (s Settings) NextReminderAt(cat Category, base, last time.Time) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	if base.IsZero() {
		return time.Time{}, ErrNoBase
	}
	burst, err := s.BurstCountFor(cat)
	if err != nil {
		return time.Time{}, err
	}
	interval, err := s.IntervalFor(cat)
	if err != nil {
		return time.Time{}, err
	}

	// No reminder yet for this schedule — or the last one belongs to a
	// schedule that a fresher assignment already replaced.
	if last.IsZero() || !last.After(base) {
		return s.MarkAt(cat, base, 1)
	}

	elapsed := last.Sub(base)

	// Still inside the burst window: answer with the next burst mark.
	if burst > 0 && elapsed < BurstWindow {
		// Smallest k with BurstWindow*k/burst > elapsed, i.e.
		// k = floor(elapsed*burst / BurstWindow) + 1. Clamped to the burst
		// length: the guard above already ensures k <= burst, the clamp is
		// belt-and-braces against a future BurstWindow that is not exactly
		// divisible.
		k := int(int64(elapsed)*int64(burst)/int64(BurstWindow)) + 1
		if k > burst {
			k = burst
		}
		return s.MarkAt(cat, base, k)
	}

	// Background phase. The phase starts at the end of the burst window when
	// there is a burst, and at base itself when there is none.
	phaseStart := base
	if burst > 0 {
		phaseStart = base.Add(BurstWindow)
	}
	sinceStart := last.Sub(phaseStart)
	if sinceStart < 0 {
		// Only reachable with burst == 0, where phaseStart == base and the
		// caller-supplied last was already handled above; kept so the integer
		// division below can never go negative.
		sinceStart = 0
	}
	steps := int64(sinceStart/interval) + 1
	off, err := scaleInterval(interval, steps)
	if err != nil {
		return time.Time{}, err
	}
	return phaseStart.Add(off), nil
}

// EarliestFirstMark returns a LOWER BOUND on how long after the base the FIRST
// reminder of the category can possibly fall due, across every settings value
// the contract admits.
//
// # What it is NOT
//
// It is not a schedule and must never be used as one. The schedule is
// MarkAt/NextReminderAt/IsDue and there is exactly one of it (INV-11 is stated
// in terms of those). This is a BOUND derived from the very same constants:
// it answers "no setting whatsoever can make the first mark earlier than this",
// and nothing else. Anything that needs the actual moment must ask MarkAt.
//
// # Why it exists
//
// The nudge worker's candidate query cannot evaluate the schedule — that would
// be a second implementation of it in SQL, which is exactly what INV-3 forbids.
// But it CAN skip the rows that no schedule could possibly have made due yet,
// and skipping them is not an optimisation: a recipient whose first mark has
// not arrived stays in the result set every single tick, occupies a slot of the
// per-tick budget and produces nothing, so everyone past the budget boundary
// starves for as long as the head keeps refreshing its base. That head does
// keep refreshing — Base is MAX(assignee_set_at), so a person who is handed
// tasks continuously never falls due at all.
//
// # Why the numbers are these
//
//	unaccepted: a burst of B in 1..MaxBurstCount puts the first mark at
//	            BurstWindow/B, whose minimum over the admitted range is
//	            BurstWindow/MaxBurstCount; B == 0 puts it at the background
//	            interval, at least MinInterval. The bound is the smaller.
//	unreviewed: no burst, ever (SC-013), so the first mark is the background
//	            interval — at least MinInterval.
//
// The bound is therefore SAFE by construction: a candidate excluded by it could
// not have been due under any settings, so INV-11 cannot be violated in the
// permissive direction and no reminder is lost in the other. The property is
// asserted against MarkAt over the whole settings space, not asserted by this
// comment.
func EarliestFirstMark(cat Category) (time.Duration, error) {
	switch cat {
	case CategoryUnaccepted:
		burstFirst := BurstWindow / time.Duration(MaxBurstCount)
		if MinInterval < burstFirst {
			return MinInterval, nil
		}
		return burstFirst, nil
	case CategoryUnreviewed:
		return MinInterval, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownCategory, string(cat))
	}
}

// IsDue reports whether a reminder of this category may be formed at now.
//
// It is a strict "the moment has arrived" test: due at exactly the mark, not
// before. That strictness is what makes the schedule immune to the clock
// moving BACKWARDS — an earlier now simply is not after the mark, so no
// segment is declared expired ahead of time and no flood of catch-up
// reminders is produced when the clock is corrected. The same strictness lets
// two workers racing the same tick compute the same answer, leaving INV-8 to
// the single conditional update that consumes the segment.
func (s Settings) IsDue(cat Category, base, last, now time.Time) (bool, error) {
	next, err := s.NextReminderAt(cat, base, last)
	if err != nil {
		return false, err
	}
	return !now.Before(next), nil
}
