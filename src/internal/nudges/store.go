package nudges

// store.go — sk-303.14, task T017: SELECTION of repeat-reminder
// addressees.
//
// # What is here and what is deliberately not
//
// Here is the walk over addressees: who exactly is waiting for a reminder of
// each category, which issues are in their set, and from which moment their
// schedule is counted. There is NO membership condition here: both predicates
// live as a single instance in internal/review (INV-3, task T012) and are
// substituted into the WHERE as text. A fourth consumer with its own opinion
// of what "unaccepted" means is exactly how the three surfaces of the previous
// wave drifted apart.
//
// # Why selection is by ADDRESSEE, not by issue
//
// The rate limit is per user, not per issue (INV-8), and the digest is
// addressed to a person and speaks of several issues at once. So the tick's
// unit of work is the "addressee and category" pair, and issues arrive as an
// aggregation: a count and a list of ids. The reverse order (walk issues, then
// group in memory) would require reading the whole set before knowing who is
// even due — i.e. it would lose both batch bounds.
//
// # Why both selections are one query per category, not a query per addressee
//
// The set fragments support two forms of the addressee identifier: a value
// (placeholder) and a JOIN COLUMN. The column form exists for this file
// (review/set.go, columnRe): the per-item form would force every candidate to
// re-read the set of all addressees and pick one from it — at volume that is
// not a plan, it is a full scan multiplied by the batch size.
//
// # Aliases
//
// The fragments internally use aliases u, cu, pr, pm, p, au. This file's
// outer aliases are chosen so that none of them is shadowed: issues are `i`,
// the addressee is `ru`, rate-limit state is `rl`, the reason prefs are `rp`.
// A collision of an outer alias with an inner one would not error — it would
// silently retarget the fragment's condition onto a foreign row.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/assignee"
	"skein-api/internal/notifications"
	"skein-api/internal/review"
)

// Store — database access for the reminder worker.
type Store struct {
	pool  *pgxpool.Pool
	prefs *notifications.Store
}

// NewStore builds a store on top of the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, prefs: notifications.NewStore(pool)}
}

// Now returns the DATABASE CLOCK's moment.
//
// The only time source for the "is it due" decision: the caller's clock is
// unfit for this in principle. There are several worker instances, their
// clocks diverge, and INV-8 ("at most one reminder per segment") is promised
// precisely with several instances — so it is not entitled to lean on an
// assumption of replica clock agreement that nobody wrote down and nobody
// checked. The database clock is one for everyone, so the assumption
// disappears entirely rather than being softened.
//
// The moment is read ONCE per tick and then travels the whole chain — into
// the query's deadline predicate and into the schedule arithmetic: two
// different "now"s in one decision would give an unrepeatable answer on the
// segment boundary.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("reading the database clock: %w", err)
	}
	return now.UTC(), nil
}

// Candidate — one addressee whose set is non-empty and whose segment may have
// expired. "May" — because the query only answers the questions the store
// knows how to ask; the final "due" for an addressee who has no rate-limit
// state yet is decided by the schedule (see Scheduler).
type Candidate struct {
	// UserID — the reminder's addressee.
	UserID uuid.UUID

	// Base — the schedule's countdown base.
	//
	// For unaccepted this is the moment the issue was assigned to THIS
	// person, and it is taken from the FRESHEST issue in the set (R-5): the
	// phase is defined by the freshest assignment, otherwise a fresh issue
	// would queue behind an old one from a slow phase.
	//
	// For unreviewed this is the moment the freshest result appeared
	// (closed_at). The category has no burst (SC-013), so the base only sets
	// the phase of the uniform grid, but it sets it the same way — from the
	// freshest member of the set.
	Base time.Time

	// IssueIDs — ids of the issues in the set, in a stable order.
	//
	// A snapshot at selection time, not a promise of the notification's
	// contents: the delivery path re-checks each element and is entitled to
	// drop any or all of them (INV-14). So the list is NOT truncated here:
	// the count the person will see is computed AFTER the re-check, and
	// truncating at selection would undercount it.
	IssueIDs []string

	// NextDueAt — the FROZEN deadline of the next reminder from the
	// rate-limit state; nil means there is no state yet and a reminder of
	// this category has never been formed for this person.
	//
	// A non-nil value has already passed the `next_due_at <= now` check in
	// the query. Recomputing it with current settings is FORBIDDEN: the
	// deadline is stored precisely so that a setting change takes effect
	// from the next segment, and a started one is neither cut nor stretched
	// after the fact.
	NextDueAt *time.Time
}

