package notifications

// quiet_store_test.go — sk-340, storage of the zone and the window.
//
// The window arithmetic is checked by a table (quiet_test.go), the gate's wiring —
// on the delivery path (outbox/push_quiet_hours_test.go). Here are two promises that
// are visible neither there nor there: detection from the browser does not overwrite a value
// set by hand, and a window cannot be set so that it cannot be interpreted.

import (
	"context"
	"errors"
	"testing"
	"time"

	"skein-api/internal/testsupport"
)

// TestSetTimezone_DetectedDoesNotOverwriteManual — an acceptance requirement stated
// plainly.
//
// ⚠ WHY THIS IS NOT SELF-EVIDENT. Detection from the browser arrives on EVERY
// sign-in, and a manual edit happens once. An implementation without this predicate looks
// fine exactly until the next sign-in, after which the correction is silently
// rolled back — and the person sees not an error but "the setting is not saved".
func TestSetTimezone_DetectedDoesNotOverwriteManual(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	user, _ := testsupport.SeedUser(t, pool, "user")

	// The browser detected the zone on first sign-in — it is written, there are no zones yet.
	dto, err := store.SetTimezone(ctx, user, "America/New_York", TimezoneSourceDetected)
	if err != nil {
		t.Fatalf("SetTimezone(detected): %v", err)
	}
	if dto.Timezone == nil || *dto.Timezone != "America/New_York" {
		t.Fatalf("zone after the first detection = %v, want America/New_York", dto.Timezone)
	}
	if dto.Source == nil || *dto.Source != TimezoneSourceDetected {
		t.Errorf("source = %v, want detected", dto.Source)
	}

	// The person corrected it by hand.
	dto, err = store.SetTimezone(ctx, user, "Europe/Moscow", TimezoneSourceManual)
	if err != nil {
		t.Fatalf("SetTimezone(manual): %v", err)
	}
	if dto.Timezone == nil || *dto.Timezone != "Europe/Moscow" {
		t.Fatalf("the manual edit was not written: zone = %v", dto.Timezone)
	}
	if dto.Source == nil || *dto.Source != TimezoneSourceManual {
		t.Errorf("source = %v, want manual", dto.Source)
	}

	// The next sign-in from the browser in another zone — must yield.
	dto, err = store.SetTimezone(ctx, user, "Asia/Tokyo", TimezoneSourceDetected)
	if err != nil {
		t.Fatalf("SetTimezone(detected over manual): %v", err)
	}
	if dto.Timezone == nil || *dto.Timezone != "Europe/Moscow" {
		t.Errorf("detection from the browser overwrote the manual value: zone = %v, want Europe/Moscow", dto.Timezone)
	}
	if dto.Source == nil || *dto.Source != TimezoneSourceManual {
		t.Errorf("source = %v, want manual — the response must name the value IN FORCE, "+
			"otherwise a call that wrote nothing is indistinguishable from one that wrote", dto.Source)
	}
	// And in the storage too — the response could lie.
	stored, err := store.Timezone(ctx, user)
	if err != nil {
		t.Fatalf("Timezone: %v", err)
	}
	if stored.Timezone == nil || *stored.Timezone != "Europe/Moscow" {
		t.Errorf("zone in the storage = %v, want Europe/Moscow", stored.Timezone)
	}

	// And a manual edit over a manual one is written.
	dto, err = store.SetTimezone(ctx, user, "Asia/Tokyo", TimezoneSourceManual)
	if err != nil {
		t.Fatalf("SetTimezone(manual over manual): %v", err)
	}
	if dto.Timezone == nil || *dto.Timezone != "Asia/Tokyo" {
		t.Errorf("the manual edit did not overwrite the manual value: zone = %v", dto.Timezone)
	}
}

// TestSetTimezone_RejectsAZoneThatDoesNotExist — the zone name is checked
// by loading, not by its shape.
func TestSetTimezone_RejectsAZoneThatDoesNotExist(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	user, _ := testsupport.SeedUser(t, pool, "user")

	if _, err := store.SetTimezone(ctx, user, "Europe/Moskva", TimezoneSourceManual); !errors.Is(err, ErrInvalidTimezone) {
		t.Errorf("error = %v, want ErrInvalidTimezone — a plausible non-existent name "+
			"would silently switch quiet off for one person", err)
	}
	stored, err := store.Timezone(ctx, user)
	if err != nil {
		t.Fatalf("Timezone: %v", err)
	}
	if stored.Timezone != nil {
		t.Errorf("the rejected name was written anyway: %v", stored.Timezone)
	}
}

