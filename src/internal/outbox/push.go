package outbox

// push.go — sk-303.13 / T010: the push delivery runtime.
//
// Push used to be a SIDE EFFECT of the NATS publication: tickNotifications
// collected the rows whose published_at had just flipped, and delivered them
// after the commit. That coupling had two consequences the wave removes.
//
//   - A process without NATS had no push at all. `cmd/api/main.go` built the
//     dispatcher only when the connection was live, so an empty NATS_URL — or
//     a failed connect — silently disabled every web push in the deployment.
//     Push and the live feed are different transports serving different
//     promises; one being down is not a reason for the other to stop.
//
//   - "Delivered" had no durable meaning. The send happened once, best-effort,
//     outside any transaction, and nothing recorded what came of it: a restart
//     mid-flight lost the signal with no trace, and a recipient with no device
//     was indistinguishable from a successful delivery.
//
// So the push now has its own runner and its own DB-backed lifecycle:
// pending → leased → final(outcome). Claims use FOR UPDATE SKIP LOCKED, a
// lease bounds a crashed worker's hold on a row, attempts and a deadline bound
// retries, and every row ends in exactly one named outcome.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
	"skein-api/internal/push"
)

// Push delivery outcomes — the closed set the schema's
// notifications_push_outcome_chk accepts. Each one is FINAL: the row is done
// and never sent again.
//
// They are deliberately not collapsed. "Did not send because preferences
// could not be read" and "sent and the transport fell over" are different
// incidents with different fixes, and the spec forbids merging their metrics.
const (
	// outcomeSuccess — the provider accepted the push for at least one active
	// device. That is what "delivery" means here: delivery to the device is
	// the provider's business and is not observable from our side.
	outcomeSuccess = "success"
	// outcomeNoDevice — the recipient has no active subscription. A legitimate
	// final outcome, not an error: "the signal is not repeated".
	outcomeNoDevice = "no_device"
	// outcomeAccessRevoked — the recipient lost access to the project between
	// the row being written and the send. Nothing is built and nothing is
	// sent: "no project push reveals data to someone who lost access before
	// the actual send".
	outcomeAccessRevoked = "access_revoked"
	// outcomePrefsUnavailable — the preferences could not be READ at delivery
	// time. Not an outcome of a delivery attempt at all: nothing was sent and
	// nothing is retried (Q6/SC-016). One signal is knowingly lost so that an
	// explicit refusal cannot be violated by a database hiccup.
	outcomePrefsUnavailable = "prefs_unavailable"
	// outcomeConfigurationNoSend — the row is finished, nothing is retried,
	// and the reason is a decision or a configuration on OUR side — neither a
	// transport failure, nor lost access, nor unreadable preferences. The
	// schema's outcome set is closed (migration 000039), so the cases share
	// one value and are told apart only by the metric's cause label and the
	// log (the row itself stores no cause). Two kinds share it:
	//
	//   - finished WITHOUT a send: the sender is not configured at all
	//     (sender_disabled); the recipient switched this reason's push channel
	//     off since the row was written (channel_disabled); the event no longer
	//     resolves to an issue (event_unresolvable); a digest emptied on the
	//     delivery-time re-check (digest_empty) or does not fit the record
	//     budget even in its shortest form (digest_oversize);
	//   - SENT and refused (sk-627, provider_forbidden): no device accepted,
	//     none failed transiently, and at least one push service answered 403.
	//     A 403 is the push service rejecting OUR credentials — Apple answers
	//     it to a bad VAPID JWT — so a retry re-sends the same rejected JWT,
	//     and the device is not dead, so the subscription is kept.
	//
	// They are one outcome because they answer the same operational question
	// the same way — "nothing to retry; what needs fixing is on our side".
	outcomeConfigurationNoSend = "configuration_no_send"
	// outcomeTransportExhausted — every attempt failed and the row ran out of
	// attempts or out of deadline. The only outcome reached through retries.
	outcomeTransportExhausted = "transport_exhausted"
	// outcomeSuperseded — sk-303.14: the row is SUPERSEDED by a
	// fresher row of the same "recipient and category" pair and finished
	// without a send.
	//
	// The case exists because, when the transport is down longer than a
	// segment, the nudge worker is entitled to form several notifications
	// in a row, and of those only the freshest must be delivered: otherwise
	// a person coming back online would get as many notifications in a row
	// as there were segments of outage — exactly the hammering people
	// unsubscribe from wholesale.
	//
	// The value is INTRODUCED BY A MIGRATION (000044), not reused: the
	// outcome set is closed by a schema constraint, and a sub-cause inside
	// an existing outcome would leave supersede indistinguishable from a
	// configuration refusal in the row's STATE, observable only in a metric
	// label. The requirement is about the state itself.
	//
	// It is stamped by the PRODUCER (the nudge worker, T018) at the moment
	// a new row is formed, not by the tract: supersede is a decision about
	// which of two rows is current, and only the one who sees both can
	// take it. The value is declared here because the closed outcome set
	// lives in the code as ONE list, and a list that has fallen behind the
	// schema is silent on a miss.
	outcomeSuperseded = "superseded"
	// outcomeQuietHours — sk-340: the row finished WITHOUT SENDING because
	// the delivery instant fell into the recipient's quiet-hours window.
	//
	// The maintainers' decision that this value writes into the row's state:
	// quiet wins — a push that falls into the window is not sent at all. Neither
	// a shift to the end of the window nor a catch-up after it (the rationale is in the header
	// of migration 000052). The notification is NOT LOST: the feed row
	// is created as usual, the person sees it in the UI in the morning. Only the
	// push is silent.
	//
	// ⚠ WHY A VALUE OF ITS OWN, NOT A SUB-REASON OF configuration_no_send.
	// The acceptance requires TELLING APART "not sent because quiet hours" and "not
	// sent because the path broke". Both look the same —
	// the absence of a push — so the discriminator must lie in the STATE of the
	// row, not only in the measurement label: a sub-reason inside an existing
	// outcome would leave quiet hours indistinguishable from a configuration refusal when reading
	// the row itself. sk-303.14 took the same decision for the same reason for
	// `superseded`.
	//
	// The value was INTRODUCED BY A MIGRATION (000052): the set of outcomes is closed by a schema
	// constraint, and a list in code that lags behind the schema stays silent on a miss.
	outcomeQuietHours = "quiet_hours"
)

