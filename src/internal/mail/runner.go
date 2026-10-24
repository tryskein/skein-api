package mail

// runner.go — the mail worker (sk-674.18, T022, K-9, D-5, INV-25, E-12).
//
// It drains outgoing_emails the way outbox/push.go#PushRunner drains web
// push, and differs from it where mail promises something else:
//
//   - a LEASE per email: claim with FOR UPDATE SKIP LOCKED, the attempt
//     counter bumped at claim time is the lease generation, and every
//     later write — the lease renewal, the retry, the final outcome — is
//     conditional on (id, state='leased', attempts=generation). A worker
//     whose lease ran out and was taken over writes nothing.
//   - the link token is issued at the START of every attempt by the token
//     protocol (TokenIssuer, implemented by tokens.EmailIssuer — K-11): it
//     issues only while the email is this lease's and its action is still
//     open, and supersedes only this email's previous token. A refusal
//     finalises the email as cancelled (INV-25). The issue commits on its
//     own; no transaction is held while SMTP talks.
//   - the raw token lives in memory for one attempt: it is rendered into
//     the message and dropped. Nothing rendered is stored.
//   - TEMPORARY failures (see smtp.go#Classify) are retried with back-off
//     (30 s doubling, capped at 10 min) with NO attempt budget: an email
//     waits for the relay until its deadline_at, and the deadline sweep
//     then finalises it as expired. PERMANENT failures are rejected at once.
//
// # Time
//
// Every time the queue is compared with or stamped by — lease expiry,
// next attempt, deadline, finalisation — is the worker's clock
// (Options.Now, time.Now in production), passed to SQL as a parameter;
// the queue never reads the database's now(). So a test drives the whole
// queue with a controlled clock (testing/synctest or a fake Now), and the
// replicas of one deployment agree on it as well as their clocks agree —
// seconds against a lease of minutes. deadline_at itself is stamped by
// the database at enqueue (mail.Enqueue) and is a point in time like any
// other. The tokens' own expiry and the invitation's are the database's
// business (internal/tokens).
//
// # Delivery is at least once
//
// A worker that dies between the relay's acceptance and the final UPDATE
// leaves a lease that expires; the next attempt issues a new token —
// only while the action is still open — and sends again. The first link
// then stops working and the last one counts (plan Known residuals).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Outcomes of an email (outgoing_emails.outcome).
const (
	OutcomeSuccess   = "success"
	OutcomeRejected  = "rejected"
	OutcomeExpired   = "expired"
	OutcomeCancelled = "cancelled"
)

// Worker tuning.
const (
	defaultPollInterval = 5 * time.Second
	defaultBatchSize    = 20
	// DefaultLeaseTTL — K-9: an email is leased for two minutes. The SMTP
	// session is bounded well inside it (smtp.go: 15 s dial, 60 s session).
	DefaultLeaseTTL = 2 * time.Minute
	// leaseMargin is lease the work of one attempt never eats into: the
	// attempt's context ends this long before the lease does, so the
	// bookkeeping write still lands on a lease that is ours.
	leaseMargin = 15 * time.Second
	// retryBaseDelay / retryMaxDelay — K-9 back-off: 30 s doubling per
	// attempt, capped at 10 minutes.
	retryBaseDelay = 30 * time.Second
	retryMaxDelay  = 10 * time.Minute
	// maxTickBackoff caps the wait after failed passes.
	maxTickBackoff = time.Minute
	// bookkeepingTimeout bounds one write of an attempt's outcome. The write
	// is detached from the worker's cancellation: once the
	// relay has taken a message, a graceful stop must still record it. At
	// most two such writes follow a stop (the outcome, then the release of
	// the rest of the batch), well inside the teardown's wait for the workers
	// (cmd/api runnerShutdownTimeout, 10 s).
	bookkeepingTimeout = 3 * time.Second
)

// Link paths of the web UI (skein-frontend App.tsx), joined to the public
// base SKEIN_APP_BASE_URL; the raw token is the last path segment.
var linkPaths = map[Kind]string{
	KindEmailConfirm:  "/confirm-email/",
	KindPasswordReset: "/reset-password/",
	KindSpaceInvite:   "/invite/",
}

