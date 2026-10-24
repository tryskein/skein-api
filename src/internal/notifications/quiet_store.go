package notifications

// quiet_store.go — sk-340: storage of the time zone and the quiet window bounds.
//
// The split from quiet.go is deliberate: there is PURE day arithmetic, here is
// reading and writing. So the table of midnight-crossing cases needs no database,
// and the database needs no table of cases.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Quiet-setting refusals. Application-level, not transport-level: each is about
// the requested state making no sense, not about the database not
// answering. The handler maps them to 422.
var (
	// ErrInvalidTimezone — the zone name does not load from the time zone database.
	ErrInvalidTimezone = errors.New("unknown IANA time zone")
	// ErrTimezoneRequired — a quiet window is set for a recipient whose zone
	// is unknown. A window without a zone is uninterpretable: "22:00" without a zone is not a
	// moment but a string. The refusal here is what makes the pair "window set, zone
	// unknown" unreachable, instead of interpreting it in the predicate.
	ErrTimezoneRequired = errors.New("time zone must be set before quiet hours")
	// ErrQuietWindowDegenerate — the start and the end coincide. Such a window means
	// either "never quiet" or "always quiet", and choosing one of the two is a
	// product decision that must not be made silently for the person.
	ErrQuietWindowDegenerate = errors.New("quiet hours start and end must differ")
	// ErrQuietWindowHalfSet — exactly one bound is set. That defines no window,
	// and nobody guesses the second bound for the person.
	ErrQuietWindowHalfSet = errors.New("quiet hours need both start and end, or neither")
)

// TimezoneSource — the origin of the stored zone.
type TimezoneSource string

const (
	// TimezoneSourceManual — the zone was set by the person in the settings.
	TimezoneSourceManual TimezoneSource = "manual"
	// TimezoneSourceDetected — the zone was detected by the browser on sign-in.
	TimezoneSourceDetected TimezoneSource = "detected"
)

// TimezoneDTO — the recipient's zone in the form the contract returns it.
type TimezoneDTO struct {
	// Timezone — the IANA zone name. Empty — the zone is unknown.
	Timezone *string `json:"timezone"`
	// Source — where the value came from. Empty together with Timezone.
	Source *TimezoneSource `json:"source"`
}

// QuietHoursDTO — the quiet window plus the zone in which it is interpreted.
//
// ⚠ THE ZONE IS NOT REDUNDANT HERE, although it has its own contract. A window without a
// zone is uninterpretable, so the settings form must show IN WHICH
// ZONE "22:00" is counted — otherwise the person sees a setting whose meaning
// depends on a value that is not on the screen.
type QuietHoursDTO struct {
	// Start — the start of the window, "HH:MM". Empty together with End — the window is off.
	Start *string `json:"start"`
	// End — the end of the window, "HH:MM", exclusive.
	End *string `json:"end"`
	// Timezone — the zone in which the bounds are interpreted. Empty — a window
	// cannot be set (see ErrTimezoneRequired).
	Timezone *string `json:"timezone"`
}

// ValidateTimezone checks the zone name the way the predicate will read it: by the
// same loading as on delivery.
//
// ⚠ WHY NOT A REGULAR EXPRESSION ON THE SHAPE OF THE NAME. A shape check would let through
// "Europe/Moskva" — a plausible but non-existent name that would silently
// switch quiet off for one person. The only honest check of a zone's
// existence is trying to load it.
//
// "Local" is rejected separately: it is not an IANA zone but an instruction to "take the
// process zone", that is, the SERVER's zone in the guise of the user's zone — exactly the
// decision the task refused.
func ValidateTimezone(name string) (*time.Location, error) {
	if name == "" || name != strings.TrimSpace(name) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidTimezone, name)
	}
	if name == "Local" {
		return nil, fmt.Errorf("%w: %q is the server's own zone, not the user's", ErrInvalidTimezone, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidTimezone, name)
	}
	return loc, nil
}

