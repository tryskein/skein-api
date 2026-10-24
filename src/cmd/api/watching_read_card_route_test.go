package main

// watching_read_card_route_test.go — sk-622.21, T023, matrix row No. 46
// (US-7): POST /api/v1/me/watching/{issueId}/read is mounted on the
// PRODUCTION router. The handler-level
// test (internal/handler/me_watching_read_card_test.go) builds a local chi
// router, because buildRouter is not reachable from that package, so nothing
// went red if main stopped mounting the route — the only REST path of US-7
// for the web client.
//
// The stack is the one of No. 41a (watchingWindowRouter: config.Load,
// newNotificationReads, buildRouter), the request carries the internal
// secret and the BFF user headers. Reference — the endpoint's contract
// (MarkCardRead godoc): 200 with `marked`, the caller's unread notification
// carded with the issue becomes read. A route that is not mounted answers
// 404 (or 405 for a sibling pattern), and the notification stays unread.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

func TestWatchingReadCardRoute_MountedOnProductionRouter(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	router := watchingWindowRouter(t, pool)

	user, _ := testsupport.SeedUser(t, pool, "user")
	actor, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, user, "wrcr")
	issue := testsupport.SeedIssue(t, pool, pid, actor, prefix+"-1", "card", "open", 2)
	seedAgedNotification(t, pool, user, actor, pid, issue, "issue.status_changed", issue, 1)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/watching/"+issue+"/read", nil)
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderUserID, user.String())
	req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
	req.Header.Set(auth.HeaderUserRole, "user")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/me/watching/%s/read = %d, want 200 (route mounted); body %s",
			issue, rec.Code, rec.Body.String())
	}
	var body struct {
		Marked *int64 `json:"marked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Marked == nil {
		t.Fatalf("decode body %s: %v (want a `marked` field)", rec.Body.String(), err)
	}
	if *body.Marked != 1 {
		t.Errorf("marked = %d, want 1 — the caller's one unread notification of the card", *body.Marked)
	}
	var unread int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM notifications WHERE user_id = $1 AND card_issue_id = $2 AND read_at IS NULL`,
		user, issue).Scan(&unread); err != nil {
		t.Fatalf("count unread: %v", err)
	}
	if unread != 0 {
		t.Errorf("unread notifications of the card after the request = %d, want 0", unread)
	}
}
