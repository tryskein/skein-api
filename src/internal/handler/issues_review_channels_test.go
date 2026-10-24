package handler

// issues_review_channels_test.go — sk-303.14 T007, contract C1.
//
// This proves that NO event-journal reader sees a private journal row,
// and that what closes them is the row's SHAPE (the "user" entity, empty
// project, issue id in the payload), not an exception list at each
// reader.
//
// Why this is a separate file and why there are so many assertions. The
// plan rejected the alternative "an event on the issue plus exceptions
// in five reading places" precisely because the exception list must not
// be forgotten in a sixth place. So what must be checked is not "we
// added a filter", but "a reader ASKING FOR EXACTLY THESE TYPES still
// gets nothing": every call below asks for the private types explicitly,
// and an empty answer proves the selection cuts by shape, not by type.
//
// The mutation this file is written for: put the row's shape back on
// the issue (entity_type='issue' + project_id) — and every assertion
// here reddens, while the rest of the T007 class stays green.
//
// Matrix: row 22 (the event block of the agent issue read) and rows 35,
// 36, 38 — three observation channels, checked SEPARATELY from each
// other: the issue activity feed, the project live feed, dashboard
// aggregates.
//
// Wave A/4 (T008) added the missing half here. The live feed is NOT
// closed by the row's shape: the user subject `skein.user.<author>.*` is
// fanned out by the bridge to the addressee and to every administrator,
// so the channel is closed by NON-PUBLICATION, and the previous claim
// "the address is not a project one, it is a user one" became "is not
// published at all" — TestReviewChannel_NotPublishedToBus. The queue
// contract (terminal stamp, lag metric, not crowding out ordinary
// events, rows 32–34) is checked where the queue lives:
// internal/outbox/dispatcher_private_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/authz"
	"skein-api/internal/dashboard"
	"skein-api/internal/issues"
	"skein-api/internal/outbox"
	"skein-api/internal/redact"
)

// reviewEventTypes are both private types, as a reader who is
// interested in them would ask for them.
var reviewEventTypes = []string{
	issues.EventReviewMarked,
	issues.EventReviewFlagChanged,
}

// doBothReviewOps runs both of the author's operations and returns the
// fixture. Both, not one: the types differ, and the row shape is shared
// — an assertion checked on one type says nothing about the second.
func doBothReviewOps(t *testing.T, namePrefix string) *reviewFixture {
	t.Helper()
	f := newReviewFixture(t, namePrefix)

	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("mark: %d %s", res.Status, res.Raw)
	}
	v := readReviewRow(t, f.pool, f.issueID).Version
	if res := f.callPatch(t, f.author, "user", v,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("flag change: %d %s", res.Status, res.Raw)
	}

	// Precondition: the rows ARE in the journal. Without it every "the
	// reader returned nothing" below would be proven by the absence of
	// data, not by the shape.
	//
	// Counted WITHOUT regard to shape — by event type alone. This is
	// principled: a precondition that knew the private shape would, on
	// putting the shape back on the issue, fail first and absorb the
	// mutation, and the reader assertions would never run. The
	// precondition must survive the mutation so that it is the channels
	// that redden.
	for _, eventType := range reviewEventTypes {
		if n := countEventsAnyForm(t, f.pool, f.issueID, eventType); n != 1 {
			t.Fatalf("precondition: %s rows = %d, want one — the assertions below "+
				"would be proven by the absence of data, not by the row's shape", eventType, n)
		}
	}
	return f
}

