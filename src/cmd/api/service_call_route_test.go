package main

// service_call_route_test.go — sk-998 US-2, INV-1: the service routes of the
// internal API (FR-001) run only for a request that carries the service-call
// marker X-Skein-Service-Call: 1, under both prefixes of the internal API,
// while the user routes neither need nor check it. The check belongs to the
// group (mountServiceGroup), so a route added to the group is closed without a
// step of its own (SC-003).
//
// The tests go through the PRODUCTION router (buildRouter). The contract
// literals — the header, its value, the code and the paths — are written out
// rather than taken from the product constants.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"skein-api/internal/auth"
	"skein-api/internal/projects"
)

// serviceRoute is one service route of FR-001, its path relative to the
// prefix of the internal API.
type serviceRoute struct {
	name, method, path string
}

// fr001Routes are the twelve service routes of the spec's FR-001.
var fr001Routes = []serviceRoute{
	{"oidc provision", http.MethodPost, "/auth/oidc/provision"},
	{"hub project access", http.MethodPost, "/hub/project-access"},
	{"instance config", http.MethodGet, "/internal/instance-config"},
	{"signup", http.MethodPost, "/auth/signup"},
	{"username suggestions", http.MethodGet, "/auth/username-suggestions?email=service-call@example.test"},
	{"password reset", http.MethodPost, "/auth/password-reset"},
	{"password reset by link", http.MethodPost, "/auth/password-reset/service-call-reset-token"},
	{"confirm email", http.MethodPost, "/auth/confirm-email/service-call-confirm-token"},
	{"invitation preview", http.MethodGet, "/auth/invitations/service-call-invitation-token"},
	{"invitation signup", http.MethodPost, "/auth/invitations/service-call-invitation-token/signup"},
	{"login", http.MethodPost, "/auth/login"},
	{"logout", http.MethodPost, "/auth/logout"},
}

// TestServiceCall_FR001RefusedWithoutMarker — matrix row 3 (SC-002 without the
// marker): with the right secret, every FR-001 route under both prefixes
// answers 403 service_call_required when the marker is absent, empty or of
// another value. Needs no database: the refusal comes before any handler.
func TestServiceCall_FR001RefusedWithoutMarker(t *testing.T) {
	r := newRouterUnderTest(t)

	for _, prefix := range []string{internalAPIPrefixUnderTest, transitionalPrefixUnderTest} {
		for _, rt := range fr001Routes {
			for _, marker := range []struct {
				name   string
				values []string
			}{
				{"absent", nil},
				{"empty", []string{""}},
				{"other value", []string{"x"}},
			} {
				t.Run(prefix+" "+rt.name+" marker "+marker.name, func(t *testing.T) {
					req := httptest.NewRequest(rt.method, prefix+rt.path, nil)
					req.Header.Set(auth.HeaderInternalSecret, routerInternalSec)
					for _, v := range marker.values {
						req.Header.Add("X-Skein-Service-Call", v)
					}
					rec := httptest.NewRecorder()
					r.ServeHTTP(rec, req)

					if rec.Code != http.StatusForbidden {
						t.Fatalf("%s %s%s = %d, want 403; body %s",
							rt.method, prefix, rt.path, rec.Code, rec.Body.String())
					}
					if code := errorCode(t, rec); code != "service_call_required" {
						t.Errorf("error = %q, want service_call_required", code)
					}
				})
			}
		}
	}
}

