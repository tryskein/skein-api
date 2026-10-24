package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// poll_for_monitor_test — store-level tests for PollForMonitor.
//
// sk-164 cutover: the monitor poll now reads the primary `events` audit log
// scoped to the token, NOT the personal `notifications` inbox. So every
// fixture here is EVENT-SEEDED (insertEvent / insertCommentEvent return the
// events.id — the id/cursor space the poll now operates in). Delivery is
// eligible by scope (project + issue/subtree) + type (minus the mention
// family) + the cursor/watermark/floor fences — NEVER by a user_id match and
// NEVER by a notifications row existing / being unread / being un-archived.
// The old notification-seeded suite asserted the opposite (user-scoped,
// archived-excluded); those assertions are INVERTED here (F1).

// epochCursor is a permissive watermark lower bound used by every non
// live-start sub-test: occurred_at > 1970 holds for any real row, so the
// cursor never excludes anything and each test isolates a single dimension
// (scope / type / session / watermark).
func epochCursor() (*time.Time, *int64) {
	ts := time.Unix(1, 0).UTC()
	id := int64(0)
	return &ts, &id
}

// dbNow returns the database clock — used so cursors derived in the test
// share the same clock the store's now() reads (no app/DB skew).
func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("db now(): %v", err)
	}
	return now
}

// sptr returns a pointer to s — a convenience for the actor_session_id arg.
func sptr(s string) *string { return &s }

// cleanupMonitorRows deletes the events (and any notifications) authored by the
// supplied users. events.actor_user_id is ON DELETE RESTRICT, so a test that
// seeds events under a user must remove them before seedTestUser's own
// user-delete cleanup runs. Registered AFTER the seedTestUser calls so LIFO
// runs this FIRST. Registering it before seedTestProject's issue/project
// cleanup is also fine — comment events reference the comment uuid by text
// (no FK), so ordering against comment deletion does not matter.
func cleanupMonitorRows(t *testing.T, pool *pgxpool.Pool, users ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM notifications WHERE user_id = ANY($1)`, users)
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE actor_user_id = ANY($1)`, users)
	})
}

// insertEvent inserts one events row and returns its bigint id (events.id) —
// the id/cursor space the monitor poll operates in after the sk-164 cutover.
// entityType/entityID set the event target (issue events carry the
// hierarchical id directly; comment events carry a comment uuid). occurredAt is
// a raw SQL expression (e.g. "now() - interval '1 minute'") so tests can place
// the row precisely relative to the DB clock for the watermark fence. session
// is written to actor_session_id verbatim; nil → SQL NULL (a session-less
// human / REST / system actor).
func insertEvent(t *testing.T, pool *pgxpool.Pool, actor, projectID uuid.UUID,
	entityType, entityID, ntype, occurredAt string, session *string) int64 {
	t.Helper()
	var eid int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_session_id, actor_type, payload)
VALUES (`+occurredAt+`, $1, $2, $3, $4, 'tester', $5, $6, 'human', '{}'::jsonb)
RETURNING id`, ntype, entityType, entityID, projectID, actor, session).Scan(&eid); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return eid
}

// insertCommentEvent seeds a comment under issueID, then an event of
// entity_type='comment' pointing at that comment's uuid. This is the primary
// monitor case (comment.created) and exercises the guarded event→issue
// normalisation join. issueID must reference a real issue row (comments.issue_id
// FK). Returns the events.id.
func insertCommentEvent(t *testing.T, pool *pgxpool.Pool, actor, projectID uuid.UUID,
	issueID, ntype, occurredAt string, session *string) int64 {
	t.Helper()
	commentID := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO comments (id, issue_id, author, author_user_id, author_type, body)
VALUES ($1, $2, 'tester', $3, 'human', 'hi')`, commentID, issueID, actor); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	return insertEvent(t, pool, actor, projectID,
		"comment", commentID.String(), ntype, occurredAt, session)
}

