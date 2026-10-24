package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/users"
)

func newTestUsersStore(t *testing.T) *users.Store {
	t.Helper()
	pool := newTestPool(t)
	return users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
}

// TestUserCreate_HappyPath admin creates a fresh user; response shape and
// audit row are verified.
func TestUserCreate_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)

	handler := handleUserCreate(store)
	ctx := withCtx(t, adminID, "admin")

	email := "ucfr" + adminID.String()[:6] + "@test.local"
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"email":    email,
		"name":     "Created Via MCP",
		"password": "passw0rd!",
		"role":     "user",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError: %+v", body)
	}
	body := extractToolJSON(t, res)
	user, ok := body["user"].(map[string]any)
	if !ok {
		t.Fatalf("user missing or wrong type")
	}
	uid, _ := user["id"].(string)
	if uid == "" {
		t.Fatalf("user id missing")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})
	if user["email"] != email {
		t.Fatalf("email = %v, want %v", user["email"], email)
	}
	// Verify audit event records actor_type='agent' from the agent path.
	var actorType string
	if err := pool.QueryRow(context.Background(), `
SELECT actor_type FROM events
 WHERE entity_type='user' AND entity_id=$1 AND type='user.created'
 LIMIT 1`, uid).Scan(&actorType); err != nil {
		t.Fatalf("verify event: %v", err)
	}
	if actorType != "agent" {
		t.Fatalf("actor_type = %q, want agent", actorType)
	}
}

// TestUserCreate_NotAdmin — non-admin caller gets not_admin BEFORE any
// DB call.
func TestUserCreate_NotAdmin(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserCreate(store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"email":    "xx@test.local",
		"name":     "Should Not Insert",
		"password": "passw0rd!",
		"role":     "user",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-admin caller")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeNotAdmin {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeNotAdmin)
	}
}

// TestUserCreate_EmailTaken — second call with same email returns
// email_taken.
func TestUserCreate_EmailTaken(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserCreate(store)
	ctx := withCtx(t, adminID, "admin")

	email := "uet" + adminID.String()[:6] + "@test.local"
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"email":    email,
		"name":     "First",
		"password": "passw0rd!",
		"role":     "user",
	}
	res, err := handler(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("first call failed: err=%v isError=%v", err, res != nil && res.IsError)
	}
	body := extractToolJSON(t, res)
	user, _ := body["user"].(map[string]any)
	uid, _ := user["id"].(string)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})

	res, err = handler(ctx, req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected email_taken")
	}
	body = extractToolJSON(t, res)
	if body["error"] != ErrCodeEmailTaken {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeEmailTaken)
	}
}

// TestUserCreate_ShortPassword — validation_error.
func TestUserCreate_ShortPassword(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserCreate(store)
	ctx := withCtx(t, adminID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"email":    "shortp" + adminID.String()[:6] + "@x.local",
		"name":     "Short",
		"password": "short",
		"role":     "user",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
}

// TestUserCreate_NoUser — missing user id in ctx returns unauthenticated.
func TestUserCreate_NoUser(t *testing.T) {
	pool := newTestPool(t)
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserCreate(store)
	// No auth context.
	ctx := context.Background()
	res, err := handler(ctx, mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError")
	}
	body := extractToolJSON(t, res)
	if body["error"] != "unauthenticated" {
		t.Fatalf("error = %v, want unauthenticated", body["error"])
	}
}

// TestUserCreate_External — sk-163 S8. skein_user(action="create") with
// is_external=true + role=user creates an external account; the response
// UserDTO carries isExternal=true and the row is flagged in the DB.
func TestUserCreate_External(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserCreate(store)
	ctx := withCtx(t, adminID, "admin")

	email := "mcpext" + adminID.String()[:6] + "@customer.com"
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"email":       email,
		"name":        "External via MCP",
		"password":    "passw0rd!",
		"role":        "user",
		"is_external": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", extractToolJSON(t, res))
	}
	body := extractToolJSON(t, res)
	user, _ := body["user"].(map[string]any)
	uid, _ := user["id"].(string)
	if uid == "" {
		t.Fatalf("user id missing")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})
	if ext, _ := user["isExternal"].(bool); !ext {
		t.Fatalf("isExternal = %v, want true (dto=%+v)", user["isExternal"], user)
	}
	var dbExternal bool
	if err := pool.QueryRow(context.Background(),
		`SELECT is_external FROM users WHERE id = $1`, uid).Scan(&dbExternal); err != nil {
		t.Fatalf("verify is_external: %v", err)
	}
	if !dbExternal {
		t.Fatalf("db is_external = false, want true")
	}
}

// TestUserCreate_ExternalAdminRejected — sk-163 S8/S3. is_external=true +
// role=admin is refused with the clean validation_error (external can never
// be admin), no row written.
func TestUserCreate_ExternalAdminRejected(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserCreate(store)
	ctx := withCtx(t, adminID, "admin")

	email := "mcpextadm" + adminID.String()[:6] + "@customer.com"
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"email":       email,
		"name":        "External Admin Attempt",
		"password":    "passw0rd!",
		"role":        "admin",
		"is_external": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError for external+admin")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeValidationError)
	}
	// No row should have been written.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE lower(email) = lower($1)`, email).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows for rejected external admin, got %d", n)
	}
}

// satisfy unused import linter when only some tests reference uuid.
var _ = uuid.Nil