// Sub-causes of outcomeConfigurationNoSend. Finite, low-cardinality.
const (
	causeSenderDisabled  = "sender_disabled"
	causeChannelDisabled = "channel_disabled"
	// causeEventUnresolvable — the event behind the row resolves to no issue.
	// Terminal, not transient: see the branch in deliver.
	causeEventUnresolvable = "event_unresolvable"
	// causeDigestEmpty — sk-303.14 / T015: the digest emptied on the
	// per-element re-check at delivery time. None of the issues in the
	// event payload survived the access and membership check, so there is
	// nothing to send: "you have 0 unreviewed" is exactly the stale
	// reminder the product does not allow.
	//
	// Why this is configuration_no_send, not access_revoked: a single-issue
	// delivery names access_revoked because it has EXACTLY ONE project and
	// that project is either visible or not. A digest spans many projects
	// and many drop-out causes at once (acceptance, reassignment, close,
	// a review mark, the flag being cleared, a decline, the mechanic being
	// switched off), and naming the culprit would take a second query —
	// the very query fail-closed refused to run against the inaccessible.
	// The honest answer is "we knowingly did not send, there is nothing to
	// retry", and this outcome already has that.
	causeDigestEmpty = "digest_empty"
	// causeDigestOversize — sk-303.14 / T015, follow-up from review A/7:
	// the digest payload does not fit the transport budget EVEN in its
	// shortest form — the one with no list at all, just the count.
	//
	// This is not a mode of operation, it is a DEFECT, and it is named
	// separately for that reason. It is reachable only with degenerate
	// data (maximum-length identifiers plus a very long APP_BASE_URL),
	// and silently handing such a payload to the transport would produce
	// a "transport failure" — sending triage to a provider nobody
	// bothered. Its own sub-cause exists so degenerate data is tellable
	// apart from a knowing refusal to send, and is found by search, not
	// by guesswork.
	causeDigestOversize = "digest_oversize"
	// causeProviderForbidden — sk-627: no device accepted the push, no device
	// failed transiently, and at least one push service answered 403
	// (push.ErrForbidden). See outcomeConfigurationNoSend for why a 403 is a
	// configuration refusal and not a transport failure.
	causeProviderForbidden = "provider_forbidden"
	// causeInvitationStale — sk-674.18 T040: a space-invitation push whose
	// invitation lost its force before the send — it is no longer pending
	// (accepted, declined, revoked, expired), its space was deleted, or the
	// recipient is no longer its addressee (email changed, account blocked
	// or deleted). Pushing «you are invited» then would invite to nothing.
	causeInvitationStale = "invitation_stale"
	causeNone            = ""
)

// Push runner tuning. Overridable through PushOptions.
const (
	defaultPushPollInterval = 1 * time.Second
	defaultPushBatchSize    = 50
	// defaultPushLeaseTTL bounds how long a crashed worker keeps a row. It is
	// generously longer than a send round-trip and far shorter than the
	// delivery deadline, so a crash costs one lease, not the notification.
	defaultPushLeaseTTL = 2 * time.Minute
	// defaultPushMaxAttempts bounds retries independently of the deadline:
	// whichever bound is hit first ends the row.
	defaultPushMaxAttempts = 5
	// pushRetryBaseDelay is the first back-off step; it doubles per attempt.
	pushRetryBaseDelay = 30 * time.Second
	// pushRetryMaxDelay caps the back-off so a row still gets several tries
	// inside its deadline.
	pushRetryMaxDelay = 10 * time.Minute
)

// PushOptions configures the push runner. Zero values take the defaults above.
type PushOptions struct {
	PollInterval time.Duration
	BatchSize    int
	LeaseTTL     time.Duration
	MaxAttempts  int
	// QuietClock — the clock against which the recipient's quiet-hours window is checked
	// (sk-340). nil means time.Now, and in production it is always nil. Replaced only in
	// tests of the window arithmetic: see PushRunner.now for why this seam
	// does NOT extend to deadlines and the lease.
	QuietClock func() time.Time
}

// PushRunner drains push-eligible notification rows. It has NO NATS
// dependency — that is the point of the split, and it is enforced by the type:
// there is nowhere to pass a connection.
type PushRunner struct {
	pool   *pgxpool.Pool
	sender PushSender
	store  PushSubscriptionStore
	// notif — the gateway to notification settings. Today only the quiet-hours
	// window is read from it (sk-340); the runner still reads the reason settings with its own
	// query, and merging them was not part of this task.
	notif      *notifications.Store
	appBaseURL string
	opts       PushOptions
}

// NewPushRunner builds the runner. sender/store may be nil or disabled — the
// runner still runs, and finalises the rows it claims as
// configuration_no_send, because the alternative is an eternal pending backlog
// that nobody ever looks at.
func NewPushRunner(pool *pgxpool.Pool, sender PushSender, store PushSubscriptionStore, appBaseURL string, opts PushOptions) *PushRunner {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPushPollInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultPushBatchSize
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = defaultPushLeaseTTL
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultPushMaxAttempts
	}
	return &PushRunner{
		pool:       pool,
		sender:     sender,
		store:      store,
		notif:      notifications.NewStore(pool),
		appBaseURL: appBaseURL,
		opts:       opts,
	}
}

// Run blocks until ctx is cancelled. Mirrors the dispatcher's loop shape —
// tick, back off on error, drain on a full batch — but shares no state with
// it: its own goroutine, its own ticker, its own shutdown.
func (r *PushRunner) Run(ctx context.Context) error {
	slog.InfoContext(ctx, "push runner started",
		"pollIntervalMs", r.opts.PollInterval.Milliseconds(),
		"batchSize", r.opts.BatchSize,
		"leaseTtlMs", r.opts.LeaseTTL.Milliseconds(),
		"maxAttempts", r.opts.MaxAttempts,
		"senderEnabled", r.senderReady(),
	)
	defer slog.InfoContext(ctx, "push runner stopped")

	backoff := r.opts.PollInterval
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := r.Tick(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			pushRunnerErrorsTotal.Inc()
			slog.ErrorContext(ctx, "push runner tick failed",
				"backoffMs", backoff.Milliseconds(),
				"error", err.Error(),
			)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff *= 2
			if backoff > defaultMaxBackoff {
				backoff = defaultMaxBackoff
			}
			continue
		}
		backoff = r.opts.PollInterval
		if n >= r.opts.BatchSize {
			continue
		}
		if !sleepCtx(ctx, r.opts.PollInterval) {
			return nil
		}
	}
}

// senderReady reports whether a push can physically be sent.
func (r *PushRunner) senderReady() bool {
	return r.sender != nil && r.store != nil && r.sender.Enabled()
}

// pushClaim is one leased row.
type pushClaim struct {
	id         int64
	userID     uuid.UUID
	eventID    int64
	projectID  *uuid.UUID
	reason     *string
	attempts   int
	deadlineAt *time.Time
}

// Tick runs one pass: expire stale leases, retire rows past their deadline,
// claim a batch and deliver it. Exported so tests drive it deterministically
// instead of racing the Run loop.
func (r *PushRunner) Tick(ctx context.Context) (int, error) {
	if err := r.reclaimExpiredLeases(ctx); err != nil {
		return 0, err
	}
	if err := r.retireExpiredDeadlines(ctx); err != nil {
		return 0, err
	}
	claims, err := r.claim(ctx)
	if err != nil {
		return 0, err
	}
	for _, c := range claims {
		r.deliver(ctx, c)
	}
	return len(claims), nil
}

