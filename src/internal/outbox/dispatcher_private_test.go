package outbox

// dispatcher_private_test.go — sk-303.14 T008. Coverage-matrix rows 32
// (the "leaves the queue" half), 33 and 34.
//
// What is checked is the QUEUE CONTRACT, not the shape of the event row:
// a private row is claimed by the selection on a par with every other, does
// not go onto the bus and receives the same terminal stamp as published
// ones. The end-to-end half of row 32 ("it is not on the bus" against real
// author operations) and rows 35/36/38 live in
// internal/handler/issues_review_channels_test.go — real operations are
// there, a real queue is here.
//
// Mutations this file exists for, and what goes red:
//
//   - drop the unpublished (publish a private row) — the assertion about
//     the subscriber of the user subject fails;
//   - unpublish but not stamp (or, equivalently, strike the private type
//     from the SELECTION, as the plan rejected) — stamp, lag metric and
//     non-starvation fail: the row returns to the head of the batch
//     forever.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"skein-api/internal/notifications"
)

// insertPrivateEvent puts a row of the PRIVATE shape into the log: entity
// "user", project empty, the issue in the payload. Exactly how
// issues.WritePrivateReviewEvent writes it.
//
// project_id is NULL here not for decoration: because of it neither
// resetOutbox nor CountUnpublishedEventsForProject see the row — both are
// project-scoped. So the assertions below rest on the rows' own identifiers,
// not on project counters.
func insertPrivateEvent(t *testing.T, pool *pgxpool.Pool, author uuid.UUID, evtType, issueID string, occurredAt time.Time) int64 {
	t.Helper()
	at := occurredAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	payload, err := json.Marshal(map[string]any{"issueId": issueID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var id int64
	err = pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, field, payload, published_at)
VALUES ($1, $2, 'tester', $3, 'human', 'user', $4, NULL, NULL, $5::jsonb, NULL)
RETURNING id`, at, evtType, author, author.String(), string(payload)).Scan(&id)
	if err != nil {
		t.Fatalf("insert private event: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, id)
	})
	return id
}

// countUnpublished counts how many of the LISTED rows are still in the
// queue. Our own rows, not a global count: a dispatcher tick drains the
// shared table, and a global number under parallel packages is not about
// us.
func countUnpublished(t *testing.T, pool *pgxpool.Pool, ids []int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE id = ANY($1) AND published_at IS NULL`,
		ids).Scan(&n); err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	return n
}

// drainQueueToEmpty drains the queue WHOLESALE, to an empty tick, BEFORE
// putting the test fixture into it.
//
// Why: a tick takes rows in identifier order over the whole table, and a
// foreign accumulated tail would sit in front of our rows. Then "our rows
// did not move in N ticks" would mean the size of the database, not a
// breakage — i.e. the wait below would measure population, not behaviour.
// Draining the queue first makes the next tick about us.
//
// The batch here is large on purpose: draining a foreign tail must not
// cost hundreds of ticks.
func drainQueueToEmpty(t *testing.T, pool *pgxpool.Pool, nc *nats.Conn) {
	t.Helper()
	d := New(pool, nc, Options{BatchSize: 500})
	deadline := time.Now().Add(drainWaitBudget)
	for {
		n, err := tickBounded(func(ctx context.Context) (int, error) { return d.tick(ctx) }, deadline)
		if err != nil {
			t.Fatalf("pre-drain the queue: %v", err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queue did not drain in %s — the test fixture would sit behind a foreign tail",
				drainWaitBudget)
		}
	}
}