// TestServiceCall_FR001ServedWithMarker — matrix row 4 (SC-002 with the
// marker, plan risk 2): with the secret and the marker 1 every FR-001 route
// under both prefixes gives its own answer by its contract (godoc and swag of
// the handler), not 403 and not a 404/405 of a lost route. The installation
// keeps registration and mail off and wires no OIDC provisioner.
func TestServiceCall_FR001ServedWithMarker(t *testing.T) {
	t.Setenv("SKEIN_SIGNUP_ENABLED", "false")
	t.Setenv("SKEIN_EMAIL_ENABLED", "false")
	s := newPrefixStack(t)

	access, err := json.Marshal(projects.ProjectAccessRequest{Users: []projects.ProjectAccessQuery{
		{UserID: s.owner, ProjectIDs: []uuid.UUID{s.project}},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	headers := map[string]string{
		auth.HeaderInternalSecret: wiringInternalSecret,
		"X-Skein-Service-Call":    "1",
	}

	cases := []struct {
		route      string // serviceRoute.name
		body       []byte
		wantStatus int
		wantError  string // "" — the answer carries no error code
	}{
		{"oidc provision", []byte(`{}`), http.StatusGone, "oidc_disabled"},
		{"hub project access", access, http.StatusOK, ""},
		{"instance config", nil, http.StatusOK, ""},
		{"signup", []byte(`{}`), http.StatusUnprocessableEntity, "signup_disabled"},
		{"username suggestions", nil, http.StatusUnprocessableEntity, "signup_disabled"},
		{"password reset", []byte(`{"email":"service-call@example.test","language":"en"}`), http.StatusUnprocessableEntity, "email_disabled"},
		{"password reset by link", []byte(`{"password":"a-new-password"}`), http.StatusUnprocessableEntity, "email_disabled"},
		{"confirm email", nil, http.StatusUnprocessableEntity, "email_disabled"},
		{"invitation preview", nil, http.StatusOK, ""},
		{"invitation signup", []byte(`{}`), http.StatusUnprocessableEntity, "signup_disabled"},
		{"login", []byte(`{"email":"no-such-account@example.test","password":"not-the-password"}`), http.StatusUnprocessableEntity, "invalid credentials"},
		{"logout", nil, http.StatusNoContent, ""},
	}
	if len(cases) != len(fr001Routes) {
		t.Fatalf("%d cases for %d FR-001 routes", len(cases), len(fr001Routes))
	}
	for i, tc := range cases {
		rt := fr001Routes[i]
		if rt.name != tc.route {
			t.Fatalf("case %d is %q, route %d is %q", i, tc.route, i, rt.name)
		}
		for _, prefix := range []string{internalAPIPrefixUnderTest, transitionalPrefixUnderTest} {
			t.Run(prefix+" "+rt.name, func(t *testing.T) {
				rec := s.serve(rt.method, prefix+rt.path, tc.body, headers)
				if rec.Code != tc.wantStatus {
					t.Fatalf("%s %s%s = %d, want %d; body %s",
						rt.method, prefix, rt.path, rec.Code, tc.wantStatus, rec.Body.String())
				}
				if tc.wantError != "" {
					if code := errorCode(t, rec); code != tc.wantError {
						t.Errorf("error = %q, want %q", code, tc.wantError)
					}
				}
			})
		}
	}
}

// TestServiceCall_UserRoutesNeedNoMarker — matrix row 5 (US-2 Unchanged):
// GET auth/me and a route of the user group answer 200 to a user's request
// without the marker, under both prefixes.
func TestServiceCall_UserRoutesNeedNoMarker(t *testing.T) {
	s := newPrefixStack(t)
	user := map[string]string{
		auth.HeaderInternalSecret: wiringInternalSecret,
		auth.HeaderUserID:         s.owner.String(),
		auth.HeaderSessionEpoch:   wiringSessionEpoch,
		auth.HeaderUserRole:       "user",
	}
	for _, prefix := range []string{internalAPIPrefixUnderTest, transitionalPrefixUnderTest} {
		for _, path := range []string{"/auth/me", "/me/notification-counts"} {
			t.Run(prefix+path, func(t *testing.T) {
				if rec := s.serve(http.MethodGet, prefix+path, nil, user); rec.Code != http.StatusOK {
					t.Fatalf("GET %s%s without the marker = %d, want 200; body %s",
						prefix, path, rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// TestServiceCall_GroupClosesARouteAddedToIt — matrix row 6 (SC-003, INV-1 as
// a property of the group): a probe route that is not in FR-001, registered
// through mountServiceGroup on a bare router, does not run without the marker
// and runs with it.
func TestServiceCall_GroupClosesARouteAddedToIt(t *testing.T) {
	ran := false
	r := chi.NewRouter()
	mountServiceGroup(r, func(r chi.Router) {
		r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) {
			ran = true
			w.WriteHeader(http.StatusOK)
		})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))
	if rec.Code != http.StatusForbidden || ran {
		t.Fatalf("without the marker: status %d, handler ran %v; want 403 and not run", rec.Code, ran)
	}
	if code := errorCode(t, rec); code != "service_call_required" {
		t.Errorf("error = %q, want service_call_required", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Skein-Service-Call", "1")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !ran {
		t.Fatalf("with the marker: status %d, handler ran %v; want 200 and run", rec.Code, ran)
	}
}

// errorCode is the "error" field of a JSON answer.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	return body.Error
}