// reclaimExpiredLeases returns rows held by a worker that died mid-flight.
// The attempt it burned is NOT refunded: at-least-once is the contract, and a
// crash after a successful provider hand-off may already have delivered.
func (r *PushRunner) reclaimExpiredLeases(ctx context.Context) error {
	tag, err := r.pool.Exec(ctx, `
UPDATE notifications
   SET push_state = 'pending',
       push_leased_until = NULL,
       push_next_attempt_at = now()
 WHERE push_state = 'leased'
   AND push_leased_until IS NOT NULL
   AND push_leased_until < now()`)
	if err != nil {
		return fmt.Errorf("reclaiming expired push leases: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		pushLeaseReclaimsTotal.Add(float64(n))
		slog.WarnContext(ctx, "push runner reclaimed expired leases", "count", n)
	}
	return nil
}

// retireExpiredDeadlines is what makes "no eternal pending backlog" true by
// construction rather than by hoping a retry eventually happens. A row whose
// deadline has passed while it was still owed is finalised as exhausted, with
// no send: the deadline is the promise that a push is worth making only while
// the action it reports is current.
func (r *PushRunner) retireExpiredDeadlines(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `
UPDATE notifications
   SET push_state = 'final',
       push_outcome = $1,
       push_finalized_at = now(),
       push_next_attempt_at = NULL,
       push_leased_until = NULL
 WHERE push_state = 'pending'
   AND push_deadline_at IS NOT NULL
   AND push_deadline_at < now()
RETURNING reason`, outcomeTransportExhausted)
	if err != nil {
		return fmt.Errorf("retiring push rows past deadline: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var reason *string
		if err := rows.Scan(&reason); err != nil {
			return fmt.Errorf("scanning retired push rows: %w", err)
		}
		pushOutcomesTotal.WithLabelValues(reasonLabel(reason), outcomeTransportExhausted, causeNone).Inc()
		n++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("retiring push rows past deadline: %w", err)
	}
	if n > 0 {
		slog.WarnContext(ctx, "push rows retired past their delivery deadline", "count", n)
	}
	return nil
}

// claimPushSQL takes a batch atomically. FOR UPDATE SKIP LOCKED is what makes
// the claim itself safe: two workers never claim the same row in the same
// instant.
//
// It is NOT, on its own, what makes two concurrent send attempts impossible.
// The batch is claimed under ONE lease and delivered row by row, so a row at
// the tail of a batch can reach its turn after its lease has already run out
// on a perfectly healthy worker — and an expired lease is, by construction,
// reclaimable by anybody. Exclusion is therefore restored per row, at the
// moment the worker actually reaches it: see holdLease, which re-stamps the
// lease and re-asserts ownership before anything is sent, and the same
// ownership predicate on finalise / retryOrExhaust.
//
// The attempt counter is incremented AT CLAIM, not at send. A worker that dies
// after the provider accepted the push must not get its attempt back — that
// would turn a crash into an unbounded retry loop against a provider that
// already delivered. That the counter moves on every claim is also what makes
// it usable as the owner generation: the pair (id, push_attempts) names one
// claim, and any re-claim invalidates it.
const claimPushSQL = `
WITH claimed AS (
    SELECT id
      FROM notifications
     WHERE push_state = 'pending'
       AND (push_next_attempt_at IS NULL OR push_next_attempt_at <= now())
     ORDER BY push_next_attempt_at NULLS FIRST, id
     FOR UPDATE SKIP LOCKED
     LIMIT $1
)
UPDATE notifications n
   SET push_state = 'leased',
       push_leased_until = now() + ($2::bigint * interval '1 second'),
       push_attempts = n.push_attempts + 1
  FROM claimed c
 WHERE n.id = c.id
RETURNING n.id, n.user_id, n.event_id, n.project_id, n.reason, n.push_attempts, n.push_deadline_at`

func (r *PushRunner) claim(ctx context.Context) ([]pushClaim, error) {
	rows, err := r.pool.Query(ctx, claimPushSQL,
		r.opts.BatchSize, int64(r.opts.LeaseTTL/time.Second))
	if err != nil {
		return nil, fmt.Errorf("claiming push rows: %w", err)
	}
	defer rows.Close()

	out := make([]pushClaim, 0, r.opts.BatchSize)
	for rows.Next() {
		var c pushClaim
		if err := rows.Scan(&c.id, &c.userID, &c.eventID, &c.projectID,
			&c.reason, &c.attempts, &c.deadlineAt); err != nil {
			return nil, fmt.Errorf("scanning claimed push rows: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claiming push rows: %w", err)
	}
	return out, nil
}

// pushLeaseRenewMargin is how much lease a device send must have in front of
// it before it is started. It is deliberately larger than the sender's own
// per-device timeout (push.pushSendTimeout, 10s): a send that begins with less
// lease left than it may take can finish after the row has become reclaimable,
// which is the double-send this whole ownership dance exists to prevent.
const pushLeaseRenewMargin = 15 * time.Second

// holdLease re-stamps the row's lease FOR THIS CLAIM and reports whether the
// claim still owns the row.
//
// This is the per-row half of the exclusion. The batch lease was stamped once,
// for every row at once, at claim time; by the time the worker reaches a row
// further down the batch that lease may be spent, in which case the row has
// legitimately been reclaimed by somebody else. `push_attempts` is the owner
// generation — it is bumped by every claim — so `push_state='leased' AND
// push_attempts = <the value this claim saw>` is true for exactly one worker
// and becomes false the instant the row is re-claimed.
//
// Zero rows affected therefore means "not mine any more": the caller must walk
// away silently — no send, no outcome, no metric. Recording anything at that
// point would count one row's outcome twice.
func (r *PushRunner) holdLease(ctx context.Context, c pushClaim) bool {
	tag, err := r.pool.Exec(ctx, `
UPDATE notifications
   SET push_leased_until = now() + ($3::bigint * interval '1 second')
 WHERE id = $1
   AND push_state = 'leased'
   AND push_attempts = $2`, c.id, c.attempts, int64(r.opts.LeaseTTL/time.Second))
	if err != nil {
		slog.WarnContext(ctx, "push deferred: lease renewal failed",
			"notificationId", c.id, "attempt", c.attempts, "error", err.Error())
		return false
	}
	if tag.RowsAffected() == 0 {
		slog.WarnContext(ctx, "push abandoned: lease lost to another worker",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"attempt", c.attempts,
		)
		return false
	}
	return true
}

// deliver takes one leased row all the way to a final outcome, or back to
// pending with a back-off.
//
// The order of the gates is the order of the promises they keep: nothing is
// built before we know we are allowed to send, and nothing about the issue is
// read before the recipient's access has been re-checked.
//
// Two contexts, on purpose. The DELIVERY work — the gates and the sends — runs
// under a deadline no later than the lease it is covered by, so a slow
// recipient cannot keep working on a row the queue has already handed to
// somebody else. The BOOKKEEPING writes (finalise / retryOrExhaust) run under
// the caller's context instead: the outcome of work already done must still be
// recordable when the delivery window itself has just closed. They carry the
// same ownership predicate, so a row that really was taken away records
// nothing regardless.
func (r *PushRunner) deliver(ctx context.Context, c pushClaim) {
	// 0. Take the lease for THIS row, now that its turn has actually come.
	//    Everything below — including the finalising branches — then runs on a
	//    claim that has just been proven to still own the row.
	if !r.holdLease(ctx, c) {
		return
	}
	leaseUntil := time.Now().Add(r.opts.LeaseTTL)
	work, cancelWork := context.WithDeadline(ctx, leaseUntil)
	defer cancelWork()

	// 1. Is there a sender at all? A deployment without VAPID keys still has
	//    rows queued by the write path; finalising them is what keeps the
	//    backlog from growing forever.
	if !r.senderReady() {
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeSenderDisabled)
		return
	}

	// 2. Current preferences. A READ FAILURE is terminal and sends nothing
	//    (Q6): at a narrow default, erring towards sending is erring towards
	//    violating an explicit refusal.
	enabled, err := r.pushEnabledNow(work, c.userID, c.reason)
	if err != nil {
		slog.ErrorContext(ctx, "push not sent: reason preferences unavailable at delivery",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"error", err.Error(),
		)
		r.finalise(ctx, c, outcomePrefsUnavailable, causeNone)
		return
	}
	if !enabled {
		// The recipient switched this reason's push channel off between the
		// row being written and the send. An explicit refusal outranks a
		// queued signal.
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeChannelDisabled)
		return
	}

	// 2.4 QUIET HOURS OF THE RECIPIENT — sk-340.
	//
	//     The gate stands here, between the settings and building the payload, for two
	//     reasons. It belongs to the same family as the channel refusal —
	//     it is a deliberate refusal to SEND, not a path failure — and therefore
	//     lives next to it. And it stands BEFORE building the payload: a silent push
	//     must not cost a single extra query about the issue.
	//
	//     ⚠ ONE GATE FOR BOTH KINDS OF PAYLOAD — a single push and a digest
	//     both pass it before the fork by kind (2.5). Quiet hours are a property of the ADDRESSEE,
	//     not of the reason, and a second copy of the check would diverge from the first
	//     exactly when one of them is edited.
	quiet, err := r.quietNow(work, c.userID)
	if err != nil {
		// The quiet-hours settings could not be read. The refusal is of the same kind as the
		// unreadable reason settings above, and answers the same way: we send
		// nothing and retry nothing. Sending on an unreadable setting
		// would risk violating an explicit refusal; the outcome is the same, because
		// that is exactly what it is — "the settings could not be read".
		slog.ErrorContext(ctx, "push not sent: quiet hours settings unavailable at delivery",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"error", err.Error(),
		)
		r.finalise(ctx, c, outcomePrefsUnavailable, causeNone)
		return
	}
	if quiet {
		slog.InfoContext(ctx, "push not sent: recipient is in quiet hours",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
		)
		r.finalise(ctx, c, outcomeQuietHours, causeNone)
		return
	}

	// 2.5 WHICH PAYLOAD KIND is this row? — sk-303.14 / T015, R-2.
	//
	//     The tract used to have exactly one kind: one event, one issue. A
	//     digest speaks of SEVERAL issues, is addressed to a person rather
	//     than to a project, and leads to a surface rather than to a card —
	//     so it is a second delivery kind, told apart BY THE ROW'S REASON.
	//
	//     The branch sits AFTER the preferences gate on purpose: unsubscribing
	//     is checked by the one mechanism, for both kinds, before either of
	//     them builds anything. That single copy of the unsubscribe check is
	//     the whole reason the digest reuses this runner instead of getting a
	//     queue of its own.
	//
	//     It sits BEFORE the row-level access gate because a digest row has no
	//     project (its event is a user event): access for it is re-checked per
	//     ELEMENT inside buildDigest, which is what INV-14 asks for and what a
	//     single row-level gate structurally cannot give.
	// 2.6 A SPACE INVITATION (sk-674.18 T040) is the third payload kind: no
	//     issue and no project — it is about a space, and its row is written
	//     by the invitations package, not by the fan-out
	//     (Reason.DirectWriter). It re-checks at send time that the
	//     invitation still holds (push_invitation.go).
	if notifications.ReasonKey(reasonLabel(c.reason)) == notifications.ReasonKeySpaceInvitation {
		payload, ok := r.buildInvitation(ctx, work, c)
		if !ok {
			return
		}
		r.sendPayload(ctx, work, c, payload)
		return
	}

	if cat, isDigest := notifications.DigestCategoryForReason(
		notifications.ReasonKey(reasonLabel(c.reason))); isDigest {
		payload, ok := r.buildDigest(ctx, work, c, cat)
		if !ok {
			// buildDigest already finalised the row, or deliberately left it
			// leased (fail-closed). Either way there is nothing to send.
			return
		}
		r.sendPayload(ctx, work, c, payload)
		return
	}

	// 3. Current project access — re-checked at send time for EVERY reason,
	//    not just for mentions. Nothing about the issue is read until this
	//    passes, so a revoked recipient cannot learn a title from a payload
	//    that was never built.
	if c.projectID != nil {
		allowed, err := r.hasProjectAccess(work, c.userID, *c.projectID)
		if err != nil {
			// Fail CLOSED and leave the row leased: the lease expires and the
			// row is retried. Sending on an unreadable access check is the one
			// thing this gate exists to prevent.
			slog.WarnContext(ctx, "push deferred: access re-check failed",
				"notificationId", c.id, "error", err.Error())
			return
		}
		if !allowed {
			r.finalise(ctx, c, outcomeAccessRevoked, causeNone)
			return
		}
	}

	// 4. Payload. The event is resolved to its ISSUE — including through the
	//    comment for a mention in a comment, which is what makes the push
	//    "lead to the task with its context" rather than dead-ending.
	ei, ok, err := r.issueForEvent(work, c.eventID)
	if err != nil {
		// The LOOKUP failed — the database could not answer. Transient by
		// nature: leave the row leased so the lease expiry retries it,
		// bounded by attempts and the deadline like any other failure.
		slog.WarnContext(ctx, "push deferred: issue lookup failed",
			"notificationId", c.id, "eventId", c.eventID, "error", err.Error())
		return
	}
	if !ok {
		// The lookup SUCCEEDED and found nothing. That is not "not yet": the
		// event and the notification are written in one transaction, so a row
		// whose event resolves to no issue points at something that is gone
		// for good, and retrying it can only wake the runner up every lease
		// until retireExpiredDeadlines finally records it as
		// transport_exhausted — blaming the provider for a missing entity.
		// Finish it here, under its own sub-cause.
		slog.WarnContext(ctx, "push not sent: the event resolves to no issue",
			"notificationId", c.id,
			"eventId", c.eventID,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
		)
		r.finalise(ctx, c, outcomeConfigurationNoSend, causeEventUnresolvable)
		return
	}
	payload, err := json.Marshal(buildPushPayload(reasonLabel(c.reason), ei, r.appBaseURL))
	if err != nil {
		slog.ErrorContext(ctx, "push not sent: payload marshal failed",
			"notificationId", c.id, "error", err.Error())
		r.retryOrExhaust(ctx, c)
		return
	}

	r.sendPayload(ctx, work, c, payload)
}

