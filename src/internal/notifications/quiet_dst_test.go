package notifications

import (
	"testing"
	"time"
)

// quiet_dst_test.go — sk-539 (review of sk-340): the quiet window on the
// days the clocks change.
//
// TestQuietSettings_FollowsDaylightSaving already proves the zone NAME is
// honoured across seasons (summer vs winter offset). What it does not reach
// are the two transition days themselves, where a wall-clock boundary either
// does not exist (spring forward) or happens twice (fall back).
//
// The reference is the contract in migration 000052: a boundary is a WALL
// CLOCK mark, read in the recipient's zone at the moment of the check. So on
// those days the window follows the wall clock the person sees:
//
//   - spring forward, window 02:30–07:00: 02:30 never shows on the clock, the
//     clock jumps from 01:59 to 03:00, and from 03:00 on it is quiet;
//   - fall back, window 22:00–02:30: 02:00–02:30 shows twice, and it is quiet
//     both times; 02:45 after the second pass is not.
//
// A counterexample this catches: an implementation that turns the boundary
// into an instant with a frozen offset (for example the previous midnight's
// UTC offset plus the boundary duration) answers "not quiet" at 03:00 on the
// spring-forward day, or "not quiet" on the second pass of 02:15.
func TestQuietSettings_DaylightSavingTransitionDays(t *testing.T) {
	t.Parallel()
	berlin := mustLoad(t, "Europe/Berlin")

	// Europe/Berlin 2026: CET→CEST on 29 March at 01:00 UTC (02:00→03:00
	// local), CEST→CET on 25 October at 01:00 UTC (03:00→02:00 local).
	springWindow := QuietWindow{Start: hm(2, 30), End: hm(7, 0)}
	fallWindow := QuietWindow{Start: hm(22, 0), End: hm(2, 30)}

	cases := []struct {
		name    string
		window  QuietWindow
		instant time.Time
		want    bool
	}{
		{"spring: 01:59 CET, before the jump", springWindow,
			time.Date(2026, 3, 29, 0, 59, 0, 0, time.UTC), false},
		{"spring: 03:00 CEST, right after the jump over the 02:30 start", springWindow,
			time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), true},
		{"spring: 06:59 CEST, still inside", springWindow,
			time.Date(2026, 3, 29, 4, 59, 0, 0, time.UTC), true},
		{"spring: 07:00 CEST, end is exclusive", springWindow,
			time.Date(2026, 3, 29, 5, 0, 0, 0, time.UTC), false},

		{"fall: 02:15 CEST, first pass", fallWindow,
			time.Date(2026, 10, 25, 0, 15, 0, 0, time.UTC), true},
		{"fall: 02:15 CET, second pass of the same wall clock", fallWindow,
			time.Date(2026, 10, 25, 1, 15, 0, 0, time.UTC), true},
		{"fall: 02:45 CET, after the window", fallWindow,
			time.Date(2026, 10, 25, 1, 45, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := QuietSettings{Location: berlin, Window: &tc.window}
			if got := s.QuietAt(tc.instant); got != tc.want {
				t.Errorf("QuietAt(%s = %s local) = %v, want %v",
					tc.instant.Format(time.RFC3339), tc.instant.In(berlin).Format("15:04 MST"), got, tc.want)
			}
		})
	}
}
