package mail_test

// runner_test.go — the mail worker against a real queue (sk-674.18, T022;
// matrix rows 66, 67, 69, 70, 71, 72, 75, 76).
//
// The relay is a scripted Sender; the token protocol is the real one
// (tokens.EmailIssuer), so a link that "works" is proven by tokens.Use. Time
// is the worker's controlled clock (mail.Options.Now), which the worker passes
// to SQL for every queue comparison (runner.go «Time», R-66): a 40-minute
// relay outage is crossed by moving the clock, not by waiting.
//
// The worker takes ANY pending email of the database, so every test starts by
// clearing the queue (resetQueue) — the same convention as internal/outbox's
// resetOutbox, safe because integration packages run one at a time (-p 1)
// and the tests here are not parallel.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"skein-api/internal/mail"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
)

const linkBase = "https://skein.example.com"

// ---------------------------------------------------------------- fixtures

// clock is the controlled clock handed to the worker.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// newClock starts at the present, at the microsecond precision of
// timestamptz, so a time the worker stored reads back equal.
func newClock() *clock { return &clock{t: time.Now().UTC().Truncate(time.Microsecond)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// sent is one message the scripted relay received.
type sent struct {
	to  string
	msg []byte
}

// relay is a scripted Sender: each call takes the next scripted answer
// (nil = accepted); past the script it accepts. onSend, when set, runs
// inside the call before the answer — what happens elsewhere while the
// worker is talking to the relay.
type relay struct {
	mu      sync.Mutex
	script  []error
	always  error
	onSend  func()
	calls   int
	accepts []sent
}

func (r *relay) Send(_ context.Context, to string, msg []byte) error {
	r.mu.Lock()
	r.calls++
	var ans error
	switch {
	case len(r.script) > 0:
		ans, r.script = r.script[0], r.script[1:]
	default:
		ans = r.always
	}
	hook := r.onSend
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	if ans == nil {
		r.mu.Lock()
		r.accepts = append(r.accepts, sent{to: to, msg: msg})
		r.mu.Unlock()
	}
	return ans
}

func (r *relay) From() string { return "skein@example.com" }

func (r *relay) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *relay) Accepted() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sent(nil), r.accepts...)
}

// The relay's answers, as smtp.go builds them.
func unreachable() error {
	return &mail.SendError{Stage: mail.StageConnect, Err: errors.New("dial tcp 192.0.2.1:465: connect: connection refused")}
}

func busy() error {
	return &mail.SendError{Stage: mail.StageRcpt, Err: &textproto.Error{Code: 421, Msg: "4.3.2 try again later"}}
}

func noSuchMailbox() error {
	return &mail.SendError{Stage: mail.StageRcpt, Err: &textproto.Error{Code: 550, Msg: "5.1.1 mailbox unavailable"}}
}

func newRunner(t *testing.T, pool *pgxpool.Pool, r *relay, c *clock) *mail.Runner {
	t.Helper()
	run, err := mail.NewRunner(pool, r, tokens.EmailIssuer(pool), linkBase, mail.Options{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// resetQueue empties outgoing_emails (see the file header).
func resetQueue(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM outgoing_emails`); err != nil {
		t.Fatalf("resetting the mail queue: %v", err)
	}
}

// seedUser is a user whose tokens and emails are removed before the user.
func seedUser(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	id, uname := testsupport.SeedUser(t, pool, "")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM auth_tokens WHERE user_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM outgoing_emails WHERE user_id = $1`, id)
	})
	return id, uname + "@test.local"
}

