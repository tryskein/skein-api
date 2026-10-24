package nudges

// privacy_test.go — sk-303.14, repair A/8. A critical finding of
// the review: the author's private ledger leaked onto the bus to every
// administrator.
//
// # What was broken
//
// The audit row of a formed digest is written in USER form
// (entity_type='user', entity_id=<addressee>, project_id=NULL,
// payload={"issueIds":[…]}). The form is correct and takes the row out of
// the project subject — but NOT out of the user one. The queue dispatcher
// selects rows by "the publication stamp is empty", cuts only types of the
// closed set, and publishes the rest; outbox.BuildSubject with
// entity_type='user' yields `skein.user.<uid>.<action>`; the live-feed
// bridge (skein-web, NatsBridge.ForwardUserEvent) distributes that
// namespace under the condition `entry.IsAdmin || userId == targetUserId`.
// The nudge.*_digest types were not in the set — so EVERY connected
// administrator received a frame "user X has unreviewed issues <list of
// ids>".
//
// # How this is caught now
//
// By three assertions, and none of them repeats someone else's list:
//
//  1. the FORM of the row is pinned (nobody checked it before: a mutation
//     of entity_type 'user'→'issue' was green on the whole tree);
//  2. the row's type must be listed in the private-types REGISTRY
//     (notifications.PrivateEventTypes) — it is the registry the dispatcher
//     reads;
//  3. end-to-end: the real dispatcher runs the queue through a real bus,
//     and no message of a private type appears on it.
//
// A mutation of "strike the digest from the private-types registry" goes
// red on (2) and (3); a mutation of "return the row form to an issue" goes
// red on (1).
//
// ⚠ NARROWED WORDING (repair A/8, second round). This used to say that
// "writing a third private type past the registry is impossible at all".
// The narrower claim is true: you cannot CALL a typed writer
// (writeDigestEvent, issues.WritePrivateReviewEvent) with an unregistered
// type — its value is issued only by registration. Writing a private FORM
// with an ordinary INSERT inside the package is still possible, and it
// compiles; such a write is caught by assertions (1)–(3), not by the type
// system.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"skein-api/internal/notifications"
	"skein-api/internal/outbox"
	"skein-api/internal/review"
)

// digestAuditRow — the form of an audit row of a formed reminder.
type digestAuditRow struct {
	ID         int64
	Type       string
	EntityType string
	EntityID   string
	ProjectID  *uuid.UUID
	Payload    []byte
}

// digestAuditRowsFor reads the addressee's digest audit rows.
//
// Selection goes BY THE DELIVERY ROW'S REFERENCE (notifications.event_id),
// not by an assumed event form: a precondition that knows the form would
// fail first on a form mutation and pull the mutation into itself — it
// would go red as "no data", and the form assertion would never run.
func digestAuditRowsFor(t *testing.T, e *env, userID uuid.UUID, cat review.Category) []digestAuditRow {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `
SELECT ev.id, ev.type, ev.entity_type, ev.entity_id, ev.project_id, ev.payload
  FROM notifications n JOIN events ev ON ev.id = n.event_id
 WHERE n.user_id = $1 AND n.reason = $2
 ORDER BY ev.id`, userID, reasonOf(t, cat))
	if err != nil {
		t.Fatalf("reading digest audit rows: %v", err)
	}
	defer rows.Close()

	var out []digestAuditRow
	for rows.Next() {
		var r digestAuditRow
		if err := rows.Scan(&r.ID, &r.Type, &r.EntityType, &r.EntityID,
			&r.ProjectID, &r.Payload); err != nil {
			t.Fatalf("scanning an audit row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("walking audit rows: %v", err)
	}
	return out
}

// TestDigestAudit_PrivateFormAndRegisteredType — assertions (1) and (2).
func TestDigestAudit_PrivateFormAndRegisteredType(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	// Unaccepted — the addressee is the assignee. Unreviewed — the
	// addressee is the author.
	e.seed(t, issue{suffix: "privunacc", status: "open", assignee: workerName,
		assigneeSetAt: ago(90 * time.Minute), createdBy: author})
	e.seed(t, issue{suffix: "privunrev", status: "closed", assignee: workerName,
		assigneeSetAt: ago(40 * time.Hour), createdBy: author, reviewRequired: true})
	if _, err := e.pool.Exec(ctx,
		`UPDATE issues SET closed_at = $2 WHERE id = $1`,
		e.prefix+"-privunrev", time.Now().UTC().Add(-30*time.Hour)); err != nil {
		t.Fatalf("shifting the close moment: %v", err)
	}

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())

	for _, tc := range []struct {
		cat  review.Category
		user uuid.UUID
	}{
		{review.CategoryUnaccepted, worker},
		{review.CategoryUnreviewed, author},
	} {
		rows := digestAuditRowsFor(t, e, tc.user, tc.cat)
		if len(rows) != 1 {
			t.Fatalf("%s: digest audit rows %d, expected one — there is nothing to "+
				"check the form assertions against", tc.cat, len(rows))
		}
		r := rows[0]

		// (1) FORM. Each field is its own observation channel, so all three
		// are checked, not "roughly similar".
		if r.EntityType != "user" {
			t.Errorf("%s: audit-row entity %q, expected \"user\". A row on an issue "+
				"lands in the issue activity feed and in the live-feed project "+
				"subject — the assignee would see the author's private ledger (INV-2)",
				tc.cat, r.EntityType)
		}
		if r.EntityID != tc.user.String() {
			t.Errorf("%s: entity id %q, expected addressee %s — the digest is "+
				"addressed to a person, and another id would take the row into a "+
				"foreign namespace", tc.cat, r.EntityID, tc.user)
		}
		if r.ProjectID != nil {
			t.Errorf("%s: the audit row has project %s stamped — a non-empty "+
				"project returns it to the live-feed project subject and to "+
				"dashboard aggregates",
				tc.cat, *r.ProjectID)
		}
		var payload struct {
			IssueIDs []string `json:"issueIds"`
		}
		if err := json.Unmarshal(r.Payload, &payload); err != nil {
			t.Fatalf("%s: parsing the payload: %v", tc.cat, err)
		}
		if len(payload.IssueIDs) == 0 {
			t.Errorf("%s: the issue list in the payload is empty — the delivery "+
				"path reads issues from there", tc.cat)
		}

		// (1a) INV-7: the delivery row is NOT shown in the notification
		// feed. Checked by the observable state of the row, not by reading
		// the code: before repair A/8 the value was written as a literal,
		// there was no check against the registry, and a false→true
		// mutation was green on the whole tree.
		var feedChannel bool
		if err := e.pool.QueryRow(ctx,
			`SELECT feed_channel FROM notifications WHERE event_id = $1`, r.ID).
			Scan(&feedChannel); err != nil {
			t.Fatalf("%s: reading the delivery row: %v", tc.cat, err)
		}
		if feedChannel {
			t.Errorf("%s: the delivery row is marked as shown in the feed — "+
				"reminders do not create notification-feed entries (INV-7, SC-012); "+
				"the same fact would sit in the feed as many times as there were "+
				"segments the person did not react to",
				tc.cat)
		}

		// (2) the TYPE must be listed in the private registry: it is the
		// registry the queue dispatcher reads when deciding not to publish.
		if !notifications.IsPrivateEventType(r.Type) {
			t.Errorf("%s: audit-row type %q is not listed in the private-types "+
				"registry — the dispatcher will publish it on skein.user.<addressee>.*, "+
				"and the live-feed bridge distributes that namespace to the addressee "+
				"AND ALL ADMINISTRATORS (INV-2, SC-003)",
				tc.cat, r.Type)
		}
	}
}