// sendPayload is the tail of delivery shared by BOTH payload kinds: the
// devices, the sends, the verdict.
//
// It is a separate function precisely because the two kinds differ only in how
// the payload is BUILT. Everything downstream of a built payload — the device
// list, the lease re-assertions around the provider hand-offs, the per-device
// metric and the outcome machine — is one implementation, not two. A digest
// that failed the transport must retry and exhaust exactly like a single-issue
// push does, and it does so because it runs the same code, not because two
// copies were kept in step.
//
// `work` is the delivery-bounded context from deliver; `ctx` is the caller's,
// used for the bookkeeping writes so an outcome is still recordable when the
// delivery window has just closed.
func (r *PushRunner) sendPayload(ctx, work context.Context, c pushClaim, payload []byte) {
	// 5. Devices.
	subs, err := r.store.ListForUser(work, c.userID)
	if err != nil {
		slog.WarnContext(ctx, "push deferred: subscription load failed",
			"notificationId", c.id, "userId", c.userID.String(), "error", err.Error())
		return
	}
	if len(subs) == 0 {
		r.finalise(ctx, c, outcomeNoDevice, causeNone)
		return
	}

	// The last gate before the provider, and the one that actually keeps the
	// promise: re-assert ownership immediately before the first hand-off. The
	// gates above are database round-trips and the row may have been reclaimed
	// while they ran.
	if !r.holdLease(ctx, c) {
		return
	}
	leaseUntil := time.Now().Add(r.opts.LeaseTTL)

	var tally sendTally
	for _, sub := range subs {
		// A recipient can have many devices and each send has its own
		// timeout, so one lease window is not a bound the loop may assume.
		// Renew — with the same ownership predicate — whenever less than a
		// send's worth of lease is left.
		if time.Until(leaseUntil) < pushLeaseRenewMargin {
			if !r.holdLease(ctx, c) {
				// Taken away mid-loop: the devices already served have been
				// served (at-least-once), and the new owner finishes the row.
				return
			}
			leaseUntil = time.Now().Add(r.opts.LeaseTTL)
		}
		sendCtx, cancelSend := context.WithDeadline(ctx, leaseUntil)
		serr := r.sender.Send(sendCtx, sub, payload)
		cancelSend()
		result := "sent"
		switch {
		case serr == nil:
			tally.sent++
		case errors.Is(serr, push.ErrGone):
			// The endpoint is dead and the sender has pruned it.
			tally.gone++
			result = "gone"
		case errors.Is(serr, push.ErrForbidden):
			// The push service rejected our credentials. Never pruned, and
			// never a reason to retry the row by itself — see
			// classifySendOutcome for the mixed case.
			tally.forbidden++
			result = "forbidden"
			logSendFailure(ctx, c, serr)
		default:
			tally.failed++
			result = "error"
			logSendFailure(ctx, c, serr)
		}
		pushDeliveriesTotal.WithLabelValues(reasonLabel(c.reason), result).Inc()
	}

	outcome, cause, terminal := classifySendOutcome(tally)
	if terminal {
		r.finalise(ctx, c, outcome, cause)
		return
	}
	r.retryOrExhaust(ctx, c)
}