// seedInvitation is a pending invitation of a fresh space to a fresh address,
// in English; its tokens and emails are removed before the invitation.
func seedInvitation(t *testing.T, pool *pgxpool.Pool) (invID uuid.UUID, to, inviterName, spaceName string) {
	t.Helper()
	ctx := context.Background()
	owner, ownerName := testsupport.SeedUser(t, pool, "")
	spaceID := testsupport.SeedSpace(t, pool, owner)
	if err := pool.QueryRow(ctx, `SELECT name FROM spaces WHERE id = $1`, spaceID).Scan(&spaceName); err != nil {
		t.Fatal(err)
	}
	to = "inv" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@test.local"
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'en', now() + interval '7 days')
RETURNING id`, spaceID, to, owner).Scan(&invID); err != nil {
		t.Fatalf("seeding an invitation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE space_invitations SET current_token_id = NULL WHERE id = $1`, invID)
		_, _ = pool.Exec(ctx, `DELETE FROM auth_tokens WHERE invitation_id = $1`, invID)
		_, _ = pool.Exec(ctx, `DELETE FROM outgoing_emails WHERE invitation_id = $1`, invID)
		_, _ = pool.Exec(ctx, `DELETE FROM space_invitations WHERE id = $1`, invID)
	})
	return invID, to, ownerName, spaceName
}

// enqueue queues m in its own committed transaction (the action).
func enqueue(t *testing.T, pool *pgxpool.Pool, m mail.Message, ttl time.Duration) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		id, err = mail.Enqueue(context.Background(), tx, m, ttl)
		return err
	}); err != nil {
		t.Fatalf("queueing an email: %v", err)
	}
	return id
}

type emailRow struct {
	state    string
	outcome  string
	attempts int
	next     *time.Time
	deadline time.Time
}

func readEmail(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) emailRow {
	t.Helper()
	var r emailRow
	var outcome *string
	if err := pool.QueryRow(context.Background(), `
SELECT state, outcome, attempts, next_attempt_at, deadline_at FROM outgoing_emails WHERE id = $1`, id).
		Scan(&r.state, &outcome, &r.attempts, &r.next, &r.deadline); err != nil {
		t.Fatalf("reading email %s: %v", id, err)
	}
	if outcome != nil {
		r.outcome = *outcome
	}
	return r
}

func tick(t *testing.T, run *mail.Runner) {
	t.Helper()
	if _, err := run.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// linkToken returns the token of the link <base><path><token> in the
// plain-text part of a delivered message.
func linkToken(t *testing.T, msg []byte, path string) string {
	t.Helper()
	body := []byte(textPart(t, msg))
	re := regexp.MustCompile(regexp.QuoteMeta(linkBase+path) + `([A-Za-z0-9_-]+)`)
	got := re.FindSubmatch(body)
	if got == nil {
		t.Fatalf("no %s link in the delivered text:\n%s", path, body)
	}
	return string(got[1])
}

// textPart is the decoded plain-text part of a delivered message.
func textPart(t *testing.T, msg []byte) string {
	t.Helper()
	m, err := netmail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("parsing the delivered message: %v", err)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type: %v", err)
	}
	part, err := multipart.NewReader(m.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatalf("first part: %v", err)
	}
	// multipart.Part decodes the quoted-printable transfer encoding itself.
	body, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("reading the text part: %v", err)
	}
	return string(body)
}

// use presents a raw token to the token protocol in a committed action and
// reports the protocol's verdict.
func use(t *testing.T, pool *pgxpool.Pool, p tokens.Purpose, raw, addressee string) error {
	t.Helper()
	return pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := tokens.Use(context.Background(), tx, p, raw, addressee)
		return err
	})
}

// ---------------------------------------------------------------- metrics

var (
	allKinds    = []mail.Kind{mail.KindEmailConfirm, mail.KindPasswordReset, mail.KindSpaceInvite}
	allOutcomes = []string{mail.OutcomeSuccess, mail.OutcomeRejected, mail.OutcomeExpired, mail.OutcomeCancelled}
)

// counter reads a counter of the default registry by name and labels; a
// series not yet created reads 0.
func counter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue series
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

type outcomeKey struct {
	kind    mail.Kind
	outcome string
}

func deliveries(t *testing.T) map[outcomeKey]float64 {
	t.Helper()
	out := map[outcomeKey]float64{}
	for _, k := range allKinds {
		for _, o := range allOutcomes {
			out[outcomeKey{k, o}] = counter(t, "skein_email_deliveries_total", map[string]string{"kind": string(k), "outcome": o})
		}
	}
	return out
}

