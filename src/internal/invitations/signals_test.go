package invitations_test

// signals_test.go — sk-674.18 test matrix rows 175 and 175a (T040; US-19,
// US-5, E-12, "Metrics").
//
// References, written before the code:
//   - 175: the personal signal `spaces` carries `invited` to the invited
//     user, `accepted` and `declined` to whoever invited (signals contract:
//     declined and expired also to the space owner), `expired` to the owner
//     and the sender; the invitation email is written in the inviter's
//     language (the decision of Stage 1: "the invitation is in the inviter's language");
//     with SMTP unavailable (the email undelivered in the queue) the Inbox
//     card and the push of an existing addressee exist all the same (E-12).
//   - 175a: skein_space_operations_total{operation,result} for invite, accept
//     and decline: a success and a refusal raise their own pair and no other.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/invitations"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

// Row 175.
func TestSignals_InvitationReasonsLanguageAndSMTPIndependence(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	sink := startSigSink(t)

	t.Run("invited, accepted", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: false, mail: true})
		email, addressee := w.account(true)
		from := sink.mark()
		inv := w.invite(svc, w.admin, email) // the sender is not the owner
		got := sink.settle(t, pool, from)
		if !reasonsOf(got, addressee.UserID)["invited"] {
			t.Errorf("the invited user got no `invited`: %+v", got)
		}
		from = sink.mark()
		if _, err := svc.AcceptMine(ctx, addressee, inv); err != nil {
			t.Fatal(err)
		}
		got = sink.settle(t, pool, from)
		if !reasonsOf(got, w.admin.UserID)["accepted"] {
			t.Errorf("the sender got no `accepted`: %+v", got)
		}
		if reasonsOf(got, w.owner.UserID)["accepted"] {
			t.Errorf("the owner, who did not invite, got `accepted`: %+v", got)
		}
	})

	t.Run("declined", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: false, mail: true})
		email, addressee := w.account(true)
		inv := w.invite(svc, w.admin, email)
		from := sink.mark()
		if err := svc.DeclineMine(ctx, addressee, inv); err != nil {
			t.Fatal(err)
		}
		got := sink.settle(t, pool, from)
		for _, u := range []spaces.Caller{w.admin, w.owner} {
			if !reasonsOf(got, u.UserID)["declined"] {
				t.Errorf("%s got no `declined`: %+v", u.Username, got)
			}
		}
	})

	t.Run("expired", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		inv := w.invite(svc, w.admin, newEmail())
		if _, err := pool.Exec(ctx, `UPDATE space_invitations SET expires_at = now() - interval '1 minute' WHERE id = $1`, inv); err != nil {
			t.Fatal(err)
		}
		from := sink.mark()
		if _, err := invitations.NewExpirer(pool, invitations.ExpiryOptions{}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got := sink.settle(t, pool, from)
		for _, u := range []spaces.Caller{w.admin, w.owner} {
			if !reasonsOf(got, u.UserID)["expired"] {
				t.Errorf("%s got no `expired`: %+v", u.Username, got)
			}
		}
	})

	t.Run("expired: an owner who sent it gets it once", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		inv := w.invite(svc, w.owner, newEmail())
		if _, err := pool.Exec(ctx, `UPDATE space_invitations SET expires_at = now() - interval '1 minute' WHERE id = $1`, inv); err != nil {
			t.Fatal(err)
		}
		from := sink.mark()
		if _, err := invitations.NewExpirer(pool, invitations.ExpiryOptions{}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range sink.settle(t, pool, from) {
			if m.user != w.owner.UserID {
				continue
			}
			for _, r := range m.reasons {
				if r == "expired" {
					n++
				}
			}
		}
		if n != 1 {
			t.Errorf("the owner who sent the invitation got `expired` %d times, want once", n)
		}
	})

	t.Run("the email in the inviter's language", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		for _, lang := range []string{"ru", "en"} {
			email := newEmail()
			if _, err := svc.Create(ctx, w.owner, w.short, spaces.CreateInvitationRequest{Email: &email, Language: lang}, ""); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := pool.QueryRow(ctx, `
SELECT o.lang FROM outgoing_emails o JOIN space_invitations i ON i.id = o.invitation_id
 WHERE i.space_id = $1 AND i.email = $2`, w.space, email).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != lang {
				t.Errorf("the inviter wrote in %s, the email is queued in %s", lang, got)
			}
		}
	})

	t.Run("SMTP unavailable: the card and the push exist", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: false, mail: true})
		email, addressee := w.account(false)
		inv := w.invite(svc, w.owner, email)
		// No mail worker delivered anything: the email waits in the queue.
		var emailState, pushState string
		if err := pool.QueryRow(ctx, `
SELECT (SELECT state FROM outgoing_emails WHERE invitation_id = $1),
       (SELECT push_state FROM notifications WHERE user_id = $2 AND type = 'space_invitation')`,
			inv, addressee.UserID).Scan(&emailState, &pushState); err != nil {
			t.Fatalf("reading the email and the push: %v", err)
		}
		if emailState != "pending" || pushState != "pending" {
			t.Errorf("email %q, push %q; want the email undelivered and the push queued on its own", emailState, pushState)
		}
		mine, err := svc.Mine(ctx, addressee)
		if err != nil {
			t.Fatal(err)
		}
		if len(mine.Items) != 1 || mine.Items[0].ID != inv.String() {
			t.Errorf("the Inbox card: %+v, want the invitation", mine.Items)
		}
	})
}

