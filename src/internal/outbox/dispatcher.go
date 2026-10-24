// Package outbox implements the transactional outbox dispatcher for
// Skein's live-feed architecture (R3.2). A single goroutine polls the
// events table for rows where published_at IS NULL, publishes them to
// NATS Core under the skein.events.* subject scheme, and marks them
// published in the same database transaction.
//
// Design:
//
//   - Source of truth is the events table (audit log + outbox in one).
//     The published_at column added in migration 000012 is the only
//     schema change relative to R3.1.
//   - One goroutine per replica. SELECT FOR UPDATE SKIP LOCKED makes
//     multi-replica safe; a single replica is enough for typical load.
//   - At-least-once delivery. A crash between nc.Publish and tx.Commit
//     leaves rows unpublished; the next tick re-sends. Subscribers MUST
//     dedup by event id.
//   - Drain-on-full: when a tick fills its batch, the next tick runs
//     immediately (no sleep) so a backlog catches up at full speed.
//   - Exponential backoff on error: 250ms → 10s, doubling each failure,
//     reset on success. Keeps a NATS rolling restart from spamming logs.
//
// Hard constraints respected by this package:
//
//   - No mutation of existing event row fields (type, entity_type,
//     payload, etc). The outbox is read-only on those columns.
//   - No NATS-required dependency on application boot: when NATS_URL is
//     empty, main.go skips constructing the dispatcher and the API runs
//     normally — events accumulate until a deploy with NATS_URL drains
//     them.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"skein-api/internal/push"
)

// tickError is the typed wrapper used inside dispatcher.tick so the
// classification label on errorsTotal is bound to the call site rather
// than parsed out of the error message at runtime. classifyError unwraps
// this type via errors.As; any error that does not carry a tickError
// falls back to reasonOther.
type tickError struct {
	reason string
	err    error
}

func (e *tickError) Error() string { return fmt.Sprintf("%s: %v", e.reason, e.err) }
func (e *tickError) Unwrap() error { return e.err }

// reason* constants are the only legal values of the errorsTotal "reason"
// label. Keeping them defined here (and used both in the wrapper sites
// and in metrics.go's docstring) makes the cardinality bounded and easy
// to audit.
const (
	reasonTxBegin     = "tx_begin"
	reasonDBQuery     = "db_query"
	reasonScan        = "scan"
	reasonEnvelope    = "envelope"
	reasonNATSPublish = "nats_publish"
	reasonNATSFlush   = "nats_flush"
	reasonDBUpdate    = "db_update"
	reasonDBCommit    = "db_commit"
	reasonOther       = "other"
)

// dbErrorReasons enumerates the reason* values that imply a DB-side
// failure. Used by Run to skip the lag-gauge refresh in the error path
// (no point hitting a DB that just failed) while still keeping it fresh
// during NATS outages.
var dbErrorReasons = map[string]struct{}{
	reasonTxBegin:  {},
	reasonDBQuery:  {},
	reasonScan:     {},
	reasonDBUpdate: {},
	reasonDBCommit: {},
}

func isDBError(reason string) bool {
	_, ok := dbErrorReasons[reason]
	return ok
}

// Default tuning constants. Override via Options.
const (
	defaultPollInterval = 250 * time.Millisecond
	defaultMaxBackoff   = 10 * time.Second
	defaultBatchSize    = 100
	// defaultFlushTimeout bounds nc.FlushWithContext per batch. Larger
	// than PollInterval so a transient NATS slowdown doesn't classify
	// as a hard failure on the first tick, but small enough that a
	// shutdown initiated mid-flush surfaces ctx cancel quickly. 2s is
	// generous for healthy reconnect windows yet keeps shutdown snappy.
	defaultFlushTimeout = 2 * time.Second
)

