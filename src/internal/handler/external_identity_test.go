package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// TestParseUserHeaders_IsExternalRoundTrip proves the cookie→header→REST
// ctx hop for sk-163 S1: the BFF mints X-Skein-User-Is-External from the
// cookie principal, ParseUserHeaders parses it into ctx, and the caller()
// resolver + centralized Principal both surface the flag. The admin role
// combined with is_external=true must clamp IsAdmin() to false (ceiling).
func TestParseUserHeaders_IsExternalRoundTrip(t *testing.T) {
	uid := uuid.New()

	cases := []struct {
		name           string
		role           string
		externalHeader string // "" = header omitted
		wantExternal   bool
		wantIsAdmin    bool
	}{
		{"external user", "user", "true", true, false},
		{"external admin clamped", "admin", "true", true, false},
		{"internal admin", "admin", "", false, true},
		{"internal user", "user", "false", false, false},
		{"internal user omitted header", "user", "", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCaller callerCtx
			var gotOK bool

			h := ParseUserHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCaller, gotOK = caller(r)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
			req.Header.Set(auth.HeaderUserID, uid.String())
			req.Header.Set(auth.HeaderUserRole, tc.role)
			if tc.externalHeader != "" {
				req.Header.Set(auth.HeaderUserIsExternal, tc.externalHeader)
			}

			h.ServeHTTP(httptest.NewRecorder(), req)

			if !gotOK {
				t.Fatalf("caller() returned ok=false")
			}
			if gotCaller.IsExternal != tc.wantExternal {
				t.Errorf("callerCtx.IsExternal = %v, want %v", gotCaller.IsExternal, tc.wantExternal)
			}
			if got := gotCaller.Principal().IsAdmin(); got != tc.wantIsAdmin {
				t.Errorf("Principal().IsAdmin() = %v, want %v", got, tc.wantIsAdmin)
			}
		})
	}
}
