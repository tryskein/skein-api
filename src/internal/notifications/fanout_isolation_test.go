package notifications

// fanout_isolation_test.go — sk-674.18 T005 (SC-003): an
// event in a project creates no notifications row — and therefore no push
// row, push being a column of that row — for anyone who does not see the
// project; those who see it get theirs.
//
// Fixture testsupport.SeedTenancy (T003); reference testsupport.TenancyVisible
// (legend of row 22). Every actor is made a candidate on BOTH recipient paths
// the fan-out unions, so a missing access filter on either path is a red cell:
//
//   - subscription path, arm by arm (issue watchers; project watchers):
//     every actor has a stale watch row on the event's issue/project (as
//     after losing access), the event is comment.created;
//   - role path: for every actor, an issue.status_changed whose creator is
//     that actor and whose assignee is someone else (the delegating-creator
//     audience).
//
// Every reason has feed and push switched on for every actor, so an empty
// answer cannot come from preferences — the bait (visible actors get a row)
// proves the paths deliver at all.
//
// The recipient filter is authz.RecipientEligibleExpr (sk-674.18 T010):
// active AND sees the project, on every arm and the role path — so the two
// holes this matrix found on the pre-T010 code (the project-watch arm did not
// recheck access at all; no arm read deactivated_at) are asserted cells now.
//
// Every cell is asserted since T017 (it closed the last one: an admin on
// another person's "Personal"); the hole hook stays for the next narrowing.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/testsupport"
)

type isoFanoutWorld struct {
	pool   *pgxpool.Pool
	f      *testsupport.Tenancy
	events []int64
}

func newIsoFanoutWorld(t *testing.T) *isoFanoutWorld {
	t.Helper()
	pool := testsupport.NewTestPool(t)
	w := &isoFanoutWorld{pool: pool, f: testsupport.SeedTenancy(t, pool)}
	for _, a := range testsupport.TenancyActors {
		for _, r := range AllReasons() {
			enableReasonChannels(t, pool, w.f.User(t, a).ID, r.Key, true, true)
		}
	}
	// Registered after the fixture, so it runs before the projects and users
	// go: the events (and, by cascade, their notifications) leave first.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = ANY($1)`, w.events)
	})
	return w
}

// fanOut writes one event about issue p.IssueID and runs the product's
// FanOut on it in one committed transaction. Returns user → push_state of
// the rows the event produced.
func (w *isoFanoutWorld) fanOut(t *testing.T, p testsupport.TenancyProject, eventType, entityType, entityID string, in FanOutInput) map[uuid.UUID]*string {
	t.Helper()
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var eid int64
	if err := tx.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_type, payload)