// countEventsAnyForm counts journal rows for an issue WITHOUT knowing
// their shape: both the private one (the "user" entity, issue in the
// payload) and the old one (the "issue" entity).
//
// This is not a convenience, it is a condition of the mutation check
// making sense. A precondition that can only count the private shape
// would, on putting the shape back on the issue, fail first — and absorb
// the mutation: the test would redden on "there is no data", never
// running a single reader assertion. The precondition must survive the
// mutation so that it is the channels that redden.
func countEventsAnyForm(t *testing.T, pool *pgxpool.Pool, issueID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM events
 WHERE type = $2
   AND (   (entity_type = 'issue' AND entity_id = $1)
        OR (entity_type = 'user'  AND payload->>'issueId' = $1))`,
		issueID, eventType).Scan(&n); err != nil {
		t.Fatalf("count events (any form): %v", err)
	}
	return n
}

// assertNoReviewTypes fails if a private type is among the types.
func assertNoReviewTypes(t *testing.T, reader string, types []string) {
	t.Helper()
	for _, got := range types {
		for _, bad := range reviewEventTypes {
			if got == bad {
				t.Errorf("%s returned private type %q. The channel must be closed "+
					"by the row's SHAPE (the \"user\" entity, empty project), not "+
					"by a type filter at the reader (INV-2, SC-003, SC-028)", reader, got)
			}
		}
	}
}

// TestReviewChannel_IssueEventBlockIsSilent — reader 1: the event block
// of the agent issue read (issues.Store.ListEvents, the `events` block in
// skein_show). It does not filter types at all, so the only thing that
// can close it is the selection entity_type='issue' AND entity_id=<issue>.
//
// Matrix row 22: the card does not carry the three keys, and the event
// block beside it carries no traces of the mechanic.
func TestReviewChannel_IssueEventBlockIsSilent(t *testing.T) {
	f := doBothReviewOps(t, "revchev")

	events, err := f.h.issuesStore.ListEvents(context.Background(), f.issueID, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, e.Type)
	}
	assertNoReviewTypes(t, "ListEvents (event block of the agent read)", types)
}

// TestReviewChannel_RecentEventsForUserIsSilent — reader 2: the
// priority user-event selection (skein_prime). Checked for BOTH
// participants: the assignee the mechanic is hidden from, and the
// author themselves — the row must not surface even for them, because
// this selection is not a private surface, it is "what happened in my
// world", also visible to a project administrator.
func TestReviewChannel_RecentEventsForUserIsSilent(t *testing.T) {
	f := doBothReviewOps(t, "revchprime")
	ctx := context.Background()

	for name, caller := range map[string]uuid.UUID{
		"assignee": f.admin,
		"author":   f.author,
	} {
		t.Run(name, func(t *testing.T) {
			evs, err := f.h.issuesStore.RecentEventsForUser(ctx, caller, authz.ForUser(caller), nil, 50)
			if err != nil {
				t.Fatalf("RecentEventsForUser: %v", err)
			}
			types := make([]string, 0, len(evs))
			for _, e := range evs {
				types = append(types, e.Type)
			}
			assertNoReviewTypes(t, "RecentEventsForUser", types)
		})
	}
}

// TestReviewChannel_ActivityFeedsAreSilent — readers 3–5: three
// activity feeds built on one activityListSQL (issue feed, project
// feed, the user's cross-project feed).
//
// Each asks for EXACTLY the private types: the type filter is passed
// explicitly here, so an empty answer cannot be explained by a default
// type set — only the row's shape explains it.
func TestReviewChannel_ActivityFeedsAreSilent(t *testing.T) {
	f := doBothReviewOps(t, "revchact")
	ctx := context.Background()
	visible := map[uuid.UUID]struct{}{f.projectID: {}}

	issueItems, _, err := f.h.issuesStore.ListIssueActivity(
		ctx, f.projectID, f.issueID, reviewEventTypes, nil, 50)
	if err != nil {
		t.Fatalf("ListIssueActivity: %v", err)
	}
	if len(issueItems) != 0 {
		t.Errorf("the issue activity feed returned %d mechanic rows — SC-003 "+
			"requires this channel to stay silent", len(issueItems))
	}

	projectItems, _, err := f.h.issuesStore.ListProjectActivity(
		ctx, f.projectID, reviewEventTypes, nil, 50)
	if err != nil {
		t.Fatalf("ListProjectActivity: %v", err)
	}
	if len(projectItems) != 0 {
		t.Errorf("the project activity feed returned %d mechanic rows", len(projectItems))
	}

	// The cross-project feed — in both incarnations: narrowed to the
	// visible ("my stream") and global ("HQ feed"), where there is no
	// visibility predicate at all and only project_id IS NOT NULL holds
	// the row.
	for name, global := range map[string]bool{"my stream": false, "HQ feed": true} {
		t.Run(name, func(t *testing.T) {
			var reach map[uuid.UUID]struct{}
			if global {
				reach = visible
			}
			userItems, _, err := f.h.issuesStore.ListUserActivity(
				ctx, f.author, visible, true, reviewEventTypes, nil, 50, reach, redact.None)
			if err != nil {
				t.Fatalf("ListUserActivity: %v", err)
			}
			if len(userItems) != 0 {
				t.Errorf("the cross-project feed returned %d mechanic rows", len(userItems))
			}
		})
	}
}

// startEmbeddedNATS brings up NATS Core on a random port. Needed by
// exactly one test below: the other journal readers live inside the API,
// and this one is about the bus, and faking it with a mock would mean
// checking the mock.
func startEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL(), nats.Name("review-channels-test"))
	if err != nil {
		t.Fatalf("connect embedded nats: %v", err)
	}
	t.Cleanup(func() { _ = nc.Drain() })
	return nc
}

// privateRow is a journal row selected BY SHAPE, together with its type.
type privateRow struct {
	ID         int64
	Type       string
	EntityType string
	EntityID   string
	ProjectID  *uuid.UUID
	Field      string
}

// loadPrivateRowsByForm selects private rows BY SHAPE (the "user" entity,
// issue in the payload), NOT by a type list.
//
// ⚠ CORRECTED ERROR (A/8 repair). Selection went by `payload->>'issueId'`
// — in the SINGULAR — and the comment beside it declared this selection
// a guard against forgetting a closed set of private types: "a type not
// put there will publish here and redden". The promise was false: the
// reminder digest writes `issueIds` in the PLURAL and never entered the
// selection, so both digest types, forgotten in the set, were published
// to the bus, and this "guard" stayed silent. The selection now knows
// both payload shapes.
//
// The real guard against forgetfulness is not the selection, it is the
// WRITERS' shape: they accept an opaque notifications.PrivateEventType
// whose value only registration yields (see
// internal/notifications/private.go), so a stock writer cannot be called
// with an unregistered type.
//
// ⚠ NARROWED (A/8 repair, second round): previously this said "it is
// IMPOSSIBLE to write the private shape past the registry". That is
// stronger than the fact — a plain INSERT into events with
// entity_type = 'user' inside the package compiles and works. The stock
// path is closed, not every path; the rest is held by assertions, not
// by types.
func loadPrivateRowsByForm(t *testing.T, pool *pgxpool.Pool, issueID string) []privateRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT id, type, entity_type, entity_id, project_id, COALESCE(field, '')
  FROM events
 WHERE entity_type = 'user'
   AND (   payload->>'issueId' = $1
        OR payload->'issueIds' @> to_jsonb($1::text))
 ORDER BY id`, issueID)
	if err != nil {
		t.Fatalf("read private rows: %v", err)
	}
	defer rows.Close()

	var out []privateRow
	for rows.Next() {
		var r privateRow
		if err := rows.Scan(&r.ID, &r.Type, &r.EntityType, &r.EntityID,
			&r.ProjectID, &r.Field); err != nil {
			t.Fatalf("scan private row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate private rows: %v", err)
	}
	return out
}

