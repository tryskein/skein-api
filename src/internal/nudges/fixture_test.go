package nudges

// fixture_test.go — sk-303.14, task T020: the shared fixture of the
// reminder worker's integration class.
//
// Issues are inserted by DIRECT WRITE, not by product paths. The reason is
// the same as for the unreviewed class
// (handler/review_unreviewed_manifestations_test.go): the fixture needs
// states the product does not create in one call — an assignment with a
// countdown base in the past, a standing task with an assignee, an
// assignee-string that does not resolve to a user — and mixing two methods
// inside one fixture is worse than picking one. Operation semantics are
// checked by their own classes; here SELECTION and FORMATION are checked.

// ⚠ The database is shared and sequential. Every assertion addresses ITS OWN
// users: the worker's walk is global and legitimately sees foreign rows left
// by other packages.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/notifications"
	"skein-api/internal/review"
	"skein-api/internal/testsupport"
)

type env struct {
	pool    *pgxpool.Pool
	store   *Store
	project uuid.UUID
	prefix  string
	seq     int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, owner, "nudge")
	return &env{pool: pool, store: NewStore(pool), project: pid, prefix: prefix}
}

// user creates an active user and makes them a project member: the
// unreviewed set checks the visibility horizon, and without membership the
// author would not see their own issue.
func (e *env) user(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	id, name := testsupport.SeedUser(t, e.pool, "user")
	testsupport.AddProjectMember(t, e.pool, e.project, id, "member")
	return id, name
}

// issue — the state of one fixture issue. Every field is set EXPLICITLY: a
// default in a mechanics fixture would mean the check depends on a value
// nobody chose.
type issue struct {
	suffix string
	status string
	// assignee — the assignee string. Precisely a string: the domain stores
	// the assignee as text, and "unresolvable to a user" is a data state,
	// not an impossibility.
	assignee string
	// assigneeSetAt — the burst countdown base. nil means "there is no
	// base" — that is how issues assigned before the mechanic shipped look.
	assigneeSetAt *time.Time
	accepted      bool
	createdBy     uuid.UUID
	// projectID — the issue's project; the zero value means the fixture
	// project.
	projectID uuid.UUID

	// review-mechanic fields
	reviewRequired bool
	reviewedBy     *uuid.UUID
	dismissedBy    []uuid.UUID
}

func (e *env) seed(t *testing.T, in issue) string {
	t.Helper()
	e.seq++
	id := e.prefix + "-" + in.suffix
	pid := in.projectID
	if pid == uuid.Nil {
		pid = e.project
	}

	var assigneeArg any
	if in.assignee != "" {
		assigneeArg = in.assignee
	}
	var closedAt any
	if in.status == "closed" {
		closedAt = time.Now().UTC()
	}
	var acceptedAt, acceptedBy any
	if in.accepted {
		acceptedAt = time.Now().UTC()
		acceptedBy = in.createdBy
	}
	var reviewedAt, reviewedBy any
	if in.reviewedBy != nil {
		reviewedAt = time.Now().UTC()
		reviewedBy = *in.reviewedBy
	}
	dismissed := in.dismissedBy
	if dismissed == nil {
		dismissed = []uuid.UUID{}
	}

	if _, err := e.pool.Exec(t.Context(), `
INSERT INTO issues (id, project_id, parent_id, version,
                    title, description, type, priority, status, pinned,
                    source, source_context, assignee,
                    design, acceptance_criteria, notes,
                    estimated_minutes, due_at, defer_until,
                    started_at, closed_at, closed_reason,
                    external_ref, created_by, created_at, updated_at,
                    accepted_at, accepted_by, metadata,
                    previous_assignees, dismissed_by,
                    review_required, reviewed_at, reviewed_by, assignee_set_at)
VALUES ($1::varchar, $2, NULL, 1, $1::varchar, '', 'task', 2, $3, false, 'human', NULL, $4::varchar,
        '', '', '', NULL, NULL, NULL, NULL, $5::timestamptz, NULL,
        NULL, $6, now(), now(), $7::timestamptz, $8::uuid, '{}'::jsonb,
        '{}'::uuid[], $9::uuid[],
        $10::boolean, $11::timestamptz, $12::uuid, $13::timestamptz)`,
		id, pid, in.status, assigneeArg, closedAt, in.createdBy,
		acceptedAt, acceptedBy, dismissed,
		in.reviewRequired, reviewedAt, reviewedBy, in.assigneeSetAt); err != nil {
		t.Fatalf("seed issue %s: %v", id, err)
	}
	return id
}

// ago — a moment in the past, readably.
func ago(d time.Duration) *time.Time {
	v := time.Now().UTC().Add(-d)
	return &v
}

// candidateFor finds a candidate of this category among ALL addressees of
// the database.
//
// The walk is batched and keyset, so one's own user may not sit in the first
// batch: the database is shared, and foreign addressees legitimately stand
// in front. The search repeats the worker's walk rather than substituting
// its own query — otherwise the assertions would check a fictional
// selection.
func candidateFor(
	t *testing.T, ctx context.Context, s *Store, cat review.Category, now time.Time, userID uuid.UUID,
) (Candidate, bool) {
	t.Helper()
	after := uuid.Nil
	for range 100 {
		batch, err := s.SelectDue(ctx, cat, now, 50, after)
		if err != nil {
			t.Fatalf("selecting %s: %v", cat, err)
		}
		if len(batch) == 0 {
			return Candidate{}, false
		}
		for _, c := range batch {
			if c.UserID == userID {
				return c, true
			}
		}
		after = batch[len(batch)-1].UserID
	}
	t.Fatalf("selecting %s: the walk did not converge in 100 batches", cat)
	return Candidate{}, false
}

