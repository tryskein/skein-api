package reminders

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidDuration is the sentinel for an unparseable snooze `by`
// expression. Handlers map it to validation_error / 400.
type ErrInvalidDuration struct {
	Input string
}

func (e *ErrInvalidDuration) Error() string {
	return fmt.Sprintf("invalid snooze duration %q: expected <n><unit> with unit in h|d|w", e.Input)
}

// ParseBy parses a relative snooze expression of the form "<n><unit>"
// where unit is one of:
//
//	h — hours
//	d — days  (24h)
//	w — weeks (7d)
//
// n must be a positive integer (1..N). The canonical presets the UI
// exposes — 1h, 1d, 3d, 5d, 1w — are all covered by the general grammar,
// so there is no separate preset table to drift out of sync. Returns the
// duration to add to now().
//
// Examples: "1h" → 1h, "3d" → 72h, "1w" → 168h.
//
// We deliberately do NOT lean on time.ParseDuration: it has no day/week
// unit and silently accepts sub-second units (ns/us/ms) and negative /
// fractional values we don't want for a human-facing alarm. A bespoke
// parser keeps the accepted set tight and the error message actionable.
func ParseBy(s string) (time.Duration, error) {
	raw := strings.TrimSpace(s)
	if len(raw) < 2 {
		return 0, &ErrInvalidDuration{Input: s}
	}
	unit := raw[len(raw)-1]
	numPart := raw[:len(raw)-1]

	n, err := strconv.Atoi(numPart)
	if err != nil || n <= 0 {
		return 0, &ErrInvalidDuration{Input: s}
	}

	var per time.Duration
	switch unit {
	case 'h', 'H':
		per = time.Hour
	case 'd', 'D':
		per = 24 * time.Hour
	case 'w', 'W':
		per = 7 * 24 * time.Hour
	default:
		return 0, &ErrInvalidDuration{Input: s}
	}

	// Overflow guard: a pathological n (e.g. 1000000000w) would wrap the
	// int64 nanosecond count and yield a remind_at in the past. Cap at a
	// year of duration's worth — well past any sane alarm horizon — and
	// reject anything larger as invalid input.
	const maxDuration = 366 * 24 * time.Hour
	d := time.Duration(n) * per
	if d <= 0 || d > maxDuration {
		return 0, &ErrInvalidDuration{Input: s}
	}
	return d, nil
}
