package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestPrincipal_IsAdmin pins the ceiling invariant (sk-163 S1): admin
// authority requires role=="admin" AND NOT is_external. An external user is
// clamped to non-admin for EVERY role value — the DB CHECK from migration
// 000033 makes the external+admin row impossible to persist, but IsAdmin()
// is the app-level belt that holds even if a row were tampered into that
// state by hand.
func TestPrincipal_IsAdmin(t *testing.T) {
	cases := []struct {
		name       string
		role       string
		isExternal bool
		want       bool
	}{
		{"internal admin", "admin", false, true},
		{"internal user", "user", false, false},
		{"external admin (clamped)", "admin", true, false},
		{"external user", "user", true, false},
		{"external empty-role (clamped)", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Principal{UserID: uuid.New(), Role: tc.role, IsExternal: tc.isExternal}
			if got := p.IsAdmin(); got != tc.want {
				t.Errorf("IsAdmin() = %v, want %v (role=%q external=%v)", got, tc.want, tc.role, tc.isExternal)
			}
		})
	}
}

// TestIsAdminRole pins the S2 (role, isExternal) helper used by store-layer
// args structs and DB-reread paths. It MUST agree with Principal.IsAdmin for
// every combination — an external caller is never admin regardless of role.
func TestIsAdminRole(t *testing.T) {
	cases := []struct {
		role       string
		isExternal bool
		want       bool
	}{
		{"admin", false, true},
		{"admin", true, false},
		{"user", false, false},
		{"user", true, false},
		{"", true, false},
		{"", false, false},
	}
	for _, tc := range cases {
		if got := IsAdminRole(tc.role, tc.isExternal); got != tc.want {
			t.Errorf("IsAdminRole(%q, %v) = %v, want %v", tc.role, tc.isExternal, got, tc.want)
		}
		// Equivalence with the canonical method.
		p := Principal{Role: tc.role, IsExternal: tc.isExternal}
		if IsAdminRole(tc.role, tc.isExternal) != p.IsAdmin() {
			t.Errorf("IsAdminRole(%q,%v) disagrees with Principal.IsAdmin()", tc.role, tc.isExternal)
		}
	}
}

// TestPrincipalFrom_CtxRoundTrip proves the identity values stashed in ctx
// by the trust-header / api-key middleware reassemble into a Principal —
// the single derivation point both transports feed.
func TestPrincipalFrom_CtxRoundTrip(t *testing.T) {
	// No identity → not present.
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatalf("PrincipalFrom on empty ctx returned ok=true")
	}

	id := uuid.New()
	ctx := WithUserID(context.Background(), id)
	ctx = WithUserRole(ctx, "admin")
	ctx = WithIsExternal(ctx, true)

	p, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatalf("PrincipalFrom returned ok=false for populated ctx")
	}
	if p.UserID != id || p.Role != "admin" || !p.IsExternal {
		t.Fatalf("Principal mismatch: %+v", p)
	}
	// External clamps the admin role to non-admin.
	if p.IsAdmin() {
		t.Errorf("external+admin principal resolved IsAdmin()=true")
	}

	// Role defaults to "user" when the header was absent.
	ctxNoRole := WithUserID(context.Background(), id)
	p2, _ := PrincipalFrom(ctxNoRole)
	if p2.Role != "user" {
		t.Errorf("missing role defaulted to %q, want \"user\"", p2.Role)
	}
	if p2.IsExternal {
		t.Errorf("missing is_external header defaulted to true, want false")
	}
}

// TestIsExternalFrom_DefaultsFalse documents that an absent value reads as
// false — internal is the safe default, so the slice is a no-op for callers
// that never set the flag.
func TestIsExternalFrom_DefaultsFalse(t *testing.T) {
	if IsExternalFrom(context.Background()) {
		t.Errorf("IsExternalFrom on empty ctx = true, want false")
	}
	if !IsExternalFrom(WithIsExternal(context.Background(), true)) {
		t.Errorf("IsExternalFrom after WithIsExternal(true) = false, want true")
	}
}
