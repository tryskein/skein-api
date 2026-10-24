package review

// minutes_test.go — sk-303.14 (T010). Storage and transport unit: whole
// minutes. This checks exactly what ValidateIntervalMinutes and ResolveMinutes
// exist for — that conversion into the domain unit changes neither the bounds
// nor the distinction between "empty" and "explicit zero".

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestValidateIntervalMinutesBounds(t *testing.T) {
	t.Parallel()

	// Bounds are inclusive on both sides: a value ON the bound is accepted.
	for _, m := range []int{MinIntervalMinutes, MaxIntervalMinutes,
		DefaultUnacceptedIntervalMinutes, DefaultUnreviewedIntervalMinutes} {
		if err := ValidateIntervalMinutes("unacceptedIntervalMinutes", m); err != nil {
			t.Errorf("%d minutes rejected, though the contract admits it: %v", m, err)
		}
	}

	tooShort := []int{MinIntervalMinutes - 1, 0, -1, math.MinInt64}
	for _, m := range tooShort {
		err := ValidateIntervalMinutes("unreviewedIntervalMinutes", m)
		if !errors.Is(err, ErrIntervalTooShort) {
			t.Errorf("%d minutes: expected refusal «too short», got %v", m, err)
		}
	}

	tooLong := []int{MaxIntervalMinutes + 1, math.MaxInt64}
	for _, m := range tooLong {
		err := ValidateIntervalMinutes("unacceptedIntervalMinutes", m)
		// math.MaxInt64 minutes is the very case the minutes check exists
		// for: conversion to time.Duration overflows it and yields a
		// NEGATIVE duration, and a negative is below MinInterval, so the
		// refusal would have arrived for the false reason "too short".
		if !errors.Is(err, ErrIntervalTooLong) {
			t.Errorf("%d minutes: expected refusal «too long», got %v", m, err)
		}
	}
}

func TestValidateIntervalMinutesNamesTheParam(t *testing.T) {
	t.Parallel()

	err := ValidateIntervalMinutes("unreviewedIntervalMinutes", 1)
	var invalid *InvalidSettingError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected *InvalidSettingError, got %T (%v)", err, err)
	}
	if invalid.Param != "unreviewedIntervalMinutes" {
		t.Errorf("refusal named field %q instead of the one passed in", invalid.Param)
	}
}

func TestResolveMinutesEmptyMeansFactory(t *testing.T) {
	t.Parallel()

	got, err := ResolveMinutes(nil, nil, nil)
	if err != nil {
		t.Fatalf("empty input rejected: %v", err)
	}
	if got != Defaults() {
		t.Errorf("empty input gave %+v, factory is %+v", got, Defaults())
	}
}

func TestResolveMinutesExplicitZeroBurstIsNotEmpty(t *testing.T) {
	t.Parallel()

	// The difference between "empty" and "explicit zero" is why the schema
	// columns are NULLABLE. Collapsing them would take away the person's
	// ability to turn the burst off: 0 would silently become the factory 3.
	zero := 0
	got, err := ResolveMinutes(&zero, nil, nil)
	if err != nil {
		t.Fatalf("explicit zero rejected: %v", err)
	}
	if got.BurstCount != 0 {
		t.Errorf("explicit zero gave BurstCount=%d; factory substituted for the saved value", got.BurstCount)
	}
}

func TestResolveMinutesConvertsUnits(t *testing.T) {
	t.Parallel()

	burst := 5
	unacc := 45
	unrev := 1440
	got, err := ResolveMinutes(&burst, &unacc, &unrev)
	if err != nil {
		t.Fatalf("legal input rejected: %v", err)
	}
	if got.BurstCount != burst {
		t.Errorf("BurstCount = %d, want %d", got.BurstCount, burst)
	}
	if got.UnacceptedInterval != 45*time.Minute {
		t.Errorf("UnacceptedInterval = %s, want 45m", got.UnacceptedInterval)
	}
	if got.UnreviewedInterval != 24*time.Hour {
		t.Errorf("UnreviewedInterval = %s, want 24h", got.UnreviewedInterval)
	}
}

func TestResolveMinutesRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	tooBig := MaxBurstCount + 1
	shortInterval := MinIntervalMinutes - 1
	longInterval := MaxIntervalMinutes + 1

	cases := []struct {
		name       string
		burst      *int
		unacc      *int
		unrev      *int
		wantParam  string
		wantSentin error
	}{
		{"burst above ceiling", &tooBig, nil, nil, "burstCount", ErrBurstCountOutOfRange},
		{"unaccepted below floor", nil, &shortInterval, nil, "unacceptedIntervalMinutes", ErrIntervalTooShort},
		{"unreviewed above ceiling", nil, nil, &longInterval, "unreviewedIntervalMinutes", ErrIntervalTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveMinutes(tc.burst, tc.unacc, tc.unrev)
			if err == nil {
				t.Fatalf("out-of-bounds value accepted and gave %+v; rewriting instead of refusing is forbidden (SC-022, SC-027)", got)
			}
			if !errors.Is(err, tc.wantSentin) {
				t.Errorf("refused for %v, want %v", err, tc.wantSentin)
			}
			var invalid *InvalidSettingError
			if !errors.As(err, &invalid) {
				t.Fatalf("refusal did not name the field: %T", err)
			}
			if invalid.Param != tc.wantParam {
				t.Errorf("refusal named field %q, want %q", invalid.Param, tc.wantParam)
			}
		})
	}
}