func countUnpublishedByIDs(t *testing.T, pool *pgxpool.Pool, ids []int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE id = ANY($1) AND published_at IS NULL`,
		ids).Scan(&n); err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	return n
}

// TestReviewChannel_NotPublishedToBus — reader 6: the live feed, that
// is the bus itself. The full gate of rows 32 and 36.
//
// Before wave A/4 a weaker claim stood here: "the private row's address
// is not a project one, it is a user one". That was an HONEST description
// of the state — the row's shape took it out of the project subject, but
// the user subject is not closed by shape: the live-feed bridge fans the
// `skein.user.<author>.*` namespace out to the addressee AND TO EVERY
// ADMINISTRATOR, so the author's private bookkeeping went to every
// administrator. T008 closed this channel by NON-PUBLICATION, and the
// claim had to become "is not published at all" — otherwise the test
// would keep confirming a retired state.
//
// What is checked, on a real queue and a real bus:
//
//  1. a private row does not arrive at a subscriber on ANY subject;
//  2. it nevertheless LEAVES the queue with a terminal stamp — without
//     that, not publishing it would cost an ever-growing lag metric;
//  3. auxiliary: the shape would still have taken the address into the
//     user namespace. This is not a duplicate of point 1, it is its
//     CAUSE: point 3 explains why shape alone is not enough and why
//     point 1 must exist.
//
// Rows are selected BY SHAPE, not by a type list — see loadPrivateRowsByForm.
func TestReviewChannel_NotPublishedToBus(t *testing.T) {
	f := doBothReviewOps(t, "revchbus")
	nc := startEmbeddedNATS(t)

	// Subscribe to EVERYTHING: the claim "is not published at all" has
	// no right to know in advance which subject the row would have gone
	// to. Selection of the relevant messages happens afterwards, by the
	// event type in the envelope.
	sub, err := nc.SubscribeSync("skein.>")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush sub: %v", err)
	}

	private := loadPrivateRowsByForm(t, f.pool, f.issueID)
	if len(private) != 2 {
		t.Fatalf("private rows found %d, want 2 — the assertions below "+
			"are not checked on every row", len(private))
	}
	ids := make([]int64, 0, len(private))
	privateTypes := make(map[string]struct{}, len(private))
	for _, r := range private {
		ids = append(ids, r.ID)
		privateTypes[r.Type] = struct{}{}
	}

	// Precondition: the rows STAND IN THE QUEUE unpublished. Without it
	// "they are not on the bus" would be proven by there being nothing
	// to publish.
	if n := countUnpublishedByIDs(t, f.pool, ids); n != len(ids) {
		t.Fatalf("precondition: %d of %d private rows in the queue — the test below "+
			"would prove the absence of data, not non-publication", n, len(ids))
	}

	// A real dispatcher, a real loop. It drains the queue as a whole, so
	// a foreign tail of the batch will also land on the bus — therefore
	// selection below is by the event type from the envelope, not by the
	// fact of a message arriving.
	ctx, cancel := context.WithCancel(context.Background())
	d := outbox.New(f.pool, nc, outbox.Options{
		BatchSize:    200,
		PollInterval: 20 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()

	deadline := time.Now().Add(60 * time.Second)
	for countUnpublishedByIDs(t, f.pool, ids) > 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("private rows did not leave the queue: they do not receive the same " +
				"terminal stamp as published ones, so they occupy the head of the batch " +
				"forever — the lag metric grows, the \"queue is behind\" alert burns " +
				"and does not go out (INV-2 must not cost a broken queue)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	// (1) Not a single message of a private type on the bus.
	for {
		msg, err := sub.NextMsg(300 * time.Millisecond)
		if err != nil {
			break
		}
		var env struct {
			EventType string `json:"eventType"`
		}
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			continue
		}
		if _, private := privateTypes[env.EventType]; private {
			t.Errorf("private row %q went onto the bus on subject %q — the live-feed bridge "+
				"fans the user namespace out to the addressee AND TO EVERY "+
				"ADMINISTRATOR, so the author's private bookkeeping is visible "+
				"to every administrator (INV-2, SC-003)", env.EventType, msg.Subject)
		}
	}

	// (3) The reason point 1 must exist: the private row's shape takes
	// the address out of the project subject — and exactly into the user
	// namespace the bridge fans out to administrators. The assertion is
	// pure (BuildSubject talks to neither the database nor the network),
	// so non-publication does not devalue it.
	for _, r := range private {
		subject := outbox.BuildSubject(outbox.SubjectInput{
			EventType:  r.Type,
			EntityType: r.EntityType,
			EntityID:   r.EntityID,
			ProjectID:  r.ProjectID,
			Field:      r.Field,
		})
		if strings.HasPrefix(subject, "skein.events.project.") {
			t.Errorf("private-row address %q = %q — the live-feed project subject "+
				"fans it out to every project subscriber, the assignee included "+
				"(INV-2, SC-003)", r.Type, subject)
		}
		if want := "skein.user." + f.author.String() + "."; !strings.HasPrefix(subject, want) {
			t.Errorf("private-row address %q = %q, want prefix %q — the row's "+
				"shape must take it into the user namespace",
				r.Type, subject, want)
		}
	}
}

// TestReviewChannel_DashboardDoesNotCount — reader 7: dashboard aggregates.
//
// The "before/after" difference is counted on the very aggregate that
// takes EVERY journal row without a type filter (projects → events). It
// is closed by the condition project_id IS NOT NULL, that is exactly by
// the private row's shape: give the row a project — and the number grows
// by two.
func TestReviewChannel_DashboardDoesNotCount(t *testing.T) {
	f := newReviewFixture(t, "revchdash")
	ctx := context.Background()
	ds := dashboard.New(f.pool)
	visible := map[uuid.UUID]struct{}{f.projectID: {}}

	countEventsMetric := func() int64 {
		t.Helper()
		res, err := ds.Projects(ctx, dashboard.ProjectsWindowAll, visible, true, nil)
		if err != nil {
			t.Fatalf("dashboard Projects: %v", err)
		}
		for _, row := range res.Rows {
			if row.ProjectID != nil && *row.ProjectID == f.projectID {
				return row.Events.Count
			}
		}
		return 0
	}

	before := countEventsMetric()

	if res := f.callMark(t, f.author, "user", nil); res.Status != http.StatusOK {
		t.Fatalf("mark: %d %s", res.Status, res.Raw)
	}
	v := readReviewRow(t, f.pool, f.issueID).Version
	if res := f.callPatch(t, f.author, "user", v,
		map[string]any{"reviewRequired": false}); res.Status != http.StatusOK {
		t.Fatalf("flag change: %d %s", res.Status, res.Raw)
	}
	if n := countEventsAnyForm(t, f.pool, f.issueID, issues.EventReviewMarked); n != 1 {
		t.Fatalf("precondition: mark rows %d, want one", n)
	}

	if after := countEventsMetric(); after != before {
		t.Errorf("the dashboard aggregate grew from %d to %d — the author's "+
			"private bookkeeping landed in project statistics that every "+
			"project member can see", before, after)
	}
}
