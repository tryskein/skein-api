package tokens_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/mail"
	"skein-api/internal/testsupport"
	"skein-api/internal/tokens"
)

// Matrix rows 61–65 of the test matrix: the K-11 protocol of
// internal/tokens against a real schema. The mail worker's part is played
// by the test: lease() is the worker's claim (state → leased, attempts — the
// lease generation — bumped), reclaim() is an expired lease handed back.

const deliveryTTL = 24 * time.Hour

// fixture is one user (subject of email_confirm / password_reset) and one
// pending invitation of a space (subject of space_invite).
type fixture struct {
	pool         *pgxpool.Pool
	userID       uuid.UUID
	userEmail    string
	invitationID uuid.UUID
	inviteEmail  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	ctx := context.Background()
	owner, _ := testsupport.SeedUser(t, pool, "")
	user, uname := testsupport.SeedUser(t, pool, "")
	f := &fixture{pool: pool, userID: user, userEmail: uname + "@test.local"}

	var spaceID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO spaces (short_id, kind, owner_id, name)
VALUES ($1, 'regular', $2, 'tokens test') RETURNING id`, shortID(t), owner).Scan(&spaceID); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	f.inviteEmail = "invitee-" + uuid.NewString()[:8] + "@test.local"
	if err := pool.QueryRow(ctx, `
INSERT INTO space_invitations (space_id, email, invited_by, lang, expires_at)
VALUES ($1, $2, $3, 'ru', now() + interval '7 days') RETURNING id`,
		spaceID, f.inviteEmail, owner).Scan(&f.invitationID); err != nil {
		t.Fatalf("seed invitation: %v", err)
	}
	// Registered after the users' own cleanups, so it runs before them.
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `UPDATE space_invitations SET current_token_id = NULL WHERE id = $1`, f.invitationID)
		_, _ = pool.Exec(c, `DELETE FROM auth_tokens WHERE user_id = ANY($1) OR invitation_id = $2`,
			[]uuid.UUID{owner, user}, f.invitationID)
		_, _ = pool.Exec(c, `DELETE FROM outgoing_emails WHERE user_id = ANY($1) OR invitation_id = $2`,
			[]uuid.UUID{owner, user}, f.invitationID)
		_, _ = pool.Exec(c, `DELETE FROM space_invitations WHERE id = $1`, f.invitationID)
		_, _ = pool.Exec(c, `DELETE FROM spaces WHERE id = $1`, spaceID)
	})
	return f
}

// shortID draws a valid spaces.short_id (8 characters of lowercase base32).
func shortID(t *testing.T) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// message is the email of purpose p for the fixture's subject.
func (f *fixture) message(p tokens.Purpose) mail.Message {
	if p == tokens.PurposeSpaceInvite {
		return mail.Message{Kind: p, To: f.inviteEmail, Lang: mail.LangRU, InvitationID: f.invitationID}
	}
	return mail.Message{Kind: p, To: f.userEmail, Lang: mail.LangRU, UserID: f.userID}
}

func (f *fixture) subject(p tokens.Purpose) tokens.Subject {
	if p == tokens.PurposeSpaceInvite {
		return tokens.InvitationSubject(f.invitationID)
	}
	return tokens.UserSubject(p, f.userID)
}

// addressee is the account email that may use a token of purpose p.
func (f *fixture) addressee(p tokens.Purpose) string {
	if p == tokens.PurposeSpaceInvite {
		return f.inviteEmail
	}
	return ""
}

func (f *fixture) inTx(t *testing.T, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return pgx.BeginTxFunc(context.Background(), f.pool, pgx.TxOptions{}, fn)
}

// enqueue queues the first email of purpose p and returns its id.
func (f *fixture) enqueue(t *testing.T, p tokens.Purpose) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.inTx(t, func(tx pgx.Tx) error {
		var err error
		id, err = mail.Enqueue(context.Background(), tx, f.message(p), deliveryTTL)
		return err
	}); err != nil {
		t.Fatalf("enqueue %s: %v", p, err)
	}
	return id
}

// replace asks for a new email of purpose p (tokens.Replace).
func (f *fixture) replace(t *testing.T, p tokens.Purpose) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.inTx(t, func(tx pgx.Tx) error {
		var err error
		id, err = tokens.Replace(context.Background(), tx, f.message(p), deliveryTTL)
		return err
	}); err != nil {
		t.Fatalf("replace %s: %v", p, err)
	}
	return id
}

func (f *fixture) cancel(t *testing.T, p tokens.Purpose) {
	t.Helper()
	if err := f.inTx(t, func(tx pgx.Tx) error {
		return tokens.Cancel(context.Background(), tx, f.subject(p))
	}); err != nil {
		t.Fatalf("cancel %s: %v", p, err)
	}
}

// use presents raw for purpose p in its own committed transaction.
func (f *fixture) use(t *testing.T, p tokens.Purpose, raw, addressee string) error {
	t.Helper()
	return f.inTx(t, func(tx pgx.Tx) error {
		_, err := tokens.Use(context.Background(), tx, p, raw, addressee)
		return err
	})
}

// lease is the worker's claim of a pending email: the generation it returns
// names this lease.
func (f *fixture) lease(t *testing.T, emailID uuid.UUID) int {
	t.Helper()
	var gen int
	if err := f.pool.QueryRow(context.Background(), `
UPDATE outgoing_emails
   SET state = 'leased', attempts = attempts + 1, leased_until = now() + interval '2 minutes'
 WHERE id = $1 AND state = 'pending'
RETURNING attempts`, emailID).Scan(&gen); err != nil {
		t.Fatalf("lease email %s: %v", emailID, err)
	}
	return gen
}

// reclaim hands an expired lease back to the queue (the worker that held it
// crashed between issue and finalisation).
func (f *fixture) reclaim(t *testing.T, emailID uuid.UUID) {
	t.Helper()
	tag, err := f.pool.Exec(context.Background(), `
UPDATE outgoing_emails SET state = 'pending', leased_until = NULL
 WHERE id = $1 AND state = 'leased'`, emailID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reclaim email %s: rows=%d err=%v", emailID, tag.RowsAffected(), err)
	}
}

func (f *fixture) issue(t *testing.T, emailID uuid.UUID, gen int) string {
	t.Helper()
	raw, err := tokens.IssueForEmail(context.Background(), f.pool, emailID, gen)
	if err != nil {
		t.Fatalf("IssueForEmail(%s, %d): %v", emailID, gen, err)
	}
	return raw
}

// tokenRowsOfEmail counts every token row ever issued to the email.
func (f *fixture) tokenRowsOfEmail(t *testing.T, emailID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_tokens WHERE email_id = $1`, emailID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// liveTokensOfEmail counts the live tokens of the email.
func (f *fixture) liveTokensOfEmail(t *testing.T, emailID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `
SELECT count(*) FROM auth_tokens
 WHERE email_id = $1 AND used_at IS NULL AND superseded_at IS NULL AND expires_at > now()`,
		emailID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// liveTokensOf counts the live tokens of the subject and purpose.
func (f *fixture) liveTokensOf(t *testing.T, p tokens.Purpose) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `
SELECT count(*) FROM auth_tokens
 WHERE purpose = $1 AND (user_id = $2 OR invitation_id = $3)
   AND used_at IS NULL AND superseded_at IS NULL AND expires_at > now()`,
		string(p), f.userID, f.invitationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) emailState(t *testing.T, emailID uuid.UUID) (state string, outcome *string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT state, outcome FROM outgoing_emails WHERE id = $1`, emailID).Scan(&state, &outcome); err != nil {
		t.Fatal(err)
	}
	return state, outcome
}

var allPurposes = []tokens.Purpose{tokens.PurposeEmailConfirm, tokens.PurposePasswordReset, tokens.PurposeSpaceInvite}

// Row 61: an email cancelled by Cancel or replaced by Replace of the same
// subject and purpose is never issued a token again, and keeps no live one.
func TestIssueForEmail_CancelledOrReplacedEmailGetsNoToken(t *testing.T) {
	for _, p := range allPurposes {
		for _, op := range []string{"cancel", "replace"} {
			t.Run(string(p)+"/"+op, func(t *testing.T) {
				f := newFixture(t)
				old := f.enqueue(t, p)
				gen := f.lease(t, old)
				f.issue(t, old, gen) // the old email had a live token before
				if got := f.liveTokensOfEmail(t, old); got != 1 {
					t.Fatalf("before %s: live tokens of the old email = %d, want 1", op, got)
				}
				if op == "cancel" {
					f.cancel(t, p)
				} else {
					f.replace(t, p)
				}
				rows := f.tokenRowsOfEmail(t, old)

				_, err := tokens.IssueForEmail(context.Background(), f.pool, old, gen)
				if !errors.Is(err, tokens.ErrNotIssuable) {
					t.Fatalf("IssueForEmail of the %sed email: err = %v, want ErrNotIssuable", op, err)
				}
				if got := f.liveTokensOfEmail(t, old); got != 0 {
					t.Errorf("live tokens of the %sed email = %d, want 0", op, got)
				}
				if got := f.tokenRowsOfEmail(t, old); got != rows {
					t.Errorf("refused issue wrote token rows: %d → %d", rows, got)
				}
				if st, oc := f.emailState(t, old); st != "final" || oc == nil || *oc != "cancelled" {
					t.Errorf("old email = %s/%v, want final/cancelled", st, oc)
				}
			})
		}
	}
}

// Row 62: an outdated lease generation is refused and touches no token of
// the current email; a retry after a crash between issue and finalisation
// issues a new token only while the action is still open.
func TestIssueForEmail_StaleGenerationAndRetryAfterCrash(t *testing.T) {
	for _, p := range allPurposes {
		t.Run(string(p)+"/stale generation of the same email", func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(t, p)
			gen1 := f.lease(t, e)
			f.issue(t, e, gen1)
			f.reclaim(t, e)
			gen2 := f.lease(t, e)
			current := f.issue(t, e, gen2)

			if _, err := tokens.IssueForEmail(context.Background(), f.pool, e, gen1); !errors.Is(err, tokens.ErrNotIssuable) {
				t.Fatalf("IssueForEmail with outdated generation: err = %v, want ErrNotIssuable", err)
			}
			if got := f.liveTokensOfEmail(t, e); got != 1 {
				t.Fatalf("live tokens of the email = %d, want 1 (the current lease's)", got)
			}
			if err := f.use(t, p, current, f.addressee(p)); err != nil {
				t.Errorf("the current lease's token stopped working: %v", err)
			}
		})

		t.Run(string(p)+"/stale worker of a replaced email", func(t *testing.T) {
			f := newFixture(t)
			old := f.enqueue(t, p)
			oldGen := f.lease(t, old)
			f.issue(t, old, oldGen)
			cur := f.replace(t, p)
			curGen := f.lease(t, cur)
			current := f.issue(t, cur, curGen)

			if _, err := tokens.IssueForEmail(context.Background(), f.pool, old, oldGen); !errors.Is(err, tokens.ErrNotIssuable) {
				t.Fatalf("IssueForEmail of the replaced email: err = %v, want ErrNotIssuable", err)
			}
			if got := f.liveTokensOfEmail(t, cur); got != 1 {
				t.Fatalf("live tokens of the current email = %d, want 1", got)
			}
			if err := f.use(t, p, current, f.addressee(p)); err != nil {
				t.Errorf("the current email's token was retired by a stale worker: %v", err)
			}
		})

		t.Run(string(p)+"/retry after crash, action open", func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(t, p)
			first := f.issue(t, e, f.lease(t, e))
			f.reclaim(t, e) // crashed after issue, before finalisation
			second := f.issue(t, e, f.lease(t, e))
			if first == second {
				t.Fatal("retry returned the same raw token")
			}
			if got := f.liveTokensOfEmail(t, e); got != 1 {
				t.Fatalf("live tokens after the retry = %d, want 1 (the newest)", got)
			}
			if err := f.use(t, p, first, f.addressee(p)); !errors.Is(err, tokens.ErrInvalid) {
				t.Errorf("the first delivery's token: err = %v, want ErrInvalid", err)
			}
			if err := f.use(t, p, second, f.addressee(p)); err != nil {
				t.Errorf("the retry's token: %v", err)
			}
		})

		t.Run(string(p)+"/retry after crash, action completed", func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(t, p)
			raw := f.issue(t, e, f.lease(t, e))
			f.reclaim(t, e)
			if err := f.use(t, p, raw, f.addressee(p)); err != nil { // the recipient completed the action
				t.Fatalf("use: %v", err)
			}
			gen := f.leaseIfPending(t, e)
			if _, err := tokens.IssueForEmail(context.Background(), f.pool, e, gen); !errors.Is(err, tokens.ErrNotIssuable) {
				t.Fatalf("IssueForEmail after the action completed: err = %v, want ErrNotIssuable", err)
			}
			if got := f.liveTokensOf(t, p); got != 0 {
				t.Errorf("live tokens of the completed action = %d, want 0", got)
			}
		})
	}
}

// leaseIfPending leases the email if the queue still offers it, else
// returns its current generation (a worker racing with the cancellation).
func (f *fixture) leaseIfPending(t *testing.T, emailID uuid.UUID) int {
	t.Helper()
	var gen int
	err := f.pool.QueryRow(context.Background(), `
UPDATE outgoing_emails
   SET state = 'leased', attempts = attempts + 1, leased_until = now() + interval '2 minutes'
 WHERE id = $1 AND state = 'pending'
RETURNING attempts`, emailID).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := f.pool.QueryRow(context.Background(),
			`SELECT attempts FROM outgoing_emails WHERE id = $1`, emailID).Scan(&gen); err != nil {
			t.Fatal(err)
		}
		return gen
	}
	if err != nil {
		t.Fatal(err)
	}
	return gen
}

// completeAction performs the action a token of purpose p authorises, in
// the transaction that uses the token — as the handlers will.
func (f *fixture) completeAction(t *testing.T, p tokens.Purpose, raw string) error {
	t.Helper()
	return f.inTx(t, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tokens.Use(ctx, tx, p, raw, f.addressee(p)); err != nil {
			return err
		}
		var err error
		switch p {
		case tokens.PurposeEmailConfirm:
			_, err = tx.Exec(ctx, `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, f.userID)
		case tokens.PurposeSpaceInvite:
			_, err = tx.Exec(ctx, `
UPDATE space_invitations SET status = 'accepted', resolved_at = now(), updated_at = now()
 WHERE id = $1`, f.invitationID)
		}
		return err
	})
}