// drainOwnRows ticks until OUR rows leave the queue.
//
// The wait is built on progress over our rows, not on a tick count: a tick
// count is a dressed-up assumption about the size of the database. A tick
// that did not move OUR count is idle; the idle-tick window is the same as
// drainProjectWith's — it covers a foreign transaction holding rows under
// SKIP LOCKED with room to spare. Call after drainQueueToEmpty.
//
// Under the mutation "unpublished but not stamped" our row count never
// decreases, and the test fails fast with exact text instead of spinning
// out a two-minute budget.
func drainOwnRows(t *testing.T, d *Dispatcher, pool *pgxpool.Pool, ids []int64) {
	t.Helper()
	deadline := time.Now().Add(drainWaitBudget)
	prev := countUnpublished(t, pool, ids)
	idle := 0
	for {
		if prev == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("our rows did not leave the queue in %s (%d left)", drainWaitBudget, prev)
		}
		if _, err := tickBounded(func(ctx context.Context) (int, error) { return d.tick(ctx) }, deadline); err != nil {
			t.Fatalf("tick: %v", err)
		}
		now := countUnpublished(t, pool, ids)
		if now < prev {
			prev, idle = now, 0
			continue
		}
		idle++
		if idle >= drainMaxIdleTicks {
			t.Fatalf("in %d consecutive ticks none of our %d rows left the queue — "+
				"the rows are not getting a terminal stamp and return to the head of "+
				"the batch forever: the lag metric will grow forever, and ordinary events "+
				"will be starved out of the selection (T008)", idle, now)
		}
		time.Sleep(drainIdlePause)
	}
}

// TestDispatcher_PrivateEventLeavesQueueUnpublished — row 32 at queue
// level: a private row does NOT go onto the bus and still leaves the queue
// with the same terminal stamp published rows receive.
//
// The subscription is on `skein.user.<author>.>` — exactly the subject
// BuildSubject would build for the private shape and that the live-feed
// bridge fans out to the recipient and every administrator. The author's
// identifier is unique per run, so foreign messages cannot arrive on this
// subject.
func TestDispatcher_PrivateEventLeavesQueueUnpublished(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	author := seedUser(t, pool)
	pid := seedProject(t, pool, author)
	resetOutbox(t, pool, pid)

	drainQueueToEmpty(t, pool, nc)

	sub, err := nc.SubscribeSync(fmt.Sprintf("skein.user.%s.>", author.String()))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush sub: %v", err)
	}

	// The WHOLE registry of private types, not a list typed here.
	//
	// There was a list, and that is the one that once stayed silent: two
	// reminder-digest types were not put in the set, and the guard listed
	// the same two types as the set — i.e. it checked the set against
	// itself. Walking the registry makes a new private type covered by
	// this assertion the moment it is registered.
	privateTypes := notifications.PrivateEventTypes()
	if len(privateTypes) == 0 {
		t.Fatal("the registry of private types is empty — the assertion below would check emptiness")
	}
	var ids []int64
	for _, evtType := range privateTypes {
		ids = append(ids, insertPrivateEvent(t, pool, author, evtType, "obt-1", time.Time{}))
	}

	// Precondition: the rows ARE IN THE QUEUE. Without it "nothing is on
	// the bus" below would be proven by the absence of data.
	if n := countUnpublished(t, pool, ids); n != len(ids) {
		t.Fatalf("precondition: %d of %d private rows in the queue", n, len(ids))
	}

	d := New(pool, nc, Options{BatchSize: 10})
	drainOwnRows(t, d, pool, ids)

	for _, id := range ids {
		if at := publishedAt(t, pool, id); at == nil {
			t.Errorf("private row id=%d was left without a terminal stamp — it "+
				"occupies the head of the batch forever, the lag metric grows, the "+
				"\"queue is behind\" alert burns and never goes quiet (T008)", id)
		}
	}

	if msg, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("a private row went onto the bus: subject %q, body %s — the live-feed bridge "+
			"fans the user namespace out to the recipient AND TO EVERY "+
			"ADMINISTRATOR, so the author's private bookkeeping is visible "+
			"to every administrator (INV-2, SC-003)", msg.Subject, string(msg.Data))
	}
}

