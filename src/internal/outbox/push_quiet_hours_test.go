package outbox

// push_quiet_hours_test.go — sk-340, acceptance of quiet hours on the delivery path.
//
// ⚠ WHAT THE "NO PUSH" CHECK DOES NOT PROVE. The absence of a push looks
// the same for four different causes: quiet hours, a disabled channel, revoked
// access and a failed transport. A test asserting only "Send was not called"
// is green under any of them — including a completely broken path
// that sends NOTHING to NOBODY. So what is asserted here is the OUTCOME of the
// row (`quiet_hours`), and next to it stand two tests that make that
// assertion meaningful: a control (outside the window the same push must go out) and
// a discriminating one (a broken transport must give a DIFFERENT outcome).
//
// ⚠ WHY THE RECIPIENTS ARE IN DIFFERENT ZONES, NOT UTC. A fixture where everyone is in UTC
// does not distinguish an implementation that ignores the zone altogether: both "by the
// recipient's zone" and "by server time" give one answer on it. Here one
// instant and ONE window give DIFFERENT answers to two recipients, and the only thing
// that differs between them is the zone.
//
// ⚠ WHY THE WINDOW CROSSES MIDNIGHT. 22:00–07:00 is the shape that breaks
// the naive "between start and end"; a window within one day would pass even for a wrong
// implementation. The full table of day-arithmetic cases lives separately
// (notifications/quiet_test.go): here the INTEGRATION of the gate is checked, there
// its arithmetic.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
)

// quietInstant — the instant for which all three tests of the file are set up.
//
// 20:30 UTC on 10 March 2026: in Moscow (UTC+3) it is 23:30 — inside the night
// window, in New York (UTC-4, daylight saving already in effect) it is 16:30 — outside.
// UTC itself gives 20:30, i.e. "not quiet" — so an implementation
// that counts by server time will send BOTH pushes and turn red on the very first
// assertion.
var quietInstant = time.Date(2026, 3, 10, 20, 30, 0, 0, time.UTC)

// setUserTimezone writes the recipient's zone directly, bypassing the settings
// contract: the delivery path is checked from the storage state, not from the form.
func setUserTimezone(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, tz string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET timezone = $2 WHERE id = $1`, userID, tz); err != nil {
		t.Fatalf("set user timezone: %v", err)
	}
}

// setQuietWindow writes the window bounds. The settings row is created lazily,
// so the write is an upsert, as in the product code.
func setQuietWindow(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, start, end string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO user_notification_prefs (
    user_id, default_inbox_scope, suppress_agent_actions,
    quiet_hours_start, quiet_hours_end, updated_at)
VALUES ($1, 'all', true, $2::time, $3::time, now())
ON CONFLICT (user_id) DO UPDATE
   SET quiet_hours_start = EXCLUDED.quiet_hours_start,
       quiet_hours_end   = EXCLUDED.quiet_hours_end,
       updated_at        = now()`, userID, start, end)
	if err != nil {
		t.Fatalf("set quiet window: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`UPDATE user_notification_prefs SET quiet_hours_start = NULL, quiet_hours_end = NULL WHERE user_id = $1`,
			userID)
	})
}

// runnerAt — a runner whose quiet-hours clock is fixed at the given instant.
func runnerAt(pool *pgxpool.Pool, sender PushSender, at time.Time) *PushRunner {
	return NewPushRunner(pool, sender, push.NewStore(pool), "https://app.example.com",
		PushOptions{QuietClock: func() time.Time { return at }})
}

