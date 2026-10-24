package outbox

// supersede.go — sk-303.14, task T018: SUPERSEDE of undelivered
// reminders of one "recipient and category" pair.
//
// # Why this lives in the tract, while the producer takes the decision
//
// The decision "which of two rows is current" can only be taken by the one
// who sees both — that is, the nudge worker at the moment a new row is
// formed. But what it CHANGES is the delivery state machine: push_state,
// push_outcome, the lease, the next-attempt deadline. The machine belongs
// to this package, and a second editor of it with its own idea of legal
// transitions is how a row ends up in a state the machine does not provide
// for. So the transition lives here, and the producer calls it.
//
// # Why NOT EVERY undelivered row is superseded
//
// The spec requires: after a transport outage longer than a segment, only
// the freshest is delivered. A literal "every undelivered" would include
// the row the delivery runner is sending right now — and its outcome is
// already its decision, so a race for one row would give two authors of
// one outcome.
//
// Two states are selected, and both are safe by the machine's construction:
//
//   - pending — the row has not been taken by the transport yet. The
//     transport can take it only by a claim, and the claim selects rows
//     by push_state = 'pending'; a finalised row it will not see;
//   - leased with an EXPIRED lease — the row has been abandoned: the
//     expired-lease return would put it back on the queue without us.
//     Capturing such a row is safe because the delivery runner
//     re-asserts ownership before every send (holdLease: push_state =
//     'leased' AND push_attempts = <the claim's generation>), and on a
//     finalised row that assertion matches zero rows — the owner leaves
//     silently, having sent nothing.
//
// A live lease is not touched. The residual window is one row that was
// claimed before the supersede: it will go out as an ordinary delivery,
// and the person will get one extra notification, not one per segment of
// downtime. It is the pile for every downtime segment that the spec
// forbids.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OutcomeSuperseded is the name of the terminal supersede outcome in the
// schema.
//
// Exported so the producer and the tests name the outcome with the same
// value as this package's closed set, not with a literal of their own.
const OutcomeSuperseded = outcomeSuperseded

// SupersedeUndeliveredTx finishes undelivered rows of the "recipient and
// reason" pair with the supersede outcome and returns how many they were.
//
// It runs IN THE PRODUCER'S TRANSACTION: the supersede and the appearance
// of the superseding row must be one fact. Split them and a crash between
// them would leave either the person with no reminder at all (superseded,
// new one not created) or two live rows of the same pair (created, old
// one not superseded).
func SupersedeUndeliveredTx(
	ctx context.Context, tx pgx.Tx, userID uuid.UUID, reason string,
) (int, error) {
	tag, err := tx.Exec(ctx, `
UPDATE notifications
   SET push_state = 'final',
       push_outcome = $3,
       push_finalized_at = now(),
       push_leased_until = NULL,
       push_next_attempt_at = NULL
 WHERE user_id = $1
   AND reason = $2
   AND (push_state = 'pending'
        OR (push_state = 'leased' AND push_leased_until <= now()))`,
		userID, reason, outcomeSuperseded)
	if err != nil {
		return 0, fmt.Errorf("superseding undelivered %s notifications: %w", reason, err)
	}
	return int(tag.RowsAffected()), nil
}

// ObserveSuperseded records supersede outcomes in the shared outcome
// counter.
//
// Called by the producer AFTER its transaction commits, and only then:
// the outcome counter promises exactly one increment per row over its
// life, and an increment from inside an unfinished transaction would
// survive a rollback.
//
// The counter is the same one every other outcome uses, not a counter of
// its own. A separate counter would mean the outcome-label list on the
// shared counter names a value that never appears in it — i.e. the metric
// would silently under-count supersedes, and its description would lie.
func ObserveSuperseded(reason string, count int) {
	if count <= 0 {
		return
	}
	pushOutcomesTotal.WithLabelValues(reason, outcomeSuperseded, causeNone).Add(float64(count))
}