// seedInboxRow materialises a personal notifications row for eventID so a test
// can prove the monitor delivers the EVENT regardless of the human inbox row's
// read/archived state (or its absence) — the sk-164 event-tail is independent
// of the fan-out projection (F1).
func seedInboxRow(t *testing.T, pool *pgxpool.Pool, recipient uuid.UUID, eventID int64,
	ntype string, projectID uuid.UUID, read, archived bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO notifications (user_id, event_id, type, project_id, created_at, feed_channel, read_at, archived_at)
VALUES ($1, $2, $3, $4, now(), true,
        CASE WHEN $5 THEN now() ELSE NULL END,
        CASE WHEN $6 THEN now() ELSE NULL END)`,
		recipient, eventID, ntype, projectID, read, archived); err != nil {
		t.Fatalf("seed inbox row: %v", err)
	}
}

func idSet(res PollResult) map[int64]bool {
	got := make(map[int64]bool, len(res.Items))
	for _, it := range res.Items {
		got[it.ID] = true
	}
	return got
}

// ============================ T080 — scope / isolation / index ============================

// TestPollForMonitor_Scoping covers comment-aware scoping, subtree matching,
// the guarded issue-event cast, the event-type whitelist, and the ascending
// (occurred_at,id) order + cursor. sk-164 INVERSIONS vs the old suite:
//   - the archived-inbox row is now DELIVERED (its event is in scope; the
//     monitor no longer reads notifications, so archived_at cannot gate it);
//   - the "other user's notification excluded" case is GONE — delivery is by
//     scope+type, not by recipient user_id, so any actor's in-scope event is
//     delivered (that is exactly the cross-session/cross-user fix).
func TestPollForMonitor_Scoping(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	other := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor, other)
	projectID := seedTestProject(t, pool, caller)

	parent := "mp42"
	child := "mp42.1"
	seedTestIssue(t, pool, projectID, caller, parent, 2)
	// sk-622.21 (US-3): a descendant is a descendant by its parent-child
	// edge, not by the id prefix.
	seedChild(t, pool, projectID, caller, child, parent)

	const old = "now() - interval '1 minute'"

	// Comment on the parent issue.
	eParentComment := insertCommentEvent(t, pool, actor, projectID, parent, "comment.created", old, nil)
	// Comment on the child issue — visible only under subtree.
	eChildComment := insertCommentEvent(t, pool, actor, projectID, child, "comment.created", old, nil)
	// Issue event on the parent (proves the guarded ::uuid cast doesn't blow up
	// on the hierarchical, non-uuid entity_id).
	eParentIssueEvt := insertEvent(t, pool, actor, projectID, "issue", parent, "issue.status_changed", old, nil)
	// An event whose personal inbox row is ARCHIVED for the caller — under the
	// events source it is STILL delivered (inversion: archived_at cannot gate a
	// row the monitor never reads from notifications).
	eArchivedInbox := insertCommentEvent(t, pool, actor, projectID, parent, "comment.created", old, nil)
	seedInboxRow(t, pool, caller, eArchivedInbox, "comment.created", projectID, true /*read*/, true /*archived*/)
	// An event authored by a DIFFERENT user, in scope — now delivered (no
	// user_id predicate; this is the cross-user delivery the fix restores).
	eOtherUser := insertCommentEvent(t, pool, other, projectID, parent, "comment.created", old, nil)

	allTypes := []string{"comment.created", "issue.status_changed"}
	sinceTS, sinceID := epochCursor()

	base := func(subtree bool, types []string) PollParams {
		return PollParams{
			UserID:       caller,
			ProjectID:    projectID,
			ScopeIssueID: parent,
			ScopeSubtree: subtree,
			EventTypes:   types,
			SinceTS:      sinceTS,
			SinceID:      sinceID,
			Limit:        50,
			SafetyLag:    5 * time.Second,
		}
	}

	// --- subtree=false: parent comment + parent issue event; NOT child ---
	res, err := store.PollForMonitor(ctx, base(false, allTypes))
	if err != nil {
		t.Fatalf("poll subtree=false: %v", err)
	}
	got := idSet(res)
	if !got[eParentComment] {
		t.Errorf("parent comment %d must be visible", eParentComment)
	}
	if !got[eParentIssueEvt] {
		t.Errorf("parent issue event %d must be visible (guarded cast)", eParentIssueEvt)
	}
	if got[eChildComment] {
		t.Errorf("child comment %d must NOT be visible without subtree", eChildComment)
	}
	// INVERSION: archived-inbox event and other-user event ARE delivered now.
	if !got[eArchivedInbox] {
		t.Errorf("event %d with an archived personal inbox row must STILL be delivered (monitor reads events, not notifications)", eArchivedInbox)
	}
	if !got[eOtherUser] {
		t.Errorf("in-scope event %d authored by another user must be delivered (delivery is by scope+type, not user_id)", eOtherUser)
	}

	// --- subtree=true: child comment now visible ---
	res, err = store.PollForMonitor(ctx, base(true, allTypes))
	if err != nil {
		t.Fatalf("poll subtree=true: %v", err)
	}
	got = idSet(res)
	if !got[eParentComment] {
		t.Errorf("parent comment %d must be visible under subtree", eParentComment)
	}
	if !got[eChildComment] {
		t.Errorf("child comment %d must be visible under subtree", eChildComment)
	}
	// On a fixture without moves the subtree poll returns exactly the
	// literal set of in-scope events (sk-622.21, US-3: "on a fixture without
	// moves the poll result matches today's").
	want := map[int64]bool{eParentComment: true, eChildComment: true, eParentIssueEvt: true,
		eArchivedInbox: true, eOtherUser: true}
	if len(got) != len(want) {
		t.Errorf("subtree poll returned %d events, want exactly %d: got %v", len(got), len(want), got)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("subtree poll is missing in-scope event %d", id)
		}
	}

	// --- type filter: only comment.created → issue event excluded ---
	res, err = store.PollForMonitor(ctx, base(false, []string{"comment.created"}))
	if err != nil {
		t.Fatalf("poll type filter: %v", err)
	}
	got = idSet(res)
	if !got[eParentComment] {
		t.Errorf("comment %d must pass the comment.created whitelist", eParentComment)
	}
	if got[eParentIssueEvt] {
		t.Errorf("issue event %d must be excluded by the comment.created whitelist", eParentIssueEvt)
	}

	// --- ascending (occurred_at, id) order + cursor == last emitted tuple ---
	res, err = store.PollForMonitor(ctx, base(true, allTypes))
	if err != nil {
		t.Fatalf("poll order: %v", err)
	}
	for i := 1; i < len(res.Items); i++ {
		a, b := res.Items[i-1], res.Items[i]
		if a.CreatedAt.After(b.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && a.ID > b.ID) {
			t.Fatalf("rows not in ascending (occurred_at,id) order at %d", i)
		}
	}
	if n := len(res.Items); n > 0 {
		last := res.Items[n-1]
		if !res.CursorTS.Equal(last.CreatedAt) || res.CursorID != last.ID {
			t.Errorf("cursor %v/%d != last row %v/%d", res.CursorTS, res.CursorID, last.CreatedAt, last.ID)
		}
	}
}

// TestPollForMonitor_EscapeLike — sk-622.21 (US-3, row No. 28): a
// neighbouring branch is not a descendant. Scope "mpund_x" with subtree must
// deliver its child "mpund_x.1" (edge to mpund_x) and must NOT deliver
// "mpundYx.1" (whose parent is mpundYx). The property this test was written
// for — LIKE metacharacters in a custom id must not leak into a subtree
// match — disappeared with the LIKE; the '_' id is kept, so a return to an
// unescaped prefix match reddens it as before.
func TestPollForMonitor_EscapeLike(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	seedTestIssue(t, pool, projectID, caller, "mpund_x", 2)
	seedTestIssue(t, pool, projectID, caller, "mpundYx", 2)
	seedChild(t, pool, projectID, caller, "mpund_x.1", "mpund_x")
	seedChild(t, pool, projectID, caller, "mpundYx.1", "mpundYx")

	const old = "now() - interval '1 minute'"
	eTrueChild := insertEvent(t, pool, actor, projectID, "issue", "mpund_x.1", "issue.status_changed", old, nil)
	eSibling := insertEvent(t, pool, actor, projectID, "issue", "mpundYx.1", "issue.status_changed", old, nil)

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: "mpund_x",
		ScopeSubtree: true,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if !got[eTrueChild] {
		t.Errorf("true child mpund_x.1 (%d) must be in the subtree", eTrueChild)
	}
	if got[eSibling] {
		t.Errorf("mpundYx.1 (%d), child of another branch, must NOT be in the subtree of mpund_x", eSibling)
	}
}

// TestPollForMonitor_OtherProjectIsolation pins the defensive project filter
// (e.project_id = ProjectID, INV-6): an event whose project_id disagrees with
// the polled project — OR is NULL — is dropped even when its entity normalises
// to the in-scope issue. Under the events source this equality is load-bearing:
// hierarchical ids are globally unique, but a corrupt/foreign event that reuses
// the scoped id must never leak across the project boundary.
func TestPollForMonitor_OtherProjectIsolation(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	otherProject := uuid.Must(uuid.NewRandom())
	const issueID = "g8proj1"
	const old = "now() - interval '1 minute'"

	eSame := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, nil)
	eOther := insertEvent(t, pool, actor, otherProject, "issue", issueID, "issue.status_changed", old, nil)
	// A NULL project_id event with a matching entity — also excluded (INV-6).
	var eNull int64
	if err := pool.QueryRow(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_type, payload)
VALUES (now() - interval '1 minute', 'issue.status_changed', 'issue', $1, NULL,
        'tester', $2, 'human', '{}'::jsonb)
RETURNING id`, issueID, actor).Scan(&eNull); err != nil {
		t.Fatalf("insert null-project event: %v", err)
	}

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if !got[eSame] {
		t.Errorf("same-project event %d must be returned", eSame)
	}
	if got[eOther] {
		t.Errorf("other-project event %d must be excluded by the project filter", eOther)
	}
	if got[eNull] {
		t.Errorf("NULL-project event %d must be excluded by the project filter", eNull)
	}
}