// Row 63: a completed action gets no new live token, neither by a retry of
// its email nor by Replace; used, superseded and expired tokens fail Use.
func TestCompletedActionGetsNoNewToken(t *testing.T) {
	for _, p := range allPurposes {
		t.Run(string(p), func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(t, p)
			gen := f.lease(t, e)
			raw := f.issue(t, e, gen)
			if err := f.completeAction(t, p, raw); err != nil {
				t.Fatalf("complete the action: %v", err)
			}

			// A retry of the delivered email.
			if _, err := tokens.IssueForEmail(context.Background(), f.pool, e, gen); !errors.Is(err, tokens.ErrNotIssuable) {
				t.Errorf("retry of the email: err = %v, want ErrNotIssuable", err)
			}
			// Replace: a confirmed email or an accepted invitation is closed
			// for good; a password reset can be asked for again, but that is
			// a new action — the used token stays used.
			err := f.inTx(t, func(tx pgx.Tx) error {
				_, err := tokens.Replace(context.Background(), tx, f.message(p), deliveryTTL)
				return err
			})
			if p == tokens.PurposePasswordReset {
				if err != nil {
					t.Fatalf("a new reset request after a completed reset: %v", err)
				}
			} else if !errors.Is(err, tokens.ErrCompleted) {
				t.Errorf("Replace of a completed %s: err = %v, want ErrCompleted", p, err)
			}
			if got := f.liveTokensOf(t, p); got != 0 {
				t.Errorf("live tokens after the completed action = %d, want 0", got)
			}
			if err := f.use(t, p, raw, f.addressee(p)); !errors.Is(err, tokens.ErrInvalid) {
				t.Errorf("used token presented again: err = %v, want ErrInvalid", err)
			}
		})
	}

	for _, p := range allPurposes {
		t.Run(string(p)+"/superseded and expired tokens", func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(t, p)
			superseded := f.issue(t, e, f.lease(t, e))
			f.reclaim(t, e)
			expiring := f.issue(t, e, f.lease(t, e)) // supersedes the first
			if err := f.use(t, p, superseded, f.addressee(p)); !errors.Is(err, tokens.ErrInvalid) {
				t.Errorf("superseded token: err = %v, want ErrInvalid", err)
			}
			if _, err := f.pool.Exec(context.Background(),
				`UPDATE auth_tokens SET expires_at = now() - interval '1 second' WHERE email_id = $1`, e); err != nil {
				t.Fatal(err)
			}
			if err := f.use(t, p, expiring, f.addressee(p)); !errors.Is(err, tokens.ErrInvalid) {
				t.Errorf("expired token: err = %v, want ErrInvalid", err)
			}
		})
	}
}

