package handler

// sk-551.10 — the Open / Closed / All filter of GET /api/v1/me/watching.
// Expected values come from the accepted plan: "Open" shows only tasks
// that are not closed, the number under the filter is the size of the
// chosen variant, and open + closed = all. Since sk-622.21 (US-5) the
// filter and the split apply to cards of both kinds — subscribed and
// historical — while subscriptionsTotal counts subscriptions only.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
)

// closeMeIssue marks a seeded issue closed the way the store leaves it:
// status, closed_at and an outcome together (the schema requires the
// outcome exactly when the status is closed).
func closeMeIssue(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
UPDATE issues SET status = 'closed', closed_at = now(), close_outcome = 'done'
 WHERE id = $1`, id); err != nil {
		t.Fatalf("close issue %s: %v", id, err)
	}
}

func callWatchingState(t *testing.T, h *MeHandler, userID uuid.UUID, state string) (int, map[string]any) {
	t.Helper()
	url := "/api/v1/me/watching"
	if state != "" {
		url += "?state=" + state
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := auth.WithUserID(req.Context(), userID)
	ctx = auth.WithUserRole(ctx, "user")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.GetWatching(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func watchingItemIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing/not array; body=%v", body)
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		item := r.(map[string]any)["item"].(map[string]any)
		out = append(out, item["id"].(string))
	}
	sort.Strings(out)
	return out
}

func numField(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	v, ok := body[key].(float64)
	if !ok {
		t.Fatalf("%s missing/not a number; body=%v", key, body)
	}
	return int(v)
}

func TestGetWatching_StateFilter(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	openA, openB, closedC := prefix+"-1", prefix+"-2", prefix+"-3"
	// Historical cards (no subscription, a notification in the window
	// carries the task as its card), one per state.
	histOpen, histClosed := prefix+"-4", prefix+"-5"
	for _, id := range []string{openA, openB, closedC, histOpen, histClosed} {
		seedMeIssue(t, pool, pid, user, id, "", nil)
	}
	closeMeIssue(t, pool, closedC)
	closeMeIssue(t, pool, histClosed)
	seedWatcher(t, pool, openA, user, []string{"creator"})
	seedWatcher(t, pool, openB, user, []string{"manual"})
	seedWatcher(t, pool, closedC, user, []string{"creator"})
	seedHistoricalCard(t, pool, pid, histOpen, user)
	seedHistoricalCard(t, pool, pid, histClosed, user)

	cases := []struct {
		state      string
		wantIDs    []string
		wantState  string
		stateTotal int
	}{
		{"open", []string{openA, openB, histOpen}, "open", 3},
		{"closed", []string{closedC, histClosed}, "closed", 2},
		{"all", []string{openA, openB, closedC, histOpen, histClosed}, "all", 5},
		// Omitted = all: an older client keeps the pre-filter list.
		{"", []string{openA, openB, closedC, histOpen, histClosed}, "all", 5},
	}
	for _, tc := range cases {
		t.Run("state="+tc.state, func(t *testing.T) {
			code, body := callWatchingState(t, h, user, tc.state)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%v", code, body)
			}
			got := watchingItemIDs(t, body)
			want := append([]string(nil), tc.wantIDs...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("items = %v, want %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("items = %v, want %v", got, want)
				}
			}
			if s, _ := body["state"].(string); s != tc.wantState {
				t.Errorf("state = %q, want %q", s, tc.wantState)
			}
			if n := numField(t, body, "stateTotal"); n != tc.stateTotal {
				t.Errorf("stateTotal = %d, want %d", n, tc.stateTotal)
			}
			// The split is reported whatever the filter is.
			if n := numField(t, body, "openTotal"); n != 3 {
				t.Errorf("openTotal = %d, want 3 (2 subscribed + 1 historical)", n)
			}
			if n := numField(t, body, "closedTotal"); n != 2 {
				t.Errorf("closedTotal = %d, want 2 (1 subscribed + 1 historical)", n)
			}
			if n := numField(t, body, "subscriptionsTotal"); n != 3 {
				t.Errorf("subscriptionsTotal = %d, want 3 (historical cards are not subscriptions)", n)
			}
			if tr, _ := body["truncated"].(bool); tr {
				t.Error("truncated = true for a list far below the cap")
			}
		})
	}
}

// An empty selection must still carry the totals of the other choices:
// "no closed subscriptions" is not "no subscriptions".
func TestGetWatching_StateFilterEmptySelectionKeepsTotals(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	pid, prefix := seedMeProject(t, pool, user)
	h := newMeWatchingHandler(pool)

	seedMeIssue(t, pool, pid, user, prefix+"-1", "", nil)
	seedWatcher(t, pool, prefix+"-1", user, []string{"creator"})

	code, body := callWatchingState(t, h, user, "closed")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", code, body)
	}
	if ids := watchingItemIDs(t, body); len(ids) != 0 {
		t.Fatalf("items = %v, want none", ids)
	}
	if n := numField(t, body, "stateTotal"); n != 0 {
		t.Errorf("stateTotal = %d, want 0", n)
	}
	if n := numField(t, body, "openTotal"); n != 1 {
		t.Errorf("openTotal = %d, want 1", n)
	}
	if n := numField(t, body, "subscriptionsTotal"); n != 1 {
		t.Errorf("subscriptionsTotal = %d, want 1", n)
	}
}

func TestGetWatching_UnknownStateIs400(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := seedMeUser(t, pool, "user")
	h := newMeWatchingHandler(pool)
	code, body := callWatchingState(t, h, user, "resolved")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", code, body)
	}
}