// ErrNotIssuable: the token protocol refused to issue a token for the
// email — it is not leased under this generation, was cancelled or
// finished, or its action is no longer open. Nothing was written; the
// worker finalises the email as cancelled. tokens.ErrNotIssuable is this
// same value.
var ErrNotIssuable = errors.New("tokens: email may not be issued a token")

// Issued is a link token issued for one attempt.
type Issued struct {
	// Token is the raw token — in memory only.
	Token string
	// ExpiresAt is when the token stops working.
	ExpiresAt time.Time
}

// TokenIssuer is the token protocol's IssueForEmail as the worker sees it
// (tokens.EmailIssuer; the tokens package imports this one, so the worker
// reaches it through this interface).
type TokenIssuer interface {
	IssueForEmail(ctx context.Context, emailID uuid.UUID, generation int) (Issued, error)
}

// Options configures the worker. Zero values take the defaults above.
type Options struct {
	PollInterval time.Duration
	BatchSize    int
	LeaseTTL     time.Duration
	// Now is the queue's clock (see the file header); nil is time.Now.
	Now func() time.Time
}

// Runner is the mail worker.
type Runner struct {
	pool    *pgxpool.Pool
	sender  Sender
	issuer  TokenIssuer
	baseURL string
	opts    Options
}

// NewRunner builds the worker. appBaseURL is SKEIN_APP_BASE_URL — the
// public address of the web UI the links point into, validated by
// config.Load when mail is on.
func NewRunner(pool *pgxpool.Pool, sender Sender, issuer TokenIssuer, appBaseURL string, opts Options) (*Runner, error) {
	if pool == nil || sender == nil || issuer == nil {
		return nil, errors.New("mail: runner needs a pool, a sender and a token issuer")
	}
	base := strings.TrimRight(strings.TrimSpace(appBaseURL), "/")
	if u, err := url.Parse(base); err != nil || !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("mail: the link base %q is not an absolute URL", appBaseURL)
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = DefaultLeaseTTL
	}
	if opts.LeaseTTL <= leaseMargin {
		return nil, fmt.Errorf("mail: lease %s must exceed the margin %s", opts.LeaseTTL, leaseMargin)
	}
	return &Runner{pool: pool, sender: sender, issuer: issuer, baseURL: base, opts: opts}, nil
}

func (r *Runner) now() time.Time {
	if r.opts.Now != nil {
		return r.opts.Now()
	}
	return time.Now()
}

// Run blocks until ctx is cancelled: tick, drain on a full batch, back off
// after a failed pass.
func (r *Runner) Run(ctx context.Context) {
	slog.InfoContext(ctx, "mail runner started",
		"pollIntervalMs", r.opts.PollInterval.Milliseconds(),
		"batchSize", r.opts.BatchSize,
		"leaseTtlMs", r.opts.LeaseTTL.Milliseconds())
	defer slog.InfoContext(ctx, "mail runner stopped")

	backoff := r.opts.PollInterval
	for ctx.Err() == nil {
		n, err := r.Tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.ErrorContext(ctx, "mail runner tick failed",
				"backoffMs", backoff.Milliseconds(), "error", err.Error())
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxTickBackoff)
			continue
		}
		backoff = r.opts.PollInterval
		if n >= r.opts.BatchSize {
			continue
		}
		if !sleepCtx(ctx, r.opts.PollInterval) {
			return
		}
	}
}

// Tick runs one pass — return expired leases, retire emails past their
// deadline, claim a batch and attempt each — and returns how many emails
// it claimed. A failed pass counts in skein_email_runner_errors_total.
// Exported so tests drive the worker one pass at a time.
func (r *Runner) Tick(ctx context.Context) (int, error) {
	n, err := r.tick(ctx)
	if err != nil && ctx.Err() == nil {
		runnerErrorsTotal.Inc()
	}
	return n, err
}

func (r *Runner) tick(ctx context.Context) (int, error) {
	now := r.now()
	if err := reclaimExpiredLeases(ctx, r.pool, now); err != nil {
		return 0, err
	}
	if err := retireExpired(ctx, r.pool, now); err != nil {
		return 0, err
	}
	claims, err := r.claim(ctx, now)
	if err != nil {
		return 0, err
	}
	for i, c := range claims {
		// A stopped worker starts no new attempt: the rest of the batch
		// goes back to the queue at once instead of waiting out a lease.
		if ctx.Err() != nil {
			r.release(ctx, claims[i:])
			break
		}
		r.attempt(ctx, c)
	}
	return len(claims), nil
}

