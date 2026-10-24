package main

// watching_window_wiring_test.go — sk-622.21, T022/T024, matrix row No. 41a
// (US-5, US-8, edge 17, INV-8): the Watching window read by config.Load
// reaches the Watching reads of the PRODUCTION router.
//
// The stack: config.Load with the non-default WATCHING_FEED_WINDOW_DAYS=7 and
// NOTIFICATIONS_RETENTION_DAYS=30, the readers composed by
// newNotificationReads — the composition function main calls (decision R16)
// — and buildRouter. Requests carry the internal secret and the BFF user
// headers, as the web BFF sends them.
//
// The ages are chosen so that every wrong wiring gives a different answer:
// an unread carded notification 10 days old is outside a 7-day window but
// inside the 21-day default (a literal 21, or a window not taken from the
// loaded configuration, lets it in); one 5 days old is inside; an unread
// mention 40 days old is counted by Mentions (no window) and never by
// Watching (a window hung on Mentions drops it).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// watchingWindowRouter builds the production router from config.Load with
// the Watching readers composed by newNotificationReads.
func watchingWindowRouter(t *testing.T, pool *pgxpool.Pool) chi.Router {
	t.Helper()
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	t.Setenv("WATCHING_FEED_WINDOW_DAYS", "7")
	t.Setenv("NOTIFICATIONS_RETENTION_DAYS", "30")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	issuesStore := issues.NewStore(pool)
	notifStore, meH := newNotificationReads(pool, cfg, issuesStore)

	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // REST handlers authH…dashboardH
		nil,                               // usersStore
		issuesStore,                       // issuesStore
		dependencies.NewStore(pool),       // depsStore
		projects.NewStore(pool),           // projectsStore
		notifStore,                        // notifStore
		nil,                               // remindersStore
		searchSvc,                         // searchSvc
		nil,                               // s3Client
		nil,                               // attachmentsH
		meH,                               // meH — the readers under test
		nil, nil, nil, nil, nil, nil, nil, // themesH…pushH
	)
	return router
}

func serveMe(t *testing.T, router chi.Router, user uuid.UUID, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderUserID, user.String())
	req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
	req.Header.Set(auth.HeaderUserRole, "user")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

// seedAgedNotification writes an event on issueID and an unread feed
// notification of user, `days` days old, with the Watching card `card`
// ("" = none).
func seedAgedNotification(t *testing.T, pool *pgxpool.Pool, user, actor, projectID uuid.UUID, issueID, ntype, card string, days int) {
	t.Helper()
	var cardArg *string
	if card != "" {
		cardArg = &card
	}
	if _, err := pool.Exec(t.Context(), `
WITH ev AS (
    INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                        actor, actor_user_id, actor_type, payload)
    VALUES (now() - make_interval(days => $6), $4, 'issue', $5, $3, 'tester', $2, 'human', '{}'::jsonb)
    RETURNING id, occurred_at
)
INSERT INTO notifications (user_id, event_id, type, project_id, feed_channel,
                           card_issue_id, card_resolved, created_at)
SELECT $1, ev.id, $4, $3, true, $7, true, ev.occurred_at FROM ev`,
		user, actor, projectID, ntype, issueID, days, cardArg); err != nil {
		t.Fatalf("seed notification %d days old: %v", days, err)
	}
}

// TestWatchingWindowWiring_ConfiguredWindowReachesTheReads — No. 41a.
func TestWatchingWindowWiring_ConfiguredWindowReachesTheReads(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	router := watchingWindowRouter(t, pool)

	user, _ := testsupport.SeedUser(t, pool, "user")
	actor, _ := testsupport.SeedUser(t, pool, "user")
	pid, prefix := testsupport.SeedProject(t, pool, user, "wwin")
	tenDays := testsupport.SeedIssue(t, pool, pid, actor, prefix+"-1", "ten days", "open", 2)
	fiveDays := testsupport.SeedIssue(t, pool, pid, actor, prefix+"-2", "five days", "open", 2)
	mentioned := testsupport.SeedIssue(t, pool, pid, actor, prefix+"-3", "mentioned", "open", 2)

	// Two historical cards (no subscription): only notifications make them.
	seedAgedNotification(t, pool, user, actor, pid, tenDays, "issue.status_changed", tenDays, 10)
	seedAgedNotification(t, pool, user, actor, pid, fiveDays, "issue.status_changed", fiveDays, 5)
	seedAgedNotification(t, pool, user, actor, pid, mentioned, "issue.mention", "", 40)

	counts := serveMe(t, router, user, "/api/v1/me/notification-counts")
	if got := counts["watching"]; got != float64(1) {
		t.Errorf("header watching = %v, want 1 — only the 5-day row is inside the configured 7-day window", got)
	}
	if got := counts["mentions"]; got != float64(1) {
		t.Errorf("header mentions = %v, want 1 — a 40-day mention has no window", got)
	}

	watching := serveMe(t, router, user, "/api/v1/me/watching")
	if got := watching["unreadTotal"]; got != float64(1) {
		t.Errorf("/me/watching unreadTotal = %v, want 1", got)
	}
	items, _ := watching["items"].([]any)
	var ids []string
	for _, raw := range items {
		item, _ := raw.(map[string]any)["item"].(map[string]any)
		id, _ := item["id"].(string)
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != fiveDays {
		t.Errorf("/me/watching cards = %v, want only %s (the 10-day card is outside the window, the mention has no card)", ids, fiveDays)
	}
}
