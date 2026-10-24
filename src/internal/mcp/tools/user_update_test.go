package tools

import (
	"context"
	"os"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/users"
)

// requireSerialPackages skips the test when env SKEIN_TEST_PARALLEL_PKGS
// is set. Tests touching the DB-wide last-admin invariant must run with
// `go test -p 1`; otherwise a concurrent admin-mutating test in another
// package (e.g. internal/users) pollutes the global active-admin count
// and defeats the last-admin protection under test. Mirrors the guard in
// internal/users/store_test.go. Default behaviour is to RUN — `-p 1` is
// the canonical serialization signal; this env check is best-effort.
func requireSerialPackages(t *testing.T) {
	t.Helper()
	if os.Getenv("SKEIN_TEST_PARALLEL_PKGS") != "" {
		t.Skip("skipped under SKEIN_TEST_PARALLEL_PKGS; requires `go test -p 1`")
	}
}

func TestUserUpdate_PatchName(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	target, _ := seedUser(t, pool, "user")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserUpdate(store)
	ctx := withCtx(t, adminID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"userId": target.String(),
		"name":   "Patched Name",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError: %+v", body)
	}
}

func TestUserUpdate_DeactivateThenReactivate(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	target, _ := seedUser(t, pool, "user")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserUpdate(store)
	ctx := withCtx(t, adminID, "admin")

	// Deactivate via flag.
	deactivateReq := mcpgo.CallToolRequest{}
	deactivateReq.Params.Arguments = map[string]any{
		"userId":      target.String(),
		"deactivated": true,
	}
	res, err := handler(ctx, deactivateReq)
	if err != nil {
		t.Fatalf("deactivate handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError on deactivate: %+v", body)
	}
	body := extractToolJSON(t, res)
	user, _ := body["user"].(map[string]any)
	if user["deactivatedAt"] == nil || user["deactivatedAt"] == "" {
		t.Fatalf("deactivatedAt should be populated, got %v", user["deactivatedAt"])
	}

	// Reactivate.
	reactivateReq := mcpgo.CallToolRequest{}
	reactivateReq.Params.Arguments = map[string]any{
		"userId":      target.String(),
		"deactivated": false,
	}
	res, err = handler(ctx, reactivateReq)
	if err != nil {
		t.Fatalf("reactivate handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError on reactivate: %+v", body)
	}
}

func TestUserUpdate_LastAdminCannotDemote(t *testing.T) {
	pool := newTestPool(t)
	requireSerialPackages(t)
	adminID, _ := seedUser(t, pool, "admin")
	// Squash all OTHER admins so adminID is the last one.
	if _, err := pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = now()
 WHERE role='admin' AND id <> $1`, adminID); err != nil {
		t.Fatalf("squash: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
UPDATE users SET deactivated_at = NULL WHERE role='admin'`)
	})

	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserUpdate(store)
	ctx := withCtx(t, adminID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"userId": adminID.String(),
		"role":   "user",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError for last-admin demote")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeLastAdminCannotDemote {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeLastAdminCannotDemote)
	}
}

func TestUserUpdate_NotAdmin(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserUpdate(store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"userId": userID.String(),
		"name":   "self",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected not_admin")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeNotAdmin {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeNotAdmin)
	}
}

func TestUserUpdate_NoOpReturnsValidation(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	target, _ := seedUser(t, pool, "user")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserUpdate(store)
	ctx := withCtx(t, adminID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"userId": target.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation_error for empty patch")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
}
