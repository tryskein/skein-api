package notifications

// quiet.go — sk-340: the recipient's quiet window.
//
// MAINTAINERS' DECISION that this file carries out: a push that falls into the quiet
// window is NOT SENT AT ALL. No shift to the end of the window, no catch-up after
// it — the rationale is in the header of migration 000052. The feed row is
// created as usual: only the push is silent, the person sees the notification in
// the morning in the interface.
//
// WHAT LIVES HERE AND WHY EXACTLY HERE. The predicate "is it quiet for this
// recipient now" is a PURE function of the moment, the zone and the bounds. It is deliberately
// separated from reading the settings from the database: the only place where an
// implementation of the window goes wrong is the day arithmetic (crossing midnight, the bounds,
// clock changes), and it has to be checked by a table of cases, not by
// bringing up the delivery path.
//
// ⚠ BUILT-IN TIME ZONE DATABASE. The blank import `time/tzdata` embeds the
// IANA database into the binary. Without it `time.LoadLocation` depends on whether
// the image has the tzdata package installed — and "depends on the image" here means "quiet
// silently stops working for everyone whose zone failed to load", because
// an unloaded zone is treated as "not quiet" (see QuietSettings.QuietAt).
// A failure observable only as "for some reason night pushes arrive" costs
// more than half a megabyte in the binary.

import (
	"time"
	_ "time/tzdata"
)

// QuietWindow — the bounds of the quiet window in the recipient's WALL-CLOCK time, as
// offsets from midnight.
//
// Start is inclusive, End is exclusive. The asymmetry is not arbitrary: it
// makes adjacent windows join without a gap and without an overlap, and it is exactly what
// `ContainsLocal` repeats.
type QuietWindow struct {
	// Start — the start of the window, an offset from midnight. Inclusive.
	Start time.Duration
	// End — the end of the window, an offset from midnight. NOT inclusive.
	End time.Duration
}

// ContainsLocal answers whether the recipient's LOCAL time falls into the window.
//
// ⚠ CROSSING MIDNIGHT is not an edge case but the main one. A night window
// 22:00–07:00 has End LESS than Start, and the naive "between start and end"
// never fires on it: 23:00 does not lie between 22:00 and 07:00 in the sense of the
// number line. So there are two branches, and the second is about a day closed into
// a ring.
//
// ⚠ A DEGENERATE WINDOW (Start == End) answers "not quiet". Such a value is
// rejected by the schema (user_prefs_quiet_hours_nondegenerate_chk), so it does
// not get here; the branch exists because ring arithmetic on it
// would return "quiet ALWAYS" — the loudest possible way to go wrong
// on a setting that defines no window.
func (w QuietWindow) ContainsLocal(local time.Time) bool {
	if w.Start == w.End {
		return false
	}
	since := time.Duration(local.Hour())*time.Hour +
		time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second +
		time.Duration(local.Nanosecond())
	if w.Start < w.End {
		// A window within a single day: 13:00–14:00.
		return since >= w.Start && since < w.End
	}
	// The window crosses midnight: 22:00–07:00.
	return since >= w.Start || since < w.End
}

// QuietSettings — everything needed to answer the question "is it quiet for this
// recipient now?": their zone and their window.
//
// Both fields are pointers, and both being "empty" mean DIFFERENT kinds of ignorance:
//   - Location == nil — the zone is unknown (the person never came in through the
//     browser) or the zone name failed to load;
//   - Window == nil — the window is off, the factory state.
type QuietSettings struct {
	// Location — the recipient's zone. nil — unknown.
	Location *time.Location
	// Window — the bounds of the window. nil — the window is off.
	Window *QuietWindow
}

// QuietAt answers whether the push is silent for this recipient at the given moment.
//
// ⚠ UNCERTAINTY IS RESOLVED IN FAVOUR OF SENDING, and this is the only
// direction that can be defended here. A window without a zone is uninterpretable:
// "22:00" without a zone is not a moment but a string. Staying silent on it would mean
// muting pushes forever because of a setting nobody set meaningfully, and
// the failure would be visible only as "notifications do not arrive". The opposite error —
// one push at the wrong hour — is loud and fixable.
//
// The pair "window set, zone unknown" is unreachable through the application
// layer: setting a window with a NULL zone is rejected (422 timezone_required).
// The branch guards against a state written around it.
func (s QuietSettings) QuietAt(instant time.Time) bool {
	if s.Window == nil || s.Location == nil {
		return false
	}
	return s.Window.ContainsLocal(instant.In(s.Location))
}
