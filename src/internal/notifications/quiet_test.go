package notifications

import (
	"testing"
	"time"
)

// quiet_test.go — sk-340: the quiet window arithmetic.
//
// ⚠ WHY THIS TABLE EXISTS SEPARATELY FROM THE DELIVERY-PATH TEST. The delivery path proves
// that the gate is WIRED IN and that quiet is distinguishable from a breakage. It does not prove that the
// window is counted correctly: it has one moment and one set of bounds. An implementation of the window
// breaks in exactly two places — at crossing midnight and at the
// bounds — and both are reachable only by a table of cases with controlled clocks.

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone %q: %v — without the time zone database the window check "+
			"is tautological: an unloaded zone always answers \"not quiet\"", name, err)
	}
	return loc
}

func hm(h, m int) time.Duration {
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
}

// TestQuietWindow_CrossesMidnight — the case for which the predicate has two
// branches.
//
// The night window 22:00–07:00 has an end LESS than the start, and the naive "between the start
// and the end" never fires on it: 23:00 does not lie between 22:00 and 07:00
// on the number line. The rows below are chosen so that such an implementation
// goes red on the very first of them, not green by accident.
func TestQuietWindow_CrossesMidnight(t *testing.T) {
	t.Parallel()
	night := QuietWindow{Start: hm(22, 0), End: hm(7, 0)}
	for _, tc := range []struct {
		name  string
		h, m  int
		quiet bool
	}{
		{"a minute before the start", 21, 59, false},
		{"exactly the start — inclusive", 22, 0, true},
		{"evening after the start", 23, 30, true},
		{"right after midnight", 0, 1, true},
		{"deep night", 3, 0, true},
		{"a minute before the end", 6, 59, true},
		{"exactly the end — NOT inclusive", 7, 0, false},
		{"morning", 9, 0, false},
		{"day", 15, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := time.Date(2026, 3, 10, tc.h, tc.m, 0, 0, time.UTC)
			if got := night.ContainsLocal(local); got != tc.quiet {
				t.Errorf("%02d:%02d in the window 22:00–07:00 = %v, want %v", tc.h, tc.m, got, tc.quiet)
			}
		})
	}
}

// TestQuietWindow_WithinOneDay — the second branch, without which the window "13:00–14:00"
// would silently become a ring and mute twenty-three hours out of twenty-four.
func TestQuietWindow_WithinOneDay(t *testing.T) {
	t.Parallel()
	lunch := QuietWindow{Start: hm(13, 0), End: hm(14, 0)}
	for _, tc := range []struct {
		name  string
		h, m  int
		quiet bool
	}{
		{"before the window", 12, 59, false},
		{"exactly the start", 13, 0, true},
		{"inside", 13, 30, true},
		{"exactly the end", 14, 0, false},
		{"after the window", 14, 1, false},
		{"night — outside a within-day window", 3, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := time.Date(2026, 3, 10, tc.h, tc.m, 0, 0, time.UTC)
			if got := lunch.ContainsLocal(local); got != tc.quiet {
				t.Errorf("%02d:%02d in the window 13:00–14:00 = %v, want %v", tc.h, tc.m, got, tc.quiet)
			}
		})
	}
}

// TestQuietWindow_DegenerateIsNotAlwaysQuiet — a degenerate window answers "not
// quiet", not "quiet round the clock".
//
// The schema rejects such a value, so the branch guards against a state
// written around the application layer. The claim exists because the cost of
// the error here is asymmetric: ring arithmetic on Start == End would return
// "quiet ALWAYS", that is, would silently switch pushes off for good.
func TestQuietWindow_DegenerateIsNotAlwaysQuiet(t *testing.T) {
	t.Parallel()
	w := QuietWindow{Start: hm(22, 0), End: hm(22, 0)}
	for _, h := range []int{0, 6, 12, 22, 23} {
		local := time.Date(2026, 3, 10, h, 0, 0, 0, time.UTC)
		if w.ContainsLocal(local) {
			t.Errorf("the degenerate window 22:00–22:00 declared quiet at %02d:00 — "+
				"ring arithmetic on it mutes the whole day", h)
		}
	}
}

// TestQuietSettings_SameInstantDiffersByZone — what the zone is stored for
// at all.
//
// ONE moment, ONE window, DIFFERENT recipients: one is quiet, the other is not.
// An implementation that ignores the zone and counts in UTC answers both questions
// THE SAME and goes red here — while on a fixture where everyone is in UTC it would stay
// green.
func TestQuietSettings_SameInstantDiffersByZone(t *testing.T) {
	t.Parallel()
	night := QuietWindow{Start: hm(22, 0), End: hm(7, 0)}
	// 20:30 UTC. In Moscow (UTC+3) that is 23:30 — inside the night window; in
	// New York (UTC-4 under daylight saving) that is 16:30 — outside.
	instant := time.Date(2026, 3, 10, 20, 30, 0, 0, time.UTC)

	moscow := QuietSettings{Location: mustLoad(t, "Europe/Moscow"), Window: &night}
	newYork := QuietSettings{Location: mustLoad(t, "America/New_York"), Window: &night}

	if !moscow.QuietAt(instant) {
		t.Errorf("in Moscow 23:30 did not fall into the window 22:00–07:00 — the recipient's zone is not taken into account")
	}
	if newYork.QuietAt(instant) {
		t.Errorf("in New York 16:30 fell into the window 22:00–07:00 — the recipient's zone is not taken into account")
	}
	// And the same claim in the form in which it fails for an implementation
	// substituting UTC for the zone: at this moment UTC gives 20:30, that is,
	// "not quiet" for BOTH.
	if moscow.QuietAt(instant) == newYork.QuietAt(instant) {
		t.Errorf("both recipients got the same answer — the window was counted in a common zone, not in their own")
	}
}

