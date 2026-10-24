package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// TestRequireAdmin_ExternalDenied pins the sk-163 S7b REST deny-row contract:
// the RequireAdmin middleware (which guards /api/v1/users* and every other
// admin-only route) rejects an EXTERNAL caller with 403, even when the caller's
// stored role says "admin". This is the route-level regression behind the
// admin-ceiling derivation already unit-tested in internal/auth/principal_test.go
// (Principal.IsAdmin) — here we prove the gate actually 403s at the HTTP layer
// for the whole RequireAdmin route class, and that an internal admin still
// passes (zero behaviour change for existing callers).
func TestRequireAdmin_ExternalDenied(t *testing.T) {
	cases := []struct {
		name       string
		hasUser    bool
		role       string
		isExternal bool
		wantNext   bool
		wantStatus int
	}{
		// External callers are clamped to non-admin regardless of role — the
		// deny-row for /api/v1/users* and all RequireAdmin routes.
		{"external admin (tampered role)", true, "admin", true, false, http.StatusForbidden},
		{"external user", true, "user", true, false, http.StatusForbidden},
		// Positive controls: internal admin passes; internal non-admin 403s
		// exactly as before (the slice must not change internal behaviour).
		{"internal admin", true, "admin", false, true, http.StatusOK},
		{"internal user", true, "user", false, false, http.StatusForbidden},
		// No identity at all → 403 (PrincipalFrom ok=false → zero Principal).
		{"no user", false, "", false, false, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
			ctx := req.Context()
			if tc.hasUser {
				ctx = auth.WithUserID(ctx, uuid.New())
				ctx = auth.WithUserRole(ctx, tc.role)
				ctx = auth.WithIsExternal(ctx, tc.isExternal)
			}
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			RequireAdmin(next).ServeHTTP(rec, req)

			if reached != tc.wantNext {
				t.Fatalf("next reached = %v, want %v", reached, tc.wantNext)
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}