VALUES (now(), $1, $2, $3, $4, 'isomatrix', 'human', '{}'::jsonb)
RETURNING id`, eventType, entityType, entityID, p.ID).Scan(&eid); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	w.events = append(w.events, eid)
	in.EventID, in.EventType, in.EntityType = eid, eventType, entityType
	in.IssueID, in.ProjectID, in.ActorType = p.IssueID, p.ID, "human"
	if _, err := FanOut(ctx, tx, in); err != nil {
		t.Fatalf("FanOut %s: %v", eventType, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := w.pool.Query(ctx, `SELECT user_id, push_state FROM notifications WHERE event_id = $1`, eid)
	if err != nil {
		t.Fatalf("read notifications: %v", err)
	}
	defer rows.Close()
	got := map[uuid.UUID]*string{}
	for rows.Next() {
		var uid uuid.UUID
		var push *string
		if err := rows.Scan(&uid, &push); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[uid] = push
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}

// isoFanoutHole names the narrowing task of a cell the current code does not
// isolate yet; "" when the cell is checkable today.
type isoFanoutHole func(a testsupport.Actor, p testsupport.TenancyProject) string

// checkCell asserts one actor's outcome for an event in project p: a row for
// those who see p, none for the others. A N (no) cell that hole() names is a skip
// with the task — but only while it still leaks, so a narrowed cell is
// asserted at once.
//
// wantPush is the push_state a visible recipient's row must carry, taken
// from the reason registry for the path (reasons.go): "pending" where the
// reason has a push channel for that audience, "none" where it has none.
func (w *isoFanoutWorld) checkCell(t *testing.T, a testsupport.Actor, p testsupport.TenancyProject, got map[uuid.UUID]*string, hole isoFanoutHole, wantPush string) {
	t.Helper()
	push, has := got[w.f.User(t, a).ID]
	if w.f.Sees(a, p) {
		if !has {
			t.Errorf("%s sees %s and got no notification", a, p.Key())
		} else if derefState(push) != wantPush {
			// Push is on for every reason of every actor: where the reason
			// pushes, the row must carry a queued push, or a suppressed push
			// would pass.
			t.Errorf("%s sees %s: push_state %s, want %s", a, p.Key(), derefState(push), wantPush)
		}
		return
	}
	if !has {
		return
	}
	if task := hole(a, p); task != "" {
		t.Skipf("awaits narrowing: %s", task)
	}
	t.Errorf("%s does not see %s and got a notification (push_state %v)", a, p.Key(), derefState(push))
}

func derefState(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// commonFanoutHoles is the hole common to every path — none since T017
// closed the last one (an admin on another person's "Personal").
func commonFanoutHoles(testsupport.Actor, testsupport.TenancyProject) string {
	return ""
}

// subscriptionCase seeds one kind of stale watch row for every actor on every
// fixture project and fans out a comment.created there. watchSQL takes
// ($1 project id, $2 issue id, $3 user id), each typed explicitly.
func subscriptionCase(t *testing.T, watchSQL string, hole isoFanoutHole) {
	w := newIsoFanoutWorld(t)
	ctx := context.Background()
	for _, p := range w.f.Projects() {
		t.Run(p.Key(), func(t *testing.T) {
			for _, a := range testsupport.TenancyActors {
				if _, err := w.pool.Exec(ctx, watchSQL, p.ID, p.IssueID, w.f.User(t, a).ID); err != nil {
					t.Fatalf("watch row: %v", err)
				}
			}
			commentID := testsupport.SeedComment(t, w.pool, p.IssueID, p.OwnerID, "isomatrix fan-out")
			got := w.fanOut(t, p, "comment.created", "comment", commentID.String(), FanOutInput{})
			// A project nobody of the legend sees — the "Personal" of the
			// deactivated actor — must reach nobody; anywhere else an empty
			// fan-out would make the cells prove nothing.
			seen := false
			for _, a := range testsupport.TenancyActors {
				seen = seen || w.f.Sees(a, p)
			}
			if !seen {
				if len(got) != 0 {
					t.Errorf("nobody sees %s, yet the event reached %d recipients", p.Key(), len(got))
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("the event reached nobody — the cell would prove nothing")
			}
			for _, a := range testsupport.TenancyActors {
				// comment.created reaches subscribers through the "comment"
				// reason: feed only, the reason has no push channel.
				t.Run(string(a), func(t *testing.T) { w.checkCell(t, a, p, got, hole, "none") })
			}
		})
	}
}

// Arm 1: watchers of the issue itself.
func TestFanOutIsolation_IssueWatchers(t *testing.T) {
	subscriptionCase(t, `
INSERT INTO issue_watchers (issue_id, user_id, reasons)
SELECT $2::varchar, $3::uuid, '{manual}' WHERE $1::uuid IS NOT NULL
ON CONFLICT DO NOTHING`, commonFanoutHoles)
}

// Arm 3: project watchers. Before sk-674.18 T010 this arm did not recheck
// access at all (found by this matrix); it is an asserted cell now.
func TestFanOutIsolation_ProjectWatchers(t *testing.T) {
	subscriptionCase(t, `
INSERT INTO project_watchers (project_id, user_id)
SELECT $1::uuid, $3::uuid WHERE $2::varchar <> ''
ON CONFLICT DO NOTHING`, commonFanoutHoles)
}

// Role path: the delegating creator of the issue.
func TestFanOutIsolation_RolePath(t *testing.T) {
	w := newIsoFanoutWorld(t)
	for _, p := range w.f.Projects() {
		t.Run(p.Key(), func(t *testing.T) {
			for _, a := range testsupport.TenancyActors {
				t.Run(string(a), func(t *testing.T) {
					creator := w.f.User(t, a).ID
					// Someone else holds the issue: the creator has delegated it.
					assignee := w.f.User(t, testsupport.ActorOutsider).ID
					if a == testsupport.ActorOutsider {
						assignee = w.f.User(t, testsupport.ActorOnlyA).ID
					}
					got := w.fanOut(t, p, "issue.status_changed", "issue", p.IssueID, FanOutInput{
						CreatorUserID:  creator,
						AssigneeUserID: assignee,
					})
					// delegated_status pushes to the delegating creator.
					w.checkCell(t, a, p, got, commonFanoutHoles, "pending")
				})
			}
		})
	}
}
