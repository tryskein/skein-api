package invitations_test

// lifecycle_test.go — sk-674.18 test matrix rows 166–171 (T040; US-5, INV-16,
// E-3, E-4, E-6, E-8).
//
// References, written before the code (spec US-5,
// "Lifecycle", spaces/contract.go):
//   - 166: accepting from the Inbox — only the addressee (another person's
//     invitation is not found, like a nonexistent one), and with mail on only
//     with a confirmed email (email_unconfirmed); with mail off without it.
//   - 167 (E-4): resend works only on a pending invitation
//     (invitation_not_pending otherwise) and gives a new link and a new
//     expiry; the old link is "invalid"; inviting the same email
//     again is that same invitation.
//   - 168 (E-6): revoking, deleting the space, deleting the invited account →
//     revoked; the link answers "deleted" for a deleted space,
//     "invalid" otherwise.
//   - 169: the expiry worker makes overdue invitations expired with the
//     signal `expired` (to the owner and the sender); a declined invitation
//     is shown to the owner and the admin as not_accepted for 7 days, then
//     not at all.
//   - 170 (E-3): an acceptance concurrent with a revocation — exactly one
//     outcome; accepting after the revocation — "no longer valid"
//     (invitation_invalid).
//   - 171 (E-8): an account registered on an email with pending invitations
//     sees them in its Inbox; accepting them needs the confirmation (or the
//     link).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/accounts"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/invitations"
	"skein-api/internal/signals"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
	"skein-api/internal/users"
)

// ---------------------------------------------------------------------------
// Helpers shared by the invitation tests
// ---------------------------------------------------------------------------

// sigMsg is one personal signal: its areas and the space reasons it carries.
type sigMsg struct {
	user    uuid.UUID
	areas   []string
	reasons []string
}

// sigSink collects the personal signals published on skein.user.*.signal
// through an in-process NATS server.
type sigSink struct {
	mu   sync.Mutex
	msgs []sigMsg
}

func startSigSink(t *testing.T) *sigSink {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	pub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pub.Close)
	sub, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	s := &sigSink{}
	if _, err := sub.Subscribe(signals.SignalSubjectPattern, func(m *nats.Msg) {
		uid, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(m.Subject, "skein.user."), ".signal"))
		if err != nil {
			return
		}
		var body struct {
			Areas  []string `json:"areas"`
			Spaces []struct {
				Reason string `json:"reason"`
			} `json:"spaces"`
		}
		if err := json.Unmarshal(m.Data, &body); err != nil {
			t.Errorf("signal body %s: %v", m.Data, err)
			return
		}
		msg := sigMsg{user: uid, areas: body.Areas}
		for _, sp := range body.Spaces {
			msg.reasons = append(msg.reasons, sp.Reason)
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, msg)
		s.mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}
	signals.Configure(pub)
	t.Cleanup(func() { signals.Configure(nil) })
	return s
}