// TestPollForMonitor_SubtreeIsByIdPrefixOnly — sk-622.21 (US-3, INV-3,
// row No. 28) replaced the documented v1 limitation it pinned: subtree
// membership is the live canonical hierarchy, NOT the id prefix. An issue
// whose id starts with the watched id but has no edge to it is not
// delivered; an issue with an unrelated id but an edge under the watched
// one is. (The name is kept so the history of the property stays findable.)
func TestPollForMonitor_SubtreeIsByIdPrefixOnly(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	seedTestIssue(t, pool, projectID, caller, "g9par", 2)
	seedTestIssue(t, pool, projectID, caller, "g9par.1", 2) // prefix only, no edge
	seedChild(t, pool, projectID, caller, "g9moved1", "g9par")
	const old = "now() - interval '1 minute'"

	ePrefixOnly := insertEvent(t, pool, actor, projectID, "issue", "g9par.1", "issue.status_changed", old, nil)
	eByEdge := insertEvent(t, pool, actor, projectID, "issue", "g9moved1", "issue.status_changed", old, nil)

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: "g9par",
		ScopeSubtree: true,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if got[ePrefixOnly] {
		t.Errorf("g9par.1 (%d) shares the id prefix but has no edge to g9par — it must NOT be delivered", ePrefixOnly)
	}
	if !got[eByEdge] {
		t.Errorf("g9moved1 (%d) is a child of g9par by edge — it must be delivered", eByEdge)
	}
}

// TestPollForMonitor_SubtreeFollowsAReparent — sk-622.21 (US-3, row No. 28): T
// moves from under A to under B. After the move, A's subtree monitor does not
// return T's new events and B's does. The move is the edge rewrite
// dependencies.Store#Reparent performs (that package imports this one, so
// the edge rows are rewritten here directly).
func TestPollForMonitor_SubtreeFollowsAReparent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	seedTestIssue(t, pool, projectID, caller, "mprpa", 2)
	seedTestIssue(t, pool, projectID, caller, "mprpb", 2)
	seedChild(t, pool, projectID, caller, "mprpt", "mprpa")
	const old = "now() - interval '1 minute'"

	poll := func(scope string) map[int64]bool {
		t.Helper()
		sinceTS, sinceID := epochCursor()
		res, err := store.PollForMonitor(ctx, PollParams{
			UserID: caller, ProjectID: projectID, ScopeIssueID: scope, ScopeSubtree: true,
			EventTypes: []string{"issue.status_changed"},
			SinceTS:    sinceTS, SinceID: sinceID, Limit: 50, SafetyLag: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("poll %s: %v", scope, err)
		}
		return idSet(res)
	}

	before := insertEvent(t, pool, actor, projectID, "issue", "mprpt", "issue.status_changed", old, nil)
	if !poll("mprpa")[before] {
		t.Fatalf("precondition: A's subtree monitor returns T's event before the move")
	}

	if _, err := pool.Exec(ctx, `
DELETE FROM dependencies WHERE from_id = 'mprpt' AND type = 'parent-child';
`); err != nil {
		t.Fatalf("drop old edge: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
VALUES ('mprpt', 'mprpb', 'parent-child', '{}'::jsonb, 1, $1, now())`, caller); err != nil {
		t.Fatalf("new edge: %v", err)
	}

	after := insertEvent(t, pool, actor, projectID, "issue", "mprpt", "issue.status_changed", old, nil)
	if poll("mprpa")[after] {
		t.Errorf("A's subtree monitor returned T's new event %d after T moved under B", after)
	}
	if !poll("mprpb")[after] {
		t.Errorf("B's subtree monitor did not return T's new event %d after the move", after)
	}
}

// TestPollForMonitor_StatementsPerPollAreFixed — sk-622.21 (SC-013, SC-002,
// row No. 28): the subtree is resolved inside the poll statement, never by a
// query per ancestor or per event. The statements one poll sends are counted
// by a pgx tracer and compared for equality between a shallow and a deep
// subtree, and between a small and a large page of events.
func TestPollForMonitor_StatementsPerPollAreFixed(t *testing.T) {
	pool, tracer := newTracedTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)

	// corpus seeds a chain root ← … ← leaf of the given depth and `events`
	// issue events on the leaf, and returns the statements of one subtree
	// poll on the root plus how many events it returned.
	corpus := func(tag string, depth, events int) ([]string, int) {
		t.Helper()
		root := "mpst" + tag + "0"
		seedTestIssue(t, pool, projectID, caller, root, 2)
		leaf := root
		for d := 1; d <= depth; d++ {
			id := "mpst" + tag + string(rune('0'+d))
			seedChild(t, pool, projectID, caller, id, leaf)
			leaf = id
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_user_id, actor_type, payload)
SELECT now() - interval '1 minute', 'issue.status_changed', 'issue', $1, $2, 'tester', $3, 'human', '{}'::jsonb
  FROM generate_series(1, $4) g`, leaf, projectID, actor, events); err != nil {
			t.Fatalf("seed events: %v", err)
		}
		sinceTS, sinceID := epochCursor()
		var res PollResult
		var err error
		queries := tracer.record(func() {
			res, err = store.PollForMonitor(ctx, PollParams{
				UserID: caller, ProjectID: projectID, ScopeIssueID: root, ScopeSubtree: true,
				EventTypes: []string{"issue.status_changed"},
				SinceTS:    sinceTS, SinceID: sinceID, Limit: 100, SafetyLag: 5 * time.Second,
			})
		})
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		return queries, len(res.Items)
	}

	shallow, nShallow := corpus("a", 1, 10)
	deep, nDeep := corpus("b", 5, 10)
	many, nMany := corpus("c", 1, 500)
	if nShallow != 10 || nDeep != 10 || nMany != 100 {
		t.Fatalf("polls returned %d / %d / %d events, want 10 / 10 / 100 (a full page) — "+
			"a count over an empty poll proves nothing", nShallow, nDeep, nMany)
	}
	if len(deep) != len(shallow) {
		t.Errorf("statements per poll: %d at depth 5, %d at depth 1 — the subtree costs a query per ancestor\n%s",
			len(deep), len(shallow), strings.Join(deep, "\n---\n"))
	}
	if len(many) != len(shallow) {
		t.Errorf("statements per poll: %d with 500 events, %d with 10 — a query per event\n%s",
			len(many), len(shallow), strings.Join(many, "\n---\n"))
	}
}

// TestPollForMonitor_SameCreatedAtIDTiebreak asserts the (occurred_at, id)
// ordering breaks ties on events.id: two rows sharing an identical occurred_at
// are both returned, in ascending id order, and the cursor is the LAST
// (highest) id.
func TestPollForMonitor_SameCreatedAtIDTiebreak(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "g8tie1"
	const same = "now() - interval '1 minute'"

	e1 := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", same, nil)
	e2 := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", same, nil)
	if e2 <= e1 {
		t.Fatalf("expected e2 (%d) > e1 (%d) — bigserial should advance", e2, e1)
	}

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if !got[e1] || !got[e2] {
		t.Fatalf("both same-occurred_at rows must be returned: got %v (want %d and %d)", got, e1, e2)
	}
	var i1, i2 = -1, -1
	for i, it := range res.Items {
		if it.ID == e1 {
			i1 = i
		}
		if it.ID == e2 {
			i2 = i
		}
	}
	if i1 < 0 || i2 < 0 || i1 >= i2 {
		t.Fatalf("rows not ordered by id ascending on an occurred_at tie: i1=%d i2=%d", i1, i2)
	}
	if res.CursorID != e2 {
		t.Errorf("cursor id = %d, want %d (last/highest id on the occurred_at tie)", res.CursorID, e2)
	}
}