// Options configures dispatcher tuning. Zero values get sensible defaults.
type Options struct {
	// PollInterval is the sleep between empty/partial ticks. The dispatcher
	// drains immediately (no sleep) after a full batch, so this only kicks
	// in when the queue is mostly empty. Default 250ms.
	PollInterval time.Duration

	// MaxBackoff caps the exponential backoff applied on tick failure.
	// On error the next sleep doubles up to this ceiling, then resets to
	// PollInterval after the next successful tick. Default 10s.
	MaxBackoff time.Duration

	// BatchSize is the upper bound on rows fetched per tick. Used in the
	// SELECT ... LIMIT clause and as the threshold for drain-immediately
	// behaviour. Default 100. Higher values lengthen the FOR UPDATE
	// SKIP LOCKED transaction; lower values pay more round-trips. 100
	// is well below where lock duration becomes a vacuum concern.
	BatchSize int

	// FlushTimeout bounds nc.FlushWithContext per batch. Default 2s.
	FlushTimeout time.Duration

	// Rows supplies the phase-2 neutral rows of issue events (sk-622.21,
	// T027, K16): the envelope field `row`. nil → envelopes carry no row
	// (the phase-1 shape); production wiring passes issues.NeutralRowSource
	// (cmd/api/main.go).
	//
	// FAIL-CLOSED, deliberately (found in review of sk-622.21):
	// an error from NeutralRowsJSON fails the WHOLE tick, not just the
	// field. tickEvents returns tickError{reason: "db_query"} and rolls the
	// tick's transaction back — no event of the batch is published, none is
	// stamped published, the SKIP LOCKED claims are released — and tick()
	// returns before tickNotifications, so personal notifications wait too.
	// Run counts the failure in skein_outbox_publish_errors_total{reason="db_query"}
	// and backs off; the next tick claims the same events again. Nothing is
	// lost, but while the row query keeps failing the whole outbox stands.
	// The alternative — publish the batch without `row` — was not taken:
	// by contract `row` is absent only where no row can exist (a non-issue
	// event, a private event, a deleted issue, an event outside the issue's
	// project), and degrading every issue event of an outage to the phase-1
	// shape would hide a broken query behind working delivery. An issue
	// that is simply absent from the answer is NOT an error: its envelope
	// goes out without `row`.
	Rows NeutralRowSource
}

// NeutralRowSource yields the reader-independent list rows of issues for
// the envelope field `row` (sk-622.21, T027, K16 / K17). Spelled in stdlib
// and pgx types only, so this package does not import `issues` (K16); the
// implementation is issues.NeutralRowSource.
//
// NeutralRowsJSON answers for every id it finds, in ONE statement run on
// tx (the tick's transaction): rows maps issue id → the JSON of
// issues.NeutralRow, projects maps issue id → the issue's project. An id
// without an issue is absent from both maps. The rows are the issues'
// state when the statement runs — never older than the events of the
// tick, which became visible only after their commits.
type NeutralRowSource interface {
	NeutralRowsJSON(ctx context.Context, tx pgx.Tx, ids []string) (
		rows map[string]json.RawMessage, projects map[string]uuid.UUID, err error)
}

// PushSender is the subset of *push.Sender the dispatcher needs to deliver a
// web-push. Declared as an interface so the dispatcher is unit-testable with a
// fake sender (no VAPID / no network).
type PushSender interface {
	// Enabled reports whether push delivery is wired (VAPID configured). When
	// false the dispatcher skips the push path entirely.
	Enabled() bool
	// Send delivers payload to one subscription. Errors are best-effort —
	// the dispatcher logs and continues.
	Send(ctx context.Context, sub push.Subscription, payload []byte) error
}

// PushSubscriptionStore is the subset of *push.Store the dispatcher needs to
// load a recipient's registered devices.
type PushSubscriptionStore interface {
	ListForUser(ctx context.Context, userID uuid.UUID) ([]push.Subscription, error)
}

// Dispatcher is the long-lived outbox poller. Construct via New and run
// via Run; the goroutine exits cleanly when its ctx is cancelled.
type Dispatcher struct {
	pool *pgxpool.Pool
	nc   *nats.Conn
	opts Options
}