// logSendFailure records one device's failed send.
//
// The sender wraps the transport error, and a transport error normally quotes
// the URL it failed on — which IS the device endpoint, i.e. a subscription
// secret. Redact it: what makes the record useful is the failure ("connection
// refused", "status 500"), not the address. A push-service refusal
// (*push.StatusError) adds its status, the push-service HOST and the bounded
// response body — the body is where the provider names the reason, and it is
// redacted the same way in case a provider echoes the endpoint back.
//
// Field names avoid the logging sanitizer's secret tokens (internal/logging
// secretKeyTokens): a key such as "responseToken" would be masked to "***"
// and hide exactly the diagnosis this line exists for.
func logSendFailure(ctx context.Context, c pushClaim, serr error) {
	attrs := []any{
		"notificationId", c.id,
		"userId", c.userID.String(),
		"reason", reasonLabel(c.reason),
		"error", redactEndpoints(serr.Error()),
	}
	var se *push.StatusError
	if errors.As(serr, &se) {
		attrs = append(attrs,
			"status", se.StatusCode,
			"pushHost", se.Host,
			"responseBody", redactEndpoints(se.Body),
			"responseBodyTruncated", se.BodyTruncated,
		)
		if se.BodyReadError != "" {
			attrs = append(attrs, "responseBodyReadError", redactEndpoints(se.BodyReadError))
		}
	}
	slog.WarnContext(ctx, "push send failed", attrs...)
}

// sendTally counts the per-device answers of ONE delivery attempt.
type sendTally struct {
	sent, gone, failed, forbidden int
}

// classifySendOutcome turns the per-device answers of ONE delivery attempt
// into the notification's verdict. Pure: it is the machine, and the machine is
// where the taxonomy lives.
//
// The rules, in the order they decide:
//
//   - any success wins. "Delivery is a successful hand-off to the provider for
//     at least one of the recipient's active devices" — a second device failing
//     does not un-deliver the first;
//   - any transport failure keeps the row alive: it is NOT an outcome yet, the
//     row goes back to the queue until its attempts or its deadline run out —
//     a live device that failed to answer is still something to retry, even
//     when another device refused or was dead. The retry re-sends to EVERY
//     device of the recipient, the one that answered 403 included: which
//     device refused is not remembered across attempts (that would need
//     per-device state in the schema), so a mixed attempt costs one extra,
//     refused request per retry, bounded by the attempt budget;
//   - no success, no transport failure, and at least one 403 (sk-627): the
//     push service rejected our credentials. configuration_no_send with cause
//     provider_forbidden, final — a retry would re-send the same rejected JWT;
//   - otherwise every endpoint was dead and has just been pruned: the
//     recipient has NO ACTIVE DEVICE. That is a legitimate final outcome, not
//     an error, and it must not be retried.
//
// terminal=false is therefore the only answer that leaves the row alive.
func classifySendOutcome(t sendTally) (outcome, cause string, terminal bool) {
	switch {
	case t.sent > 0:
		return outcomeSuccess, causeNone, true
	case t.failed > 0:
		return "", causeNone, false
	case t.forbidden > 0:
		return outcomeConfigurationNoSend, causeProviderForbidden, true
	default:
		return outcomeNoDevice, causeNone, true
	}
}

