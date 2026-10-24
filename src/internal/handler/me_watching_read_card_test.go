package handler

// me_watching_read_card_test.go — sk-622.21, T023, matrix row No. 46 (US-7 and
// the "mark card as read" row of the personal-signal matrix).
//
// Subject: POST /api/v1/me/watching/{issueId}/read. Expected values are the
// US-7 rule written as literals: every unread notification of the caller
// whose Watching card is X is read — the descendants' events carded to X, in
// the window and outside it; the badge drops by exactly the card's unread
// notifications inside the window; other cards and other users are not
// touched; a card that is somebody else's or does not exist answers 200 with
// marked = 0; mark-read-by-entity on the same task still reads the task's own
// events only; the owner gets the personal signal `watching` after the
// commit, nobody else does.
//
// The request goes through a chi router with the production route pattern,
// so the {issueId} path parameter reaches the handler as it does in the
// process. Whether cmd/api mounts the route is not observable from this
// package (buildRouter lives in package main).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

// postReadCard sends POST /api/v1/me/watching/{issueId}/read as userID and
// returns the status and the decoded body.
func postReadCard(t *testing.T, h *MeHandler, userID uuid.UUID, issueID string) (int, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/me/watching/{issueId}/read", h.MarkCardRead)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/watching/"+issueID+"/read", nil)
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, "user")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req.WithContext(ctx))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// seedAgedCardNotification seeds an unread notification of userID about
// issueID carrying the Watching card `card`, written `age` ago.
func seedAgedCardNotification(t *testing.T, pool *pgxpool.Pool, userID, projectID uuid.UUID, actor uuid.UUID, issueID, card, age string) int64 {
	t.Helper()
	ev := seedMeEvent(t, pool, projectID, "issue.status_changed", "issue", issueID, actor)
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel,
                           card_issue_id, card_resolved, created_at)
VALUES ($1, $2, 'issue.status_changed', $3, true, $4, true, now() - $5::interval)
RETURNING id`, userID, ev, projectID, card, age).Scan(&id); err != nil {
		t.Fatalf("seed carded notification: %v", err)
	}
	return id
}

func notifRead(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var read bool
	if err := pool.QueryRow(t.Context(),
		`SELECT read_at IS NOT NULL FROM notifications WHERE id = $1`, id).Scan(&read); err != nil {
		t.Fatalf("read notification %d: %v", id, err)
	}
	return read
}

// TestReadCard_ReadsTheCardOnly — No. 46.
func TestReadCard_ReadsTheCardOnly(t *testing.T) {
	pool := newMeTestPool(t)
	sink := startPrefsSigSink(t)
	user, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	actor, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	epic, child1, child2, other1 := prefix+"-1", prefix+"-1.1", prefix+"-1.2", prefix+"-2"
	for _, id := range []string{epic, child1, child2, other1} {
		seedMeIssue(t, pool, pid, actor, id, "", nil)
	}
	seedScopedWatcher(t, pool, epic, user, []string{"manual"}, "subtree")
	seedScopedWatcher(t, pool, other1, user, []string{"manual"}, "issue")

	// The epic's card: two descendants' events in the window, one of the
	// epic's own outside the 21-day window. Another card: one in the window.
	// Another user: a row on the same card.
	onChild1 := seedAgedCardNotification(t, pool, user, pid, actor, child1, epic, "1 hour")
	onChild2 := seedAgedCardNotification(t, pool, user, pid, actor, child2, epic, "2 hours")
	oldOnEpic := seedAgedCardNotification(t, pool, user, pid, actor, epic, epic, "30 days")
	onOther := seedAgedCardNotification(t, pool, user, pid, actor, other1, other1, "1 hour")
	othersRow := seedAgedCardNotification(t, pool, other, pid, actor, child1, epic, "1 hour")

	_, before := callCounts(t, h, user)
	if got := mustInt(t, before, "watching"); got != 3 {
		t.Fatalf("precondition: badge = %d, want 3 (2 on the epic's card + 1 on the other, in the window)", got)
	}

	from := sink.mark()
	code, body := postReadCard(t, h, user, epic)
	if code != http.StatusOK {
		t.Fatalf("POST read card = %d, body %v", code, body)
	}
	if n := numField(t, body, "marked"); n != 3 {
		t.Errorf("marked = %d, want 3 (two descendants' rows in the window + the epic's own outside it)", n)
	}
	signalled := sink.settle(t, pool, from)

	_, after := callCounts(t, h, user)
	if got := mustInt(t, after, "watching"); got != 1 {
		t.Errorf("badge after = %d, want 1 — it drops by exactly the card's 2 unread in the window", got)
	}
	for _, id := range []int64{onChild1, onChild2, oldOnEpic} {
		if !notifRead(t, pool, id) {
			t.Errorf("notification %d on the epic's card still unread", id)
		}
	}
	if notifRead(t, pool, onOther) {
		t.Errorf("notification %d of another card was read", onOther)
	}
	if notifRead(t, pool, othersRow) {
		t.Errorf("another user's notification %d on the same card was read", othersRow)
	}

	if got := signalled[user]; len(got) != 1 || got[0] != "watching" {
		t.Errorf("owner's signal areas = %v, want [watching]", got)
	}
	for u, areas := range signalled {
		if u != user {
			t.Errorf("user %s got a signal %v — only the owner of the rows is addressed", u, areas)
		}
	}

	// Somebody else's card (the actor has no rows on the epic's card) and a
	// card that does not exist: 200, nothing marked, nobody's rows touched.
	for _, tc := range []struct {
		who  uuid.UUID
		card string
	}{{actor, epic}, {user, prefix + "-999"}} {
		code, body := postReadCard(t, h, tc.who, tc.card)
		if code != http.StatusOK || numField(t, body, "marked") != 0 {
			t.Errorf("read card %s by %s = %d %v, want 200 marked 0", tc.card, tc.who, code, body)
		}
	}
	if notifRead(t, pool, othersRow) || notifRead(t, pool, onOther) {
		t.Errorf("a non-owner / missing-card request read somebody's notification")
	}
}

// TestReadCard_MarkReadByEntityKeepsItsContract — No. 46: mark-read-by-entity
// on the card's task reads the task's own events only, not the descendants'
// events carded to it.
func TestReadCard_MarkReadByEntityKeepsItsContract(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	actor, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)

	epic, child := prefix+"-1", prefix+"-1.1"
	seedMeIssue(t, pool, pid, actor, epic, "", nil)
	seedMeIssue(t, pool, pid, actor, child, "", nil)
	seedScopedWatcher(t, pool, epic, user, []string{"manual"}, "subtree")
	onEpic := seedAgedCardNotification(t, pool, user, pid, actor, epic, epic, "1 hour")
	onChild := seedAgedCardNotification(t, pool, user, pid, actor, child, epic, "1 hour")

	nh := NewNotificationsHandler(pool)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/mark-read-by-entity",
		bytes.NewBufferString(`{"entityType":"issue","entityId":"`+epic+`"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithUserID(req.Context(), user)
	ctx = auth.WithUserRole(ctx, "user")
	rec := httptest.NewRecorder()
	nh.MarkReadByEntity(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("mark-read-by-entity = %d: %s", rec.Code, rec.Body.String())
	}
	if !notifRead(t, pool, onEpic) {
		t.Errorf("the task's own notification %d is unread after mark-read-by-entity", onEpic)
	}
	if notifRead(t, pool, onChild) {
		t.Errorf("the descendant's notification %d was read by mark-read-by-entity — that is \"mark card as read\"", onChild)
	}
}