// New constructs a Dispatcher. Both pool and nc must be non-nil — when
// NATS_URL is unset, main.go is responsible for not constructing the
// dispatcher at all rather than passing nil here.
//
// sk-303.13 / T010: the dispatcher no longer knows anything about web push.
// Push has its own runner (PushRunner) with its own lifetime, precisely
// because a deployment without NATS used to lose its pushes as collateral.
func New(pool *pgxpool.Pool, nc *nats.Conn, opts Options) *Dispatcher {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = defaultMaxBackoff
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.FlushTimeout <= 0 {
		opts.FlushTimeout = defaultFlushTimeout
	}
	return &Dispatcher{pool: pool, nc: nc, opts: opts}
}

// Run blocks until ctx is cancelled. Returns nil on graceful shutdown
// (ctx.Done) — the caller may log and move on. All errors are handled
// inside the loop with exponential backoff; Run never returns an error
// for callers to check.
//
// Lifecycle (mirrors search.RunBackfillWorker):
//
//  1. Tick once immediately so backlog drains without waiting PollInterval.
//  2. Loop: tick → on error log+backoff, on success either drain (full
//     batch) or sleep PollInterval.
//  3. ctx.Done() → return.
//
// Concurrency: this method is single-threaded. Do not call Run from
// multiple goroutines on the same Dispatcher.
func (d *Dispatcher) Run(ctx context.Context) error {
	slog.InfoContext(ctx, "outbox dispatcher started",
		"pollIntervalMs", d.opts.PollInterval.Milliseconds(),
		"batchSize", d.opts.BatchSize,
		"maxBackoffMs", d.opts.MaxBackoff.Milliseconds(),
	)
	defer slog.InfoContext(ctx, "outbox dispatcher stopped")

	backoff := d.opts.PollInterval
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		n, err := d.tick(ctx)
		if err != nil {
			// ctx cancellation surfaces as an error from pgx / nats; treat
			// as a graceful shutdown rather than a logged failure.
			if errors.Is(err, context.Canceled) {
				return nil
			}
			reason := classifyError(err)
			errorsTotal.WithLabelValues(reason).Inc()
			slog.ErrorContext(ctx, "outbox tick failed",
				"reason", reason,
				"backoffMs", backoff.Milliseconds(),
				"error", err.Error(),
			)
			// Keep the lag gauge fresh during NATS / envelope outages so
			// external alerts (skein_outbox_lag_seconds > 60) fire on
			// time. Skip the refresh for DB-side failures — the gauge
			// query would just fail too, costing one more error.
			if !isDBError(reason) {
				d.refreshLagGauge(ctx)
			}
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff *= 2
			if backoff > d.opts.MaxBackoff {
				backoff = d.opts.MaxBackoff
			}
			continue
		}

		// Success — reset backoff and refresh the lag gauge so dashboards
		// catch up even when the queue is mostly empty.
		backoff = d.opts.PollInterval
		d.refreshLagGauge(ctx)

		// Drain immediately when the batch saturates BatchSize: there are
		// likely more rows queued and we want sub-second tail latency.
		if n >= d.opts.BatchSize {
			continue
		}

		if !sleepCtx(ctx, d.opts.PollInterval) {
			return nil
		}
	}
}

