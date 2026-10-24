package reminders

import (
	"errors"
	"testing"
	"time"
)

func TestParseBy_Presets(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
		{"3d", 72 * time.Hour},
		{"5d", 120 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"12h", 12 * time.Hour},
		// case-insensitive units
		{"1H", time.Hour},
		{"2D", 48 * time.Hour},
		{"1W", 7 * 24 * time.Hour},
		// surrounding whitespace is trimmed
		{"  3d ", 72 * time.Hour},
	}
	for _, c := range cases {
		got, err := ParseBy(c.in)
		if err != nil {
			t.Errorf("ParseBy(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBy(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseBy_Invalid(t *testing.T) {
	bad := []string{
		"",        // empty
		"h",       // no number
		"5",       // no unit
		"0d",      // zero
		"-1d",     // negative
		"1m",      // minutes not supported
		"1s",      // seconds not supported
		"1y",      // years not supported
		"abc",     // garbage
		"1.5d",    // fractional
		"d1",      // unit-first
		"100000w", // overflows the 366-day cap
	}
	for _, in := range bad {
		_, err := ParseBy(in)
		if err == nil {
			t.Errorf("ParseBy(%q) expected error, got nil", in)
			continue
		}
		var ide *ErrInvalidDuration
		if !errors.As(err, &ide) {
			t.Errorf("ParseBy(%q) error type = %T, want *ErrInvalidDuration", in, err)
		}
	}
}

func TestDeriveState(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	if got := DeriveState(now.Add(time.Hour), now); got != StateArmed {
		t.Errorf("future remind_at: got %s, want armed", got)
	}
	if got := DeriveState(now.Add(-time.Hour), now); got != StateRinging {
		t.Errorf("past remind_at: got %s, want ringing", got)
	}
	// Exactly now is ringing (remind_at <= now).
	if got := DeriveState(now, now); got != StateRinging {
		t.Errorf("remind_at == now: got %s, want ringing", got)
	}
}

func TestParseListFilter(t *testing.T) {
	cases := []struct {
		in     string
		want   ListFilter
		wantOk bool
	}{
		{"", FilterAll, true},
		{"all", FilterAll, true},
		{"ringing", FilterRinging, true},
		{"armed", FilterArmed, true},
		{"RINGING", "", false}, // case-sensitive
		{"bogus", "", false},
	}
	for _, c := range cases {
		got, ok := ParseListFilter(c.in)
		if ok != c.wantOk {
			t.Errorf("ParseListFilter(%q) ok = %v, want %v", c.in, ok, c.wantOk)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParseListFilter(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
