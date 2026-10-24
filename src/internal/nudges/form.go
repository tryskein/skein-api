package nudges

// form.go — sk-303.14, task T018: SEGMENT CONSUMPTION, instance
// coordination and supersession.
//
// # The unit that consumes a segment
//
// INV-8: a person receives at most one reminder of each category per segment,
// and the promise is about the FACT OF BEING PUT INTO DELIVERY — the segment
// is consumed by a CREATED notification, not by a successful push to a device.
// Everything else follows:
//
//   - a retry batch on one already-formed notification does NOT consume the
//     segment: retries live in the delivery path and never touch this state;
//   - absence of registered devices DOES consume the segment: the notification
//     has been formed, and the delivery path will stamp the "no device"
//     outcome. Otherwise the read would spawn a new notification every tick —
//     an endless loop over a person who has nothing to deliver to;
//   - the promise must hold with several instances running at once and with a
//     crash between steps.
//
// # How the promise holds under contention — and why without locks
//
// A segment is consumed by ONE conditional statement on the pair's primary
// key: an insert with ON CONFLICT DO UPDATE ... WHERE next_due_at <= now and
// RETURNING. Zero returned rows means "the segment was already eaten by a
// neighbouring instance" — that is the coordination. Advisory locks and a
// leader election are deliberately absent: they would add state outside the
// database, and the pair's row is already unique and already protected by its
// primary key.
//
// Isolation is ordinary READ COMMITTED, and that is enough PRECISELY because
// of the statement's form: on conflict ON CONFLICT DO UPDATE re-reads the
// CURRENT version of the row, so the late instance sees the future deadline
// the neighbour already wrote and its WHERE is false. SERIALIZABLE would give
// the same result at the cost of retries on serialisation failure.
//
// # The clock is the DATABASE's ONLY (repair A/8, the hole that was closed)
//
// The READ COMMITTED reasoning above is correct and unrelated: what broke was
// not that, but an UNWRITTEN assumption about replica clock agreement. The
// segment condition was compared with a "now" computed by time.Now() of THE
// PROCESS doing the tick. A replica whose clock had run ahead by more than a
// segment saw the neighbour's future deadline as already expired and formed a
// SECOND reminder for the same segment — a direct violation of INV-8. The
// threshold is three hours at factory settings and fifteen minutes at the
// minimum setting; a clock skew of that size in a cluster is not exotic.
// Proven by a review probe: Form(…, now, now+3h) forms, then Form(…, now+5h, …)
// with the same candidate forms AGAIN.
//
// The fix is to compare against the DATABASE clock: `WHERE next_due_at <=
// now()`. The database clock is one for every instance, so the agreement
// assumption is not softened — it disappears entirely, and the INV-8 promise
// stops depending on NTP. For the same reason the tick's base-moment is read
// from the database (Store.Now), not from the process clock: "now" in the
// decision is still ONE, it just now has a single source for the cluster.
//
// # The time source is ONE — including the deadline guard (repair A/8, second
// round)
//
// The first fix moved the segment GATE onto the database clock, but the
// "new deadline is strictly after the formation moment" guard still compared
// against the moment the caller passed in. Two sources in one contract — and
// the gap between them opened exactly where the guard was supposed to close.
//
// The caller's moment is the database clock as read AT THE START OF THE TICK.
// While the tick runs, the database clock moves forward, and the schedule
// computes the new deadline from the OLD moment, returning it in (0, interval].
// If that interval is shorter than what the tick has already spent, by the
// time of the write the deadline LIES IN THE PAST on the database clock: the
// guard lets it through, the row is written, and the next tick's gate sees the
// segment as immediately expired — the very "a reminder every tick" the guard
// exists to stop. Shown by the second-round critic on an untouched tree:
// Form(cat, c, now = actually now − 2h, next = now + 1h) passes the guard and
// opens an already-expired segment.
//
// The fix is the same move already applied to consumeSegment: there is NO
// parameter. The guard compares against `now()` of ITS OWN transaction, and
// `now()` in PostgreSQL is transaction_timestamp(), the moment the transaction
// began, the same for every statement inside it. So the guard and the gate
// compare not "roughly the same time" but LITERALLY one value, and they have
// nothing to diverge on: the second source is gone from the signature.
//
// # How the promise holds on a crash between steps
//
// Consuming the segment, superseding earlier rows, writing the audit and
// creating the delivery row are ONE transaction. A crash between steps rolls
// everything back: the segment is not consumed, there is no notification, the
// audit is empty. Unprocessed addressees are picked up by the next tick. The
// reverse order — notification first, then the mark — would duplicate on a
// crash between them.
//
// # Frozen deadline: computing it by a shift is FORBIDDEN
//
// The new deadline is written by THE SAME statement and is computed ONLY by
// the schedule from the countdown base and the formation moment
// (review.Settings.NextReminderAt). An implementation of "take the previous
// deadline and add the interval" looks natural and produces the forbidden
// behaviour when the worker is behind: it would replay every slept-through
// segment and hand the restored worker a volley of reminders — exactly the
// hammering the whole table exists to prevent. NextReminderAt skips the missed
// marks and returns a strictly future deadline.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/notifications"
	"skein-api/internal/outbox"
	"skein-api/internal/review"
)