// tick processes one batch in one transaction. Returns the number of
// rows RETIRED from the queue (events + notifications combined) — which
// is not the same as the number published: a private event row (T008) is
// retired by the terminal stamp without ever going on the wire. The Run
// loop consumes this number only to decide whether the batch saturated
// and the next tick should drain immediately, and for that «claimed and
// stamped» is exactly the right measure. Any error short-circuits the
// tick — the deferred rollback releases the SKIP LOCKED rows so the next
// tick (or another replica) retries from scratch.
//
// Steps:
//  1. BEGIN tx (default isolation; SKIP LOCKED gives the needed safety).
//  2. SELECT ... WHERE published_at IS NULL ORDER BY id
//     FOR UPDATE SKIP LOCKED LIMIT BatchSize.
//  3. For each row: build envelope, nc.Publish.
//  4. nc.FlushWithContext — required, otherwise Publish is fire-and-forget
//     over the local TCP buffer and a crash mid-tick can lose data.
//  5. UPDATE events SET published_at = now() WHERE id = ANY($1).
//  6. COMMIT.
//
// R3.3: after the events drain commits, we run a SECOND transaction
// scanning notifications WHERE published_at IS NULL and publish each row
// onto skein.user.{uid}.notification. Same SKIP LOCKED + same-tx mark
// pattern. Two separate transactions (not one big union) so a NATS
// outage on the notifications channel doesn't roll back already-published
// events.
func (d *Dispatcher) tick(ctx context.Context) (int, error) {
	eventsCount, err := d.tickEvents(ctx)
	if err != nil {
		return 0, err
	}
	notifCount, err := d.tickNotifications(ctx)
	if err != nil {
		// Surface as a tick error so the Run loop applies backoff and the
		// errorsTotal counter increments. Events already committed in the
		// first phase are safe — they keep their published_at = now() and
		// won't be re-sent.
		return eventsCount, err
	}
	return eventsCount + notifCount, nil
}