// ---------------------------------------------------------------------------
// State readers
// ---------------------------------------------------------------------------

// notifications counts the addressee's delivery rows for the category's
// reason.
func (e *env) notifications(t *testing.T, userID uuid.UUID, cat review.Category) int {
	t.Helper()
	reason := reasonOf(t, cat)
	var n int
	if err := e.pool.QueryRow(t.Context(), `
SELECT count(*) FROM notifications WHERE user_id = $1 AND reason = $2`,
		userID, reason).Scan(&n); err != nil {
		t.Fatalf("counting notifications: %v", err)
	}
	return n
}

// rateLimit reads the pair's rate-limit state.
func (e *env) rateLimit(t *testing.T, userID uuid.UUID, cat review.Category) (last, next time.Time, ok bool) {
	t.Helper()
	err := e.pool.QueryRow(t.Context(), `
SELECT last_formed_at, next_due_at FROM reminder_rate_limit
 WHERE user_id = $1 AND category = $2`, userID, string(cat)).Scan(&last, &next)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return time.Time{}, time.Time{}, false
		}
		t.Fatalf("reading the rate-limit state: %v", err)
	}
	return last, next, true
}

// forceNextDue moves the frozen deadline — the very move the schema
// deliberately did not put a binding constraint on: an operator is
// legitimately allowed to set the deadline into the past to force a
// reminder. The class needs it for exactly the same — to live through a
// segment without moving the clock.
func (e *env) forceNextDue(t *testing.T, userID uuid.UUID, cat review.Category, at time.Time) {
	t.Helper()
	tag, err := e.pool.Exec(t.Context(), `
UPDATE reminder_rate_limit SET next_due_at = $3
 WHERE user_id = $1 AND category = $2`, userID, string(cat), at)
	if err != nil {
		t.Fatalf("shifting the frozen deadline: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("shifting the frozen deadline: the pair's row is missing")
	}
}

// forceClosedAt sets the issue's close moment EXACTLY.
//
// The unreviewed countdown base is MAX(closed_at), and the fixture stamps it
// as "now". An assertion about the first-mark BOUND needs a moment chosen by
// itself and that has survived rounding to microseconds, otherwise the bound
// is checked with "approximately" precision, and a unit-of-measure slip
// stays green.
func (e *env) forceClosedAt(t *testing.T, issueID string, at time.Time) {
	t.Helper()
	tag, err := e.pool.Exec(t.Context(),
		`UPDATE issues SET closed_at = $2 WHERE id = $1`, issueID, at)
	if err != nil {
		t.Fatalf("shifting the close moment: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("shifting the close moment: issue %s has no row", issueID)
	}
}

// setSchedule saves the user's schedule. nil is "empty", the built-in default
// applies.
func (e *env) setSchedule(t *testing.T, userID uuid.UUID, burst, unaccMinutes, unrevMinutes *int) {
	t.Helper()
	if _, err := e.pool.Exec(t.Context(), `
INSERT INTO user_notification_prefs (user_id, reminder_burst_count,
                                     unaccepted_interval_minutes, unreviewed_interval_minutes)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE
   SET reminder_burst_count         = EXCLUDED.reminder_burst_count,
       unaccepted_interval_minutes  = EXCLUDED.unaccepted_interval_minutes,
       unreviewed_interval_minutes  = EXCLUDED.unreviewed_interval_minutes`,
		userID, burst, unaccMinutes, unrevMinutes); err != nil {
		t.Fatalf("saving the schedule: %v", err)
	}
}

// setCategoryPush turns the category reason's push channel on or off.
func (e *env) setCategoryPush(t *testing.T, userID uuid.UUID, cat review.Category, enabled bool) {
	t.Helper()
	if _, err := e.pool.Exec(t.Context(), `
INSERT INTO user_notification_reason_prefs (user_id, reason, feed_enabled, push_enabled)
VALUES ($1, $2, false, $3)
ON CONFLICT (user_id, reason) DO UPDATE SET push_enabled = EXCLUDED.push_enabled`,
		userID, reasonOf(t, cat), enabled); err != nil {
		t.Fatalf("configuring the push channel: %v", err)
	}
}

// reasonOf — the category's reason, from the same translation the code uses.
func reasonOf(t *testing.T, cat review.Category) string {
	t.Helper()
	r, ok := notifications.DigestReasonForCategory(cat)
	if !ok {
		t.Fatalf("category %s has no reason", cat)
	}
	return string(r)
}

func intp(v int) *int { return &v }

// parsePayloadIDs extracts ids from the event payload with the same parser
// the delivery path uses.
func parsePayloadIDs(raw []byte) ([]string, error) {
	p, err := notifications.ParseDigestPayload(raw)
	if err != nil {
		return nil, err
	}
	return p.IssueIDs, nil
}