// SelectDue returns the next batch of addressees of a category.
//
// The walk is batched and KEYSET: `after` is the id of the last addressee of
// the previous batch, ordered by id. OFFSET is unfit here — an addressee left
// without a reminder by INV-11 (the first segment has not expired yet) stays
// in the selection, and on the next batch of the same tick an offset walk
// would spin the same head of the list until the batch budget is exhausted.
//
// `now` is passed in, not taken from the database's now(), on purpose: the
// same moment is used by the schedule in Go, and two different "now"s in one
// decision would give an unrepeatable answer on the segment boundary.
func (s *Store) SelectDue(
	ctx context.Context, cat review.Category, now time.Time, limit int, after uuid.UUID,
) ([]Candidate, error) {
	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	sql, err := selectDueSQL(bind, cat, now, limit, after)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("selecting %s reminder candidates: %w", cat, err)
	}
	defer rows.Close()

	out := make([]Candidate, 0, limit)
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.UserID, &c.Base, &c.IssueIDs, &c.NextDueAt); err != nil {
			return nil, fmt.Errorf("scanning %s reminder candidates: %w", cat, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("selecting %s reminder candidates: %w", cat, err)
	}
	return out, nil
}

// selectDueSQL assembles the selection text for one category.
//
// Extracted from SelectDue as a separate function because it is not only the
// worker that reads it: the 000047 index-applicability gate builds a plan
// from THIS SAME text with the same parameters. A query assembled by the test
// as "roughly the same" would prove the index applies to a fictional
// selection.
func selectDueSQL(
	bind review.Binder, cat review.Category, now time.Time, limit int, after uuid.UUID,
) (string, error) {
	// Unsubscribing from a category silences FORMATION, not only sending
	// (SC-011, matrix row 21a): a person who turned the category off never
	// becomes a candidate at all, so their segment is not consumed and no
	// audit is written for them. The check lives in the query, not after it,
	// so as not to spend a batch slot on addressees who will not be formed
	// anyway.
	prefExpr, err := pushEnabledExpr(bind, cat, "ru.id")
	if err != nil {
		return "", err
	}

	// LOWER BOUND of the first mark. It is not a schedule and must not be
	// one (there is no second scale arithmetic in the system and none is
	// being introduced) — it only answers "could this addressee have become
	// due at all", and it answers from the same constants the review scale
	// is computed from.
	//
	// Without it the selection dragged into the batch addressees whose first
	// mark had not arrived yet: they do not leave the selection, they spawn
	// no reminder, and they simply occupy a budget slot — EVERY TICK.
	// Everything past the budget bound in id order went unprocessed, and the
	// head could fail to drain indefinitely: the unaccepted countdown base
	// is MAX(assignee_set_at), and a person who keeps being assigned issues
	// never becomes due. Finding of review A/8, proven by a probe: two
	// addressees, batch size 1, fifty ticks, the tail received zero
	// reminders.
	firstMark, err := review.EarliestFirstMark(cat)
	if err != nil {
		return "", err
	}

	nowPh := bind(now)
	afterPh := bind(after)
	limitPh := bind(limit)
	catPh := bind(string(cat))
	firstMarkPh := bind(int64(firstMark / time.Second))

	var join, base, membership string
	switch cat {
	case review.CategoryUnaccepted:
		// The assignee is stored as TEXT, so the addressee is obtained by a join
		// on the RESOLUTION of the descriptor — assignee.Join matches all three accepted
		// forms (name, email, UUID), not just the name: before sk-523 a load
		// assigned by email produced no reminder at all. Whether the assignee
		// resolves and whether the addressee is active is checked by the fragment itself — they
		// are not here and must not be.
		join = assignee.Join("ru", "i")
		base = "MAX(i.assignee_set_at)"
		membership = review.UnacceptedPredicate(bind, review.UnacceptedArgs{
			Alias:         "i",
			AddresseeExpr: "ru.id",
		}) +
			// A row without a countdown base does not enter the selection:
			// the schedule refuses without a base, and historical issues
			// assigned before the release have no base and nowhere for one
			// to come from (earlier decision, migration counter
			// issues_assigned_without_base). The condition lives here, not
			// in the fragment: it does not define set membership — the
			// issue remains unaccepted — it says there is nothing to remind
			// about it from. It is also part of the 000047 partial index
			// body.
			" AND i.assignee_set_at IS NOT NULL"
	case review.CategoryUnreviewed:
		// The addressee is the issue's author, who is also the fragment's
		// caller. The join to users is needed exactly for deactivation: the
		// unreviewed fragment does not check it (its other consumers have
		// an active caller by construction), and the spec excludes a
		// deactivated addressee from both selection and delivery.
		join = "JOIN users ru ON ru.id = i.created_by AND ru.deactivated_at IS NULL"
		// closed_at of a closed issue is never empty — that is a schema
		// constraint (000004) — so a non-empty set always has a base.
		base = "MAX(i.closed_at)"
		membership = review.UnreviewedPredicate(bind, review.UnreviewedArgs{
			Alias:      "i",
			CallerExpr: "i.created_by",
		})
	default:
		return "", fmt.Errorf("%w: %q", review.ErrUnknownCategory, string(cat))
	}

	return `
SELECT ru.id,
       ` + base + ` AS base,
       array_agg(i.id ORDER BY i.id) AS issue_ids,
       rl.next_due_at
  FROM issues i
  ` + join + `
  LEFT JOIN reminder_rate_limit rl
         ON rl.user_id = ru.id AND rl.category = ` + catPh + `::text
 WHERE ru.id > ` + afterPh + `::uuid
   AND (rl.next_due_at IS NULL OR rl.next_due_at <= ` + nowPh + `::timestamptz)
   AND ` + prefExpr + `
   AND ` + membership + `
 GROUP BY ru.id, rl.next_due_at
HAVING rl.next_due_at IS NOT NULL
    OR ` + base + ` <= ` + nowPh + `::timestamptz - (` + firstMarkPh + `::bigint * interval '1 second')
 ORDER BY ru.id
 LIMIT ` + limitPh + `::int`, nil
}

