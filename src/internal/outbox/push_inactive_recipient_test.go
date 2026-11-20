package outbox

// push_inactive_recipient_test.go — who a delivery attempt may reach
// (integration, TEST_DATABASE_URL). The reference is the specification of
// device consent: INV-6 (an attempt selects only subscriptions of a user who
// is neither deactivated nor deleted, for every kind of delivery —
// notification, digest, invitation), SC-002 (a user whose cookie sessions
// have all expired still receives pushes) and composition rule 1 (delivery is
// decided by the server alone: the subscriptions, not the browser session).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

// threeKindsOwed seeds a recipient with one device and one pending row of
// each delivery kind — a single-issue notification, a digest and a space
// invitation — and returns the fixture and the three rows by kind.
func threeKindsOwed(t *testing.T, pool *pgxpool.Pool) (*pushFixture, map[string]int64) {
	t.Helper()
	key := "inr" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	f := newPushFixture(t, pool, key).withDevice(t)

	var username string
	if err := pool.QueryRow(context.Background(), `SELECT username FROM users WHERE id = $1`, f.user).Scan(&username); err != nil {
		t.Fatalf("read the recipient's username: %v", err)
	}
	other, otherName := testsupport.SeedUser(t, pool, "user")
	d := &digestFixture{
		pool: pool, user: f.user, username: username,
		other: other, otherName: otherName,
		projectID: f.projectID, endpoint: f.endpoint, key: key,
		titles: map[string]string{},
	}
	digest, _ := d.digestRow(t, review.CategoryUnaccepted, []string{d.unaccepted(t), d.unaccepted(t)})
	_, invitation := invitationPush(t, pool, other, f.user, f.user, "space.invitation_sent", "Acme")

	return f, map[string]int64{
		"notification": f.owed(t, pushRow{feedChannel: true}),
		"digest":       digest,
		"invitation":   invitation,
	}
}

// Row 24 — INV-6 for all three kinds: a deactivated or deleted recipient
// whose subscription is still stored gets neither the notification, nor the
// digest, nor the invitation; each row finishes without a send.
func TestPushRunner_InactiveRecipientGetsNoKindOfPush(t *testing.T) {
	pool := newTestPool(t)
	for _, inactive := range []struct{ name, sql string }{
		{"deactivated", `UPDATE users SET deactivated_at = now() WHERE id = $1`},
		{"deleted", `UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(random()::text), 1, 8) WHERE id = $1`},
	} {
		t.Run(inactive.name, func(t *testing.T) {
			f, rows := threeKindsOwed(t, pool)
			if _, err := pool.Exec(context.Background(), inactive.sql, f.user); err != nil {
				t.Fatalf("mark the recipient %s: %v", inactive.name, err)
			}
			var stored int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM push_subscriptions WHERE endpoint = $1`, f.endpoint).Scan(&stored); err != nil || stored != 1 {
				t.Fatalf("the recipient's subscription is not stored (count %d, err %v) — the case proves nothing", stored, err)
			}

			sender := &fakePushSender{enabled: true}
			testsupport.TickPushUntilSettled(t, pool, f.user, newRunner(pool, sender).Tick)

			if got := callsTo(sender, f.endpoint); len(got) != 0 {
				t.Fatalf("a %s recipient was pushed %d times, want 0: %+v", inactive.name, len(got), got)
			}
			for kind, id := range rows {
				state, outcome, _ := pushStateOf(t, pool, id)
				if state != "final" || outcome == nil || *outcome != outcomeNoDevice {
					t.Errorf("%s row of a %s recipient: %s/%v, want final/%s — finished without a send",
						kind, inactive.name, state, outcome, outcomeNoDevice)
				}
			}
		})
	}
}

// Row 24 — SC-002, composition rule 1: an active recipient whose every cookie
// session is invalid (the session epoch was raised) still gets all three
// kinds of push.
func TestPushRunner_ActiveRecipientWithoutASessionGetsEveryKind(t *testing.T) {
	pool := newTestPool(t)
	f, rows := threeKindsOwed(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1`, f.user); err != nil {
		t.Fatalf("raise the session epoch: %v", err)
	}

	sender := &fakePushSender{enabled: true}
	testsupport.TickPushUntilSettled(t, pool, f.user, newRunner(pool, sender).Tick)

	if got := callsTo(sender, f.endpoint); len(got) != len(rows) {
		t.Fatalf("pushes to the recipient = %d, want %d — one per kind", len(got), len(rows))
	}
	for kind, id := range rows {
		state, outcome, _ := pushStateOf(t, pool, id)
		if state != "final" || outcome == nil || *outcome != outcomeSuccess {
			t.Errorf("%s row: %s/%v, want final/%s", kind, state, outcome, outcomeSuccess)
		}
	}
}
