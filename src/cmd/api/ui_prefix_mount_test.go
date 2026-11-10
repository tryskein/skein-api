package main

// ui_prefix_mount_test.go — the internal API of the frontend moves from
// /api/v1 to /ui/v1, and for one transitional release skein-api accepts both
// prefixes on its internal port, so skein-api and skein-web can be rolled out
// in any order (US-11, Р-2, edge "transitional release"). /mcp and /agent/v1
// keep their paths and are not part of the move (US-11 "Unchanged").
//
// The tests go through the PRODUCTION router (buildRouter over config.Load):
// the defect they guard against is a route registered under one prefix only,
// or a door swallowed into or duplicated under a prefix.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/dependencies"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/search"
)

// newPrefixStack is newWiringStack with the handlers the mount points under
// test reach: authH (the OIDC provision route) and meH (a route of the user
// group).
func newPrefixStack(t *testing.T) *wiringStack {
	t.Helper()
	s := newWiringStack(t)

	authH, err := handler.NewAuthHandler(s.pool, s.cfg.BcryptCost, "local", "", nil)
	if err != nil {
		t.Fatalf("NewAuthHandler: %v", err)
	}
	issuesStore := issues.NewStore(s.pool)
	notifStore, meH := newNotificationReads(s.pool, s.cfg, issuesStore)
	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	s.router, _ = buildRouter(
		s.pool, s.cfg,
		authH,                                                                // authH — the OIDC provision route
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // REST handlers projectsH…dashboardH
		nil,                               // usersStore
		issuesStore,                       // issuesStore
		dependencies.NewStore(s.pool),     // depsStore
		projects.NewStore(s.pool),         // projectsStore
		notifStore,                        // notifStore
		nil,                               // remindersStore
		searchSvc,                         // searchSvc
		nil,                               // s3Client
		nil,                               // attachmentsH
		meH,                               // meH — a route of the user group
		nil, nil, nil, nil, nil, nil, nil, // themesH…pushH
	)
	return s
}

// serve sends one request to the stack's router.
func (s *wiringStack) serve(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// TestUIPrefix_MountPointsAnswerAlikeUnderBothPrefixes — matrix row 1: one
// request per mount point of the internal API under /ui/v1 and under /api/v1
// gets the same status and the same body, and that status is the route's own
// answer, not a 404 of an unmounted path. The mount points are the user group
// (ParseUserHeaders) and each route registered outside it.
func TestUIPrefix_MountPointsAnswerAlikeUnderBothPrefixes(t *testing.T) {
	s := newPrefixStack(t)

	secret := map[string]string{auth.HeaderInternalSecret: wiringInternalSecret}
	user := map[string]string{
		auth.HeaderInternalSecret: wiringInternalSecret,
		auth.HeaderUserID:         s.owner.String(),
		auth.HeaderSessionEpoch:   wiringSessionEpoch,
		auth.HeaderUserRole:       "user",
	}
	access, err := json.Marshal(projects.ProjectAccessRequest{Users: []projects.ProjectAccessQuery{
		{UserID: s.owner, ProjectIDs: []uuid.UUID{s.project}},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	cases := []struct {
		name       string
		method     string
		path       string // relative to the prefix
		body       []byte
		headers    map[string]string
		wantStatus int
	}{
		// The user group: the caller's header counts.
		{"user group", http.MethodGet, "/me/notification-counts", nil, user, http.StatusOK},
		// Service routes outside the group: the channel secret alone.
		{"internal/instance-config", http.MethodGet, "/internal/instance-config", nil, secret, http.StatusOK},
		{"hub/project-access", http.MethodPost, "/hub/project-access", access, secret, http.StatusOK},
		// No OIDC provisioner is wired: the route answers 410 oidc_disabled.
		{"auth/oidc/provision", http.MethodPost, "/auth/oidc/provision", []byte(`{}`), secret, http.StatusGone},
		// The account routes of a visitor; registration is closed by
		// default: 422 signup_disabled.
		{"auth account routes", http.MethodGet, "/auth/username-suggestions?email=prefix-mount@example.test", nil, secret, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui := s.serve(tc.method, internalAPIPrefixUnderTest+tc.path, tc.body, tc.headers)
			old := s.serve(tc.method, transitionalPrefixUnderTest+tc.path, tc.body, tc.headers)

			if ui.Code != tc.wantStatus {
				t.Fatalf("%s %s%s = %d, want %d; body %s",
					tc.method, internalAPIPrefixUnderTest, tc.path, ui.Code, tc.wantStatus, ui.Body.String())
			}
			if old.Code != ui.Code {
				t.Errorf("%s %s%s = %d, but %s%s = %d; body %s",
					tc.method, transitionalPrefixUnderTest, tc.path, old.Code,
					internalAPIPrefixUnderTest, tc.path, ui.Code, old.Body.String())
			}
			if old.Body.String() != ui.Body.String() {
				t.Errorf("bodies differ between the prefixes:\n%s: %s\n%s: %s",
					internalAPIPrefixUnderTest, ui.Body.String(), transitionalPrefixUnderTest, old.Body.String())
			}
		})
	}
}

// TestUIPrefix_MCPAndAgentStayOutsidePrefixes — matrix row 2: /mcp and
// /agent/v1 answer as before the move, and neither exists under a prefix of
// the internal API.
func TestUIPrefix_MCPAndAgentStayOutsidePrefixes(t *testing.T) {
	s := newPrefixStack(t)

	// /mcp: initialize with the key (wiringStack.rpc fails on anything but 200).
	session := s.initialize()

	// /agent/v1: a poll with a monitor token minted through /mcp.
	minted := s.callTool(session, "skein_get_monitor_script", map[string]any{"id": s.root})
	var mint struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(minted[0]), &mint); err != nil || mint.Token == "" {
		t.Fatalf("mint response: %v\n%s", err, minted[0])
	}
	monitor := map[string]string{
		auth.HeaderInternalSecret: wiringInternalSecret,
		auth.HeaderAPIKey:         mint.Token,
	}
	if rec := s.serve(http.MethodGet, "/agent/v1/notifications/poll", nil, monitor); rec.Code != http.StatusOK {
		t.Fatalf("GET /agent/v1/notifications/poll = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	// Neither door is mounted under a prefix of the internal API.
	mcp := map[string]string{
		auth.HeaderInternalSecret: wiringInternalSecret,
		auth.HeaderAPIKey:         s.key,
		auth.HeaderSurface:        auth.SurfaceInternal,
		"Accept":                  "application/json, text/event-stream",
	}
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"prefix-test","version":"1"}}}`)
	for _, prefix := range []string{internalAPIPrefixUnderTest, transitionalPrefixUnderTest} {
		if rec := s.serve(http.MethodPost, prefix+"/mcp", initialize, mcp); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s/mcp = %d, want 404; body %s", prefix, rec.Code, rec.Body.String())
		}
		if rec := s.serve(http.MethodGet, prefix+"/agent/v1/notifications/poll", nil, monitor); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s/agent/v1/notifications/poll = %d, want 404; body %s", prefix, rec.Code, rec.Body.String())
		}
	}
}

// The prefixes are spelled out here, not taken from the product constants: the
// contract with skein-web is the literal path.
const (
	internalAPIPrefixUnderTest  = "/ui/v1"
	transitionalPrefixUnderTest = "/api/v1"
)