// pushEnabledExpr — "this reason's push channel is on for this addressee".
//
// Unsubscribe is checked by THE SAME mechanism as for other notifications
// (INV-9): the user_notification_reason_prefs table and the default from the
// reasons registry. The default is taken FROM THE REGISTRY, not written here
// as a literal: a handwritten "true" would stay silent if the registry ever
// changed its mind, and a non-applicable channel must yield false regardless
// of the stored value — exactly as single delivery does.
func pushEnabledExpr(bind review.Binder, cat review.Category, userExpr string) (string, error) {
	// The addressee-id expression is spliced into the query text, so it is
	// checked by the same validator as the neighbouring splices in
	// internal/review. Skipping the check here was a finding of review A/8:
	// the neighbouring string of the same query is validated, this one is
	// not, and "the form is always a constant from the code" was held only
	// by word of honour.
	review.ValidateUserExpr(userExpr)

	reason, ok := notifications.DigestReasonForCategory(cat)
	if !ok {
		return "", fmt.Errorf("%w: %q", review.ErrUnknownCategory, string(cat))
	}
	spec, ok := notifications.LookupReason(reason)
	if !ok {
		return "", fmt.Errorf("reason %q missing from the registry", string(reason))
	}
	if !spec.Push.Applicable {
		return "false", nil
	}
	return "COALESCE((SELECT rp.push_enabled FROM user_notification_reason_prefs rp" +
		" WHERE rp.user_id = " + userExpr + " AND rp.reason = " + bind(string(reason)) + "), " +
		strconv.FormatBool(spec.Push.DefaultOn) + ")", nil
}