// TestDispatcher_PrivateEventInvisibleToLagGauge — row 33.
//
// The lag metric is counted by the same condition as the queue selection
// ("stamp is empty"), so "just don't publish" would not save it. The check
// is two-sided and therefore not tautological: FIRST it is proven that the
// metric SEES the private row while it is in the queue (otherwise "does
// not see after the tick" would only mean the metric is blind altogether),
// and only then — that after the tick it does not see it.
//
// The row is dated a day back: that guarantees the table minimum before
// the tick is this row, regardless of the database population, and after
// the tick the value must collapse.
func TestDispatcher_PrivateEventInvisibleToLagGauge(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	author := seedUser(t, pool)
	pid := seedProject(t, pool, author)
	resetOutbox(t, pool, pid)

	drainQueueToEmpty(t, pool, nc)

	const backdate = 24 * time.Hour
	id := insertPrivateEvent(t, pool, author,
		notifications.EventTypeReviewMarked, "obt-1", time.Now().UTC().Add(-backdate))

	d := New(pool, nc, Options{BatchSize: 10})
	ctx := context.Background()

	d.refreshLagGauge(ctx)
	if before := testutil.ToFloat64(lagSeconds); before < backdate.Seconds()*0.9 {
		t.Fatalf("precondition: lag before the tick %.0f s, expected around %.0f — "+
			"the metric does not see the private row even UNPUBLISHED, so "+
			"the assertion below proves nothing", before, backdate.Seconds())
	}

	drainOwnRows(t, d, pool, []int64{id})

	d.refreshLagGauge(ctx)
	if after := testutil.ToFloat64(lagSeconds); after >= backdate.Seconds()/2 {
		t.Errorf("lag after the tick %.0f s — the private row stayed in the queue "+
			"without a terminal stamp: the metric grows forever and the \"queue "+
			"is behind\" alert never goes quiet (T008)", after)
	}
}

// TestDispatcher_PrivateBatchDoesNotStarveOrdinaryEvents — row 34.
//
// A batch of private rows fills the tick wholesale (there are exactly
// BatchSize of them, and they are all older than the ordinary event by
// identifier, i.e. they sit at the head of the queue). The ordinary event
// must be published, not wait forever.
//
// Under the mutation "do not stamp" the private rows return to the
// selection every tick, the ordinary event never reaches the batch — and
// it is not a timeout that fails, it is the count of our rows in
// drainOwnRows.
func TestDispatcher_PrivateBatchDoesNotStarveOrdinaryEvents(t *testing.T) {
	pool := newTestPool(t)
	_, nc := startEmbeddedNATS(t)

	author := seedUser(t, pool)
	pid := seedProject(t, pool, author)
	resetOutbox(t, pool, pid)

	drainQueueToEmpty(t, pool, nc)

	sub, err := nc.SubscribeSync(fmt.Sprintf("skein.events.project.%s.>", pid.String()))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush sub: %v", err)
	}

	const batch = 5
	ids := make([]int64, 0, batch+1)
	for i := 0; i < batch; i++ {
		ids = append(ids, insertPrivateEvent(t, pool, author,
			notifications.EventTypeReviewMarked, fmt.Sprintf("obt-%d", i+1), time.Time{}))
	}
	ordinary := insertEvent(t, pool, author, pid, "issue.created", "issue", "obt-100", "", time.Time{})
	ids = append(ids, ordinary)

	d := New(pool, nc, Options{BatchSize: batch})
	drainOwnRows(t, d, pool, ids)

	if at := publishedAt(t, pool, ordinary); at == nil {
		t.Fatalf("ordinary event id=%d stayed in the queue — a batch of private rows "+
			"starved it out of the selection", ordinary)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("the ordinary event did not reach the project subscriber: %v — a batch of private "+
			"rows occupied the head of the queue", err)
	}
	wantSubj := fmt.Sprintf("skein.events.project.%s.issue.created.obt-100", pid.String())
	if msg.Subject != wantSubj {
		t.Errorf("subject mismatch: got %q want %q", msg.Subject, wantSubj)
	}
}