// TestSetQuietHours_RefusesWhatCannotBeInterpreted — three refusals, each about
// a setting that has no unambiguous meaning.
func TestSetQuietHours_RefusesWhatCannotBeInterpreted(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	user, _ := testsupport.SeedUser(t, pool, "user")

	night := &QuietWindow{Start: 22 * time.Hour, End: 7 * time.Hour}

	// No zone yet — a window cannot be set. This is what makes the pair
	// "window set, zone unknown" unreachable through the contract.
	if err := store.SetQuietHours(ctx, user, night); !errors.Is(err, ErrTimezoneRequired) {
		t.Errorf("error = %v, want ErrTimezoneRequired", err)
	}

	if _, err := store.SetTimezone(ctx, user, "Europe/Moscow", TimezoneSourceManual); err != nil {
		t.Fatalf("SetTimezone: %v", err)
	}

	// A degenerate window — a refusal, not an interpretation.
	degenerate := &QuietWindow{Start: 22 * time.Hour, End: 22 * time.Hour}
	if err := store.SetQuietHours(ctx, user, degenerate); !errors.Is(err, ErrQuietWindowDegenerate) {
		t.Errorf("error = %v, want ErrQuietWindowDegenerate — \"quiet always\" and "+
			"\"quiet never\" look the same, and one cannot be chosen for the person", err)
	}

	// A night window — is written and read back as the same.
	if err := store.SetQuietHours(ctx, user, night); err != nil {
		t.Fatalf("SetQuietHours: %v", err)
	}
	got, err := store.QuietHours(ctx, user)
	if err != nil {
		t.Fatalf("QuietHours: %v", err)
	}
	if got.Start == nil || *got.Start != "22:00" || got.End == nil || *got.End != "07:00" {
		t.Fatalf("window read as %v–%v, want 22:00–07:00", got.Start, got.End)
	}
	if got.Timezone == nil || *got.Timezone != "Europe/Moscow" {
		t.Errorf("zone in the response = %v — the settings form must show in which zone the window is counted", got.Timezone)
	}

	// And the predicate sees exactly what is written.
	settings, err := store.QuietSettingsFor(ctx, user)
	if err != nil {
		t.Fatalf("QuietSettingsFor: %v", err)
	}
	if !settings.QuietAt(time.Date(2026, 3, 10, 20, 30, 0, 0, time.UTC)) {
		t.Error("23:30 Moscow time is not recognised as quiet with a 22:00–07:00 window — the settings did not reach the predicate")
	}

	// Switching the window off.
	if err := store.SetQuietHours(ctx, user, nil); err != nil {
		t.Fatalf("SetQuietHours(nil): %v", err)
	}
	got, err = store.QuietHours(ctx, user)
	if err != nil {
		t.Fatalf("QuietHours: %v", err)
	}
	if got.Start != nil || got.End != nil {
		t.Errorf("after switching off the window = %v–%v, want empty", got.Start, got.End)
	}
}

// TestSetQuietHours_DoesNotClearNeighbouringSettings — the settings row is
// created lazily, and the inserting branch of the upsert names the neighbouring columns
// by factory values. The check is that editing the window does not reset
// a setting the person did not touch.
func TestSetQuietHours_DoesNotClearNeighbouringSettings(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	user, _ := testsupport.SeedUser(t, pool, "user")

	if _, err := store.SetTimezone(ctx, user, "Europe/Moscow", TimezoneSourceManual); err != nil {
		t.Fatalf("SetTimezone: %v", err)
	}
	// The person switched off suppression of the agent's own actions — a setting
	// the quiet window knows nothing about.
	off := false
	if _, err := store.UpdatePrefs(ctx, user, UpdatePrefsInput{SuppressAgentActions: &off}); err != nil {
		t.Fatalf("UpdatePrefs: %v", err)
	}
	if err := store.SetQuietHours(ctx, user, &QuietWindow{Start: 22 * time.Hour, End: 7 * time.Hour}); err != nil {
		t.Fatalf("SetQuietHours: %v", err)
	}
	prefs, err := store.GetPrefs(ctx, user)
	if err != nil {
		t.Fatalf("GetPrefs: %v", err)
	}
	if prefs.SuppressAgentActions {
		t.Error("editing the quiet window reset the neighbouring setting to its factory value")
	}
}
