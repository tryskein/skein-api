package handler

// sk-551.10, item 3 — POST /api/v1/me/watching/unwatch. Expected values are
// the accepted plan's rules, not the current output:
//   - only the CALLER's subscriptions change;
//   - a state sweep touches only subscriptions matching the filter;
//   - a state sweep leaves `manual` alone unless asked explicitly;
//   - an explicit selection removes what was selected, manual included;
//   - sk-622.21 (US-1, US-6, T010): a stopped subscription leaves NO row
//     behind (no 'unwatched' tombstone). A state sweep records no opt-out —
//     reopening the task subscribes the person again; an explicit selection
//     IS an explicit unwatch and records the opt-out (US-1 names "unsubscribe
//     from those selected in the list" beside the single one). The answer also counts
//     the historical cards it removed (historicalRemoved).

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

func callBulkUnwatch(t *testing.T, h *MeHandler, userID uuid.UUID, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/watching/unwatch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.BulkUnwatch(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// watcherRow reports whether (issue, user) has a subscription row, and its
// reasons.
func watcherRow(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) (bool, []string) {
	t.Helper()
	var rs []string
	err := pool.QueryRow(t.Context(),
		`SELECT reasons FROM issue_watchers WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&rs)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		t.Fatalf("reasons of %s: %v", issueID, err)
	}
	return true, rs
}

func hasOptOut(t *testing.T, pool *pgxpool.Pool, issueID string, userID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM issue_watch_optouts WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&n); err != nil {
		t.Fatalf("opt-out of %s: %v", issueID, err)
	}
	return n == 1
}

// seedHistoricalCard gives userID a feed notification carded to issueID
// without a subscription — a historical Watching card (US-5/US-6).
func seedHistoricalCard(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, issueID string, userID uuid.UUID) {
	t.Helper()
	var eid int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id, actor, actor_type, payload)
VALUES (now(), 'issue.status_changed', 'issue', $1, $2, 'tester', 'human', '{}'::jsonb)
RETURNING id`, issueID, projectID).Scan(&eid); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO notifications (user_id, event_id, type, project_id, reason, feed_channel, card_issue_id, card_resolved)
VALUES ($1, $2, 'issue.status_changed', $3, 'delegated_status', true, $4, true)`,
		userID, eid, projectID, issueID); err != nil {
		t.Fatalf("seed historical card: %v", err)
	}
}

func cardRows(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, issueID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND card_issue_id = $2`, userID, issueID).
		Scan(&n); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	return n
}

// bulkFixture: the caller watches four issues — closed/creator,
// closed/manual, closed/creator+manual, open/creator — a second user
// watches the closed/creator issue too, and the caller has a historical
// card on a fifth, closed issue (no subscription).
type bulkFixture struct {
	h                                              *MeHandler
	pool                                           *pgxpool.Pool
	me, other                                      uuid.UUID
	closedAuto, closedManual, closedBoth, openAuto string
	closedHistorical                               string
}

func newBulkFixture(t *testing.T) bulkFixture {
	t.Helper()
	pool := newMeTestPool(t)
	me, _ := seedMeUser(t, pool, "user")
	other, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, me)
	f := bulkFixture{
		h: newMeWatchingHandler(pool), pool: pool, me: me, other: other,
		closedAuto: prefix + "-1", closedManual: prefix + "-2",
		closedBoth: prefix + "-3", openAuto: prefix + "-4",
		closedHistorical: prefix + "-5",
	}
	for _, id := range []string{f.closedAuto, f.closedManual, f.closedBoth, f.openAuto, f.closedHistorical} {
		seedMeIssue(t, pool, pid, me, id, "", nil)
	}
	for _, id := range []string{f.closedAuto, f.closedManual, f.closedBoth, f.closedHistorical} {
		closeMeIssue(t, pool, id)
	}
	seedHistoricalCard(t, pool, pid, f.closedHistorical, me)
	seedWatcher(t, pool, f.closedAuto, me, []string{"creator"})
	seedWatcher(t, pool, f.closedManual, me, []string{"manual"})
	seedWatcher(t, pool, f.closedBoth, me, []string{"creator", "manual"})
	seedWatcher(t, pool, f.openAuto, me, []string{"creator"})
	seedWatcher(t, pool, f.closedAuto, other, []string{"creator"})
	return f
}

