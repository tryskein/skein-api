package main

// issue_create_prefs_wiring_test.go — sk-565, T026, test matrix row 12: GET
// and PUT /api/v1/me/issue-create-prefs are wired into the PRODUCTION
// router. The handler tests call the methods directly and cannot see a
// missing r.Get / r.Put; here the request goes through buildRouter with a
// real NotificationsHandler, the internal secret and the BFF user headers,
// as the web BFF sends it. A missing route answers 404/405, not 200.
//
// Beyond the monitor_config_wiring_test.go sample (wiringStack), which only
// talks to /mcp and /agent/v1: a REST request under /api/v1/me.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

// issueCreatePrefsRouter builds the production router from config.Load with
// a real NotificationsHandler; the other REST handlers are nil, as in
// newWiringStack.
func issueCreatePrefsRouter(t *testing.T) (chi.Router, uuid.UUID) {
	t.Helper()
	pool := testsupport.NewTestPool(t)

	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	user, _ := testsupport.SeedUser(t, pool, "user")

	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil, nil, nil, nil, nil, nil, nil, // authH…searchH
		handler.NewNotificationsHandler(pool), // notifH — the handler under test
		nil, nil, nil, nil, nil, nil, nil,     // bulkH…dashboardH
		nil,                                         // usersStore
		issues.NewStore(pool),                       // issuesStore
		dependencies.NewStore(pool),                 // depsStore
		projects.NewStore(pool),                     // projectsStore
		notifications.NewStore(pool),                // notifStore
		nil,                                         // remindersStore
		searchSvc,                                   // searchSvc
		nil,                                         // s3Client
		nil, nil, nil, nil, nil, nil, nil, nil, nil, // attachmentsH…pushH
	)
	return router, user
}

func serveIssueCreatePrefs(t *testing.T, router chi.Router, user uuid.UUID, method, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/api/v1/me/issue-create-prefs", nil)
	} else {
		req = httptest.NewRequest(method, "/api/v1/me/issue-create-prefs", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderUserID, user.String())
	req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
	req.Header.Set(auth.HeaderUserRole, "user")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out) // a 404/405 body need not be JSON; the status says it
	return rec.Code, out
}

func TestIssueCreatePrefsWiring_RoutesReachTheHandler(t *testing.T) {
	router, user := issueCreatePrefsRouter(t)

	status, body := serveIssueCreatePrefs(t, router, user, http.MethodGet, "")
	if status != http.StatusOK || body["assignAgentIssuesToMe"] != true {
		t.Errorf("GET /api/v1/me/issue-create-prefs = %d %v, want 200 {assignAgentIssuesToMe:true}", status, body)
	}

	status, body = serveIssueCreatePrefs(t, router, user, http.MethodPut, `{"assignAgentIssuesToMe":false}`)
	if status != http.StatusOK || body["assignAgentIssuesToMe"] != false {
		t.Errorf("PUT /api/v1/me/issue-create-prefs = %d %v, want 200 {assignAgentIssuesToMe:false}", status, body)
	}
}
