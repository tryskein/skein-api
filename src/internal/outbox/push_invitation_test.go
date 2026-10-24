package outbox

// push_invitation_test.go — the third payload kind of the push tract, a
// space invitation (sk-674.18 T040; decision 2026-10-14: the
// push re-checks before sending that the invitation still holds — pending,
// the same addressee — and a revoked, accepted or expired one is not sent;
// the user-typed names are bounded and carry no links; no token in the URL).
//
// The rows are what the invitations package writes as a direct writer
// (notifications.Reason.DirectWriter): an already published event of type
// space.invitation_sent / space.invitation_accepted naming the invitation,
// and a push-only notification row of reason space_invitation. They are
// seeded here as the fixture; the subject is the runner.

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/push"
	"skein-api/internal/testsupport"
)

// invitationPush seeds a space, an invitation of invitee's email sent by
// sender, the event of eventType and the push row to recipient; it returns
// the invitation and the row.
func invitationPush(t *testing.T, pool *pgxpool.Pool, sender, invitee, recipient uuid.UUID, eventType, spaceName string) (uuid.UUID, int64) {
	t.Helper()
	ctx := context.Background()
	space := testsupport.SeedSpace(t, pool, sender)
	if _, err := pool.Exec(ctx, `UPDATE spaces SET name = $2 WHERE id = $1`, space, spaceName); err != nil {
		t.Fatal(err)
	}
	var inv uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
SELECT $1, lower(email), $2, 'en', now() + interval '1 day' FROM users WHERE id = $3
RETURNING id`, space, sender, invitee).Scan(&inv); err != nil {
		t.Fatalf("seeding the invitation: %v", err)
	}
	var eventID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_type, entity_type, entity_id, payload, published_at)
SELECT $1, u.username, u.id, 'human', 'space', $2, jsonb_build_object('invitationId', $3::text), now()
  FROM users u WHERE u.id = $4
RETURNING id`, eventType, space.String(), inv, map[string]uuid.UUID{
		"space.invitation_sent": sender, "space.invitation_accepted": invitee}[eventType]).Scan(&eventID); err != nil {
		t.Fatalf("seeding the event: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO notifications (user_id, event_id, type, reason, feed_channel,
                           push_state, push_next_attempt_at, push_deadline_at, card_resolved)
VALUES ($1, $2, 'space_invitation', 'space_invitation', false, 'pending', now(), now() + interval '1 hour', true)
RETURNING id`, recipient, eventID).Scan(&id); err != nil {
		t.Fatalf("seeding the push row: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM events WHERE id = $1`, eventID)
		_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, inv)
	})
	return inv, id
}

func TestPushRunner_InvitationSentWhilePending(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	sender, senderName := testsupport.SeedUser(t, pool, "user")
	invitee := seedUser(t, pool)
	endpoint := "https://push.example.com/inv-" + uuid.NewString()[:8]
	seedPushSubscription(t, pool, invitee, endpoint)
	inv, id := invitationPush(t, pool, sender, invitee, invitee, "space.invitation_sent", "Acme")

	s := &fakePushSender{enabled: true}
	if _, err := NewPushRunner(pool, s, push.NewStore(pool), "https://app.example.com/", PushOptions{}).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := callsTo(s, endpoint)
	if len(got) != 1 {
		t.Fatalf("pushes to the invitee: %d, want 1", len(got))
	}
	p := got[0].payload
	if p.Category != "space_invitation" {
		t.Errorf("category %q, want the reason key space_invitation", p.Category)
	}
	if !strings.Contains(p.Title+" "+p.Body, senderName) || !strings.Contains(p.Title+" "+p.Body, "Acme") {
		t.Errorf("payload %+v names neither the inviter %s nor the space Acme", p, senderName)
	}
	assertWebUIDestination(t, p.URL)
	if strings.Contains(p.URL, inv.String()) || p.IssueID != "" {
		t.Errorf("url %q, issue %q: want no invitation id or token, no issue", p.URL, p.IssueID)
	}
	if state, outcome, _ := pushStateOf(t, pool, id); state != "final" || outcome == nil || *outcome != outcomeSuccess {
		t.Errorf("row %s/%v, want final/success", state, outcome)
	}
}