func TestBulkUnwatch_ClosedSweepSparesManualOpenAndOthers(t *testing.T) {
	f := newBulkFixture(t)

	// The list announces what the sweep will take before it is made:
	// closed cards without a manual subscription, historical ones included
	// (sk-622.21 US-5) — the closed automatic subscription and the closed
	// historical card.
	_, before := callWatchingState(t, f.h, f.me, "closed")
	if n := numField(t, before, "closedAutoTotal"); n != 2 {
		t.Errorf("closedAutoTotal before the sweep = %d, want 2 (closedAuto + the historical card)", n)
	}

	code, body := callBulkUnwatch(t, f.h, f.me, `{"state":"closed"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	if n := numField(t, body, "unwatched"); n != 1 {
		t.Errorf("unwatched = %d, want 1 (only the closed automatic one)", n)
	}
	if n := numField(t, body, "keptManual"); n != 2 {
		t.Errorf("keptManual = %d, want 2", n)
	}
	if n := numField(t, body, "historicalRemoved"); n != 1 {
		t.Errorf("historicalRemoved = %d, want 1 (the closed historical card)", n)
	}

	if exists, rs := watcherRow(t, f.pool, f.closedAuto, f.me); exists {
		t.Errorf("closed automatic subscription: row left with reasons %v, want no row", rs)
	}
	if hasOptOut(t, f.pool, f.closedAuto, f.me) || hasOptOut(t, f.pool, f.closedHistorical, f.me) {
		t.Errorf("a sweep by state recorded an opt-out")
	}
	if n := cardRows(t, f.pool, f.me, f.closedHistorical); n != 0 {
		t.Errorf("historical closed card still on %d rows", n)
	}
	for _, id := range []string{f.closedManual, f.closedBoth} {
		if exists, rs := watcherRow(t, f.pool, id, f.me); !exists || !slices.Contains(rs, "manual") {
			t.Errorf("manual subscription %s was touched by a sweep: exists=%v reasons %v", id, exists, rs)
		}
	}
	if exists, rs := watcherRow(t, f.pool, f.openAuto, f.me); !exists || !slices.Contains(rs, "creator") {
		t.Errorf("open subscription %s was touched by a «closed» sweep: exists=%v reasons %v", f.openAuto, exists, rs)
	}
	if exists, rs := watcherRow(t, f.pool, f.closedAuto, f.other); !exists || !slices.Contains(rs, "creator") {
		t.Errorf("another user's subscription changed: exists=%v reasons %v", exists, rs)
	}

	// The list agrees: «closed» now holds only the two manual ones.
	_, list := callWatchingState(t, f.h, f.me, "closed")
	if n := numField(t, list, "closedTotal"); n != 2 {
		t.Errorf("closedTotal after the sweep = %d, want 2", n)
	}
	if n := numField(t, list, "closedAutoTotal"); n != 0 {
		t.Errorf("closedAutoTotal after the sweep = %d, want 0", n)
	}
}

func TestBulkUnwatch_ClosedSweepWithIncludeManual(t *testing.T) {
	f := newBulkFixture(t)

	code, body := callBulkUnwatch(t, f.h, f.me, `{"state":"closed","includeManual":true}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	if n := numField(t, body, "unwatched"); n != 3 {
		t.Errorf("unwatched = %d, want 3", n)
	}
	if n := numField(t, body, "keptManual"); n != 0 {
		t.Errorf("keptManual = %d, want 0", n)
	}
	for _, id := range []string{f.closedAuto, f.closedManual, f.closedBoth} {
		if exists, rs := watcherRow(t, f.pool, id, f.me); exists {
			t.Errorf("%s: row left with reasons %v, want no row", id, rs)
		}
		if hasOptOut(t, f.pool, id, f.me) {
			t.Errorf("%s: a sweep by state recorded an opt-out", id)
		}
	}
	if exists, rs := watcherRow(t, f.pool, f.openAuto, f.me); !exists || !slices.Contains(rs, "creator") {
		t.Errorf("open subscription touched: exists=%v reasons %v", exists, rs)
	}
}

func TestBulkUnwatch_ExplicitSelection(t *testing.T) {
	f := newBulkFixture(t)

	// The selection holds a manual subscription (explicitly chosen → goes),
	// an open one, the historical card, and an id the caller does not watch
	// at all.
	body := `{"issueIds":["` + f.closedManual + `","` + f.openAuto + `","` + f.closedHistorical + `","nope-404"]}`
	code, out := callBulkUnwatch(t, f.h, f.me, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, out)
	}
	if n := numField(t, out, "unwatched"); n != 2 {
		t.Errorf("unwatched = %d, want 2", n)
	}
	if n := numField(t, out, "historicalRemoved"); n != 1 {
		t.Errorf("historicalRemoved = %d, want 1", n)
	}
	for _, id := range []string{f.closedManual, f.openAuto} {
		if exists, rs := watcherRow(t, f.pool, id, f.me); exists {
			t.Errorf("selected %s: row left with reasons %v, want no row", id, rs)
		}
		if !hasOptOut(t, f.pool, id, f.me) {
			t.Errorf("selected %s: no opt-out — a selection is an explicit unwatch", id)
		}
	}
	if hasOptOut(t, f.pool, f.closedHistorical, f.me) {
		t.Errorf("the historical card was removed with an opt-out (R3 forbids it)")
	}
	if n := cardRows(t, f.pool, f.me, f.closedHistorical); n != 0 {
		t.Errorf("selected historical card still on %d rows", n)
	}
	for _, id := range []string{f.closedAuto, f.closedBoth} {
		if exists, rs := watcherRow(t, f.pool, id, f.me); !exists || !slices.Contains(rs, "creator") {
			t.Errorf("unselected %s was touched: exists=%v reasons %v", id, exists, rs)
		}
	}

	// Another user selecting MY subscription's issue changes only theirs.
	code, out = callBulkUnwatch(t, f.h, f.other, `{"issueIds":["`+f.closedBoth+`","`+f.closedAuto+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, out)
	}
	if n := numField(t, out, "unwatched"); n != 1 {
		t.Errorf("other user's unwatched = %d, want 1 (only their own row)", n)
	}
	if exists, rs := watcherRow(t, f.pool, f.closedBoth, f.me); !exists || !slices.Contains(rs, "manual") {
		t.Errorf("my subscription changed by another user's request: exists=%v reasons %v", exists, rs)
	}
}

func TestBulkUnwatch_Validation(t *testing.T) {
	pool := newMeTestPool(t)
	me, _ := seedMeUser(t, pool, "user")
	h := newMeWatchingHandler(pool)
	cases := map[string]string{
		"neither selector":   `{}`,
		"both selectors":     `{"state":"closed","issueIds":["x-1"]}`,
		"empty selection":    `{"issueIds":[]}`,
		"unknown state":      `{"state":"resolved"}`,
		"empty id":           `{"issueIds":[""]}`,
		"unknown body field": `{"state":"closed","everything":true}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if code, out := callBulkUnwatch(t, h, me, body); code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%v", code, out)
			}
		})
	}
}