// FormResult — what happened to one candidate.
type FormResult struct {
	// Formed — the notification was formed and the segment consumed. False
	// means the segment had already been consumed by a neighbouring instance.
	Formed bool
	// Superseded — how many earlier undelivered rows of this pair were
	// completed with the supersession outcome. Counted by the caller AFTER
	// commit.
	Superseded int
	// Reason — the reason of the formed row; the caller needs it to label the
	// supersession-outcome measurement.
	Reason string
}

// Form forms a reminder of one category for one addressee and consumes their
// segment — in one transaction.
//
// `next` is the new frozen deadline, computed by the schedule from the
// countdown base and the tick moment. The tick moment is deliberately ABSENT
// from the signature (see "The time source is ONE" in the file header): both
// the deadline guard and the segment gate compare against `now()` of one and
// the same transaction, and a parameter that is not there cannot accidentally
// be returned as a second source.
func (s *Store) Form(
	ctx context.Context, cat review.Category, c Candidate, next time.Time,
) (FormResult, error) {
	reason, ok := notifications.DigestReasonForCategory(cat)
	if !ok {
		return FormResult{}, fmt.Errorf("%w: %q", review.ErrUnknownCategory, string(cat))
	}
	eventType, ok := notifications.DigestEventTypeForCategory(cat)
	if !ok {
		return FormResult{}, fmt.Errorf("%w: %q", review.ErrUnknownCategory, string(cat))
	}
	payload, err := notifications.MarshalDigestPayload(
		notifications.DigestPayload{IssueIDs: c.IssueIDs})
	if err != nil {
		return FormResult{}, err
	}

	res := FormResult{Reason: string(reason)}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FormResult{}, fmt.Errorf("beginning nudge transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Catch-up is forbidden, and its absence is checked here, not only by
	// trusting the schedule: a deadline not after the formation moment would
	// mean an immediately expired segment, i.e. a reminder every tick.
	dbNow, inFuture, err := deadlineIsAhead(ctx, tx, next)
	if err != nil {
		return FormResult{}, err
	}
	if !inFuture {
		return FormResult{}, fmt.Errorf(
			"nudges: next reminder %s is not after the formation moment %s "+
				"(database clock) for %s",
			next.Format(time.RFC3339Nano), dbNow.Format(time.RFC3339Nano), cat)
	}

	consumed, err := consumeSegment(ctx, tx, c.UserID, cat, next)
	if err != nil {
		return FormResult{}, err
	}
	if !consumed {
		// Segment eaten by a neighbour. Nothing written, rollback via defer.
		return res, nil
	}

	// Supersession BEFORE creating the new row, but in the same transaction:
	// otherwise supersession would hit the new row itself.
	superseded, err := outbox.SupersedeUndeliveredTx(ctx, tx, c.UserID, string(reason))
	if err != nil {
		return FormResult{}, err
	}

	eventID, err := writeDigestEvent(ctx, tx, eventType, c.UserID, payload)
	if err != nil {
		return FormResult{}, err
	}
	if err := insertDigestNotification(ctx, tx, c.UserID, eventID, eventType, string(reason)); err != nil {
		return FormResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return FormResult{}, fmt.Errorf("committing nudge transaction: %w", err)
	}

	res.Formed = true
	res.Superseded = superseded
	return res, nil
}

// deadlineIsAhead asks THE SAME TRANSACTION whether the new deadline lies
// strictly after the formation moment, and also returns the moment itself —
// for the refusal message.
//
// The moment is `now()`, i.e. transaction_timestamp(): it is fixed at the
// start of the transaction and is the same for EVERY statement inside it. So
// this question and the segment gate in consumeSegment below compare against
// one and the same value, not two close ones: uniqueness of the source here
// is not a convention, it is a property of the DBMS. There is no process clock
// in this function and nowhere for one to appear — exactly as in
// consumeSegment.
func deadlineIsAhead(ctx context.Context, tx pgx.Tx, next time.Time) (time.Time, bool, error) {
	var (
		now      time.Time
		inFuture bool
	)
	if err := tx.QueryRow(ctx,
		`SELECT now(), $1::timestamptz > now()`, next).Scan(&now, &inFuture); err != nil {
		return time.Time{}, false, fmt.Errorf("reading the database clock: %w", err)
	}
	return now.UTC(), inFuture, nil
}

// consumeSegment — conditional consumption of the "addressee and category"
// pair's segment.
//
// Returns true only if the row was actually written by this call. Three cases
// are distinguished by the statement itself and need no Go branch:
//
//	no row                       → insert, the segment is ours;
//	row exists, deadline expired → update, the segment is ours;
//	row exists, deadline live    → WHERE is false, zero rows, the segment is
//	                               not ours.
//
// The third case covers both a race of two instances (the neighbour already
// wrote a future deadline) and the clock going backwards (the moment has not
// arrived yet — the segment is not declared expired, there is no flood).
//
// THE COMPARISON IS AGAINST THE DATABASE CLOCK (now()), not the caller's
// moment. The caller's moment does not participate in the condition at all —
// see the file header: while it did, a replica with a runaway clock formed a
// second reminder for the same segment. It is not here and not in the
// signature: a parameter that is not there cannot accidentally be returned
// into the condition, and the absence is visible at the call site.
func consumeSegment(
	ctx context.Context, tx pgx.Tx, userID uuid.UUID, cat review.Category, next time.Time,
) (bool, error) {
	var returned uuid.UUID
	err := tx.QueryRow(ctx, `
INSERT INTO reminder_rate_limit (user_id, category, last_formed_at, next_due_at, updated_at)
VALUES ($1, $2, now(), $3, now())
ON CONFLICT (user_id, category) DO UPDATE
   SET last_formed_at = now(),
       next_due_at    = EXCLUDED.next_due_at,
       updated_at     = now()
 WHERE reminder_rate_limit.next_due_at <= now()
RETURNING user_id`, userID, string(cat), next).Scan(&returned)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("consuming %s reminder segment: %w", cat, err)
	}
	return true, nil
}