func retries(t *testing.T) float64 {
	return counter(t, "skein_email_retries_total", nil)
}

// Row 75: after an email of kind K finalised with outcome O, the series
// (K, O) grew by exactly one and no other series moved.
func assertOneDelivery(t *testing.T, before map[outcomeKey]float64, kind mail.Kind, outcome string) {
	t.Helper()
	after := deliveries(t)
	for key, b := range before {
		want := b
		if key == (outcomeKey{kind, outcome}) {
			want = b + 1
		}
		if after[key] != want {
			t.Errorf("skein_email_deliveries_total{kind=%q,outcome=%q} = %v, want %v",
				key.kind, key.outcome, after[key], want)
		}
	}
}

// ---------------------------------------------------------------- rows

// Row 66 (SC-008, E-12) and 76: the relay is unreachable for longer than
// the five attempts of push would ever have covered (30 s + 1 + 2 + 4 min of
// back-off, 7.5 minutes), then comes back before deadline_at — the email is
// delivered, outcome success. Every failed attempt is one retry. Red if the
// worker gives up on an attempt budget.
func TestRunner_DeliversAfterALongOutageWithinTheDeadline(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	userID, to := seedUser(t, pool)
	id := enqueue(t, pool, mail.Message{Kind: mail.KindPasswordReset, To: to, Lang: mail.LangEN, UserID: userID}, 24*time.Hour)

	const failures = 8
	script := make([]error, failures)
	for i := range script {
		if i%2 == 0 {
			script[i] = unreachable()
		} else {
			script[i] = busy()
		}
	}
	r := &relay{script: script}
	c := newClock()
	start := c.Now()
	run := newRunner(t, pool, r, c)
	before, retriesBefore := deliveries(t), retries(t)

	for i := 0; i < failures+5 && readEmail(t, pool, id).state != "final"; i++ {
		tick(t, run)
		if e := readEmail(t, pool, id); e.state == "pending" {
			if e.next == nil {
				t.Fatalf("attempt %d: pending without a next attempt time", e.attempts)
			}
			c.Set(*e.next)
		}
	}

	e := readEmail(t, pool, id)
	if e.state != "final" || e.outcome != mail.OutcomeSuccess {
		t.Fatalf("email = %s/%q after %d attempts, want final/success", e.state, e.outcome, e.attempts)
	}
	if e.attempts != failures+1 || r.Calls() != failures+1 {
		t.Errorf("attempts = %d, relay calls = %d; want %d each", e.attempts, r.Calls(), failures+1)
	}
	if outage := c.Now().Sub(start); outage <= 7*time.Minute+30*time.Second {
		t.Fatalf("precondition: the outage lasted %s of worker time, not longer than push's five attempts", outage)
	}
	if got := retries(t) - retriesBefore; got != failures {
		t.Errorf("skein_email_retries_total grew by %v, want %d (one per temporary failure)", got, failures)
	}
	assertOneDelivery(t, before, mail.KindPasswordReset, mail.OutcomeSuccess)
	if got := linkToken(t, r.Accepted()[0].msg, "/reset-password/"); use(t, pool, tokens.PurposePasswordReset, got, "") != nil {
		t.Error("the delivered link does not work")
	}
}

