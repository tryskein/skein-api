package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/auth"
)

// TestRequireAdminTool_ExternalDenied pins the sk-163 S7b MCP deny-row: the
// shared gate behind skein_user (create / update / list)
// rejects an EXTERNAL caller with the not_admin error code, even when the
// caller's stored role is "admin". requireAdminTool short-circuits BEFORE any
// DB call, so this needs no pool. An internal admin passes (nil result) — the
// admin tools keep working for real admins.
func TestRequireAdminTool_ExternalDenied(t *testing.T) {
	cases := []struct {
		name       string
		role       string
		isExternal bool
		wantDenied bool
	}{
		{"external admin (tampered role)", "admin", true, true},
		{"external user", "user", true, true},
		{"internal user", "user", false, true},
		{"internal admin", "admin", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = auth.WithUserID(ctx, uuid.New())
			ctx = auth.WithUserRole(ctx, tc.role)
			ctx = auth.WithIsExternal(ctx, tc.isExternal)

			res := requireAdminTool(ctx)

			if tc.wantDenied {
				if res == nil {
					t.Fatalf("requireAdminTool returned nil (allowed), want not_admin denial")
				}
				body := extractToolJSON(t, res)
				if body["error"] != ErrCodeNotAdmin {
					t.Fatalf("error = %v, want %s", body["error"], ErrCodeNotAdmin)
				}
			} else if res != nil {
				body := extractToolJSON(t, res)
				t.Fatalf("requireAdminTool denied an internal admin: %+v", body)
			}
		})
	}
}