// writeDigestEvent writes an audit record of a FORMED reminder.
//
// INV-10: the record is named after the verifiable fact. The type is taken
// from the registry (nudge.*_digest — "digest formed"), not assembled as a
// literal, and the word "sent" is not in it: delivery is asynchronous and may
// end in failure.
//
// The record's form is USER-SCOPED: entity "user", the addressee's id, the
// project empty. A digest speaks of several issues at once and is addressed
// to a person, not a project; it has no single issue that could stamp the
// record, and picking one of them would assign the digest a random project.
// Issue ids live in the payload — the delivery path reads them from there,
// re-checking each one (INV-14).
//
// The actor is the system: the tick has no person, no agent, no session.
//
// ⚠ CORRECTED RATIONALE (repair A/8). The commit message that added this form
// claimed the T015 delivery path depends on it. That claim is false:
// outbox/digest.go reads only the payload from the event and never touches
// the form. The form is taken from T008 — issues.WritePrivateReviewEvent —
// and is held by exactly the same requirement: a private-ledger row must not
// reach any journal reader. Its necessity is pinned by
// nudges.TestDigestAudit_PrivateFormAndRegisteredType (before the repair
// nobody checked the form: a mutation of entity_type 'user'→'issue' was green
// on the whole tree).
//
// The event type is notifications.PrivateEventType, not a string: the live
// feed's user subject is NOT closed by the form, it is closed by the private-
// types registry, and a value of this type is issued only by registration in
// it. While a string was accepted here, both digest types were forgotten from
// the registry — and the author's issue list went out on
// skein.user.<addressee>.*, which the bridge distributes to the addressee and
// EVERY administrator.
func writeDigestEvent(
	ctx context.Context, tx pgx.Tx, eventType notifications.PrivateEventType,
	userID uuid.UUID, payload []byte,
) (int64, error) {
	if eventType.IsZero() {
		return 0, fmt.Errorf("writing digest event for %s: the event type is not "+
			"registered as private", userID)
	}
	var id int64
	if err := tx.QueryRow(ctx, `
INSERT INTO events (type, actor, actor_user_id, actor_session_id, actor_type,
                    entity_type, entity_id, project_id, payload)
VALUES ($1, 'system', NULL, NULL, 'system',
        'user', $2, NULL, $3::jsonb)
RETURNING id`, eventType.String(), userID.String(), payload).Scan(&id); err != nil {
		return 0, fmt.Errorf("writing digest event: %w", err)
	}
	return id, nil
}