func TestPushRunner_InvitationNotSentOnceItLostItsForce(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	for _, change := range []struct{ name, sql string }{
		{"revoked", `UPDATE space_invitations SET status = 'revoked', resolved_at = now() WHERE id = $1`},
		{"accepted", `UPDATE space_invitations SET status = 'accepted', resolved_at = now() WHERE id = $1`},
		{"expired", `UPDATE space_invitations SET expires_at = now() - interval '1 second' WHERE id = $1`},
		{"another addressee", `UPDATE space_invitations SET email = 'someone-else-' || substr(md5(random()::text), 1, 8) || '@test.local' WHERE id = $1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			sender := seedUser(t, pool)
			invitee := seedUser(t, pool)
			endpoint := "https://push.example.com/inv-" + uuid.NewString()[:8]
			seedPushSubscription(t, pool, invitee, endpoint)
			inv, id := invitationPush(t, pool, sender, invitee, invitee, "space.invitation_sent", "Acme")
			if _, err := pool.Exec(ctx, change.sql, inv); err != nil {
				t.Fatal(err)
			}
			s := &fakePushSender{enabled: true}
			if _, err := NewPushRunner(pool, s, push.NewStore(pool), "", PushOptions{}).Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if got := callsTo(s, endpoint); len(got) != 0 {
				t.Errorf("an invitation %s was pushed: %+v", change.name, got)
			}
			if state, outcome, _ := pushStateOf(t, pool, id); state != "final" || outcome == nil || *outcome != outcomeConfigurationNoSend {
				t.Errorf("row %s/%v, want final/configuration_no_send — finished, not retried", state, outcome)
			}
		})
	}
}

func TestPushRunner_InvitationAcceptedToTheSender(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	sender := seedUser(t, pool)
	invitee, inviteeName := testsupport.SeedUser(t, pool, "user")
	endpoint := "https://push.example.com/acc-" + uuid.NewString()[:8]
	seedPushSubscription(t, pool, sender, endpoint)
	inv, _ := invitationPush(t, pool, sender, invitee, sender, "space.invitation_accepted", "Acme")
	if _, err := pool.Exec(ctx, `UPDATE space_invitations SET status = 'accepted', resolved_at = now() WHERE id = $1`, inv); err != nil {
		t.Fatal(err)
	}
	s := &fakePushSender{enabled: true}
	if _, err := NewPushRunner(pool, s, push.NewStore(pool), "https://app.example.com/", PushOptions{}).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := callsTo(s, endpoint)
	if len(got) != 1 {
		t.Fatalf("pushes to the sender: %d, want 1", len(got))
	}
	assertWebUIDestination(t, got[0].payload.URL)
	if strings.Contains(got[0].payload.URL, inv.String()) {
		t.Errorf("url %q carries the invitation id", got[0].payload.URL)
	}
	p := got[0].payload
	if p.Category != "space_invitation" || !strings.Contains(p.Title+" "+p.Body, inviteeName) || !strings.Contains(p.Title+" "+p.Body, "Acme") {
		t.Errorf("payload %+v: want category space_invitation naming who accepted (%s) and the space Acme", p, inviteeName)
	}
}

// The user-typed space name in a push is bounded and plain (spec
// "Constraints": user text in an email or a push is escaped and bounded, its
// links not made clickable): a harmful name — a line break, a link, markup,
// far too long — does not go in whole, keeps no control character, leaves
// the payload well-formed and does not become the push's destination.
func TestPushRunner_InvitationUserTextBounded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	sender := seedUser(t, pool)
	invitee := seedUser(t, pool)
	endpoint := "https://push.example.com/len-" + uuid.NewString()[:8]
	seedPushSubscription(t, pool, invitee, endpoint)
	long := "Click\nhttps://evil.example/\t<script>alert(1)</script>" + strings.Repeat("x", 120)
	invitationPush(t, pool, sender, invitee, invitee, "space.invitation_sent", long)
	s := &fakePushSender{enabled: true}
	if _, err := NewPushRunner(pool, s, push.NewStore(pool), "", PushOptions{}).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := callsTo(s, endpoint)
	if len(got) != 1 {
		t.Fatalf("pushes: %d, want 1", len(got))
	}
	p := got[0].payload
	text := p.Title + " " + p.Body
	if strings.Contains(text, long) || utf8.RuneCountInString(p.Body) >= utf8.RuneCountInString(long) {
		t.Errorf("a %d-character name went into the push whole: %q", utf8.RuneCountInString(long), p.Body)
	}
	if strings.ContainsAny(text, "\n\r\t") {
		t.Errorf("the push keeps control characters: %q", text)
	}
	var raw map[string]any
	if err := json.Unmarshal(got[0].raw, &raw); err != nil {
		t.Errorf("the payload with a harmful name is not well-formed JSON: %v (%s)", err, got[0].raw)
	}
	if strings.Contains(p.URL, "evil.example") {
		t.Errorf("the name's link became the push destination: %q", p.URL)
	}
}

// Review finding: the re-check before sending also reads is_external: a
// person made external after the push row was written is no party of a
// space invitation any more (K12), neither as its addressee nor as the
// sender of an accepted one.
func TestPushRunner_InvitationNotSentToAPersonMadeExternal(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	for _, c := range []struct{ name, event string }{
		{"the addressee of a sent invitation", "space.invitation_sent"},
		{"the sender of an accepted invitation", "space.invitation_accepted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sender, invitee := seedUser(t, pool), seedUser(t, pool)
			recipient := invitee
			if c.event == "space.invitation_accepted" {
				recipient = sender
			}
			endpoint := "https://push.example.com/ext-" + uuid.NewString()[:8]
			seedPushSubscription(t, pool, recipient, endpoint)
			inv, id := invitationPush(t, pool, sender, invitee, recipient, c.event, "Acme")
			if c.event == "space.invitation_accepted" {
				if _, err := pool.Exec(ctx, `UPDATE space_invitations SET status = 'accepted', resolved_at = now() WHERE id = $1`, inv); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, recipient); err != nil {
				t.Fatal(err)
			}
			s := &fakePushSender{enabled: true}
			if _, err := NewPushRunner(pool, s, push.NewStore(pool), "", PushOptions{}).Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if got := callsTo(s, endpoint); len(got) != 0 {
				t.Errorf("a push went to a person made external: %+v", got)
			}
			if state, outcome, _ := pushStateOf(t, pool, id); state != "final" || outcome == nil || *outcome != outcomeConfigurationNoSend {
				t.Errorf("row %s/%v, want final/configuration_no_send", state, outcome)
			}
		})
	}
}

// assertWebUIDestination: the push leads to the configured web UI
// (https://app.example.com/ in these tests) and nowhere else — the scheme
// https, the host exactly that one, no credentials smuggled before it
// (a prefix check would let app.example.com.evil.test
// or app.example.com@evil.test through).
func assertWebUIDestination(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("push url %q: %v", raw, err)
	}
	if u.Scheme != "https" || u.Host != "app.example.com" || u.User != nil {
		t.Errorf("push url %q: scheme %q host %q userinfo %v, want https://app.example.com without credentials",
			raw, u.Scheme, u.Host, u.User)
	}
}