// tickEvents handles the events-table drain. Mirrors the original R3.2a
// implementation; extracted from tick() so the new notifications drain
// can sit beside it without one giant function.
//
// sk-303.14 / T008 — private rows. The SELECT deliberately keeps NO type
// filter: the queue's selection predicate stays «published_at IS NULL»
// for every row, and the private types are separated AFTER the claim,
// inside the loop. That is the whole point of the design — a row skipped
// by the SELECT would never be claimed, never be stamped, and would sit
// at the head of the queue forever, inflating skein_outbox_lag_seconds
// and squeezing ordinary events out of every batch. Here a private row
// is claimed like any other, is NOT published, and receives the SAME
// terminal stamp in the SAME transaction as the published ones.
func (d *Dispatcher) tickEvents(ctx context.Context) (int, error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, &tickError{reason: reasonTxBegin, err: fmt.Errorf("tx begin: %w", err)}
	}
	// Idempotent — Rollback is a no-op after Commit.
	defer tx.Rollback(ctx) //nolint:errcheck

	// sk-21 — JOIN issues / comments / projects to enrich the envelope
	// with actor identity, the entity's human-readable title, and the
	// project's idPrefix. Browser subscribers (the /activity ticker)
	// previously got a "system" actor + empty title because the
	// envelope only carried raw events-row columns. The comment JOIN
	// uses the same CASE-wrapped uuid cast as activityListSQL (sk-17
	// review fix C1) so the planner doesn't fail on non-comment rows
	// whose entity_id is a hierarchical issue id like "demo-42".
	//
	// FOR UPDATE OF e SKIP LOCKED — lock ONLY the events row, not the
	// JOINed parent issue / project rows. The locking semantic the
	// dispatcher relies on (multi-replica safe per-row claim) applies
	// to events.published_at; we never write to issues / projects from
	// this path.
	rows, err := tx.Query(ctx, `
SELECT e.id, e.occurred_at, e.type, e.entity_type, e.entity_id, e.project_id, e.field,
       e.payload, e.old_data, e.new_data,
       e.actor, e.actor_type, e.actor_user_id,
       i.title  AS issue_title,
       ci.title AS comment_issue_title,
       prj.id_prefix AS project_id_prefix
  FROM events e
  LEFT JOIN issues i ON e.entity_type = 'issue' AND i.id = e.entity_id
  LEFT JOIN comments c ON c.id = (CASE WHEN e.entity_type = 'comment'
                                        THEN e.entity_id::uuid ELSE NULL END)
  LEFT JOIN issues ci ON ci.id = c.issue_id
  LEFT JOIN projects prj ON prj.id = e.project_id
 WHERE e.published_at IS NULL
 ORDER BY e.id
   FOR UPDATE OF e SKIP LOCKED
 LIMIT $1`, d.opts.BatchSize)
	if err != nil {
		return 0, &tickError{reason: reasonDBQuery, err: fmt.Errorf("db query: %w", err)}
	}

	type pub struct {
		id      int64
		subject string
		body    []byte
	}
	// batch — rows that will go onto the bus.
	// ids  — EVERY claimed row, private ones included: the terminal stamp
	//        is given to both, or private rows would stay in the queue.
	batch := make([]pub, 0, d.opts.BatchSize)
	// frames — personal reminder frames of the claimed `reminder.*` rows
	// (sk-622.21 T018, K14). Published in the same tick and under the same
	// flush as the batch, before the published_at stamp commits: a failed
	// publish or flush rolls the tick back and the next tick republishes
	// event and frame together (at-least-once; the frame repeats with the
	// same firingId, which the receiver deduplicates on).
	frames := make([]pub, 0)
	ids := make([]int64, 0, d.opts.BatchSize)
	suppressed := 0

	// The claimed rows are read out completely first: the phase-2 neutral
	// rows (T027) come from a second statement on the same transaction, and
	// a transaction runs one statement at a time.
	type claimed struct {
		id                int64
		occurredAt        time.Time
		evtType           string
		entityType        string
		entityID          string
		projectID         *uuid.UUID
		field             *string
		payload           []byte
		oldData           []byte
		newData           []byte
		actor             string
		actorType         string
		actorUserID       *uuid.UUID
		issueTitle        *string
		commentIssueTitle *string
		projectIDPrefix   *string
	}
	claimedRows := make([]claimed, 0, d.opts.BatchSize)
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.occurredAt, &c.evtType, &c.entityType, &c.entityID,
			&c.projectID, &c.field, &c.payload, &c.oldData, &c.newData,
			&c.actor, &c.actorType, &c.actorUserID,
			&c.issueTitle, &c.commentIssueTitle, &c.projectIDPrefix); err != nil {
			rows.Close()
			return 0, &tickError{reason: reasonScan, err: fmt.Errorf("db scan: %w", err)}
		}
		claimedRows = append(claimedRows, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, &tickError{reason: reasonDBQuery, err: fmt.Errorf("db rows: %w", err)}
	}

	// sk-622.21 T027 (K16): the neutral rows of every issue event of the
	// tick, in ONE statement however many events there are. Private rows
	// are not published, so they ask for no row.
	var (
		neutralRows     map[string]json.RawMessage
		neutralProjects map[string]uuid.UUID
	)
	if d.opts.Rows != nil {
		issueIDs := make([]string, 0, len(claimedRows))
		seen := make(map[string]struct{}, len(claimedRows))
		for _, c := range claimedRows {
			if c.entityType != "issue" || isPrivateEventType(c.evtType) {
				continue
			}
			if _, dup := seen[c.entityID]; dup {
				continue
			}
			seen[c.entityID] = struct{}{}
			issueIDs = append(issueIDs, c.entityID)
		}
		if len(issueIDs) > 0 {
			var err error
			neutralRows, neutralProjects, err = d.opts.Rows.NeutralRowsJSON(ctx, tx, issueIDs)
			if err != nil {
				// Fail-closed: the whole tick fails and rolls back,
				// notifications included — see Options.Rows.
				return 0, &tickError{reason: reasonDBQuery, err: fmt.Errorf("neutral rows: %w", err)}
			}
		}
	}

	for _, c := range claimedRows {
		id := c.id
		evtType := c.evtType
		entityType := c.entityType
		entityID := c.entityID
		projectID := c.projectID
		// T008 — a private row is taken out of the queue by a stamp and
		// is NOT published. The cut sits before subject and envelope
		// construction on purpose: a private row has neither an address
		// nor a body at all, so there is nothing to leak through an
		// error path or through the dispatcher's log.
		if isPrivateEventType(evtType) {
			ids = append(ids, id)
			suppressed++
			continue
		}
		var fieldStr string
		if c.field != nil {
			fieldStr = *c.field
		}
		subj := BuildSubject(SubjectInput{
			EventType:  evtType,
			EntityType: entityType,
			EntityID:   entityID,
			ProjectID:  projectID,
			Field:      fieldStr,
		})
		// sk-21 — pick the right title source based on entity_type:
		// issue events get the joined issue.title; comment events
		// surface the parent issue's title (comments themselves have
		// no title); other entity types have no title to enrich with.
		enrich := EnvelopeEnrichment{
			Actor:     c.actor,
			ActorType: c.actorType,
		}
		if c.actorUserID != nil {
			s := c.actorUserID.String()
			enrich.ActorUserID = &s
		}
		switch entityType {
		case "issue":
			if c.issueTitle != nil {
				enrich.EntityTitle = *c.issueTitle
			}
			// T027 (INV-17): the row goes only onto an event of the issue's
			// own project. Such an event is delivered to that project's
			// readers, who see the issue anyway; an event filed under any
			// other project (or under none) gets no row, so the envelope
			// cannot carry an issue past its readers' horizon.
			if row, ok := neutralRows[entityID]; ok && projectID != nil &&
				neutralProjects[entityID] == *projectID {
				enrich.Row = row
			}
		case "comment":
			if c.commentIssueTitle != nil {
				enrich.EntityTitle = *c.commentIssueTitle
			}
		}
		if c.projectIDPrefix != nil {
			enrich.ProjectIDPrefix = *c.projectIDPrefix
		}
		body, err := buildEnvelope(id, c.occurredAt, evtType, entityType, entityID,
			projectID, c.payload, c.oldData, c.newData, enrich)
		if err != nil {
			return 0, &tickError{reason: reasonEnvelope, err: fmt.Errorf("envelope: %w", err)}
		}
		batch = append(batch, pub{id: id, subject: subj, body: body})
		ids = append(ids, id)
		if fsubj, fbody, ok := BuildReminderFrame(evtType, entityID, projectID, c.payload); ok {
			frames = append(frames, pub{id: id, subject: fsubj, body: fbody})
		} else if strings.HasPrefix(evtType, reminderEventPrefix) {
			// An event from before the owner joined the payload: it keeps
			// its project delivery, and only the frame is impossible.
			slog.WarnContext(ctx, "outbox: reminder event carries no addressable owner; personal frame skipped",
				"eventId", id, "eventType", evtType)
		}
	}

	// Empty is about CLAIMED rows, not published ones: a tick in which
	// every row turned out private still did its job — they need a stamp.
	if len(ids) == 0 {
		// Nothing to do. Commit the empty tx (releases the implicit
		// snapshot) and return 0 so Run sleeps for PollInterval.
		if err := tx.Commit(ctx); err != nil {
			return 0, &tickError{reason: reasonDBCommit, err: fmt.Errorf("tx commit empty: %w", err)}
		}
		return 0, nil
	}

	// The bus is touched only if there is something to publish. A batch
	// of private rows alone must not depend on NATS being up: it needs a
	// stamp, not a network.
	if len(batch) > 0 {
		// Publish to NATS. If any Publish errors, abort the whole batch —
		// rollback frees the SKIP LOCKED rows so the next tick retries cleanly.
		for _, p := range batch {
			if err := d.nc.Publish(p.subject, p.body); err != nil {
				return 0, &tickError{reason: reasonNATSPublish, err: fmt.Errorf("nats publish %s id=%d: %w", p.subject, p.id, err)}
			}
		}
		for _, p := range frames {
			if err := d.nc.Publish(p.subject, p.body); err != nil {
				return 0, &tickError{reason: reasonNATSPublish, err: fmt.Errorf("nats publish reminder frame id=%d: %w", p.id, err)}
			}
		}

		// Flush blocks until the local TCP buffer drains to the NATS server.
		// Without this, a crash between Publish and Commit could lose
		// messages even though Publish returned nil. We use FlushWithContext
		// (instead of FlushTimeout) so a NATS outage during shutdown surfaces
		// ctx cancellation immediately rather than after the full timeout.
		flushCtx, cancelFlush := context.WithTimeout(ctx, d.opts.FlushTimeout)
		flushErr := d.nc.FlushWithContext(flushCtx)
		cancelFlush()
		if flushErr != nil {
			return 0, &tickError{reason: reasonNATSFlush, err: fmt.Errorf("nats flush: %w", flushErr)}
		}
	}

	if _, err := tx.Exec(ctx, `
UPDATE events SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, &tickError{reason: reasonDBUpdate, err: fmt.Errorf("db update: %w", err)}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, &tickError{reason: reasonDBCommit, err: fmt.Errorf("tx commit: %w", err)}
	}

	// Metrics + log AFTER commit so a rollback path doesn't inflate
	// counters with phantom publishes.
	//
	// batchSize remains a histogram of what was PUBLISHED in the tick (its
	// description says exactly that, and "empty ticks are not observed"),
	// so a tick of private rows alone does not enter it: observing zero
	// would turn "we published nothing" into "we published zero" and
	// shift the distribution used to judge batch saturation.
	if len(batch) > 0 {
		batchSize.Observe(float64(len(batch)))
	}
	uniqueSubjects := make(map[string]struct{}, len(batch))
	for _, p := range batch {
		publishedTotal.WithLabelValues(subjectRoot(p.subject)).Inc()
		uniqueSubjects[p.subject] = struct{}{}
	}
	slog.InfoContext(ctx, "outbox tick",
		"count", len(batch),
		"suppressed", suppressed,
		"subjectsUnique", len(uniqueSubjects),
		"firstId", ids[0],
		"lastId", ids[len(ids)-1],
	)
	return len(ids), nil
}

// tickNotifications drains the notifications table — every row with
// published_at IS NULL is published onto skein.user.{user_id}.notification
// and marked published in the same transaction. Mirrors tickEvents but
// emits a single per-user subject instead of a per-event scheme.
//
// Body shape: NotificationEnvelope (defined in envelope.go) — the
// dispatcher does NOT include the original event payload here; the
// frontend invalidates ['notifications'] queries and refetches the inbox
// over REST. Keeping the wire body small keeps multi-recipient fan-out
// cheap (1 KB envelope × N users) and avoids surfacing event payload
// data on a per-recipient channel where authorization rules already
// imply the event is allowed for this user.
func (d *Dispatcher) tickNotifications(ctx context.Context) (int, error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, &tickError{reason: reasonTxBegin, err: fmt.Errorf("tx begin (notifications): %w", err)}
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// feed_channel is the whole of the read filter now (sk-303.13 / US-2).
	// A push-only row belongs to no feed, and the live stream IS a feed: the
	// contract says such a row "is not published" into it at all, rather than
	// being published with a flag the BFF's closed envelope schema would drop
	// on the floor. Selecting it here is therefore the leak, not the bridge.
	rows, err := tx.Query(ctx, `
SELECT id, user_id, event_id, type, project_id, created_at
  FROM notifications
 WHERE published_at IS NULL
   AND feed_channel
 ORDER BY id
   FOR UPDATE SKIP LOCKED
 LIMIT $1`, d.opts.BatchSize)
	if err != nil {
		return 0, &tickError{reason: reasonDBQuery, err: fmt.Errorf("db query (notifications): %w", err)}
	}

	type notifPub struct {
		id      int64
		subject string
		body    []byte
	}
	batch := make([]notifPub, 0, d.opts.BatchSize)
	ids := make([]int64, 0, d.opts.BatchSize)

	for rows.Next() {
		var (
			id        int64
			userID    uuid.UUID
			eventID   int64
			eventType string
			projectID *uuid.UUID
			createdAt time.Time
		)
		if err := rows.Scan(&id, &userID, &eventID, &eventType, &projectID, &createdAt); err != nil {
			rows.Close()
			return 0, &tickError{reason: reasonScan, err: fmt.Errorf("db scan (notifications): %w", err)}
		}
		body, err := buildNotificationEnvelope(id, userID, eventID, eventType, projectID, createdAt)
		if err != nil {
			rows.Close()
			return 0, &tickError{reason: reasonEnvelope, err: fmt.Errorf("envelope (notification %d): %w", id, err)}
		}
		batch = append(batch, notifPub{
			id:      id,
			subject: fmt.Sprintf("skein.user.%s.notification", userID.String()),
			body:    body,
		})
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, &tickError{reason: reasonDBQuery, err: fmt.Errorf("db rows (notifications): %w", err)}
	}

	if len(batch) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return 0, &tickError{reason: reasonDBCommit, err: fmt.Errorf("tx commit empty (notifications): %w", err)}
		}
		return 0, nil
	}

	for _, p := range batch {
		if err := d.nc.Publish(p.subject, p.body); err != nil {
			return 0, &tickError{reason: reasonNATSPublish, err: fmt.Errorf("nats publish %s id=%d: %w", p.subject, p.id, err)}
		}
	}

	flushCtx, cancelFlush := context.WithTimeout(ctx, d.opts.FlushTimeout)
	flushErr := d.nc.FlushWithContext(flushCtx)
	cancelFlush()
	if flushErr != nil {
		return 0, &tickError{reason: reasonNATSFlush, err: fmt.Errorf("nats flush (notifications): %w", flushErr)}
	}

	if _, err := tx.Exec(ctx, `
UPDATE notifications SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, &tickError{reason: reasonDBUpdate, err: fmt.Errorf("db update (notifications): %w", err)}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, &tickError{reason: reasonDBCommit, err: fmt.Errorf("tx commit (notifications): %w", err)}
	}

	for _, p := range batch {
		publishedTotal.WithLabelValues(subjectRoot(p.subject)).Inc()
	}
	slog.InfoContext(ctx, "outbox notifications tick",
		"count", len(batch),
		"firstId", batch[0].id,
		"lastId", batch[len(batch)-1].id,
	)

	return len(batch), nil
}