func (s *sigSink) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// settle returns the messages received since from, once a sentinel signal
// published after them has arrived (the same connection keeps the order).
func (s *sigSink) settle(t *testing.T, pool *pgxpool.Pool, from int) []sigMsg {
	t.Helper()
	sentinel := uuid.New()
	if err := signals.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		signals.Add(tx, sentinel, signals.AreaInbox)
		_, err := tx.Exec(context.Background(), `SELECT 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		for i := from; i < len(s.msgs); i++ {
			if s.msgs[i].user == sentinel {
				out := append([]sigMsg(nil), s.msgs[from:i]...)
				s.mu.Unlock()
				return out
			}
		}
		s.mu.Unlock()
	}
	t.Fatal("the sentinel signal never arrived")
	return nil
}

// reasonsOf is every space reason user got among msgs.
func reasonsOf(msgs []sigMsg, user uuid.UUID) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		if m.user == user {
			for _, r := range m.reasons {
				out[r] = true
			}
		}
	}
	return out
}

// issueLink plays the mail worker's start of an attempt for the newest
// unsent email of invitation inv: it takes the email's lease and issues its
// token (tokens.IssueForEmail) — the raw link token.
func issueLink(t *testing.T, pool *pgxpool.Pool, inv uuid.UUID) string {
	t.Helper()
	var emailID uuid.UUID
	var gen int
	if err := pool.QueryRow(context.Background(), `
UPDATE outgoing_emails
   SET state = 'leased', attempts = attempts + 1, leased_until = now() + interval '2 minutes'
 WHERE id = (SELECT id FROM outgoing_emails WHERE invitation_id = $1 AND state = 'pending'
             ORDER BY created_at DESC LIMIT 1)
RETURNING id, attempts`, inv).Scan(&emailID, &gen); err != nil {
		t.Fatalf("leasing the invitation email: %v", err)
	}
	raw, err := tokens.IssueForEmail(context.Background(), pool, emailID, gen)
	if err != nil {
		t.Fatalf("IssueForEmail: %v", err)
	}
	return raw
}

// invitationOf is the id of the invitation of email to the space.
func (w *world) invitationOf(email string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `
SELECT id FROM space_invitations WHERE space_id = $1 AND email = $2 ORDER BY created_at DESC LIMIT 1`,
		w.space, email).Scan(&id); err != nil {
		w.t.Fatalf("the invitation of %s: %v", email, err)
	}
	return id
}

func (w *world) status(inv uuid.UUID) string {
	w.t.Helper()
	var s string
	if err := w.pool.QueryRow(context.Background(), `SELECT status FROM space_invitations WHERE id = $1`, inv).Scan(&s); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) joined(user uuid.UUID) bool {
	w.t.Helper()
	var ok bool
	if err := w.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`, w.space, user).Scan(&ok); err != nil {
		w.t.Fatal(err)
	}
	return ok
}

// invite sends an invitation to email through the service; fails on a refusal.
func (w *world) invite(svc *invitations.Service, by spaces.Caller, email string) uuid.UUID {
	w.t.Helper()
	if _, ref, err := createWith(svc, by, w.short, email, nil); err != nil || ref != nil {
		w.t.Fatalf("inviting %s: %v %v", email, ref, err)
	}
	return w.invitationOf(email)
}