// retryOrExhaust puts the row back to pending with a back-off, or finalises it
// when the attempts or the deadline are spent.
// The reschedule is conditional on ownership for the same reason the outcome
// is: a row that has been reclaimed while this worker was sending belongs to
// its new owner's schedule, and overwriting push_next_attempt_at from here
// would push the new owner's attempt into the future for no reason.
func (r *PushRunner) retryOrExhaust(ctx context.Context, c pushClaim) {
	next := time.Now().Add(pushBackoff(c.attempts))
	if budgetSpent(c.attempts, r.opts.MaxAttempts, next, c.deadlineAt) {
		r.finalise(ctx, c, outcomeTransportExhausted, causeNone)
		return
	}
	tag, err := r.pool.Exec(ctx, `
UPDATE notifications
   SET push_state = 'pending',
       push_leased_until = NULL,
       push_next_attempt_at = now() + ($2::bigint * interval '1 second')
 WHERE id = $1
   AND push_state = 'leased'
   AND push_attempts = $3`, c.id, int64(pushBackoff(c.attempts)/time.Second), c.attempts)
	if err != nil {
		slog.ErrorContext(ctx, "push retry scheduling failed",
			"notificationId", c.id, "error", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		slog.WarnContext(ctx, "push retry not scheduled: lease lost to another worker",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"attempt", c.attempts,
		)
		return
	}
	pushRetriesTotal.WithLabelValues(reasonLabel(c.reason)).Inc()
}

// budgetSpent reports whether a transport failure has stopped being retryable.
//
// TWO independent bounds, and either one ends the row: the attempt budget, and
// the delivery deadline. The deadline is compared against the time of the NEXT
// attempt rather than against now, because a retry scheduled past the deadline
// would be claimed only to be retired — one more pointless wake-up and one
// more attempt burnt on nothing.
func budgetSpent(attempts, maxAttempts int, next time.Time, deadline *time.Time) bool {
	if attempts >= maxAttempts {
		return true
	}
	return deadline != nil && !next.Before(*deadline)
}

// pushBackoff doubles from pushRetryBaseDelay, capped.
func pushBackoff(attempts int) time.Duration {
	d := pushRetryBaseDelay
	for i := 1; i < attempts && d < pushRetryMaxDelay; i++ {
		d *= 2
	}
	if d > pushRetryMaxDelay {
		d = pushRetryMaxDelay
	}
	return d
}

// finalise records a terminal outcome. After this the row is never claimed
// again — push_state='final' is outside the claim predicate — and the cleanup
// loop (T013) is free to remove it once it is old enough and invisible.
//
// The write is conditional on this claim still owning the row: exactly one
// increment per notification over its lifetime is what makes
// pushOutcomesTotal answer "what happened to the pushes we owed", and a worker
// whose lease expired mid-delivery must not add a second verdict to a row
// somebody else has already decided. Zero rows affected = not ours: logged,
// not counted.
func (r *PushRunner) finalise(ctx context.Context, c pushClaim, outcome, cause string) {
	tag, err := r.pool.Exec(ctx, `
UPDATE notifications
   SET push_state = 'final',
       push_outcome = $2,
       push_finalized_at = now(),
       push_leased_until = NULL,
       push_next_attempt_at = NULL
 WHERE id = $1
   AND push_state = 'leased'
   AND push_attempts = $3`, c.id, outcome, c.attempts)
	if err != nil {
		slog.ErrorContext(ctx, "recording push outcome failed",
			"notificationId", c.id, "outcome", outcome, "error", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		slog.WarnContext(ctx, "push outcome not recorded: lease lost to another worker",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"outcome", outcome,
			"attempt", c.attempts,
		)
		return
	}
	pushOutcomesTotal.WithLabelValues(reasonLabel(c.reason), outcome, cause).Inc()
	if outcome != outcomeSuccess {
		slog.InfoContext(ctx, "push finalised without delivery",
			"notificationId", c.id,
			"userId", c.userID.String(),
			"reason", reasonLabel(c.reason),
			"outcome", outcome,
			"cause", cause,
			"attempts", c.attempts,
		)
	}
}

// endpointPattern matches an http(s) URL anywhere in a string. Device
// endpoints are URLs, and they are the one part of a push that must never
// reach a log line.
//
// sk-627: a push service's response body is text somebody else formatted, so
// the URL may arrive encoded — JSON may escape '/' ("https:\/\/…") and a form
// body percent-encodes the separators ("https%3A%2F%2F…", either case). All
// three spellings are matched; the sender has already redacted the forms of
// the endpoint it knows (push.newStatusError), this is the backstop.
var endpointPattern = regexp.MustCompile(`(?i)https?(?::(?:\\?/){2}|%3A%2F%2F)[^\s"']+`)

// redactEndpoints removes device endpoints from a message destined for the
// log. It is applied to error text rather than to fields, because the leak is
// never in a field we chose — it is inside an error somebody else formatted.
func redactEndpoints(msg string) string {
	return endpointPattern.ReplaceAllString(msg, "<redacted-endpoint>")
}

// reasonLabel renders a nullable reason column as a bounded metric label. A
// row with no reason predates the registry; it is labelled explicitly rather
// than as an empty string so a gap is visible instead of blending in.
func reasonLabel(reason *string) string {
	if reason == nil || *reason == "" {
		return "unclassified"
	}
	return *reason
}

// pushEnabledNow reads the recipient's CURRENT push state for this reason.
// Absence of a row means the registry default, exactly as everywhere else: an
// absent row is "the narrow default is enough", not "everything is on".
func (r *PushRunner) pushEnabledNow(ctx context.Context, userID uuid.UUID, reason *string) (bool, error) {
	key := notifications.ReasonKey(reasonLabel(reason))
	spec, known := notifications.LookupReason(key)
	if !known {
		// A row whose reason this build does not know cannot be checked
		// against a preference; refusing to send is the safe direction.
		return false, nil
	}
	if !spec.Push.Applicable {
		return false, nil
	}
	var enabled bool
	err := r.pool.QueryRow(ctx, `
SELECT push_enabled
  FROM user_notification_reason_prefs
 WHERE user_id = $1 AND reason = $2`, userID, string(key)).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return spec.Push.DefaultOn, nil
	}
	if err != nil {
		return false, err
	}
	return enabled, nil
}

// quietNow answers whether the recipient is inside their quiet window at this
// moment — sk-340.
//
// ⚠ A MISSING SETTING IS NOT QUIET HOURS, and it is not the same as "the setting
// could not be read". The window is off by default, the zone of an agent or of a person who
// has never come through the browser is unknown; both mean "we do not
// silence". A READ error is another matter, and it is raised to the caller, not
// turned into "not quiet": an unreadable setting interpreted as
// permission to send would violate an explicit refusal exactly as
// an unreadable reason setting would.
func (r *PushRunner) quietNow(ctx context.Context, userID uuid.UUID) (bool, error) {
	settings, err := r.notif.QuietSettingsFor(ctx, userID)
	if err != nil {
		if errors.Is(err, notifications.ErrInvalidTimezone) {
			// The zone name is stored, but the time zone database does not know it — the zone
			// was renamed or removed. This person's quiet-hours window
			// has stopped working, and this is the only place to learn about it:
			// no metric will show it, because the push will go out
			// normally. It does not stop delivery — the settings were read,
			// exactly one of them cannot be interpreted.
			slog.WarnContext(ctx, "quiet hours ignored: the stored time zone does not load",
				"userId", userID.String(), "error", err.Error())
			return false, nil
		}
		return false, err
	}
	return settings.QuietAt(r.now()), nil
}

// now — the clock against which the quiet-hours window is checked.
//
// The seam exists precisely to make the window arithmetic testable: "it is now 23:40 in
// Moscow" is otherwise unreachable in a test, and a midnight crossing — the very
// case on which the naive implementation breaks — cannot be built in a fixture.
// Everything else in the runner still goes by the DATABASE clock (push_deadline_at,
// push_next_attempt_at, the lease), and they must not be replaced with this field: they
// are compared inside SQL with the server's now().
func (r *PushRunner) now() time.Time {
	if r.opts.QuietClock != nil {
		return r.opts.QuietClock()
	}
	return time.Now()
}

// hasProjectAccess re-checks, at send time, that the recipient may still see
// the project. Mirrors the fan-out's membership gate, internal-admin bypass
// included — an external row can never be a true admin.
func (r *PushRunner) hasProjectAccess(ctx context.Context, userID, projectID uuid.UUID) (bool, error) {
	// The expression is the SHARED one (digest.go): the per-element re-check a
	// digest does and this per-row gate must answer identically, or the same
	// recipient is visible to one path and revoked to the other.
	var ok bool
	err := r.pool.QueryRow(ctx,
		`SELECT `+projectVisibleExpr("$1", "$2"), userID, projectID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("re-checking project access: %w", err)
	}
	return ok, nil
}

// pushPayload is the JSON body POSTed to the browser push service and read by
// the service worker. Field names are the contract the frontend SW depends on.
type pushPayload struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
	Category string `json:"category"`
	IssueID  string `json:"issueId"`
}