// refreshLagGauge runs a cheap aggregate against the partial index and
// publishes the result on the lagSeconds gauge. Errors are logged at
// WARN and swallowed — gauge accuracy is non-critical relative to
// dispatch progress, and the next tick will retry.
//
// Uses a short timeout (1s) so a slow DB doesn't stall the dispatch loop.
func (d *Dispatcher) refreshLagGauge(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	var lagSec float64
	err := d.pool.QueryRow(queryCtx, `
SELECT COALESCE(EXTRACT(EPOCH FROM (now() - min(occurred_at))), 0)::float8
  FROM events WHERE published_at IS NULL`).Scan(&lagSec)
	if err != nil {
		// Log only — never escalate. ctx.Canceled during shutdown is
		// expected so don't spam at WARN.
		if !errors.Is(err, context.Canceled) {
			slog.WarnContext(ctx, "outbox lag gauge refresh failed", "error", err.Error())
		}
		return
	}
	if lagSec < 0 {
		lagSec = 0
	}
	lagSeconds.Set(lagSec)
}

// classifyError unwraps a tickError to read its reason label. Any error
// that did not originate from a tick call site (e.g. a context.Canceled
// returned directly by an external callee) falls through to reasonOther,
// keeping the cardinality bounded.
func classifyError(err error) string {
	if err == nil {
		return "none"
	}
	var te *tickError
	if errors.As(err, &te) {
		return te.reason
	}
	return reasonOther
}

// sleepCtx sleeps for d or returns false on ctx cancellation. Returning
// the boolean lets the caller treat shutdown as a clean-exit signal
// without an extra ctx.Err() check at every call site.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