// Row 63a: Use is bound to the purpose and to the addressee, and one token
// is used at most once even by concurrent callers.
func TestUse_BoundToPurposeAndAddressee(t *testing.T) {
	t.Run("purpose", func(t *testing.T) {
		f := newFixture(t)
		reset := f.enqueue(t, tokens.PurposePasswordReset)
		resetRaw := f.issue(t, reset, f.lease(t, reset))
		confirm := f.enqueue(t, tokens.PurposeEmailConfirm)
		confirmRaw := f.issue(t, confirm, f.lease(t, confirm))

		if err := f.use(t, tokens.PurposeEmailConfirm, resetRaw, ""); !errors.Is(err, tokens.ErrInvalid) {
			t.Errorf("reset token as a confirmation: err = %v, want ErrInvalid", err)
		}
		if err := f.use(t, tokens.PurposePasswordReset, confirmRaw, ""); !errors.Is(err, tokens.ErrInvalid) {
			t.Errorf("confirmation token as a reset: err = %v, want ErrInvalid", err)
		}
		// The refusals consumed nothing: each token works for its purpose.
		if err := f.use(t, tokens.PurposePasswordReset, resetRaw, ""); err != nil {
			t.Errorf("reset token for a reset: %v", err)
		}
		if err := f.use(t, tokens.PurposeEmailConfirm, confirmRaw, ""); err != nil {
			t.Errorf("confirmation token for a confirmation: %v", err)
		}
	})

	t.Run("addressee", func(t *testing.T) {
		f := newFixture(t)
		e := f.enqueue(t, tokens.PurposeSpaceInvite)
		raw := f.issue(t, e, f.lease(t, e))
		if err := f.use(t, tokens.PurposeSpaceInvite, raw, "someone-else@test.local"); !errors.Is(err, tokens.ErrNotAddressee) {
			t.Fatalf("invitation token by another account: err = %v, want ErrNotAddressee", err)
		}
		if err := f.use(t, tokens.PurposeSpaceInvite, raw, strings.ToUpper(f.inviteEmail)); err != nil {
			t.Errorf("invitation token by its addressee (case-insensitive): %v", err)
		}
	})

	t.Run("concurrent use", func(t *testing.T) {
		for _, p := range allPurposes {
			f := newFixture(t)
			e := f.enqueue(t, p)
			raw := f.issue(t, e, f.lease(t, e))
			const callers = 4
			var (
				wg    sync.WaitGroup
				start = make(chan struct{})
				errs  = make([]error, callers)
			)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					errs[i] = pgx.BeginTxFunc(context.Background(), f.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
						_, err := tokens.Use(context.Background(), tx, p, raw, f.addressee(p))
						return err
					})
				}(i)
			}
			close(start)
			wg.Wait()
			ok := 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case !errors.Is(err, tokens.ErrInvalid):
					t.Errorf("%s: concurrent Use: unexpected error %v", p, err)
				}
			}
			if ok != 1 {
				t.Errorf("%s: %d of %d concurrent Use calls succeeded, want exactly 1", p, ok, callers)
			}
		}
	})
}