// TestQuietSettings_FollowsDaylightSaving — why the zone NAME is stored, not
// the offset.
//
// The same moment of the day by UTC falls into different local hours in summer and
// winter. An implementation with a frozen offset answers both rows the same
// and goes red here.
func TestQuietSettings_FollowsDaylightSaving(t *testing.T) {
	t.Parallel()
	night := QuietWindow{Start: hm(22, 0), End: hm(7, 0)}
	berlin := QuietSettings{Location: mustLoad(t, "Europe/Berlin"), Window: &night}

	// 20:30 UTC in summer — 22:30 CEST (UTC+2), inside the window.
	summer := time.Date(2026, 7, 15, 20, 30, 0, 0, time.UTC)
	// 20:30 UTC in winter — 21:30 CET (UTC+1), outside the window.
	winter := time.Date(2026, 1, 15, 20, 30, 0, 0, time.UTC)

	if !berlin.QuietAt(summer) {
		t.Errorf("20:30 UTC in July is 22:30 in Berlin, the window 22:00–07:00 must be silent")
	}
	if berlin.QuietAt(winter) {
		t.Errorf("20:30 UTC in January is 21:30 in Berlin, the window 22:00–07:00 has not started yet")
	}
}

// TestQuietSettings_UnknownMeansSend — both kinds of ignorance are resolved in
// favour of sending, and neither is confused with the other.
func TestQuietSettings_UnknownMeansSend(t *testing.T) {
	t.Parallel()
	night := QuietWindow{Start: hm(0, 0), End: hm(23, 59)}
	instant := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	if (QuietSettings{Location: mustLoad(t, "Europe/Moscow")}).QuietAt(instant) {
		t.Error("the window is off, but the predicate declared quiet")
	}
	if (QuietSettings{Window: &night}).QuietAt(instant) {
		t.Error("the zone is unknown, but the predicate declared quiet — \"22:00\" without a zone is not a moment but a string")
	}
	if (QuietSettings{}).QuietAt(instant) {
		t.Error("empty settings declared quiet")
	}
}

// TestParseClock_RejectsWhatIsNotAWallClock — the bound is entered as a string, and
// a string accepted silently in the wrong form shifts the window by hours.
func TestParseClock_RejectsWhatIsNotAWallClock(t *testing.T) {
	t.Parallel()
	for _, ok := range []struct {
		in   string
		want time.Duration
	}{
		{"00:00", 0},
		{"07:00", hm(7, 0)},
		{"22:30", hm(22, 30)},
		{"23:59", hm(23, 59)},
	} {
		got, err := ParseClock(ok.in)
		if err != nil {
			t.Errorf("ParseClock(%q) returned error %v", ok.in, err)
			continue
		}
		if got != ok.want {
			t.Errorf("ParseClock(%q) = %v, want %v", ok.in, got, ok.want)
		}
		if back := FormatClock(got); back != ok.in {
			t.Errorf("FormatClock(ParseClock(%q)) = %q — writing and parsing diverged", ok.in, back)
		}
	}
	for _, bad := range []string{"", "7:00", "22", "22:00:00", "24:00", "22:60", "-1:00", "aa:bb", "22:0", "223:0"} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) accepted a value that is not a wall-clock reading", bad)
		}
	}
}

// TestValidateTimezone — checks that a zone exists, not its shape.
//
// "Europe/Moskva" is plausible and does not exist. A check by the shape of the string would have
// let it through, and quiet for one person would have switched off silently.
func TestValidateTimezone(t *testing.T) {
	t.Parallel()
	for _, good := range []string{"Europe/Moscow", "America/New_York", "UTC", "Asia/Tokyo"} {
		if _, err := ValidateTimezone(good); err != nil {
			t.Errorf("ValidateTimezone(%q) rejected an existing zone: %v", good, err)
		}
	}
	for _, bad := range []string{"", " ", "Europe/Moskva", "MSK+3", "Europe/Moscow ", "Local"} {
		if _, err := ValidateTimezone(bad); err == nil {
			t.Errorf("ValidateTimezone(%q) accepted a value that is not an IANA zone", bad)
		}
	}
}

// TestParseQuietSettings_HalfWindowIsNoWindow — two independent values come
// from the database, and one without the other does not define a window.
//
// The schema rejects such a pair (user_prefs_quiet_hours_pair_chk); the claim
// covers the parsing, not the schema: half a window interpreted as a window with a zero
// second bound would give quiet from midnight.
func TestParseQuietSettings_HalfWindowIsNoWindow(t *testing.T) {
	t.Parallel()
	tz := "Europe/Moscow"
	sec := int64(22 * 3600)

	half, err := ParseQuietSettings(&tz, &sec, nil)
	if err != nil {
		t.Fatalf("ParseQuietSettings: %v", err)
	}
	if half.Window != nil {
		t.Error("one bound of two assembled into a window")
	}

	badZone := "Europe/Moskva"
	end := int64(7 * 3600)
	broken, err := ParseQuietSettings(&badZone, &sec, &end)
	if err == nil {
		t.Error("a non-existent zone parsed without an error — the refusal will not reach the log")
	}
	if broken.QuietAt(time.Date(2026, 3, 10, 23, 0, 0, 0, time.UTC)) {
		t.Error("settings with an unloaded zone declared quiet — the window is interpreted in an unknown zone")
	}
}