// Row 67 (E-12): the relay never comes back — at deadline_at the email ends
// as expired, and the action that queued it stays committed. Red if the
// email hangs pending/leased or ends otherwise.
func TestRunner_ExpiresAtTheDeadline(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	userID, to := seedUser(t, pool)
	var id uuid.UUID
	// The action: a write of its own, then the email, one transaction.
	if err := pgx.BeginTxFunc(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `UPDATE users SET name = 'action-done' WHERE id = $1`, userID); err != nil {
			return err
		}
		var err error
		id, err = mail.Enqueue(context.Background(), tx, mail.Message{Kind: mail.KindPasswordReset, To: to, Lang: mail.LangRU, UserID: userID}, time.Hour)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	r := &relay{always: unreachable()}
	c := newClock()
	run := newRunner(t, pool, r, c)
	before := deliveries(t)

	for i := 0; i < 50 && readEmail(t, pool, id).state != "final"; i++ {
		tick(t, run)
		if e := readEmail(t, pool, id); e.state == "pending" && e.next != nil {
			c.Set(*e.next)
		}
	}

	e := readEmail(t, pool, id)
	if e.state != "final" || e.outcome != mail.OutcomeExpired {
		t.Fatalf("email = %s/%q at worker time %s (deadline %s), want final/expired",
			e.state, e.outcome, c.Now(), e.deadline)
	}
	if c.Now().Before(e.deadline) {
		t.Errorf("expired at %s, before its deadline %s", c.Now(), e.deadline)
	}
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT name FROM users WHERE id = $1`, userID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "action-done" {
		t.Errorf("the action was rolled back: users.name = %q", name)
	}
	assertOneDelivery(t, before, mail.KindPasswordReset, mail.OutcomeExpired)
}

// Rows 69 and 76 (K-9): a permanent refusal (5xx to RCPT) rejects the email
// after one attempt and is never retried, not even later; a temporary one
// (4xx) puts it back with a 30-second back-off and counts one retry.
func TestRunner_PermanentRejectsAtOnceTemporaryRetries(t *testing.T) {
	pool := testsupport.NewTestPool(t)

	t.Run("5xx to RCPT is rejected after one attempt", func(t *testing.T) {
		resetQueue(t, pool)
		userID, to := seedUser(t, pool)
		id := enqueue(t, pool, mail.Message{Kind: mail.KindEmailConfirm, To: to, Lang: mail.LangEN, UserID: userID}, 24*time.Hour)
		r := &relay{always: noSuchMailbox()}
		c := newClock()
		run := newRunner(t, pool, r, c)
		before, retriesBefore := deliveries(t), retries(t)

		tick(t, run)
		c.Advance(time.Hour)
		tick(t, run)

		e := readEmail(t, pool, id)
		if e.state != "final" || e.outcome != mail.OutcomeRejected || e.attempts != 1 || r.Calls() != 1 {
			t.Fatalf("email = %s/%q, %d attempts, %d relay calls; want final/rejected after exactly 1",
				e.state, e.outcome, e.attempts, r.Calls())
		}
		if got := retries(t) - retriesBefore; got != 0 {
			t.Errorf("skein_email_retries_total grew by %v on a permanent failure, want 0", got)
		}
		assertOneDelivery(t, before, mail.KindEmailConfirm, mail.OutcomeRejected)
	})

	t.Run("4xx is retried", func(t *testing.T) {
		resetQueue(t, pool)
		userID, to := seedUser(t, pool)
		id := enqueue(t, pool, mail.Message{Kind: mail.KindEmailConfirm, To: to, Lang: mail.LangEN, UserID: userID}, 24*time.Hour)
		r := &relay{always: busy()}
		c := newClock()
		run := newRunner(t, pool, r, c)
		retriesBefore := retries(t)

		tick(t, run)

		e := readEmail(t, pool, id)
		if e.state != "pending" || e.attempts != 1 || e.next == nil {
			t.Fatalf("email = %s, %d attempts, next %v; want pending after 1 attempt with a next attempt", e.state, e.attempts, e.next)
		}
		if want := c.Now().Add(30 * time.Second); !e.next.Equal(want) {
			t.Errorf("next attempt at %s, want %s (30 s back-off)", e.next, want)
		}
		if got := retries(t) - retriesBefore; got != 1 {
			t.Errorf("skein_email_retries_total grew by %v, want 1", got)
		}
	})
}

// Row 70 (INV-25, K-9), first half: an email whose worker stopped in the
// middle of an attempt — leased, a token already issued for that attempt —
// is delivered by a new worker instance once the lease runs out, and the
// link it carries works.
func TestRunner_AfterARestartTheEmailLeavesWithAWorkingLink(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	ctx := context.Background()
	userID, to := seedUser(t, pool)
	id := enqueue(t, pool, mail.Message{Kind: mail.KindEmailConfirm, To: to, Lang: mail.LangEN, UserID: userID}, 24*time.Hour)

	// The first instance: leased the email, issued its token, and stopped.
	c := newClock()
	if _, err := pool.Exec(ctx, `
UPDATE outgoing_emails SET state = 'leased', attempts = attempts + 1, leased_until = $2
 WHERE id = $1`, id, c.Now().Add(mail.DefaultLeaseTTL)); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.EmailIssuer(pool).IssueForEmail(ctx, id, 1); err != nil {
		t.Fatalf("the first instance's issue: %v", err)
	}

	// The new instance, after the old lease has run out.
	c.Advance(mail.DefaultLeaseTTL + time.Second)
	r := &relay{}
	tick(t, newRunner(t, pool, r, c))

	if e := readEmail(t, pool, id); e.state != "final" || e.outcome != mail.OutcomeSuccess {
		t.Fatalf("email = %s/%q after the restart, want final/success", e.state, e.outcome)
	}
	if len(r.Accepted()) != 1 {
		t.Fatalf("relay accepted %d messages, want 1", len(r.Accepted()))
	}
	raw := linkToken(t, r.Accepted()[0].msg, "/confirm-email/")
	if err := use(t, pool, tokens.PurposeEmailConfirm, raw, ""); err != nil {
		t.Errorf("the link delivered after the restart does not work: %v", err)
	}
}

// Row 70, second half: the subject asks again (tokens.Replace) while the
// worker is sending the earlier email. The earlier email does not leave with
// a working link and keeps the outcome the protocol gave it; the new email
// is delivered with a working link, and a stale attempt of the earlier email
// cannot touch it.
func TestRunner_EmailReplacedWhileSendingLeavesNoLiveLink(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	ctx := context.Background()
	userID, to := seedUser(t, pool)
	msg := mail.Message{Kind: mail.KindPasswordReset, To: to, Lang: mail.LangEN, UserID: userID}
	first := enqueue(t, pool, msg, 24*time.Hour)

	var second uuid.UUID
	r := &relay{}
	r.onSend = func() {
		if second != uuid.Nil {
			return
		}
		if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			var err error
			second, err = tokens.Replace(ctx, tx, msg, 24*time.Hour)
			return err
		}); err != nil {
			t.Errorf("Replace during the send: %v", err)
		}
	}
	c := newClock()
	run := newRunner(t, pool, r, c)

	tick(t, run) // sends the first email; Replace happens mid-send
	if second == uuid.Nil {
		t.Fatal("precondition: the relay was never called")
	}
	if e := readEmail(t, pool, first); e.state != "final" || e.outcome != mail.OutcomeCancelled {
		t.Errorf("first email = %s/%q, want final/cancelled (the protocol's outcome, not overwritten)", e.state, e.outcome)
	}
	oldLink := linkToken(t, r.Accepted()[0].msg, "/reset-password/")

	tick(t, run) // sends the second email
	if e := readEmail(t, pool, second); e.state != "final" || e.outcome != mail.OutcomeSuccess {
		t.Fatalf("second email = %s/%q, want final/success", e.state, e.outcome)
	}
	if len(r.Accepted()) != 2 {
		t.Fatalf("relay accepted %d messages, want 2", len(r.Accepted()))
	}
	newLink := linkToken(t, r.Accepted()[1].msg, "/reset-password/")

	if err := use(t, pool, tokens.PurposePasswordReset, oldLink, ""); !errors.Is(err, tokens.ErrInvalid) {
		t.Errorf("the replaced email's link: Use = %v, want ErrInvalid", err)
	}
	// A stale attempt of the first email cannot issue, so cannot supersede
	// the new link.
	if _, err := tokens.EmailIssuer(pool).IssueForEmail(ctx, first, 1); !errors.Is(err, mail.ErrNotIssuable) {
		t.Errorf("stale attempt of the replaced email: IssueForEmail = %v, want ErrNotIssuable", err)
	}
	if err := use(t, pool, tokens.PurposePasswordReset, newLink, ""); err != nil {
		t.Errorf("the new email's link does not work: %v", err)
	}
}

// Row 71 (INV-25): every email ends with exactly one outcome. A worker whose
// lease ran out while it was talking to the relay — and whose email another
// worker has meanwhile delivered — finalises nothing: its verdict (a 5xx
// here) does not overwrite the success, and is not counted.
func TestRunner_StaleLeaseDoesNotOverwriteTheOutcome(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	userID, to := seedUser(t, pool)
	id := enqueue(t, pool, mail.Message{Kind: mail.KindPasswordReset, To: to, Lang: mail.LangEN, UserID: userID}, 24*time.Hour)

	c := newClock()
	fast := &relay{}
	second := newRunner(t, pool, fast, c)

	slow := &relay{always: noSuchMailbox()}
	slow.onSend = func() {
		// While the first worker waits on the relay, its lease runs out and
		// the second worker takes the email over and delivers it.
		c.Advance(mail.DefaultLeaseTTL + time.Second)
		tick(t, second)
	}
	first := newRunner(t, pool, slow, c)
	before := deliveries(t)

	tick(t, first)

	e := readEmail(t, pool, id)
	if e.state != "final" || e.outcome != mail.OutcomeSuccess || e.attempts != 2 {
		t.Fatalf("email = %s/%q after %d leases, want final/success after 2 — the stale lease overwrote the outcome",
			e.state, e.outcome, e.attempts)
	}
	if slow.Calls() != 1 || fast.Calls() != 1 {
		t.Fatalf("precondition: relay calls slow=%d fast=%d, want 1 each", slow.Calls(), fast.Calls())
	}
	assertOneDelivery(t, before, mail.KindPasswordReset, mail.OutcomeSuccess)
}

// Row 72 (INV-25): an email whose addressee deleted the account, or whose
// action lost its force (the invitation revoked or expired), is not sent —
// outcome cancelled, the relay never called. A pending invitation is the
// control: it is sent, with the inviter's and the space's names.
func TestRunner_EmailWhoseActionLostItsForceIsNotSent(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	var wantNames []string // set by the control's seed

	cases := []struct {
		name string
		// seed queues the email and makes its action lose force (or not).
		seed     func(t *testing.T) uuid.UUID
		kind     mail.Kind
		wantSent bool
	}{
		{"addressee deleted the account", func(t *testing.T) uuid.UUID {
			userID, to := seedUser(t, pool)
			id := enqueue(t, pool, mail.Message{Kind: mail.KindPasswordReset, To: to, Lang: mail.LangEN, UserID: userID}, 24*time.Hour)
			if _, err := pool.Exec(ctx, `
UPDATE users SET deleted_at = now(), pseudonym = 'deleted-' || substr(md5(id::text), 1, 8)
 WHERE id = $1`, userID); err != nil {
				t.Fatal(err)
			}
			return id
		}, mail.KindPasswordReset, false},
		{"invitation revoked", func(t *testing.T) uuid.UUID {
			invID, to, _, _ := seedInvitation(t, pool)
			id := enqueue(t, pool, mail.Message{Kind: mail.KindSpaceInvite, To: to, Lang: mail.LangEN, InvitationID: invID}, 24*time.Hour)
			if _, err := pool.Exec(ctx, `UPDATE space_invitations SET status = 'revoked', resolved_at = now() WHERE id = $1`, invID); err != nil {
				t.Fatal(err)
			}
			return id
		}, mail.KindSpaceInvite, false},
		{"invitation expired", func(t *testing.T) uuid.UUID {
			invID, to, _, _ := seedInvitation(t, pool)
			id := enqueue(t, pool, mail.Message{Kind: mail.KindSpaceInvite, To: to, Lang: mail.LangEN, InvitationID: invID}, 24*time.Hour)
			if _, err := pool.Exec(ctx, `UPDATE space_invitations SET expires_at = now() - interval '1 minute' WHERE id = $1`, invID); err != nil {
				t.Fatal(err)
			}
			return id
		}, mail.KindSpaceInvite, false},
		{"control: invitation pending", func(t *testing.T) uuid.UUID {
			invID, to, inviter, space := seedInvitation(t, pool)
			wantNames = []string{inviter, space}
			return enqueue(t, pool, mail.Message{Kind: mail.KindSpaceInvite, To: to, Lang: mail.LangEN, InvitationID: invID}, 24*time.Hour)
		}, mail.KindSpaceInvite, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetQueue(t, pool)
			id := tc.seed(t)
			r := &relay{}
			run := newRunner(t, pool, r, newClock())
			before := deliveries(t)

			tick(t, run)

			e := readEmail(t, pool, id)
			if !tc.wantSent {
				if e.state != "final" || e.outcome != mail.OutcomeCancelled || r.Calls() != 0 {
					t.Fatalf("email = %s/%q, relay calls %d; want final/cancelled and no call", e.state, e.outcome, r.Calls())
				}
				assertOneDelivery(t, before, tc.kind, mail.OutcomeCancelled)
				return
			}
			if e.state != "final" || e.outcome != mail.OutcomeSuccess || len(r.Accepted()) != 1 {
				t.Fatalf("control: email = %s/%q, relay accepted %d; want final/success, 1", e.state, e.outcome, len(r.Accepted()))
			}
			assertOneDelivery(t, before, tc.kind, mail.OutcomeSuccess)
			text := textPart(t, r.Accepted()[0].msg)
			for _, n := range wantNames {
				if !strings.Contains(text, n) {
					t.Errorf("the invitation does not name %q:\n%s", n, text)
				}
			}
		})
	}
}

// The worker is stopped (its context cancelled) just
// after the relay accepted DATA. The success is still recorded before the
// worker's Run returns — the bookkeeping after an irreversible send does
// not die with the worker's context — and no new attempt starts: the rest
// of the batch goes back to the queue instead of waiting out a lease. Red
// if the delivered email stays leased (and would be sent again).
func TestRunner_StopAfterRelayAcceptedStillRecordsSuccess(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	resetQueue(t, pool)
	u1, to1 := seedUser(t, pool)
	u2, to2 := seedUser(t, pool)
	first := enqueue(t, pool, mail.Message{Kind: mail.KindPasswordReset, To: to1, Lang: mail.LangEN, UserID: u1}, 24*time.Hour)
	second := enqueue(t, pool, mail.Message{Kind: mail.KindPasswordReset, To: to2, Lang: mail.LangEN, UserID: u2}, 24*time.Hour)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	r := &relay{}
	r.onSend = stop // the worker is stopped while the relay takes the message
	c := newClock()
	run := newRunner(t, pool, r, c)
	before := deliveries(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		run.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // watchdog only; the assertions do not depend on it
		t.Fatal("the worker did not return after it was stopped")
	}

	if r.Calls() != 1 {
		t.Fatalf("relay calls = %d, want 1 — an attempt started after the worker was stopped", r.Calls())
	}
	if got := r.Accepted()[0].to; got != to1 {
		t.Fatalf("precondition: the relay took %s first, want the older email %s", got, to1)
	}
	if e := readEmail(t, pool, first); e.state != "final" || e.outcome != mail.OutcomeSuccess {
		t.Errorf("delivered email = %s/%q when the worker returned, want final/success", e.state, e.outcome)
	}
	if e := readEmail(t, pool, second); e.state != "pending" {
		t.Errorf("the email not yet attempted = %s/%q, want pending (back in the queue, not held by a dead lease)", e.state, e.outcome)
	}
	assertOneDelivery(t, before, mail.KindPasswordReset, mail.OutcomeSuccess)
}
