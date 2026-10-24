package main

// hub_access_route_test.go — sk-622.21, T006, test matrix row 11 (US-9: the
// service endpoint is mounted). Reference — the T028 contract godoc
// (projects/access_contract.go) and plan T006: POST /api/v1/hub/project-access
// on the PRODUCTION router (buildRouter over config.Load) answers only with the
// service secret — 401 without it, 400 on bad syntax — and is outside the user
// group: a call with the secret and no user headers is served (the user group
// would demand a user), and user headers neither replace the secret nor
// change whose access is answered.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

func (s *wiringStack) hubAccess(body string, headers map[string]string) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hub/project-access", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func TestHubAccessRoute_ServiceSecretOnly(t *testing.T) {
	s := newWiringStack(t)

	admin, adminName := testsupport.SeedUser(t, s.pool, "admin")
	outsider, _ := testsupport.SeedUser(t, s.pool, "user") // not a member of s.project
	body, err := json.Marshal(projects.ProjectAccessRequest{Users: []projects.ProjectAccessQuery{
		{UserID: outsider, ProjectIDs: []uuid.UUID{s.project}},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	adminHeaders := map[string]string{
		auth.HeaderUserID:   admin.String(),
		auth.HeaderUsername: adminName,
		auth.HeaderUserRole: "admin",
	}

	// No secret → 401, with or without user headers.
	if rec := s.hubAccess(string(body), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no secret: status %d, want 401; body %s", rec.Code, rec.Body.String())
	}
	if rec := s.hubAccess(string(body), adminHeaders); rec.Code != http.StatusUnauthorized {
		t.Errorf("admin user headers without the secret: status %d, want 401; body %s", rec.Code, rec.Body.String())
	}

	secret := map[string]string{auth.HeaderInternalSecret: wiringInternalSecret}

	// Secret, broken syntax → 400.
	if rec := s.hubAccess(`{"users":`, secret); rec.Code != http.StatusBadRequest {
		t.Errorf("broken JSON: status %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if rec := s.hubAccess(`{"users":[{"userId":"not-a-uuid"}]}`, secret); rec.Code != http.StatusBadRequest {
		t.Errorf("non-UUID userId: status %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// Secret and no user headers → served.
	rec := s.hubAccess(string(body), secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret, no user headers: status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp projects.ProjectAccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Users) != 1 {
		t.Fatalf("decode 200 body %s: %v", rec.Body.String(), err)
	}
	if len(resp.Users[0].Denied) != 1 || resp.Users[0].Denied[0] != s.project {
		t.Errorf("outsider: denied = %v, want [%s]", resp.Users[0].Denied, s.project)
	}

	// Secret plus an admin's user headers: the answer is still the outsider's.
	headers := map[string]string{auth.HeaderInternalSecret: wiringInternalSecret}
	for k, v := range adminHeaders {
		headers[k] = v
	}
	rec = s.hubAccess(string(body), headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret + admin headers: status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Users) != 1 {
		t.Fatalf("decode 200 body %s: %v", rec.Body.String(), err)
	}
	if len(resp.Users[0].Allowed) != 0 || resp.Users[0].InternalAdmin {
		t.Errorf("admin headers changed the outsider's answer: %+v", resp.Users[0])
	}
}