// feedRowIntact reads what quiet hours have NO RIGHT to touch: the feed
// row is created as usual and waits for the person in the morning.
func feedRowIntact(t *testing.T, pool *pgxpool.Pool, id int64) (feedChannel bool, read, archived *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT feed_channel, read_at, archived_at FROM notifications WHERE id = $1`, id).
		Scan(&feedChannel, &read, &archived); err != nil {
		t.Fatalf("read feed columns: %v", err)
	}
	return feedChannel, read, archived
}

// outcomeText unfolds the outcome for the error message.
//
// Without it `%v` prints the pointer ADDRESS, and the failure reports
// "expected quiet_hours, got 0xc000…" — i.e. reports nothing.
func outcomeText(o *string) string {
	if o == nil {
		return "<no outcome>"
	}
	return *o
}

// TestPushRunner_QuietHoursSilenceOnlyTheRecipientWhoseClockSaysNight — the
// requirement itself: a push that falls into the recipient's quiet-hours window is not sent at all and
// finishes with ITS OWN outcome; for a recipient for whom it is not night at the same instant,
// the same push goes out.
func TestPushRunner_QuietHoursSilenceOnlyTheRecipientWhoseClockSaysNight(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	night := newPushFixture(t, pool, "qhnight").withDevice(t)
	setUserTimezone(t, pool, night.user, "Europe/Moscow")
	setQuietWindow(t, pool, night.user, "22:00", "07:00")
	nightID := night.owed(t, pushRow{feedChannel: true})

	day := newPushFixture(t, pool, "qhday").withDevice(t)
	setUserTimezone(t, pool, day.user, "America/New_York")
	setQuietWindow(t, pool, day.user, "22:00", "07:00")
	dayID := day.owed(t, pushRow{feedChannel: true})

	sender := &fakePushSender{enabled: true}
	if _, err := runnerAt(pool, sender, quietInstant).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Night recipient: no sends at all and an outcome naming the REASON.
	if got := callsTo(sender, night.endpoint); len(got) != 0 {
		t.Errorf("%d pushes went out in the quiet-hours window, expected 0", len(got))
	}
	state, outcome, _ := pushStateOf(t, pool, nightID)
	if state != "final" || outcome == nil || *outcome != outcomeQuietHours {
		t.Errorf("state/outcome = %s/%s, expected final/%s — without THIS assertion "+
			"the test is green even with a completely broken path that sends nothing to nobody",
			state, outcomeText(outcome), outcomeQuietHours)
	}

	// Day recipient at the same instant: the push went out. This is what makes the first
	// assertion meaningful — the path works, the silence is targeted.
	if got := callsTo(sender, day.endpoint); len(got) != 1 {
		t.Fatalf("%d pushes went out outside the quiet-hours window, expected 1 — either the gate silences everyone, "+
			"or the window was computed not in the recipient's zone", len(got))
	}
	state, outcome, _ = pushStateOf(t, pool, dayID)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("state/outcome of the day recipient = %s/%s, expected final/%s", state, outcomeText(outcome), outcomeSuccess)
	}

	// The notification is NOT LOST: only the push is silent.
	feed, read, archived := feedRowIntact(t, pool, nightID)
	if !feed {
		t.Error("the row dropped out of the feed — quiet hours must silence the PUSH, not the notification")
	}
	if read != nil || archived != nil {
		t.Errorf("the row is marked read/archived (read_at=%v archived_at=%v) — "+
			"the person must see it in the morning", read, archived)
	}
}

// TestPushRunner_OutsideTheWindowTheSamePushGoesOut — the control test, without
// which the first proves nothing.
//
// The same recipient, the same window, the same device — ONLY the instant is shifted.
// If the push does not go out here either, the silence is caused not by the window but by the fixture:
// the wrong reason, a missing device, a lost membership. The test
// tells "the window works" from "nothing is ever sent
// to this recipient".
func TestPushRunner_OutsideTheWindowTheSamePushGoesOut(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	f := newPushFixture(t, pool, "qhctrl").withDevice(t)
	setUserTimezone(t, pool, f.user, "Europe/Moscow")
	setQuietWindow(t, pool, f.user, "22:00", "07:00")
	id := f.owed(t, pushRow{feedChannel: true})

	// 09:30 UTC is 12:30 in Moscow, a whole working day before the window starts.
	noon := time.Date(2026, 3, 10, 9, 30, 0, 0, time.UTC)

	sender := &fakePushSender{enabled: true}
	if _, err := runnerAt(pool, sender, noon).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Fatalf("%d pushes went out outside the window, expected 1 — the silence in the paired test is caused "+
			"not by the window but by the fixture", len(got))
	}
	state, outcome, _ := pushStateOf(t, pool, id)
	if state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("state/outcome = %s/%s, expected final/%s", state, outcomeText(outcome), outcomeSuccess)
	}
}

// TestPushRunner_BrokenTransportIsNotQuietHours — the discriminator, named
// by the acceptance in plain words: "the push was not sent because of quiet hours" and "the push was not
// sent because the path broke" must be distinguishable.
//
// Here there is NO quiet-hours window AT ALL, and the transport fails. Outwardly it is the same
// outcome as in the first test — Send delivered nothing — but the state
// of the row must be DIFFERENT: the row stays due and is retried, rather than
// being declared silence. Failure of this test means `quiet_hours`
// is set on the fact that nothing was sent, and then the green colour of the first
// test means nothing.
func TestPushRunner_BrokenTransportIsNotQuietHours(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	f := newPushFixture(t, pool, "qhbroken").withDevice(t)
	setUserTimezone(t, pool, f.user, "Europe/Moscow")
	// The window is deliberately not set: the failure must be classified as a failure
	// of the TRANSPORT even for a recipient whose local hour would fall at night.
	id := f.owed(t, pushRow{feedChannel: true})

	sender := &fakePushSender{enabled: true}
	sender.setErr(errors.New("provider unreachable"))
	if _, err := runnerAt(pool, sender, quietInstant).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := callsTo(sender, f.endpoint); len(got) != 1 {
		t.Fatalf("send attempts = %d, expected 1 — the broken transport must be CALLED, "+
			"otherwise the test checks the wrong thing", len(got))
	}
	state, outcome, _ := pushStateOf(t, pool, id)
	if outcome != nil && *outcome == outcomeQuietHours {
		t.Fatalf("transport failure recorded as outcome %q — quiet hours and a broken path are indistinguishable, "+
			"and the green colour of the quiet-hours check proves nothing", outcomeQuietHours)
	}
	if state != "pending" {
		t.Errorf("state = %s, expected pending — a transport failure is not terminal, "+
			"the row stays due until the attempts or the deadline run out", state)
	}
}

// TestPushRunner_QuietHoursNeedBothTheZoneAndTheWindow — "ignorance is interpreted in
// favour of sending", checked on the path, not only on the predicate.
//
// Two halves of the setting, each without the other, and one and the same night instant: neither
// of them may silence the push. More important is what this test
// forbids: an implementation that substitutes UTC or the server zone for an unknown zone —
// on such a recipient it would give 20:30 and "not quiet", i.e. it would
// match by accident. That is why the zone here is MOSCOW and the window is absent, and
// vice versa — the window is at night and there is no zone: substituting any zone on the second
// row would give silence and turn red.
func TestPushRunner_QuietHoursNeedBothTheZoneAndTheWindow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// There is a zone, no window.
	noWindow := newPushFixture(t, pool, "qhnowin").withDevice(t)
	setUserTimezone(t, pool, noWindow.user, "Europe/Moscow")
	noWindowID := noWindow.owed(t, pushRow{feedChannel: true})

	// There is a window, no zone — a state unreachable through the settings contract
	// (422 timezone_required), but writable around it.
	noZone := newPushFixture(t, pool, "qhnozone").withDevice(t)
	setQuietWindow(t, pool, noZone.user, "00:00", "23:59")
	noZoneID := noZone.owed(t, pushRow{feedChannel: true})

	sender := &fakePushSender{enabled: true}
	if _, err := runnerAt(pool, sender, quietInstant).Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	for _, tc := range []struct {
		name     string
		id       int64
		endpoint string
		why      string
	}{
		{"zone without a window", noWindowID, noWindow.endpoint,
			"the window is off — the factory state, nothing to silence with"},
		{"window without a zone", noZoneID, noZone.endpoint,
			"\"00:00\" without a zone is not an instant but a string: nothing to interpret the window against"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := callsTo(sender, tc.endpoint); len(got) != 1 {
				t.Fatalf("sends = %d, expected 1 — %s", len(got), tc.why)
			}
			state, outcome, _ := pushStateOf(t, pool, tc.id)
			if state != "final" || outcome == nil || *outcome != outcomeSuccess {
				t.Errorf("state/outcome = %s/%s, expected final/%s — %s", state, outcomeText(outcome), outcomeSuccess, tc.why)
			}
		})
	}
}