// TestPollForMonitor_IndexCoverage is the sk-164 T080 index-coverage gate. It
// is a CLASS gate, not an index-name gate: the final pollForMonitorSQL (with
// the T012 mention predicate and the T021 exclude_session predicate) must be
// served WITHOUT a `Seq Scan on events`, with a bounded sort, in BOTH subtree
// modes AND under a forced generic plan (the prod path — pgx prepared
// statements). The selective driver is the occurred_at watermark window served
// by idx_events_occurred; the scope predicates apply as bounded row-filters
// over that narrow window — confirmed by EXPLAIN on the poll query.
//
// A `Seq Scan on events` or an unbounded sort here is a STOP → escalate to
// T086 (gated DDL), NOT a silent index tacked onto the test.
func TestPollForMonitor_IndexCoverage(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	// Registered BEFORE the project, so it runs AFTER the project cleanup
	// has deleted the seeded issues and edges: re-ANALYZE those shared
	// tables so the next package plans on their true (small) contents.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ANALYZE issues`)
		_, _ = pool.Exec(context.Background(), `ANALYZE dependencies`)
	})
	projectID := seedTestProject(t, pool, caller)
	parent := "mpidx1"
	seedTestIssue(t, pool, projectID, caller, parent, 2)

	// Seed two populations so the planner has a genuine choice (a near-empty
	// table would index-scan trivially and prove nothing):
	//   1. ~8000 noise events by a DIFFERENT user on an unrelated project,
	//      spread over ~90 days so the recent-watermark window is selective.
	//   2. ~300 in-scope events on the target project/subtree.
	// events.project_id has no FK, so the noise project id can be synthetic.
	noiseProject := uuid.Must(uuid.NewRandom())
	if _, err := pool.Exec(ctx, `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
SELECT now() - ((g % 129600))*interval '1 minute',
       (ARRAY['comment.created','issue.status_changed'])[1 + g % 2],
       'tester', $1, 'human', 'issue', 'noise'||g,
       $2, '{}'::jsonb
FROM generate_series(1, 8000) g`, actor, noiseProject); err != nil {
		t.Fatalf("noise seed: %v", err)
	}
	// This test ANALYZEs the SHARED `events` table (unavoidable — the planner
	// needs real stats to make a genuine index-vs-seqscan choice). To keep that
	// from destabilising sibling packages that also read `events` under one
	// `go test -p 1 ./...` run (the dashboard activity tests bucket events into
	// day windows and are sensitive to skewed planner stats / transient volume),
	// delete the ~8.3k seeded rows and re-ANALYZE at the END of this test so the
	// table's stats reflect its true (small) contents before the next package
	// runs. Registered last → runs FIRST in LIFO: it deletes every event this
	// test seeded (all authored by `actor`) and ANALYZEs; the later
	// cleanupMonitorRows then finds no events to drop (no-op) and never
	// re-ANALYZEs, so this accurate post-delete ANALYZE is the final word on the
	// table's stats. cleanupMonitorRows stays as belt-and-braces.
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE actor_user_id = ANY($1)`, []uuid.UUID{caller, actor})
		_, _ = pool.Exec(bg, `ANALYZE events`)
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO events (occurred_at, type, actor, actor_user_id, actor_type,
                    entity_type, entity_id, project_id, payload)
SELECT now() - ((g % 5000))*interval '1 minute',
       (ARRAY['comment.created','issue.status_changed'])[1 + g % 2],
       'tester', $1, 'human', 'issue',
       CASE WHEN g % 2 = 0 THEN $2 ELSE $2||'.1' END,
       $3, '{}'::jsonb
FROM generate_series(1, 300) g`, actor, parent, projectID); err != nil {
		t.Fatalf("caller-slice seed: %v", err)
	}
	// sk-622.21 (row No. 28): the subtree is the hierarchy, so the poll walks
	// dependencies and issues. Seed the child the in-scope events name and
	// tens of thousands of other issues in parent-child chains, so the planner has a
	// genuine choice on those relations too (a near-empty table is honestly
	// seq-scanned and would make the gate below meaningless).
	seedChild(t, pool, projectID, caller, parent+".1", parent)
	if _, err := pool.Exec(ctx, `
INSERT INTO issues (id, project_id, parent_id, version, title, description, type, priority, status,
                    pinned, source, assignee, design, acceptance_criteria, notes,
                    created_by, created_at, updated_at, metadata)
SELECT 'mpidxn-' || g, $1, NULL, 1, 'noise', '', 'task', 2, 'open',
       false, 'human', NULL, '', '', '', $2, now(), now(), '{}'::jsonb
  FROM generate_series(1, 30000) g`, projectID, caller); err != nil {
		t.Fatalf("noise issues: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO dependencies (from_id, to_id, type, metadata, version, created_by, created_at)
SELECT 'mpidxn-' || g, 'mpidxn-' || (g - 1), 'parent-child', '{}'::jsonb, 1, $1, now()
  FROM generate_series(2, 30000) g
 WHERE g % 50 <> 1`, caller); err != nil {
		t.Fatalf("noise edges: %v", err)
	}
	for _, tbl := range []string{"events", "issues", "dependencies", "comments"} {
		if _, err := pool.Exec(ctx, "ANALYZE "+tbl); err != nil {
			t.Fatalf("analyze %s: %v", tbl, err)
		}
	}

	boundTS := dbNow(t, pool).Add(-10 * time.Minute)
	args := func(subtree bool) []any {
		return []any{
			[]string{"comment.created", "issue.status_changed"}, // $1 types
			5 * time.Second, // $2 safety lag
			boundTS,         // $3 cursor ts
			int64(0),        // $4 cursor id
			parent,          // $5 scope issue
			subtree,         // $6 subtree
			50,              // $7 limit
			projectID,       // $8 project
			"",              // $9 exclude_session
		}
	}

	assertPlan := func(t *testing.T, label, planText string) {
		t.Helper()
		t.Logf("%s plan:\n%s", label, planText)
		// HARD gate (the sk-164 T080 class contract): the events relation must
		// never be full-scanned. A Seq Scan here is the STOP signal → escalate to
		// T086 (gated DDL), NOT a silent index tacked on.
		if strings.Contains(planText, "Seq Scan on events") {
			t.Errorf("[%s] plan must NOT Seq Scan events (STOP → escalate T086 gated DDL), got:\n%s", label, planText)
		}
		// Events must be reached via an INDEX access path (proves the row-filter
		// window is bounded, not a full-table scan). Which index the planner
		// picks is selectivity-dependent and NOT asserted: for exact scope
		// (subtree=false) the highly-selective entity match drives idx_events_entity;
		// for subtree (the hierarchy walk) the occurred_at window drives
		// idx_events_occurred; the project predicate contributes idx_events_project.
		// All three are legitimate indexed paths — asserting one specific index is
		// brittle (it flips with the data distribution) — see the EXPLAIN
		// measurements recorded for the poll query.
		if !strings.Contains(planText, "idx_events_") {
			t.Errorf("[%s] events must be reached via an events index (idx_events_*), not a full scan, got:\n%s", label, planText)
		}
		// Bounded output: the LIMIT node caps the result, so the (top-N) sort over
		// the index-bounded row set is bounded — an unbounded sort only arises
		// atop a Seq Scan, already rejected above.
		if !strings.Contains(planText, "Limit") {
			t.Errorf("[%s] plan must carry a bounded Limit node, got:\n%s", label, planText)
		}
	}

	// --- custom plan (params known at plan time), both subtree modes ---
	for _, subtree := range []bool{false, true} {
		label := "custom subtree=" + boolStr(subtree)
		rows, err := pool.Query(ctx, `EXPLAIN `+pollForMonitorSQL, args(subtree)...)
		if err != nil {
			t.Fatalf("[%s] explain: %v", label, err)
		}
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("[%s] scan plan line: %v", label, err)
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("[%s] explain rows: %v", label, err)
		}
		assertPlan(t, label, b.String())

		var raw []byte
		if err := pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+pollForMonitorSQL, args(subtree)...).Scan(&raw); err != nil {
			t.Fatalf("[%s] explain json: %v", label, err)
		}
		assertNoGuardedSeqScan(t, label, raw)
	}

	// --- finishMonitorPollSQL, the poll's second statement (SC-002, row No. 28):
	// it carries the same admission predicate and subtree walk inside the
	// root-closed debt, plus the scope root's own ancestor walk. Same gate on
	// the guarded relations, custom and generic plan, both subtree modes. $7
	// is the credential id here; EXPLAIN does not execute the UPDATE. The
	// custom plans run before any generic one: explainGeneric DEALLOCATEs
	// ALL on its connection, pgx's statement cache included.
	credentialID := uuid.Must(uuid.NewRandom())
	for _, subtree := range []bool{false, true} {
		label := "finish custom subtree=" + boolStr(subtree)
		finishArgs := args(subtree)
		finishArgs[6] = credentialID
		var raw []byte
		if err := pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+finishMonitorPollSQL, finishArgs...).Scan(&raw); err != nil {
			t.Fatalf("[%s] explain json: %v", label, err)
		}
		t.Logf("%s plan:\n%s", label, raw)
		assertNoGuardedSeqScan(t, label, raw)
	}

	// --- generic plan (prod pgx prepared-statement path), both subtree modes ---
	for _, subtree := range []bool{false, true} {
		label := "generic subtree=" + boolStr(subtree)
		plan := explainGeneric(t, pool, pollForMonitorSQL, "50", subtree, boundTS, parent, projectID, "")
		assertPlan(t, label, plan)
		assertNoGuardedSeqScan(t, label, []byte(explainGeneric(t, pool, pollForMonitorSQL, "50",
			subtree, boundTS, parent, projectID, "(FORMAT JSON) ")))
	}

	for _, subtree := range []bool{false, true} {
		label := "finish generic subtree=" + boolStr(subtree)
		assertNoGuardedSeqScan(t, label, []byte(explainGeneric(t, pool, finishMonitorPollSQL,
			"'"+credentialID.String()+"'::uuid", subtree, boundTS, parent, projectID, "(FORMAT JSON) ")))
	}
}

// pollPlanNode is the part of a JSON plan node the SC-002 gate reads.
type pollPlanNode struct {
	NodeType     string         `json:"Node Type"`
	RelationName string         `json:"Relation Name"`
	Plans        []pollPlanNode `json:"Plans"`
}

// assertNoGuardedSeqScan walks a JSON plan (sk-622.21, row No. 28, SC-002):
// no Seq Scan on notifications, issue_watchers, issue_watch_optouts, issues,
// dependencies or users. Walked by Node Type / Relation Name, not by text.
func assertNoGuardedSeqScan(t *testing.T, label string, raw []byte) {
	t.Helper()
	guarded := map[string]bool{"notifications": true, "issue_watchers": true, "issue_watch_optouts": true,
		"issues": true, "dependencies": true, "users": true}
	var plans []struct {
		Plan pollPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) == 0 {
		t.Fatalf("[%s] parse JSON plan: %v\n%s", label, err, raw)
	}
	var walk func(n pollPlanNode)
	walk = func(n pollPlanNode) {
		if n.NodeType == "Seq Scan" && guarded[n.RelationName] {
			t.Errorf("[%s] plan has Seq Scan on %s (SC-002)", label, n.RelationName)
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(plans[0].Plan)
}

// boolStr renders a bool for a plan label (Go's %v would do, but this keeps the
// label building allocation-free and greppable).
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// explainGeneric returns the plan text of stmt (pollForMonitorSQL or
// finishMonitorPollSQL — they share the parameter layout; seventh is the SQL
// literal of $7: the page LIMIT or the credential id) under a FORCED
// generic plan — the plan pgx's prepared-statement path produces in prod. It
// runs SET plan_cache_mode=force_generic_plan + PREPARE + EXPLAIN EXECUTE +
// cleanup on ONE pooled connection via the simple protocol (so pgx does not
// mis-read the $N placeholders inside the PREPARE body as its own bind params).
// Literals are inlined from test-controlled values (no injection surface).
func explainGeneric(t *testing.T, pool *pgxpool.Pool, stmt, seventh string, subtree bool,
	boundTS time.Time, scopeIssue string, projectID uuid.UUID, options string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	script := "DEALLOCATE ALL;\n" +
		"SET plan_cache_mode = force_generic_plan;\n" +
		"PREPARE m164 AS " + stmt + ";\n" +
		"EXPLAIN " + options + "EXECUTE m164(" +
		"ARRAY['comment.created','issue.status_changed']::text[], " +
		"'5 seconds'::interval, " +
		"'" + boundTS.UTC().Format("2006-01-02 15:04:05.999999-07") + "'::timestamptz, " +
		"0::bigint, " +
		"'" + scopeIssue + "', " +
		boolStr(subtree) + ", " +
		seventh + ", " +
		"'" + projectID.String() + "'::uuid, " +
		"'');\n"

	mrr := conn.Conn().PgConn().Exec(ctx, script)
	results, err := mrr.ReadAll()
	if err != nil {
		t.Fatalf("generic explain exec: %v", err)
	}
	// Reset the session GUC + prepared statement for the pooled conn's next user.
	_, _ = conn.Conn().PgConn().Exec(ctx, "DEALLOCATE ALL; RESET plan_cache_mode;").ReadAll()

	// The EXPLAIN is the last result carrying rows.
	var b strings.Builder
	for _, r := range results {
		if len(r.Rows) == 0 {
			continue
		}
		for _, row := range r.Rows {
			b.Write(row[0])
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		t.Fatalf("generic explain produced no plan rows")
	}
	return b.String()
}

// ============================ T081 — delivery / echo / floor class gates ============================

// TestPollForMonitor_Watermark asserts an event younger than the safety lag is
// withheld, then surfaced once it ages past the lag.
func TestPollForMonitor_Watermark(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpwm1"

	// Fresh (now()) issue event — inside the 5s lag window.
	eid := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", "now()", nil)

	sinceTS, sinceID := epochCursor()
	params := PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	}

	res, err := store.PollForMonitor(ctx, params)
	if err != nil {
		t.Fatalf("poll (fresh): %v", err)
	}
	if idSet(res)[eid] {
		t.Errorf("event %d younger than the safety lag must be withheld", eid)
	}

	if _, err := pool.Exec(ctx, `UPDATE events SET occurred_at = now() - interval '10 seconds' WHERE id=$1`, eid); err != nil {
		t.Fatalf("age event: %v", err)
	}
	res, err = store.PollForMonitor(ctx, params)
	if err != nil {
		t.Fatalf("poll (aged): %v", err)
	}
	if !idSet(res)[eid] {
		t.Errorf("event %d aged past the safety lag must be returned", eid)
	}
}

// TestPollForMonitor_LiveStart asserts the absent-cursor contract (INV-3): a
// pre-existing old event is NOT returned on the first (live-start) poll, the
// returned cursor is the live anchor (~ now()), and an event created after the
// anchor IS returned on the next poll using that cursor. SafetyLag is 0 so the
// live-start exclusion is isolated from the watermark.
func TestPollForMonitor_LiveStart(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpls1"

	eOld := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed",
		"now() - interval '1 hour'", nil)

	params := func(sinceTS *time.Time, sinceID *int64) PollParams {
		return PollParams{
			UserID:       caller,
			ProjectID:    projectID,
			ScopeIssueID: issueID,
			EventTypes:   []string{"issue.status_changed"},
			SinceTS:      sinceTS,
			SinceID:      sinceID,
			Limit:        50,
			SafetyLag:    0,
		}
	}

	before := dbNow(t, pool)

	res, err := store.PollForMonitor(ctx, params(nil, nil))
	if err != nil {
		t.Fatalf("poll (live-start): %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("live-start poll must return no backlog, got %d rows", len(res.Items))
	}
	after := dbNow(t, pool)
	if res.CursorTS.Before(before) || res.CursorTS.After(after) {
		t.Fatalf("live-start cursor %v not within [%v, %v]", res.CursorTS, before, after)
	}
	if res.CursorID != 0 {
		t.Errorf("live-start cursor id = %d, want 0", res.CursorID)
	}
	anchorTS, anchorID := res.CursorTS, res.CursorID

	eNew := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", "now()", nil)

	res, err = store.PollForMonitor(ctx, params(&anchorTS, &anchorID))
	if err != nil {
		t.Fatalf("poll (continuation): %v", err)
	}
	got := idSet(res)
	if !got[eNew] {
		t.Errorf("event %d created after the anchor must surface on the next poll", eNew)
	}
	if got[eOld] {
		t.Errorf("backlog %d created before the anchor must never surface", eOld)
	}
}

// TestPollForMonitor_CursorFloor is the regression for the server-owned
// not-before floor (claims.CreatedAt, INV-3): a forged OLD cursor
// (since_ts=epoch) must not backfill in-scope events committed before the
// credential mint time. Floor runs on occurred_at (the events source).
func TestPollForMonitor_CursorFloor(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpcf1"

	eOld := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed",
		"now() - interval '1 hour'", nil)

	floor := dbNow(t, pool)

	eNew := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", "now()", nil)

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Floor:        floor,
		Limit:        50,
		SafetyLag:    0,
	})
	if err != nil {
		t.Fatalf("poll (forged old cursor): %v", err)
	}
	got := idSet(res)
	if got[eOld] {
		t.Errorf("backlog %d before the credential floor must NOT surface via a forged old cursor", eOld)
	}
	if !got[eNew] {
		t.Errorf("event %d after the floor must still surface", eNew)
	}
}

// TestPollForMonitor_CrossSessionSameUser is the SC-001 class gate: two events
// authored by the SAME owner user under DIFFERENT sessions are BOTH delivered
// (delivery does not depend on user_id), and each carries its own
// actorSessionId marker so the reader can self-skip. This is the root fix — the
// old notification-seeded path muted between two sessions of one owner.
func TestPollForMonitor_CrossSessionSameUser(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	owner := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, owner)
	projectID := seedTestProject(t, pool, owner)
	const issueID = "mpxs1"
	const old = "now() - interval '1 minute'"

	// Both events are authored by the SAME owner user, one per session.
	eSessA := insertEvent(t, pool, owner, projectID, "issue", issueID, "issue.status_changed", old, sptr("mcp-session-A"))
	eSessB := insertEvent(t, pool, owner, projectID, "issue", issueID, "issue.status_changed", old, sptr("mcp-session-B"))

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       owner, // owner == actor; old path muted, new path delivers
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	bySession := map[int64]*string{}
	for _, it := range res.Items {
		bySession[it.ID] = it.ActorSessionID
	}
	if _, ok := bySession[eSessA]; !ok {
		t.Errorf("event %d (session A, same owner) must be delivered", eSessA)
	}
	if _, ok := bySession[eSessB]; !ok {
		t.Errorf("event %d (session B, same owner) must be delivered", eSessB)
	}
	if got := bySession[eSessA]; got == nil || *got != "mcp-session-A" {
		t.Errorf("event %d actorSessionId = %v, want mcp-session-A (reader self-skip marker)", eSessA, ptrStr(got))
	}
	if got := bySession[eSessB]; got == nil || *got != "mcp-session-B" {
		t.Errorf("event %d actorSessionId = %v, want mcp-session-B", eSessB, ptrStr(got))
	}
}

// TestPollForMonitor_CrossUserDelivery is the SC-003 class gate: an in-scope
// event authored by a DIFFERENT owner (U2) is delivered to U1's monitor — the
// cross-user delivery the pre-fix path already had must survive the source
// switch.
func TestPollForMonitor_CrossUserDelivery(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	u1 := seedTestUser(t, pool, "user")
	u2 := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, u1, u2)
	projectID := seedTestProject(t, pool, u1)
	const issueID = "mpxu1"
	const old = "now() - interval '1 minute'"

	eU2 := insertEvent(t, pool, u2, projectID, "issue", issueID, "issue.status_changed", old, sptr("mcp-session-u2"))

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       u1,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !idSet(res)[eU2] {
		t.Errorf("event %d authored by another owner (U2) must be delivered to U1's monitor (SC-003)", eU2)
	}
}

// TestPollForMonitor_HumanOwnerEmptySession is the SC-007 / US-5 class gate: an
// event with a NULL actor_session_id (a human owner acting in the Web UI, a
// REST call, or a system event) is delivered, its actorSessionId is ABSENT
// (nil), and it is NEVER cut by exclude_session — echo suppression is
// strict-equality of two NON-empty sessions.
func TestPollForMonitor_HumanOwnerEmptySession(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	owner := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, owner)
	projectID := seedTestProject(t, pool, owner)
	const issueID = "mphe1"
	const old = "now() - interval '1 minute'"

	eHuman := insertEvent(t, pool, owner, projectID, "issue", issueID, "issue.status_changed", old, nil)

	sinceTS, sinceID := epochCursor()
	base := PollParams{
		UserID:       owner,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	}

	// Delivered, and the marker is absent (nil).
	res, err := store.PollForMonitor(ctx, base)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	var found *NotificationDTO
	for i := range res.Items {
		if res.Items[i].ID == eHuman {
			found = &res.Items[i]
		}
	}
	if found == nil {
		t.Fatalf("session-less human-owner event %d must be delivered (US-5)", eHuman)
	}
	if found.ActorSessionID != nil {
		t.Errorf("session-less event actorSessionId must be absent (nil), got %q", *found.ActorSessionID)
	}

	// Even with a rebind excluding some session, the NULL-session event is NOT cut.
	base.ExcludeSession = "mcp-session-anything"
	res, err = store.PollForMonitor(ctx, base)
	if err != nil {
		t.Fatalf("poll (rebind): %v", err)
	}
	if !idSet(res)[eHuman] {
		t.Errorf("session-less event %d must NEVER be cut by exclude_session (INV-2/US-5)", eHuman)
	}
}

// TestPollForMonitor_ExcludeSessionNarrows is the SC-002c / SC-009 / INV-9 class
// gate: a non-empty exclude_session (rebind) drops ONLY that session's events;
// for any value (valid / empty / forged / foreign) the result is a SUBSET of
// the no-rebind result, never a superset — it can only narrow.
func TestPollForMonitor_ExcludeSessionNarrows(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	owner := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, owner)
	projectID := seedTestProject(t, pool, owner)
	const issueID = "mpes1"
	const old = "now() - interval '1 minute'"

	eSessA := insertEvent(t, pool, owner, projectID, "issue", issueID, "issue.status_changed", old, sptr("sess-A"))
	eSessB := insertEvent(t, pool, owner, projectID, "issue", issueID, "issue.status_changed", old, sptr("sess-B"))
	eNull := insertEvent(t, pool, owner, projectID, "issue", issueID, "issue.status_changed", old, nil)

	sinceTS, sinceID := epochCursor()
	poll := func(excl string) map[int64]bool {
		res, err := store.PollForMonitor(ctx, PollParams{
			UserID:         owner,
			ProjectID:      projectID,
			ScopeIssueID:   issueID,
			EventTypes:     []string{"issue.status_changed"},
			SinceTS:        sinceTS,
			SinceID:        sinceID,
			Limit:          50,
			SafetyLag:      5 * time.Second,
			ExcludeSession: excl,
		})
		if err != nil {
			t.Fatalf("poll excl=%q: %v", excl, err)
		}
		return idSet(res)
	}

	// Baseline (no rebind): all three delivered.
	baseline := poll("")
	for _, id := range []int64{eSessA, eSessB, eNull} {
		if !baseline[id] {
			t.Fatalf("baseline must contain event %d", id)
		}
	}

	// Rebind to sess-B: only sess-B dropped; sess-A + NULL remain.
	rb := poll("sess-B")
	if rb[eSessB] {
		t.Errorf("exclude_session=sess-B must drop event %d (its own session)", eSessB)
	}
	if !rb[eSessA] {
		t.Errorf("exclude_session=sess-B must keep other-session event %d", eSessA)
	}
	if !rb[eNull] {
		t.Errorf("exclude_session=sess-B must keep the NULL-session event %d (never cut)", eNull)
	}

	// Forged / foreign session drops nothing (subset == baseline).
	foreign := poll("sess-does-not-exist")
	for id := range baseline {
		if !foreign[id] {
			t.Errorf("foreign exclude_session must not drop event %d (narrow-only, INV-9)", id)
		}
	}
	// Narrow-only invariant: every rebind result is a subset of the baseline.
	for _, m := range []map[int64]bool{rb, foreign, poll("")} {
		for id := range m {
			if !baseline[id] {
				t.Errorf("rebind result contains event %d absent from the no-rebind baseline — exclude_session must only NARROW (INV-9)", id)
			}
		}
	}
}

// TestPollForMonitor_IssueClosedRoot is the SC-005 / US-4 class gate:
// issue.closed on the scoped issue flows through the issue arm and is in the
// default whitelist, so a following agent learns the task finished. A
// non-whitelisted control proves the type filter gates delivery. And the
// closer's OWN close (its session) is suppressed via exclude_session (echo).
func TestPollForMonitor_IssueClosedRoot(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpcl1"
	const old = "now() - interval '1 minute'"

	// Closed by a peer session → delivered.
	eClosed := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.closed", old, sptr("sess-peer"))
	// A status_changed NOT in the default set → excluded.
	eStatus := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, sptr("sess-peer"))
	// Closed by the monitor's OWN session → suppressed under rebind (its echo).
	eOwnClose := insertEvent(t, pool, caller, projectID, "issue", issueID, "issue.closed", old, sptr("sess-self"))

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:         caller,
		ProjectID:      projectID,
		ScopeIssueID:   issueID,
		EventTypes:     []string{"comment.created", "issue.closed"}, // the default set
		SinceTS:        sinceTS,
		SinceID:        sinceID,
		Limit:          50,
		SafetyLag:      5 * time.Second,
		ExcludeSession: "sess-self",
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if !got[eClosed] {
		t.Errorf("issue.closed %d by a peer session must be delivered (SC-005/US-4)", eClosed)
	}
	if got[eStatus] {
		t.Errorf("issue.status_changed %d must be excluded — not in the default whitelist", eStatus)
	}
	if got[eOwnClose] {
		t.Errorf("the monitor's own close %d (sess-self) must be suppressed by exclude_session (echo)", eOwnClose)
	}
}

// TestPollForMonitor_MentionExcluded is the SC-008 / INV-7 class gate: the
// WHOLE closed mention set {issue.mention, comment.mention} is never delivered
// even under an opt-in "issue." whitelist, while a control issue.* type IS
// delivered — proving the exclusion is the closed set, not one type.
func TestPollForMonitor_MentionExcluded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	seedTestIssue(t, pool, projectID, caller, "mpmn1", 2)
	const issueID = "mpmn1"
	const old = "now() - interval '1 minute'"

	// Whitelist opts into every issue.* type AND comment.created — but mentions
	// must still be dropped.
	eIssueMention := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.mention", old, nil)
	eCommentMention := insertCommentEvent(t, pool, actor, projectID, issueID, "comment.mention", old, nil)
	eControl := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, nil)

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		ScopeSubtree: true,
		EventTypes:   []string{"comment.created", "comment.mention", "issue.mention", "issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if got[eIssueMention] {
		t.Errorf("issue.mention %d must NOT be delivered by the broadcast monitor (INV-7)", eIssueMention)
	}
	if got[eCommentMention] {
		t.Errorf("comment.mention %d must NOT be delivered by the broadcast monitor (INV-7 closed set)", eCommentMention)
	}
	if !got[eControl] {
		t.Errorf("control issue.status_changed %d must be delivered (mention exclusion is the closed set, not all types)", eControl)
	}
}

// TestPollForMonitor_FullAuditStream is the SC-010 / FR-1 class gate: under an
// opt-in "issue." whitelist the monitor delivers the FULL audit stream of those
// types (including non-notifiable ones like issue.field_changed /
// issue.label_added that the human inbox gates out) — but still NOT
// issue.mention.
func TestPollForMonitor_FullAuditStream(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpau1"
	const old = "now() - interval '1 minute'"

	eFieldChanged := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.field_changed", old, nil)
	eLabelAdded := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.label_added", old, nil)
	eMention := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.mention", old, nil)

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.field_changed", "issue.label_added", "issue.mention"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if !got[eFieldChanged] {
		t.Errorf("non-notifiable issue.field_changed %d must be delivered under opt-in (full audit stream, FR-1)", eFieldChanged)
	}
	if !got[eLabelAdded] {
		t.Errorf("non-notifiable issue.label_added %d must be delivered under opt-in (FR-1)", eLabelAdded)
	}
	if got[eMention] {
		t.Errorf("issue.mention %d must NOT be delivered even under opt-in (INV-7)", eMention)
	}
}

// TestPollForMonitor_ReadArchivedIndependence pins F1's inversion of the old
// TestPollForMonitor_ReturnsReadNotifications: the monitor is an event-tail, so
// an event is delivered regardless of whether its personal inbox row is read,
// archived, or absent entirely.
func TestPollForMonitor_ReadArchivedIndependence(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpri1"
	const old = "now() - interval '1 minute'"

	// (a) event with a read+archived inbox row.
	eReadArch := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, nil)
	seedInboxRow(t, pool, caller, eReadArch, "issue.status_changed", projectID, true, true)
	// (b) event with NO inbox row at all.
	eNoInbox := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, nil)

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := idSet(res)
	if !got[eReadArch] {
		t.Errorf("event %d with a read+archived inbox row must STILL be delivered (event-tail, F1)", eReadArch)
	}
	if !got[eNoInbox] {
		t.Errorf("event %d with NO inbox row must be delivered (delivery does not require a notifications row)", eNoInbox)
	}
}

// ============================ T084 — actorSessionId row→DTO (integration) ============================

// TestPollForMonitor_ActorSessionRowToDTO is the SC-002a integration boundary:
// a non-empty events.actor_session_id hydrates NotificationDTO.ActorSessionID
// EXACTLY (SELECT → Scan → hydrate), while a NULL / empty one leaves it absent
// (nil → omitempty). This is the SQL-row-to-wire half of SC-002a (the pure
// marshal half lives in types_test.go).
func TestPollForMonitor_ActorSessionRowToDTO(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)
	const issueID = "mpas1"
	const old = "now() - interval '1 minute'"

	eNonEmpty := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, sptr("mcp-session-xyz"))
	eNull := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, nil)
	// An explicit empty-string session must also be treated as absent (the
	// store's nil/"" guard), not a spurious "" the reader could mis-compare.
	eEmpty := insertEvent(t, pool, actor, projectID, "issue", issueID, "issue.status_changed", old, sptr(""))

	sinceTS, sinceID := epochCursor()
	res, err := store.PollForMonitor(ctx, PollParams{
		UserID:       caller,
		ProjectID:    projectID,
		ScopeIssueID: issueID,
		EventTypes:   []string{"issue.status_changed"},
		SinceTS:      sinceTS,
		SinceID:      sinceID,
		Limit:        50,
		SafetyLag:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	byID := map[int64]*string{}
	for i := range res.Items {
		byID[res.Items[i].ID] = res.Items[i].ActorSessionID
	}
	if got, ok := byID[eNonEmpty]; !ok {
		t.Fatalf("event %d not delivered", eNonEmpty)
	} else if got == nil || *got != "mcp-session-xyz" {
		t.Errorf("non-empty actor_session_id must hydrate exactly: got %v, want mcp-session-xyz", ptrStr(got))
	}
	if got, ok := byID[eNull]; !ok {
		t.Fatalf("event %d not delivered", eNull)
	} else if got != nil {
		t.Errorf("NULL actor_session_id must hydrate as absent (nil), got %q", *got)
	}
	if got, ok := byID[eEmpty]; !ok {
		t.Fatalf("event %d not delivered", eEmpty)
	} else if got != nil {
		t.Errorf("empty actor_session_id must hydrate as absent (nil), got %q", *got)
	}
}

// ptrStr renders a *string for a test message ("<nil>" when nil).
func ptrStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// seedMonitorCredential inserts a live monitor credential of caller on root
// (subtree as given) minted by a fresh api key of caller, and returns its id —
// FinishMonitorPoll stamps exactly this row. The api key's cleanup cascades
// the credential (monitor_credentials.minted_by_api_key_id ON DELETE CASCADE).
func seedMonitorCredential(t *testing.T, pool *pgxpool.Pool, caller, projectID uuid.UUID,
	root string, subtree bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	keyID := uuid.New()
	prefix := "ska_" + strings.ReplaceAll(keyID.String()[:12], "-", "")
	if _, err := pool.Exec(ctx, `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at)
VALUES ($1, $2, 'fake-hash-for-test', $3, 'poll_for_monitor test', now())`,
		keyID, caller, prefix); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, keyID)
	})
	credID := uuid.New()
	digest := []byte(credID.String())
	if _, err := pool.Exec(ctx, `
INSERT INTO monitor_credentials (
    id, token_digest, user_id, minted_by_api_key_id, audience, capability,
    project_id, scope_issue_id, scope_subtree, event_types,
    created_at, absolute_deadline, expires_at
) VALUES (
    $1, $2, $3, $4, 'skein-agent', 'read',
    $5, $6, $7, ARRAY['issue.status_changed'],
    now(), now() + interval '12 hours', now() + interval '10 minutes'
)`, credID, digest, caller, keyID, projectID, root, subtree); err != nil {
		t.Fatalf("seed monitor credential: %v", err)
	}
	return credID
}

// TestPollForMonitor_HierarchyLoopIsLogged — sk-622.21 US-3 / edge 7 (fix found
// in review): a subtree monitor on A, whose canonical parents
// loop (A → B → A), polls to completion — B's event is admitted, because A is
// B's parent — and the poll reports the loop as an error with the issue ids.
// The admission predicate (hierarchy.IsAncestorExpr) cannot log; the report
// rides on the poll's finishing statement (FinishMonitorPoll), so it is
// written once per poll whatever the page holds. Red before the fix: neither
// statement of the poll read is_cycle, so no record was written.
func TestPollForMonitor_HierarchyLoopIsLogged(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	caller := seedTestUser(t, pool, "user")
	actor := seedTestUser(t, pool, "user")
	cleanupMonitorRows(t, pool, caller, actor)
	projectID := seedTestProject(t, pool, caller)

	sfx := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	a, b := "mhla"+sfx, "mhlb"+sfx
	seedHierarchyLoop(t, pool, projectID, caller, a, b)
	credID := seedMonitorCredential(t, pool, caller, projectID, a, true)

	eid := insertEvent(t, pool, actor, projectID, "issue", b, "issue.status_changed",
		"now() - interval '1 minute'", nil)

	logs := captureLogs(t)
	sinceTS, sinceID := epochCursor()
	params := PollParams{
		UserID: caller, ProjectID: projectID, ScopeIssueID: a, ScopeSubtree: true,
		EventTypes: []string{"issue.status_changed"},
		SinceTS:    sinceTS, SinceID: sinceID, Limit: 50, SafetyLag: 5 * time.Second,
	}
	res, err := store.PollForMonitor(ctx, params)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !idSet(res)[eid] {
		t.Fatalf("precondition: A's subtree monitor must admit B's event %d (A is B's parent)", eid)
	}
	if _, err := store.FinishMonitorPoll(ctx, params, credID, res.CursorTS, res.CursorID); err != nil {
		t.Fatalf("finish poll: %v", err)
	}

	assertCycleLogged(t, logs, a, a, a, b)
}