// eventIssue is the push-relevant projection of an event joined to its issue:
// everything buildPushPayload needs, and nothing else.
type eventIssue struct {
	issueID string
	title   string
	// newData is the raw events.new_data column (JSON). For status_changed
	// events it is the JSON-encoded new status string; for issue.outcome_changed
	// it is the JSON-encoded NEW outcome; it is NULL for most other event
	// types, in which case it arrives empty.
	newData []byte
	// oldData is the raw events.old_data column (JSON). Read for exactly one
	// reason — issue.outcome_changed, whose title is the pair "was → became"
	// and therefore cannot be rendered from new_data alone (sk-489, US-3).
	oldData []byte
	// payload is the raw events.payload column (JSON). Read for the close
	// outcome a CLOSING event carries ({closeOutcome, previousCloseOutcome?},
	// sk-489 / T003): the status push must name an outcome other than
	// "done", and new_data of a status_changed carries the status, not the
	// outcome.
	payload []byte
	// actor is events.actor — already a username string, the same column the
	// activity feed renders, so push and feed name the actor identically.
	actor string
	// actorType is events.actor_type: human | agent | system.
	actorType string
}

// issueForEvent resolves the issue behind an event.
//
// TWO routes, because a mention in a COMMENT is recorded on the comment
// entity: without the second route the mention push would resolve to nothing
// and be dropped, which is exactly the gap SC-015 closes ("the push arrives
// for both kinds of mention — in a comment and in the issue text — and leads
// to the issue with its context"). The uuid cast is guarded by the entity type so a
// hierarchical issue id never reaches it.
func (r *PushRunner) issueForEvent(ctx context.Context, eventID int64) (eventIssue, bool, error) {
	var ei eventIssue
	row := r.pool.QueryRow(ctx, `
SELECT COALESCE(i.id, ci.id), COALESCE(i.title, ci.title),
       e.new_data, e.old_data, e.payload, e.actor, e.actor_type
  FROM events e
  LEFT JOIN issues i ON e.entity_type = 'issue' AND i.id = e.entity_id
  LEFT JOIN comments c ON e.entity_type = 'comment'
                       AND c.id = (CASE WHEN e.entity_type = 'comment'
                                        THEN e.entity_id::uuid
                                        ELSE NULL END)
  LEFT JOIN issues ci ON ci.id = c.issue_id
 WHERE e.id = $1
   AND (i.id IS NOT NULL OR ci.id IS NOT NULL)`, eventID)
	if err := row.Scan(&ei.issueID, &ei.title, &ei.newData, &ei.oldData, &ei.payload,
		&ei.actor, &ei.actorType); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return eventIssue{}, false, nil
		}
		return eventIssue{}, false, err
	}
	return ei, true, nil
}

// statusLabel is one issue status rendered for a push title: an emoji anchor
// plus its label.
type statusLabel struct {
	emoji string
	label string
}

// statusLabels maps an issue status to its push-title fragment.
//
// Labels name the STATE ("Closed"), never a past-tense action ("closed it").
// The original Russian copy forbade past-tense verbs because they carry
// grammatical gender and users carry none, so any verb form misgenders half
// the team. English does not have that gender, but the same rule still holds:
// the whole "<Status> · <who>" title shape exists to sidestep naming an actor
// the notification does not know — do not "fix" these into verbs.
//
// The emoji is the point of the title: the status is recognised by the glyph's
// shape before a single word is read.
//
// Every glyph here MUST render in emoji (colour) presentation. Unicode splits
// these into two groups: 📋 and ✅ carry Emoji_Presentation=Yes and are colour
// by default, while ▶️ ⏸️ ♾️ (U+25B6, U+23F8, U+267E) carry Emoji=Yes but
// Emoji_Presentation=No — bare, they render as a small monochrome TEXT glyph,
// which is exactly the scan anchor this map exists to provide. They are
// therefore written with a trailing variation selector U+FE0F, which requests
// emoji presentation. Adding a status: check emoji-data.txt and append U+FE0F
// unless the codepoint is listed under Emoji_Presentation.
var statusLabels = map[string]statusLabel{
	"open":        {"📋", "Open"},
	"in_progress": {"▶️", "In progress"},
	"deferred":    {"⏸️", "Deferred"},
	"closed":      {"✅", "Closed"},
	"standing":    {"♾️", "Standing"},
}

// pushHeadUnknownReason is the title head for a reason this build cannot name.
// It is honest for a row written by a NEWER build and a silent lie for a
// reason this build's own registry already serves — which is why the parity
// test compares against this constant instead of re-typing the literal: a
// renamed fallback must not turn the assertion vacuous.
const pushHeadUnknownReason = "Skein"

// statusUnknownEmoji is the fallback anchor for a status this map hasn't been
// taught and for an event that carries no status at all. The title contract is
// glyph-first; degrading the words is honest, degrading to NO glyph would drop
// the one thing the eye keys on.
const statusUnknownEmoji = "🔄"

// closeOutcomeLabels renders the four close outcomes (sk-489) for a push
// title. The wording is the one the notification feed summary uses
// (notifications/fanout.go#closeOutcomeLabels) — "done", "won't do",
// "duplicate", "superseded" — so the device and the feed say the same words
// about the same task.
//
// Keys are literals rather than issues.CloseOutcome* constants: internal/issues
// imports the notification stack, not the other way round, and outbox already
// keeps its own rendering of the status vocabulary (statusLabels) for the
// same reason. A value this map has not been taught degrades to
// itself — untranslated but truthful (INV-2), never silently dropped.
var closeOutcomeLabels = map[string]string{
	"done":       "done",
	"wont_do":    "won't do",
	"duplicate":  "duplicate",
	"superseded": "superseded",
}

// closeOutcomeLabel renders one outcome, falling back to the raw value.
func closeOutcomeLabel(outcome string) string {
	if lbl, ok := closeOutcomeLabels[outcome]; ok {
		return lbl
	}
	return outcome
}

// closingPayload is the shape T003 writes into events.payload for every
// CLOSING event: the outcome the issue ended up with, plus the previous one
// when a repeat close replaced it.
type closingPayload struct {
	CloseOutcome         string `json:"closeOutcome"`
	PreviousCloseOutcome string `json:"previousCloseOutcome"`
}

// closeOutcomeOf reads the outcome out of a closing event's payload. An absent
// or unparseable payload answers "" — a status push written before sk-489 (or
// by an older pod) carries no payload at all, and it must keep rendering
// exactly as it did.
func closeOutcomeOf(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p closingPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.CloseOutcome
}