// ParseClock parses a window bound written as "HH:MM" into an offset from midnight.
//
// There are no seconds in the contract on purpose: a "do not wake at night" setting to the
// second makes no sense, and extra precision in the input is one more way to go wrong.
func ParseClock(s string) (time.Duration, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("time %q is not in HH:MM form", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("time %q has an hour outside 00..23", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q has a minute outside 00..59", s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

// FormatClock — the inverse of ParseClock.
func FormatClock(d time.Duration) string {
	d %= 24 * time.Hour
	return fmt.Sprintf("%02d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// ParseQuietSettings assembles the predicate settings from the values read
// from the database: the zone name and two bounds in SECONDS from midnight.
//
// An error is returned ONLY for a zone name that failed to load, and together with it
// working settings without a zone are returned — that is, "not quiet".
// The caller needs the split: a zone that failed to load must reach the log
// (otherwise quiet is switched off for one person silently), but need not
// stop the delivery.
func ParseQuietSettings(timezone *string, startSeconds, endSeconds *int64) (QuietSettings, error) {
	var out QuietSettings
	if startSeconds != nil && endSeconds != nil {
		out.Window = &QuietWindow{
			Start: time.Duration(*startSeconds) * time.Second,
			End:   time.Duration(*endSeconds) * time.Second,
		}
	}
	if timezone == nil || *timezone == "" {
		return out, nil
	}
	loc, err := ValidateTimezone(*timezone)
	if err != nil {
		return out, err
	}
	out.Location = loc
	return out, nil
}

// quietSettingsSQL — one query for both addends of the predicate.
//
// LEFT JOIN, not INNER: the settings row is created LAZILY, and a person who
// never saved anything has none. An INNER JOIN on such a pair would return zero
// rows, that is, "no such user", and would switch quiet off exactly for those who did not
// configure it — harmless in the outcome, but indistinguishable from a real disappearance of
// the user.
//
// EXTRACT instead of reading `time` as is: seconds from midnight are what the
// predicate operates on, and converting on the database side spares a wrapper type
// on the Go side.
const quietSettingsSQL = `
SELECT u.timezone,
       EXTRACT(EPOCH FROM p.quiet_hours_start)::bigint,
       EXTRACT(EPOCH FROM p.quiet_hours_end)::bigint
  FROM users u
  LEFT JOIN user_notification_prefs p ON p.user_id = u.id
 WHERE u.id = $1`

// QuietSettingsFor reads the recipient's zone and window.
//
// A missing user row is "not quiet" without an error: there is no addressee,
// nothing to mute, and a failure here would turn a deleted user into a
// settings read failure.
func (s *Store) QuietSettingsFor(ctx context.Context, userID uuid.UUID) (QuietSettings, error) {
	var (
		tz         *string
		start, end *int64
	)
	err := s.pool.QueryRow(ctx, quietSettingsSQL, userID).Scan(&tz, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuietSettings{}, nil
	}
	if err != nil {
		return QuietSettings{}, fmt.Errorf("loading quiet hours settings: %w", err)
	}
	return ParseQuietSettings(tz, start, end)
}

// Timezone reads the recipient's zone and the origin of the value.
func (s *Store) Timezone(ctx context.Context, userID uuid.UUID) (TimezoneDTO, error) {
	var (
		tz     *string
		manual bool
	)
	err := s.pool.QueryRow(ctx,
		`SELECT timezone, timezone_set_manually FROM users WHERE id = $1`, userID).
		Scan(&tz, &manual)
	if errors.Is(err, pgx.ErrNoRows) {
		return TimezoneDTO{}, ErrNotFound
	}
	if err != nil {
		return TimezoneDTO{}, fmt.Errorf("loading timezone: %w", err)
	}
	return timezoneDTO(tz, manual), nil
}

// timezoneDTO — the common assembly of the response: the source exists only together with
// the value. No zone — no source either, not "detected by default".
func timezoneDTO(tz *string, manual bool) TimezoneDTO {
	if tz == nil || *tz == "" {
		return TimezoneDTO{}
	}
	src := TimezoneSourceDetected
	if manual {
		src = TimezoneSourceManual
	}
	return TimezoneDTO{Timezone: tz, Source: &src}
}

// SetTimezone writes the recipient's zone and returns the value IN FORCE
// after the write.
//
// ⚠ DETECTION FROM THE BROWSER DOES NOT OVERWRITE A HAND-SET VALUE — an acceptance
// requirement, and it is enforced by ONE predicate in the UPDATE, not by a read
// followed by a write: a manual edit can slip in between the read and the write, and then it
// is silently rolled back by the very next browser sign-in.
//
// Returning the value in force, not an echo of the argument, is part of the same thing: a call with
// source=detected that wrote nothing must be distinguishable from a call
// that wrote. An echo of the argument would make them indistinguishable.
func (s *Store) SetTimezone(ctx context.Context, userID uuid.UUID, name string, source TimezoneSource) (TimezoneDTO, error) {
	if _, err := ValidateTimezone(name); err != nil {
		return TimezoneDTO{}, err
	}
	manual := source == TimezoneSourceManual
	var (
		tz        *string
		setManual bool
	)
	err := s.pool.QueryRow(ctx, `
UPDATE users
   SET timezone = $2,
       timezone_set_manually = $3
 WHERE id = $1
   AND ($3 OR NOT timezone_set_manually)
RETURNING timezone, timezone_set_manually`, userID, name, manual).Scan(&tz, &setManual)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the user does not exist, or the detection from the browser collided with
		// a manual value and yielded to it. A repeated read tells them apart — and
		// it also returns the value that is in force now.
		return s.Timezone(ctx, userID)
	}
	if err != nil {
		return TimezoneDTO{}, fmt.Errorf("saving timezone: %w", err)
	}
	return timezoneDTO(tz, setManual), nil
}

// QuietHours reads the recipient's quiet window together with the zone in which it is
// interpreted.
func (s *Store) QuietHours(ctx context.Context, userID uuid.UUID) (QuietHoursDTO, error) {
	var (
		tz         *string
		start, end *int64
	)
	err := s.pool.QueryRow(ctx, quietSettingsSQL, userID).Scan(&tz, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuietHoursDTO{}, ErrNotFound
	}
	if err != nil {
		return QuietHoursDTO{}, fmt.Errorf("loading quiet hours: %w", err)
	}
	out := QuietHoursDTO{Timezone: tz}
	if start != nil && end != nil {
		s1 := FormatClock(time.Duration(*start) * time.Second)
		e1 := FormatClock(time.Duration(*end) * time.Second)
		out.Start, out.End = &s1, &e1
	}
	return out, nil
}

// SetQuietHours writes the window bounds. window == nil switches the window off.
//
// The settings row is created LAZILY, so the write is an upsert, and the inserting
// branch names the other columns by their factory values exactly as
// SetPushDisabledCategories does: otherwise editing the window would silently RESET
// a setting the person never touched.
func (s *Store) SetQuietHours(ctx context.Context, userID uuid.UUID, window *QuietWindow) error {
	var startArg, endArg any
	if window != nil {
		if window.Start == window.End {
			return ErrQuietWindowDegenerate
		}
		tz, err := s.Timezone(ctx, userID)
		if err != nil {
			return err
		}
		if tz.Timezone == nil {
			return ErrTimezoneRequired
		}
		startArg = FormatClock(window.Start)
		endArg = FormatClock(window.End)
	}
	const upsert = `
INSERT INTO user_notification_prefs (
    user_id, default_inbox_scope, suppress_agent_actions,
    quiet_hours_start, quiet_hours_end, updated_at
)
VALUES ($1, 'all', true, $2::time, $3::time, now())
ON CONFLICT (user_id) DO UPDATE
   SET quiet_hours_start = EXCLUDED.quiet_hours_start,
       quiet_hours_end   = EXCLUDED.quiet_hours_end,
       updated_at        = now()`
	if _, err := s.pool.Exec(ctx, upsert, userID, startArg, endArg); err != nil {
		return fmt.Errorf("saving quiet hours: %w", err)
	}
	return nil
}