// opsCount reads skein_space_operations_total{operation,result}.
func opsCount(t *testing.T, operation, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "skein_space_operations_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["operation"] == operation && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// Row 175a.
func TestSignals_OperationsMetric(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	w := newWorld(t, pool)
	svc := newService(pool, flagsOf{signup: false, mail: true})
	pairs := [][2]string{}
	for _, op := range []string{"invite", "accept", "decline"} {
		for _, res := range []string{"success", "refused", "error"} {
			pairs = append(pairs, [2]string{op, res})
		}
	}
	snapshot := func() map[[2]string]float64 {
		out := map[[2]string]float64{}
		for _, p := range pairs {
			out[p] = opsCount(t, p[0], p[1])
		}
		return out
	}
	step := func(name string, want [2]string, act func()) {
		t.Helper()
		before := snapshot()
		act()
		after := snapshot()
		for _, p := range pairs {
			d := after[p] - before[p]
			if p == want && d != 1 {
				t.Errorf("%s: %v grew by %v, want 1", name, p, d)
			}
			if p != want && d != 0 {
				t.Errorf("%s: %v grew by %v, want 0", name, p, d)
			}
		}
	}

	acceptMail, acceptor := w.account(false)
	declineMail, decliner := w.account(true)
	external, _ := w.seedAddressee(adExternal)
	var acceptInv, declineInv uuid.UUID
	step("invite: sent", [2]string{"invite", "success"}, func() { acceptInv = w.invite(svc, w.owner, acceptMail) })
	declineInv = w.invite(svc, w.owner, declineMail)
	step("invite: external_user", [2]string{"invite", "refused"}, func() {
		if _, ref, err := createWith(svc, w.owner, w.short, external, nil); err != nil || ref == nil {
			t.Fatalf("inviting an external user: %v %v", ref, err)
		}
	})
	step("accept: email_unconfirmed", [2]string{"accept", "refused"}, func() {
		if _, err := svc.AcceptMine(ctx, acceptor, acceptInv); refusalCode(err) != "email_unconfirmed" {
			t.Fatalf("accept: %q", refusalCode(err))
		}
	})
	if _, err := pool.Exec(ctx, `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, acceptor.UserID); err != nil {
		t.Fatal(err)
	}
	step("accept: joined", [2]string{"accept", "success"}, func() {
		if _, err := svc.AcceptMine(ctx, acceptor, acceptInv); err != nil {
			t.Fatal(err)
		}
	})
	step("decline: declined", [2]string{"decline", "success"}, func() {
		if err := svc.DeclineMine(ctx, decliner, declineInv); err != nil {
			t.Fatal(err)
		}
	})
	step("decline: invitation_invalid", [2]string{"decline", "refused"}, func() {
		if err := svc.DeclineMine(ctx, decliner, declineInv); refusalCode(err) != "invitation_invalid" {
			t.Fatalf("declining again: %q", refusalCode(err))
		}
	})

	// The early refusals of a well-formed request are
	// counted too, once: a space the caller does not see, another person's
	// invitation.
	stranger := newWorld(t, pool)
	step("invite: space_not_found", [2]string{"invite", "refused"}, func() {
		if _, ref, err := createWith(svc, stranger.owner, w.short, newEmail(), nil); err != nil || ref == nil || ref.Code != "space_not_found" {
			t.Fatalf("inviting into an unseen space: %v %v", ref, err)
		}
	})
	otherMail, _ := w.account(true)
	otherInv := w.invite(svc, w.owner, otherMail)
	step("accept: invitation_not_found", [2]string{"accept", "refused"}, func() {
		if _, err := svc.AcceptMine(ctx, decliner, otherInv); refusalCode(err) != "invitation_not_found" {
			t.Fatalf("accepting another person's invitation: %q", refusalCode(err))
		}
	})
	step("decline: invitation_not_found", [2]string{"decline", "refused"}, func() {
		if err := svc.DeclineMine(ctx, decliner, otherInv); refusalCode(err) != "invitation_not_found" {
			t.Fatalf("declining another person's invitation: %q", refusalCode(err))
		}
	})
	// A malformed request does not reach the operation: no pair moves.
	before := snapshot()
	if _, ref, _ := createWith(svc, w.owner, w.short, "not an email", nil); ref == nil || ref.Code != "validation_error" {
		t.Fatalf("a malformed email: %v", ref)
	}
	for _, p := range pairs {
		if d := snapshot()[p] - before[p]; d != 0 {
			t.Errorf("a malformed request moved %v by %v", p, d)
		}
	}
}