// bookkeeping is the context of a write that records what an attempt did:
// detached from the worker's cancellation, bounded by bookkeepingTimeout.
func bookkeeping(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
}

// release returns claimed emails that were never attempted to the queue,
// ready at once — each only while its claim still owns it.
func (r *Runner) release(ctx context.Context, claims []claim) {
	ids := make([]uuid.UUID, len(claims))
	gens := make([]int32, len(claims))
	for i, c := range claims {
		ids[i], gens[i] = c.id, int32(c.generation)
	}
	bctx, cancel := bookkeeping(ctx)
	defer cancel()
	tag, err := r.pool.Exec(bctx, `
UPDATE outgoing_emails e
   SET state = 'pending', leased_until = NULL, next_attempt_at = $3
  FROM unnest($1::uuid[], $2::int[]) AS c(id, generation)
 WHERE e.id = c.id AND e.state = 'leased' AND e.attempts = c.generation`, ids, gens, r.now())
	if err != nil {
		slog.WarnContext(bctx, "returning unattempted emails to the queue failed; their leases will expire",
			"count", len(claims), "error", err.Error())
		return
	}
	slog.InfoContext(bctx, "mail runner stopped; unattempted emails returned to the queue", "count", tag.RowsAffected())
}

// reclaimExpiredLeases returns to the queue the emails whose lease ran out
// — a worker that died, stalled or was stopped. Only leases already past
// leased_until are touched: a running lease belongs to its holder, whose
// writes stay conditional on the generation; the generation itself stays
// where it is and the next claim moves it on, locking the old holder out.
// Shared by the worker and the cleanup sweep: with mail
// switched off nothing else would ever return a leftover lease, and the
// email would keep its recipient's address without an outcome forever.
func reclaimExpiredLeases(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	tag, err := pool.Exec(ctx, `
UPDATE outgoing_emails
   SET state = 'pending', leased_until = NULL, next_attempt_at = $1
 WHERE state = 'leased' AND leased_until < $1`, now)
	if err != nil {
		return fmt.Errorf("reclaiming expired email leases: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		slog.WarnContext(ctx, "mail runner reclaimed expired leases", "count", n)
	}
	return nil
}

// retireExpired finalises as expired every pending email whose deadline
// has passed (E-12): the email waited for the relay as long as it was
// promised. Leased emails are left to their lease — the attempt in flight
// may still succeed, and a lease that runs out returns them here. Shared
// by the worker and the cleanup sweep, so mail switched off still ends
// every queued email with an outcome.
func retireExpired(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	rows, err := pool.Query(ctx, `
UPDATE outgoing_emails
   SET state = 'final', outcome = 'expired', finalized_at = $1,
       next_attempt_at = NULL, leased_until = NULL
 WHERE state = 'pending' AND deadline_at <= $1
RETURNING kind`, now)
	if err != nil {
		return fmt.Errorf("retiring emails past their deadline: %w", err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("retiring emails past their deadline: %w", err)
	}
	for _, k := range kinds {
		deliveriesTotal.WithLabelValues(k, OutcomeExpired).Inc()
	}
	if len(kinds) > 0 {
		slog.WarnContext(ctx, "emails expired undelivered past their deadline", "count", len(kinds))
	}
	return nil
}

// claim is one leased email: the pair (id, generation) names the lease.
type claim struct {
	id           uuid.UUID
	kind         Kind
	recipient    string
	lang         string
	invitationID *uuid.UUID
	generation   int
}

// claimSQL leases a batch. The attempt counter moves at claim, so it is
// the lease generation: any later claim invalidates every earlier one.
// An email past its deadline is not claimed (the sweep just before has
// retired it; the predicate covers the instant between).
const claimSQL = `
WITH ready AS (
    SELECT id
      FROM outgoing_emails
     WHERE state = 'pending'
       AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
       AND deadline_at > $1
     ORDER BY next_attempt_at NULLS FIRST, created_at
     FOR UPDATE SKIP LOCKED
     LIMIT $2
)
UPDATE outgoing_emails e
   SET state = 'leased',
       leased_until = $1::timestamptz + ($3::bigint * interval '1 microsecond'),
       attempts = e.attempts + 1
  FROM ready
 WHERE e.id = ready.id
RETURNING e.id, e.kind, e.recipient, e.lang, e.invitation_id, e.attempts`

func (r *Runner) claim(ctx context.Context, now time.Time) ([]claim, error) {
	rows, err := r.pool.Query(ctx, claimSQL, now, r.opts.BatchSize, r.opts.LeaseTTL.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("claiming emails: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (claim, error) {
		var c claim
		var kind string
		err := row.Scan(&c.id, &kind, &c.recipient, &c.lang, &c.invitationID, &c.generation)
		c.kind = Kind(kind)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("claiming emails: %w", err)
	}
	return out, nil
}

// holdLease re-stamps the lease of c now that its turn in the batch has
// come, and reports whether c still owns the email. A batch is leased at
// once; the tail of a long batch may find its lease spent and the email
// taken over — then nothing is done with it.
func (r *Runner) holdLease(ctx context.Context, c claim) bool {
	tag, err := r.pool.Exec(ctx, `
UPDATE outgoing_emails
   SET leased_until = $3::timestamptz + ($4::bigint * interval '1 microsecond')
 WHERE id = $1 AND state = 'leased' AND attempts = $2`,
		c.id, c.generation, r.now(), r.opts.LeaseTTL.Microseconds())
	if err != nil {
		slog.WarnContext(ctx, "email attempt deferred: lease renewal failed",
			"emailId", c.id, "attempt", c.generation, "error", err.Error())
		return false
	}
	if tag.RowsAffected() == 0 {
		slog.WarnContext(ctx, "email attempt abandoned: the lease is no longer ours",
			"emailId", c.id, "kind", string(c.kind), "attempt", c.generation)
		return false
	}
	return true
}

// attempt takes one leased email to a final outcome or back to the queue.
//
// The attempt's own work — issuing the token, reading the names, talking to
// the relay — runs under a context that ends leaseMargin before the lease
// does, and with the worker. The bookkeeping that follows (finalise, retry)
// runs under its own bounded context detached from the worker's
// cancellation (bookkeeping) and is conditional on the lease generation:
// the outcome of work done is still recorded when the attempt's window has
// just closed or the worker is being stopped — a message the relay took is
// recorded as delivered, not sent again — and never when the email has been
// taken over.
func (r *Runner) attempt(ctx context.Context, c claim) {
	if !r.holdLease(ctx, c) {
		return
	}
	work, cancel := context.WithTimeout(ctx, r.opts.LeaseTTL-leaseMargin)
	defer cancel()

	issued, err := r.issuer.IssueForEmail(work, c.id, c.generation)
	if errors.Is(err, ErrNotIssuable) {
		r.finalise(ctx, c, OutcomeCancelled)
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "email attempt failed: issuing the link token",
			"emailId", c.id, "kind", string(c.kind), "attempt", c.generation, "error", err.Error())
		r.retry(ctx, c)
		return
	}

	data, ok, err := r.content(work, c, issued)
	if err != nil {
		slog.WarnContext(ctx, "email attempt failed: reading the email's content",
			"emailId", c.id, "kind", string(c.kind), "attempt", c.generation, "error", err.Error())
		r.retry(ctx, c)
		return
	}
	if !ok {
		// The invitation or its space vanished after the token was issued:
		// the action has lost its force.
		r.finalise(ctx, c, OutcomeCancelled)
		return
	}
	rendered, err := Render(c.kind, c.lang, data)
	if err != nil {
		// A defect of the templates, not of the email: retried, so a fixed
		// release still delivers it before the deadline.
		slog.ErrorContext(ctx, "email attempt failed: rendering",
			"emailId", c.id, "kind", string(c.kind), "lang", c.lang, "attempt", c.generation, "error", err.Error())
		r.retry(ctx, c)
		return
	}
	msg := buildMessage(r.sender.From(), c.recipient, uuid.NewString(), r.now(), rendered)

	err = r.sender.Send(work, c.recipient, msg)
	if err == nil {
		r.finalise(ctx, c, OutcomeSuccess)
		return
	}
	// The failure text may quote the recipient or, in theory, the message:
	// it is logged only redacted, and the address itself never.
	class := Classify(err)
	attrs := []any{
		"emailId", c.id,
		"kind", string(c.kind),
		"attempt", c.generation,
		"class", string(class),
		"stage", string(FailureStage(err)),
		"smtpCode", SMTPCode(err),
		"error", redact(err.Error(), issued.Token),
	}
	if class == ClassPermanent {
		slog.ErrorContext(ctx, "email rejected by the relay", attrs...)
		r.finalise(ctx, c, OutcomeRejected)
		return
	}
	slog.WarnContext(ctx, "email attempt failed temporarily; retrying", attrs...)
	r.retry(ctx, c)
}

// content gathers what the template shows. For an invitation it reads the
// inviter's and the space's names — ok=false when the invitation or its
// space no longer exists.
func (r *Runner) content(ctx context.Context, c claim, issued Issued) (Data, bool, error) {
	d := Data{
		Link:      r.baseURL + linkPaths[c.kind] + url.PathEscape(issued.Token),
		ExpiresAt: issued.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"),
	}
	if c.kind != KindSpaceInvite {
		return d, true, nil
	}
	if c.invitationID == nil {
		return d, false, nil
	}
	err := r.pool.QueryRow(ctx, `
SELECT u.name, s.name
  FROM space_invitations i
  JOIN users  u ON u.id = i.invited_by
  JOIN spaces s ON s.id = i.space_id
 WHERE i.id = $1`, *c.invitationID).Scan(&d.InviterName, &d.SpaceName)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("reading invitation %s: %w", *c.invitationID, err)
	}
	return d, true, nil
}

// retry returns the email to the queue with back-off. There is no attempt
// budget: the deadline sweep ends the email when its time is up.
func (r *Runner) retry(ctx context.Context, c claim) {
	ctx, cancel := bookkeeping(ctx)
	defer cancel()
	next := r.now().Add(Backoff(c.generation))
	tag, err := r.pool.Exec(ctx, `
UPDATE outgoing_emails
   SET state = 'pending', leased_until = NULL, next_attempt_at = $3
 WHERE id = $1 AND state = 'leased' AND attempts = $2`, c.id, c.generation, next)
	if err != nil {
		slog.ErrorContext(ctx, "scheduling an email retry failed",
			"emailId", c.id, "attempt", c.generation, "error", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		slog.WarnContext(ctx, "email retry not scheduled: the lease is no longer ours",
			"emailId", c.id, "attempt", c.generation)
		return
	}
	retriesTotal.Inc()
}

// Backoff is the wait before the attempt after attempt number n (1-based):
// 30 s, 1 min, 2 min, … capped at 10 min.
func Backoff(n int) time.Duration {
	d := retryBaseDelay
	for i := 1; i < n && d < retryMaxDelay; i++ {
		d *= 2
	}
	return min(d, retryMaxDelay)
}

// finalise records the email's one outcome — only while c still owns it.
// An email the token protocol already finalised (cancelled by a newer
// request, a revocation, a completed action) is final and is not touched.
func (r *Runner) finalise(ctx context.Context, c claim, outcome string) {
	ctx, cancel := bookkeeping(ctx)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `
UPDATE outgoing_emails
   SET state = 'final', outcome = $3, finalized_at = $4,
       leased_until = NULL, next_attempt_at = NULL
 WHERE id = $1 AND state = 'leased' AND attempts = $2`, c.id, c.generation, outcome, r.now())
	if err != nil {
		slog.ErrorContext(ctx, "recording an email outcome failed",
			"emailId", c.id, "outcome", outcome, "attempt", c.generation, "error", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		slog.WarnContext(ctx, "email outcome not recorded: the lease is no longer ours",
			"emailId", c.id, "outcome", outcome, "attempt", c.generation)
		return
	}
	deliveriesTotal.WithLabelValues(string(c.kind), outcome).Inc()
	slog.InfoContext(ctx, "email finalised",
		"emailId", c.id, "kind", string(c.kind), "outcome", outcome, "attempts", c.generation)
}

// sleepCtx waits d or until ctx ends; false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