// account is an existing account addressee: its email (confirmed or not)
// and the caller it is.
func (w *world) account(confirmed bool) (string, spaces.Caller) {
	w.t.Helper()
	email, id := w.seedAddressee(adExists)
	if confirmed {
		if _, err := w.pool.Exec(context.Background(), `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, id); err != nil {
			w.t.Fatal(err)
		}
	}
	w.t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, id) })
	return email, spaces.Caller{UserID: id, Username: strings.TrimSuffix(email, "@test.local")}
}

func refusalCode(err error) string {
	var ref *spaces.Refusal
	if errors.As(err, &ref) {
		return ref.Code
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

// ---------------------------------------------------------------------------
// Row 166
// ---------------------------------------------------------------------------

func TestLifecycle_AcceptFromInboxAddresseeAndConfirmation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	t.Run("mail on", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		email, addressee := w.account(false)
		_, stranger := w.account(true)
		inv := w.invite(svc, w.owner, email)

		if _, err := svc.AcceptMine(ctx, stranger, inv); refusalCode(err) != "invitation_not_found" {
			t.Errorf("another person accepts: %q, want invitation_not_found", refusalCode(err))
		}
		if _, err := svc.AcceptMine(ctx, addressee, inv); refusalCode(err) != "email_unconfirmed" {
			t.Errorf("the addressee with an unconfirmed email: %q, want email_unconfirmed", refusalCode(err))
		}
		if w.joined(addressee.UserID) || w.joined(stranger.UserID) || w.status(inv) != "pending" {
			t.Fatal("a refused acceptance changed the space or the invitation")
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, addressee.UserID); err != nil {
			t.Fatal(err)
		}
		resp, err := svc.AcceptMine(ctx, addressee, inv)
		if err != nil {
			t.Fatalf("the confirmed addressee: %v", err)
		}
		if resp.Space.ID != w.short || !w.joined(addressee.UserID) || w.status(inv) != "accepted" {
			t.Errorf("after accepting: space %q member %v status %s", resp.Space.ID, w.joined(addressee.UserID), w.status(inv))
		}
	})

	t.Run("mail off", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: false, mail: false})
		email, addressee := w.account(false)
		inv := w.invite(svc, w.owner, email)
		if _, err := svc.AcceptMine(ctx, addressee, inv); err != nil {
			t.Errorf("an unconfirmed addressee with mail off: %v, want accepted", err)
		}
		if !w.joined(addressee.UserID) {
			t.Error("not a participant after accepting")
		}
	})
}

// ---------------------------------------------------------------------------
// Row 167
// ---------------------------------------------------------------------------

func TestLifecycle_ResendNewLinkOldInvalidSameInvitation(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	w := newWorld(t, pool)
	svc := newService(pool, flagsOf{signup: true, mail: true})
	email, addressee := w.account(true)
	inv := w.invite(svc, w.owner, email)
	first := issueLink(t, pool, inv)
	var expires time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM space_invitations WHERE id = $1`, inv).Scan(&expires); err != nil {
		t.Fatal(err)
	}

	// The same email again is the same invitation.
	if _, ref, err := createWith(svc, w.owner, w.short, email, nil); err != nil || ref != nil {
		t.Fatalf("inviting the same email again: %v %v", ref, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM space_invitations WHERE space_id = $1 AND email = $2`, w.space, email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || w.invitationOf(email) != inv {
		t.Errorf("inviting the same email again: %d invitations, want the same one", n)
	}

	time.Sleep(10 * time.Millisecond)
	view, err := svc.Resend(ctx, w.owner, w.short, inv, nil, "")
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if view.ExpiresAt == nil || !view.ExpiresAt.After(expires) {
		t.Errorf("resend expiry %v, want later than %v", view.ExpiresAt, expires)
	}
	second := issueLink(t, pool, inv)
	if second == first {
		t.Fatal("resend issued the same link")
	}
	if p, err := svc.Preview(ctx, first); err != nil || p.Status != "invalid" {
		t.Errorf("the old link after resend: %+v %v, want invalid", p, err)
	}
	if p, err := svc.Preview(ctx, second); err != nil || p.Status != "valid" {
		t.Errorf("the new link: %+v %v, want valid", p, err)
	}

	// Resend works only on a pending invitation.
	if _, err := svc.AcceptByToken(ctx, addressee, second); err != nil {
		t.Fatalf("accepting by the new link: %v", err)
	}
	if _, err := svc.Resend(ctx, w.owner, w.short, inv, nil, ""); refusalCode(err) != "invitation_not_pending" {
		t.Errorf("resending an accepted invitation: %q, want invitation_not_pending", refusalCode(err))
	}
}

// ---------------------------------------------------------------------------
// Row 168
// ---------------------------------------------------------------------------

func TestLifecycle_RevokedByRevocationSpaceOrAccountDeletion(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	t.Run("revocation", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		inv := w.invite(svc, w.owner, newEmail())
		link := issueLink(t, pool, inv)
		if err := svc.Revoke(ctx, w.owner, w.short, inv); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if s := w.status(inv); s != "revoked" {
			t.Errorf("status %s, want revoked", s)
		}
		if p, err := svc.Preview(ctx, link); err != nil || p.Status != "invalid" {
			t.Errorf("the link of a revoked invitation: %+v %v, want invalid", p, err)
		}
	})

	t.Run("space deletion", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		inv := w.invite(svc, w.owner, newEmail())
		link := issueLink(t, pool, inv)
		if err := spaces.NewStore(pool).Delete(ctx, w.space, spaces.Actor{UserID: w.owner.UserID, Username: w.owner.Username, Type: "human"}); err != nil {
			t.Fatalf("deleting the space: %v", err)
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM outgoing_emails WHERE invitation_id = $1`, inv)
			_, _ = pool.Exec(c, `DELETE FROM auth_tokens WHERE invitation_id = $1`, inv)
			_, _ = pool.Exec(c, `DELETE FROM space_invitation_sends WHERE invitation_id = $1`, inv)
			_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, inv)
			_, _ = pool.Exec(c, `DELETE FROM events WHERE actor_user_id = $1`, w.owner.UserID)
		})
		if s := w.status(inv); s != "revoked" {
			t.Errorf("status %s, want revoked", s)
		}
		if p, err := svc.Preview(ctx, link); err != nil || p.Status != "space_deleted" {
			t.Errorf("the link of an invitation of a deleted space: %+v %v, want space_deleted", p, err)
		}
	})

	t.Run("account deletion", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		email, addressee := w.account(true)
		inv := w.invite(svc, w.owner, email)
		link := issueLink(t, pool, inv)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `
DELETE FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, email, addressee.Username)
		})
		store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
		if err := store.DeleteAccount(ctx, addressee.UserID, "",
			users.Actor{UserID: addressee.UserID, Username: addressee.Username, Role: "human"}); err != nil {
			t.Fatalf("deleting the addressee's account: %v", err)
		}
		if s := w.status(inv); s != "revoked" {
			t.Errorf("status %s, want revoked", s)
		}
		if p, err := svc.Preview(ctx, link); err != nil || p.Status != "invalid" {
			t.Errorf("the link of an invitation to a deleted account: %+v %v, want invalid", p, err)
		}
		var kept bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM space_invitations WHERE id = $1 AND email = $2)`, inv, email).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if kept {
			t.Error("the invitation still carries the deleted account's email")
		}
	})

	// The account of the invitation's SENDER is deleted.
	// The invitation is revoked as the owner's revocation does it: the link
	// answers «invalid», the addressee's Inbox and spaces and the owner's
	// spaces are re-read.
	t.Run("sender's account deletion", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		sink := startSigSink(t)
		testsupport.SeedUser(t, pool, "admin") // the sender is not the last admin
		email, addressee := w.account(true)
		inv := w.invite(svc, w.admin, email)
		link := issueLink(t, pool, inv)
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM events WHERE actor_user_id = $1 OR (entity_type = 'user' AND entity_id = $1::text)`, w.admin.UserID)
			_, _ = pool.Exec(c, `
DELETE FROM reserved_identities
 WHERE (kind = 'email' AND value_hash = sha256(convert_to($1, 'UTF8')))
    OR (kind = 'username' AND value_hash = sha256(convert_to($2, 'UTF8')))`, w.admin.Username+"@test.local", w.admin.Username)
		})
		from := sink.mark()
		store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
		if err := store.DeleteAccount(ctx, w.admin.UserID, "",
			users.Actor{UserID: w.admin.UserID, Username: w.admin.Username, Role: "human"}); err != nil {
			t.Fatalf("deleting the sender's account: %v", err)
		}
		if s := w.status(inv); s != "revoked" {
			t.Errorf("status %s, want revoked", s)
		}
		if p, err := svc.Preview(ctx, link); err != nil || p.Status != "invalid" {
			t.Errorf("the link of an invitation from a deleted account: %+v %v, want invalid", p, err)
		}
		areas := func(msgs []sigMsg, user uuid.UUID) map[string]bool {
			out := map[string]bool{}
			for _, m := range msgs {
				if m.user == user {
					for _, a := range m.areas {
						out[a] = true
					}
				}
			}
			return out
		}
		got := sink.settle(t, pool, from)
		if a := areas(got, addressee.UserID); !a[string(signals.AreaInbox)] || !a[string(signals.AreaSpaces)] {
			t.Errorf("the addressee's signals %v, want inbox and spaces", a)
		}
		if a := areas(got, w.owner.UserID); !a[string(signals.AreaSpaces)] {
			t.Errorf("the owner's signals %v, want spaces", a)
		}
	})
}

// ---------------------------------------------------------------------------
// Row 169
// ---------------------------------------------------------------------------

func TestLifecycle_ExpiryAndDeclinedShownSevenDays(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	sink := startSigSink(t)

	t.Run("expiry", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		inv := w.invite(svc, w.admin, newEmail()) // the sender is not the owner
		if _, err := pool.Exec(ctx, `UPDATE space_invitations SET expires_at = now() - interval '1 minute' WHERE id = $1`, inv); err != nil {
			t.Fatal(err)
		}
		from := sink.mark()
		if _, err := invitations.NewExpirer(pool, invitations.ExpiryOptions{}).RunOnce(ctx); err != nil {
			t.Fatalf("expiry pass: %v", err)
		}
		if s := w.status(inv); s != "expired" {
			t.Errorf("status %s, want expired", s)
		}
		got := sink.settle(t, pool, from)
		for _, u := range []spaces.Caller{w.owner, w.admin} {
			if !reasonsOf(got, u.UserID)["expired"] {
				t.Errorf("%s got no `expired` (signals: %+v)", u.Username, got)
			}
		}
	})

	t.Run("declined: 7 days, then gone", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		email, addressee := w.account(true)
		inv := w.invite(svc, w.owner, email)
		if err := svc.DeclineMine(ctx, addressee, inv); err != nil {
			t.Fatalf("decline: %v", err)
		}
		sp := spaces.NewService(pool, spaces.Options{})
		shown := func(c spaces.Caller) string {
			d, err := sp.Get(ctx, c, w.short)
			if err != nil {
				t.Fatalf("GET /spaces/%s as %s: %v", w.short, c.Username, err)
			}
			for _, i := range d.Invitations {
				if i.ID == inv.String() {
					return i.Status
				}
			}
			return ""
		}
		for _, c := range []spaces.Caller{w.owner, w.admin} {
			if s := shown(c); s != "not_accepted" {
				t.Errorf("%s sees the declined invitation as %q, want not_accepted", c.Username, s)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE space_invitations SET resolved_at = now() - interval '7 days 1 minute' WHERE id = $1`, inv); err != nil {
			t.Fatal(err)
		}
		for _, c := range []spaces.Caller{w.owner, w.admin} {
			if s := shown(c); s != "" {
				t.Errorf("%s still sees the invitation declined over 7 days ago, as %q", c.Username, s)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Row 170
// ---------------------------------------------------------------------------

func TestLifecycle_AcceptConcurrentWithRevokeOneOutcome(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()

	t.Run("concurrent", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		email, addressee := w.account(true)
		inv := w.invite(svc, w.owner, email)

		// Both start while the space's lock is held, and race when it goes.
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Released on every exit: a failed wait must not leave the two
		// operations blocked behind it.
		defer func() { _ = holder.Rollback(context.Background()) }()
		var pid int32
		if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(ctx, `SELECT 1 FROM spaces WHERE id = $1 FOR UPDATE`, w.space); err != nil {
			t.Fatal(err)
		}
		var acceptErr, revokeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, acceptErr = svc.AcceptMine(ctx, addressee, inv) }()
		go func() { defer wg.Done(); revokeErr = svc.Revoke(ctx, w.owner, w.short, inv) }()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var n int
			// The waiters behind the holder, directly or queued behind one
			// another on the same row.
			if err := pool.QueryRow(ctx, `
WITH RECURSIVE waiters(pid) AS (
    SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
    UNION
    SELECT a.pid FROM pg_stat_activity a JOIN waiters w ON w.pid = ANY(pg_blocking_pids(a.pid)))
SELECT count(*) FROM waiters`, pid).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d of the two operations wait for the space's lock", n)
			}
		}
		if err := holder.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()

		switch status, joined := w.status(inv), w.joined(addressee.UserID); {
		case acceptErr == nil && revokeErr != nil:
			if status != "accepted" || !joined {
				t.Errorf("acceptance won: status %s, member %v", status, joined)
			}
			if refusalCode(revokeErr) != "invitation_not_found" {
				t.Errorf("the revocation after the acceptance: %q, want invitation_not_found", refusalCode(revokeErr))
			}
		case revokeErr == nil && acceptErr != nil:
			if status != "revoked" || joined {
				t.Errorf("revocation won: status %s, member %v", status, joined)
			}
			if refusalCode(acceptErr) != "invitation_invalid" {
				t.Errorf("the acceptance after the revocation: %q, want invitation_invalid", refusalCode(acceptErr))
			}
		default:
			t.Errorf("not exactly one outcome: accept %v, revoke %v (status %s, member %v)", acceptErr, revokeErr, status, joined)
		}
	})

	t.Run("accepting after the revocation", func(t *testing.T) {
		w := newWorld(t, pool)
		svc := newService(pool, flagsOf{signup: true, mail: true})
		email, addressee := w.account(true)
		inv := w.invite(svc, w.owner, email)
		link := issueLink(t, pool, inv)
		if err := svc.Revoke(ctx, w.owner, w.short, inv); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AcceptMine(ctx, addressee, inv); refusalCode(err) != "invitation_invalid" {
			t.Errorf("from the Inbox after the revocation: %q, want invitation_invalid", refusalCode(err))
		}
		if _, err := svc.AcceptByToken(ctx, addressee, link); refusalCode(err) != "invitation_invalid" {
			t.Errorf("by the link after the revocation: %q, want invitation_invalid", refusalCode(err))
		}
		if w.joined(addressee.UserID) {
			t.Error("joined after the revocation")
		}
	})
}