// insertDigestNotification creates the delivery row — PUSH-ONLY.
//
// feed_channel is DERIVED FROM THE REASONS REGISTRY, not written here as a
// literal (repair A/8). The `false` literal was a handwritten copy of a fact
// already recorded in the registry (Feed: ChannelSpec{Applicable: false});
// there was no check between the copies, and a false→true mutation was green
// on the whole tree — i.e. INV-7 "reminders do not create notification-feed
// entries" was held by nothing. Deriving from the registry removes the second
// copy entirely: the decision remains exactly one, and its preservation is
// checked by the registry gate
// (outbox.TestPrivateEventTypes_NeverReachTheFeed), not by attentiveness.
//
// The row is created already in the pending-send state with the common path's
// deadline: retries and the ultimate deadline are inherited, not invented
// again.
//
// ON CONFLICT (user_id, event_id) DO NOTHING — protection against a repeat:
// each formation has its own event, so a collision is not expected, but the
// statement stays idempotent.
func insertDigestNotification(
	ctx context.Context, tx pgx.Tx, userID uuid.UUID, eventID int64,
	eventType notifications.PrivateEventType, reason string,
) error {
	spec, ok := notifications.LookupReason(notifications.ReasonKey(reason))
	if !ok {
		return fmt.Errorf("inserting digest notification: reason %q missing from the registry",
			reason)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO notifications (
    user_id, event_id, type, project_id,
    reason, feed_channel,
    push_state, push_next_attempt_at, push_deadline_at,
    card_resolved)
VALUES ($1, $2, $3, NULL,
        $4, $6::boolean,
        'pending', now(), now() + ($5::bigint * interval '1 second'),
        true) -- no Watching card ever (sk-622.21 US-4); resolved = decided (K6)
ON CONFLICT (user_id, event_id) DO NOTHING`,
		userID, eventID, eventType.String(), reason,
		int64(notifications.PushDeliveryDeadline/time.Second),
		spec.Feed.Applicable); err != nil {
		return fmt.Errorf("inserting digest notification: %w", err)
	}
	return nil
}