// heldOp starts op in a transaction and keeps it open; release commits it.
type heldOp struct {
	tx  pgx.Tx
	pid int32
}

func hold(t *testing.T, pool *pgxpool.Pool, op func(tx pgx.Tx) error) *heldOp {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &heldOp{tx: tx}
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	if err := op(tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("held operation: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return h
}

func (h *heldOp) release(t *testing.T) {
	t.Helper()
	if err := h.tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit held operation: %v", err)
	}
}

// waitsOn reports, polling pg_stat_activity for up to a few seconds,
// whether some backend is blocked by the held transaction.
func waitsOn(t *testing.T, pool *pgxpool.Pool, h *heldOp, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
 WHERE wait_event_type = 'Lock' AND $1::int = ANY(pg_blocking_pids(pid))`, h.pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

type issueResult struct {
	raw string
	err error
}

func issueAsync(pool *pgxpool.Pool, emailID uuid.UUID, gen int) <-chan issueResult {
	ch := make(chan issueResult, 1)
	go func() {
		raw, err := tokens.IssueForEmail(context.Background(), pool, emailID, gen)
		ch <- issueResult{raw, err}
	}()
	return ch
}

func await(t *testing.T, ch <-chan issueResult) issueResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("IssueForEmail did not return")
		return issueResult{}
	}
}

// Row 64: the subject's row is the point of serialisation. IssueForEmail
// started while Replace / Cancel / Use of the same subject and purpose holds
// it waits, and after their commit yields the protocol's outcome; another
// subject does not wait; another purpose of the same user waits for the
// users row and afterwards keeps its token and its result.
func TestProtocol_SerialisesOnTheSubjectRow(t *testing.T) {
	ctx := context.Background()
	for _, p := range allPurposes {
		for _, op := range []string{"replace", "cancel", "use"} {
			t.Run(string(p)+"/"+op, func(t *testing.T) {
				f := newFixture(t)
				e := f.enqueue(t, p)
				gen := f.lease(t, e)
				raw := f.issue(t, e, gen)
				f.reclaim(t, e)
				gen = f.lease(t, e) // the worker's next attempt is about to issue

				var newEmail uuid.UUID
				h := hold(t, f.pool, func(tx pgx.Tx) error {
					var err error
					switch op {
					case "replace":
						newEmail, err = tokens.Replace(ctx, tx, f.message(p), deliveryTTL)
					case "cancel":
						err = tokens.Cancel(ctx, tx, f.subject(p))
					case "use":
						_, err = tokens.Use(ctx, tx, p, raw, f.addressee(p))
					}
					return err
				})
				res := issueAsync(f.pool, e, gen)
				if !waitsOn(t, f.pool, h, 5*time.Second) {
					t.Fatalf("IssueForEmail did not wait for the held %s", op)
				}
				h.release(t)
				r := await(t, res)
				if !errors.Is(r.err, tokens.ErrNotIssuable) {
					t.Fatalf("IssueForEmail after %s: err = %v, want ErrNotIssuable", op, r.err)
				}
				if got := f.liveTokensOf(t, p); got != 0 {
					t.Errorf("live tokens after %s and the refused issue = %d, want 0", op, got)
				}
				if op == "replace" {
					// The new email is the current one and gets its token.
					cur := f.issue(t, newEmail, f.lease(t, newEmail))
					if err := f.use(t, p, cur, f.addressee(p)); err != nil {
						t.Errorf("the replacing email's token: %v", err)
					}
				}
			})
		}
	}

	t.Run("another subject does not wait", func(t *testing.T) {
		for _, p := range allPurposes {
			a, b := newFixture(t), newFixture(t)
			eb := b.enqueue(t, p)
			gen := b.lease(t, eb)
			h := hold(t, a.pool, func(tx pgx.Tx) error { return tokens.Cancel(ctx, tx, a.subject(p)) })
			r := await(t, issueAsync(b.pool, eb, gen))
			if r.err != nil {
				t.Fatalf("%s: issue for another subject while one is held: %v", p, r.err)
			}
			h.release(t)
		}
	})

	t.Run("another purpose of the same user waits and keeps its token", func(t *testing.T) {
		f := newFixture(t)
		reset := f.enqueue(t, tokens.PurposePasswordReset)
		resetGen := f.lease(t, reset)
		confirm := f.enqueue(t, tokens.PurposeEmailConfirm)
		confirmRaw := f.issue(t, confirm, f.lease(t, confirm))

		h := hold(t, f.pool, func(tx pgx.Tx) error {
			return tokens.Cancel(ctx, tx, tokens.UserSubject(tokens.PurposeEmailConfirm, f.userID))
		})
		res := issueAsync(f.pool, reset, resetGen)
		if !waitsOn(t, f.pool, h, 5*time.Second) {
			t.Fatal("the reset issue did not wait for the users row held by the confirmation's Cancel")
		}
		h.release(t)
		r := await(t, res)
		if r.err != nil {
			t.Fatalf("reset issue after the confirmation's Cancel: %v", r.err)
		}
		if err := f.use(t, tokens.PurposePasswordReset, r.raw, ""); err != nil {
			t.Errorf("the reset token issued after the wait: %v", err)
		}
		if err := f.use(t, tokens.PurposeEmailConfirm, confirmRaw, ""); !errors.Is(err, tokens.ErrInvalid) {
			t.Errorf("the cancelled confirmation token: err = %v, want ErrInvalid", err)
		}

		// And the other way round: a reset's Cancel leaves the
		// confirmation's live token alone.
		g := newFixture(t)
		c := g.enqueue(t, tokens.PurposeEmailConfirm)
		cRaw := g.issue(t, c, g.lease(t, c))
		g.enqueue(t, tokens.PurposePasswordReset)
		g.cancel(t, tokens.PurposePasswordReset)
		if err := g.use(t, tokens.PurposeEmailConfirm, cRaw, ""); err != nil {
			t.Errorf("a reset's Cancel retired the confirmation token: %v", err)
		}
	})
}

// Row 65: the raw token is stored nowhere — not in any column of
// auth_tokens, outgoing_emails or space_invitations.
func TestRawTokenIsNotStored(t *testing.T) {
	for _, p := range allPurposes {
		t.Run(string(p), func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(t, p)
			raw := f.issue(t, e, f.lease(t, e))
			if len(raw) < 32 {
				t.Fatalf("raw token %q is implausibly short", raw)
			}
			needles := []string{raw, hex.EncodeToString([]byte(raw))}
			for _, table := range []string{"auth_tokens", "outgoing_emails", "space_invitations"} {
				for _, needle := range needles {
					var n int
					if err := f.pool.QueryRow(context.Background(),
						`SELECT count(*) FROM `+table+` r WHERE strpos(r::text, $1) > 0`, needle).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Errorf("%s: %d rows contain the raw token", table, n)
					}
				}
			}
			// The positive control: the stored hash is found.
			var n int
			if err := f.pool.QueryRow(context.Background(), `
SELECT count(*) FROM auth_tokens WHERE token_hash = sha256(convert_to($1, 'UTF8'))`, raw).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("tokens with the raw token's hash = %d, want 1", n)
			}
		})
	}
}

// a blocked (deactivated) account is closed
// for its own tokens like a deleted one — US-9: a reset letter goes only to
// an account that can sign in; US-11: blocking forbids sign-in. A queued
// email is not issued a token, a live token does not pass, a new request
// writes nothing.
func TestDeactivatedSubjectIsClosed(t *testing.T) {
	for _, p := range []tokens.Purpose{tokens.PurposeEmailConfirm, tokens.PurposePasswordReset} {
		t.Run(string(p), func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(t, p)
			gen := f.lease(t, e)
			raw := f.issue(t, e, gen)
			if _, err := f.pool.Exec(context.Background(), `UPDATE users SET deactivated_at = now() WHERE id = $1`, f.userID); err != nil {
				t.Fatal(err)
			}
			if err := f.use(t, p, raw, ""); !errors.Is(err, tokens.ErrInvalid) {
				t.Errorf("a live token of a deactivated account: err = %v, want ErrInvalid", err)
			}
			if _, err := tokens.IssueForEmail(context.Background(), f.pool, e, gen); !errors.Is(err, tokens.ErrNotIssuable) {
				t.Errorf("a retry of the email to a deactivated account: err = %v, want ErrNotIssuable", err)
			}
			err := f.inTx(t, func(tx pgx.Tx) error {
				_, err := tokens.Replace(context.Background(), tx, f.message(p), deliveryTTL)
				return err
			})
			if !errors.Is(err, tokens.ErrSubjectNotFound) {
				t.Errorf("a new %s for a deactivated account: err = %v, want ErrSubjectNotFound (no letter)", p, err)
			}
		})
	}
}

// the contract of
// POST /api/v1/auth/confirm-email/{token} (accounts/contract.go) — «Confirming
// an already confirmed email with a valid token is a success too». The email
// may be confirmed another way (opening an invitation link, US-5) while the
// confirmation letter is in flight; its live token then still succeeds, and
// is consumed.
func TestUse_LiveConfirmationOfAConfirmedEmailSucceeds(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(t, tokens.PurposeEmailConfirm)
	raw := f.issue(t, e, f.lease(t, e))
	if _, err := f.pool.Exec(context.Background(), `UPDATE users SET email_confirmed_at = now() WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	if err := f.use(t, tokens.PurposeEmailConfirm, raw, ""); err != nil {
		t.Fatalf("a live confirmation token of an already confirmed email: %v, want success", err)
	}
	if got := f.liveTokensOf(t, tokens.PurposeEmailConfirm); got != 0 {
		t.Errorf("live confirmation tokens after the use = %d, want 0", got)
	}
	if err := f.use(t, tokens.PurposeEmailConfirm, raw, ""); !errors.Is(err, tokens.ErrInvalid) {
		t.Errorf("the same token again: err = %v, want ErrInvalid", err)
	}
}