// ---------------------------------------------------------------------------
// Row 171
// ---------------------------------------------------------------------------

func TestLifecycle_SignupOnInvitedEmailSeesInvitations(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	w := newWorld(t, pool)
	svc := newService(pool, flagsOf{signup: true, mail: true})
	email := newEmail()
	inv := w.invite(svc, w.owner, email)

	// A plain self-service signup on the invited email (T038's path).
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	username := "e8" + strings.ReplaceAll(uuid.NewString()[:10], "-", "")
	resp, err := store.CreateSelfService(ctx, accounts.NewAccount{
		Email: email, Username: username, Password: "correct horse battery 171", Language: "en", DeliveryTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	account, err := uuid.Parse(resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeAccount(pool, account) })
	me := spaces.Caller{UserID: account, Username: username}

	mine, err := svc.Mine(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine.Items) != 1 || mine.Items[0].ID != inv.String() || mine.Items[0].CanAccept ||
		mine.Items[0].CannotAcceptReason == nil || *mine.Items[0].CannotAcceptReason != "email_unconfirmed" {
		t.Fatalf("the Inbox of the new account: %+v, want the invitation with canAccept false (email_unconfirmed)", mine.Items)
	}
	if _, err := svc.AcceptMine(ctx, me, inv); refusalCode(err) != "email_unconfirmed" {
		t.Errorf("accepting before the confirmation: %q, want email_unconfirmed", refusalCode(err))
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptMine(ctx, me, inv); err != nil {
		t.Errorf("accepting after the confirmation: %v", err)
	}
	if !w.joined(account) {
		t.Error("not a participant after accepting")
	}
}

// removeAccount removes an account a self-service signup created, with what
// it wrote.
func removeAccount(pool *pgxpool.Pool, id uuid.UUID) {
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM auth_tokens WHERE user_id = $1`,
		`DELETE FROM outgoing_emails WHERE user_id = $1`,
		`DELETE FROM events WHERE actor_user_id = $1`,
		`DELETE FROM space_members WHERE user_id = $1
		    OR space_id IN (SELECT id FROM spaces WHERE owner_id = $1 AND kind = 'personal')`,
		`DELETE FROM spaces WHERE owner_id = $1 AND kind = 'personal'`,
		`DELETE FROM users WHERE id = $1`,
	} {
		_, _ = pool.Exec(ctx, q, id)
	}
}