// TestDigestAudit_NeverReachesTheBus — assertion (3), end-to-end.
//
// The real worker forms reminders, the real queue dispatcher runs the queue
// through a real bus. Subscription on `skein.>`: the assertion "is not
// published at all" is not entitled to know in advance which subject the
// row would have gone to.
func TestDigestAudit_NeverReachesTheBus(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	author, _ := e.user(t)
	worker, workerName := e.user(t)

	e.seed(t, issue{suffix: "busunacc", status: "open", assignee: workerName,
		assigneeSetAt: ago(90 * time.Minute), createdBy: author})

	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.NoLog = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL(), nats.Name("nudge-privacy-test"))
	if err != nil {
		t.Fatalf("connecting to the embedded bus: %v", err)
	}
	t.Cleanup(func() { _ = nc.Drain() })

	sub, err := nc.SubscribeSync("skein.>")
	if err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flushing the subscription: %v", err)
	}

	NewScheduler(e.store, Config{}).tickAt(ctx, time.Now().UTC())

	rows := digestAuditRowsFor(t, e, worker, review.CategoryUnaccepted)
	if len(rows) != 1 {
		t.Fatalf("precondition: audit rows %d, expected one", len(rows))
	}
	id := rows[0].ID
	digestType := rows[0].Type

	// Precondition: the row STANDS IN THE QUEUE unpublished. Without it
	// "it is not on the bus" would be proven by there being nothing to
	// publish.
	var unpublished int
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE id = $1 AND published_at IS NULL`, id).
		Scan(&unpublished); err != nil {
		t.Fatalf("precondition: reading the queue: %v", err)
	}
	if unpublished != 1 {
		t.Fatalf("precondition: audit row %d is already not in the queue — the test "+
			"below would prove absence of data, not non-publication", id)
	}

	dctx, cancel := context.WithCancel(context.Background())
	d := outbox.New(e.pool, nc, outbox.Options{
		BatchSize:    500,
		PollInterval: 20 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(dctx) }()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM events WHERE id = $1 AND published_at IS NULL`, id).
			Scan(&unpublished); err != nil {
			cancel()
			<-done
			t.Fatalf("reading the queue: %v", err)
		}
		if unpublished == 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the digest audit row did not leave the queue: it does not get " +
				"the same terminal stamp as published ones, and forever occupies the " +
				"head of the batch")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

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
		if env.EventType == digestType {
			t.Errorf("digest row %q went onto the bus on subject %q, body %s — the "+
				"live-feed bridge distributes the user namespace to the addressee AND "+
				"ALL ADMINISTRATORS, i.e. every administrator sees the author's list of "+
				"unreviewed issues (INV-2, SC-003)",
				env.EventType, msg.Subject, string(msg.Data))
		}
	}

	// Auxiliary: the reason the assertion above must exist. The row's form
	// takes the address precisely into the user namespace the bridge
	// distributes to administrators. A pure function — it does not go to
	// the database or the network.
	subject := outbox.BuildSubject(outbox.SubjectInput{
		EventType:  digestType,
		EntityType: rows[0].EntityType,
		EntityID:   rows[0].EntityID,
		ProjectID:  rows[0].ProjectID,
	})
	if want := "skein.user." + worker.String() + "."; !strings.HasPrefix(subject, want) {
		t.Errorf("digest-row address %q = %q, expected prefix %q — the row's form "+
			"must take it into the user namespace", digestType, subject, want)
	}
}