// outcomeChangedHead renders the bold line of an issue.outcome_changed push:
// the pair "was → became", which is the whole news of that event.
//
// old_data / new_data are the JSON-encoded outcome strings the writer put on
// the event (issues/close_outcome.go#writeOutcomeChangedInTx). When either
// half is missing the pair cannot be shown, and half a pair on the bold line
// would read as a statement about the WRONG side — so the head degrades to
// naming the fact without the values.
func outcomeChangedHead(oldData, newData []byte) string {
	var was, now string
	_ = json.Unmarshal(oldData, &was)
	_ = json.Unmarshal(newData, &now)
	if was == "" || now == "" {
		return "🔀 Outcome changed"
	}
	return "🔀 Outcome: " + closeOutcomeLabel(was) + " → " + closeOutcomeLabel(now)
}

// delegatedStatusHead renders the title head for a delegated-status push from
// the event's new_data, and — since sk-489 — names the close outcome when the
// issue did NOT end up done (SC-011).
//
// Why only "not done" is named: "done" is the default outcome of every
// close, so naming it would lengthen the bold line of every closing push
// without telling the reader anything they did not already assume. The head is
// clipped by the OS around 40–50 characters, and that budget is spent on the
// case that is actually news.
// ⚠ THE TWO HALVES OF THE TITLE DEGRADE INDEPENDENTLY. The status is read from
// new_data, the outcome from payload, and these are DIFFERENT columns with different fates: a row
// written by a pod before the rollout carries the status and no payload, while a row with a
// corrupted new_data may carry a perfectly good payload. An early return
// on an unreadable status would swallow the second half along with the first: a
// "won't do" close would go out to the device as "🔄 Changed", i.e. lose
// exactly the news SC-011 was written for. That is why the outcome tail
// is appended to ANY title — the degraded one included.
func delegatedStatusHead(newData, payload []byte) string {
	// new_data is jsonb, and the status_changed writer marshals a plain Go
	// status string into it — so a well-formed event always decodes to
	// "in_progress" & co. Anything else (SQL NULL → empty bytes, or a
	// non-string payload from a wrong-shaped event) carries no status to
	// render, and a raw JSON fragment in the bold line would be worse than
	// saying nothing precise.
	head := statusUnknownEmoji + " Changed"
	var status string
	if err := json.Unmarshal(newData, &status); err == nil && status != "" {
		head = statusUnknownEmoji + " " + status
		if lbl, ok := statusLabels[status]; ok {
			head = lbl.emoji + " " + lbl.label
		}
		// A status this map hasn't been taught yet degrades to the raw value —
		// untranslated, but honest, and it still names the right thing. It keeps
		// the fallback glyph so the line still reads as a status change.
	}
	if outcome := closeOutcomeOf(payload); outcome != "" && outcome != "done" {
		head += " · " + closeOutcomeLabel(outcome)
	}
	return head
}

// pushHead renders the bold first line for a reason — the part of the title
// that says WHAT happened.
//
// THIS SWITCH IS THE THIRD IMPLEMENTATION OF THE EVENT CLASSIFICATION, and it
// is the one that is easy to forget. The registry (internal/notifications) and
// the SQL projection (migration 000045) answer "event × recipient → reason";
// this one answers "reason → what the person will see", and a reason it has not been
// taught falls through to the generic "Skein" head. That fallback is correct
// for a reason from a NEWER build than this binary, and a silent lie for a
// reason this build's own registry already serves — the device would say
// nothing about what happened. Hence the parity assertion in
// push_classification_parity_test.go: every reason the shared corpus produces
// for a push recipient must be named here.
//
// It is a FUNCTION rather than a block inside buildPushPayload because
// sk-303.14 added a second payload kind: the digest builds its own body,
// navigation and element list, but its bold line is the same line, produced
// from the same reason. Two copies of this switch would be two chances to
// teach one of them a new reason.
//
// ev is the event's issue projection. Two reasons read something out of it —
// delegated status (new_data + payload) and the outcome change (old_data +
// new_data); every other caller may pass the zero value, and the digest does.
// It is passed whole rather than column by column because the set of columns
// a head needs has already grown twice, and each growth otherwise rewrites
// every call site.
func pushHead(reason string, ev eventIssue) string {
	var head string
	switch notifications.ReasonKey(reason) {
	case notifications.ReasonKeyAssignment:
		head = "📥 Assigned to you"
	case notifications.ReasonKeyAccepted:
		head = "🤝 Accepted"
	case notifications.ReasonKeyReminder:
		head = "🔔 Reminder"
	case notifications.ReasonKeyMention:
		head = "💬 You were mentioned"
	case notifications.ReasonKeyDelegatedStatus:
		head = delegatedStatusHead(ev.newData, ev.payload)
	case notifications.ReasonKeyOutcomeChanged:
		head = outcomeChangedHead(ev.oldData, ev.newData)
	case notifications.ReasonKeyUnacceptedReminder:
		head = "⏳ Awaiting your acceptance"
	case notifications.ReasonKeyReviewReminder:
		head = "👀 Awaiting your review"
	default:
		// An unknown (future) reason: name the app rather than render a
		// title that lies about what happened.
		head = pushHeadUnknownReason
	}

	return head
}

// buildPushPayload renders the per-reason human-readable payload for the
// FIRST payload kind: one event, one issue. The digest — the second kind added
// by sk-303.14 — is built by marshalDigestPayload in digest.go; both share the
// bold line (pushHead) and the wire shape (pushPayload), and differ in body,
// navigation and in whether delivery re-checks anything per element.
//
// The payload's `category` field carries the REASON key. One vocabulary
// replaces two: the registry is what the user toggles, so it is what the
// device should be told. The contract explicitly allows this — "a category
// the client does not know must not break rendering".
//
// Layout is uniform across reasons:
//
//	title = <emoji> <what happened> · <who>
//	body  = <issueID> · <issue title>
//
// It follows how the OS draws a notification: title is the single bold line
// (Windows/Chromium clips it around 40–50 chars), body is up to three grey
// lines below. So the bold line carries what the eye scans for — the status
// and the actor — while the id anchors the first word of body. The app itself
// needs no naming: the icon and the origin line already say it.
//
// url is the issue card: appBaseURL + /issues/<id> when a base URL is
// configured, otherwise a relative /issues/<id> the service worker resolves
// against its own origin.
func buildPushPayload(reason string, ei eventIssue, appBaseURL string) pushPayload {
	head := pushHead(reason, ei)
	// The actor tail is dropped together with its separator — never leave a
	// dangling "·". A system actor (the reminder scheduler) is nobody worth
	// naming; an empty actor cannot be named at all.
	title := head
	if ei.actor != "" && ei.actorType != "system" {
		title = head + " · " + ei.actor
	}

	path := "/issues/" + ei.issueID
	url := path
	if base := strings.TrimRight(appBaseURL, "/"); base != "" {
		url = base + path
	}

	return pushPayload{
		Title:    title,
		Body:     ei.issueID + " · " + ei.title,
		URL:      url,
		Category: reason,
		IssueID:  ei.issueID,
	}
}
