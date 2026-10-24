package tools

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/users"
)

// TestUserList_HappyPath — admin lists active users; the seeded admin
// itself is in the result.
func TestUserList_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserList(store)
	ctx := withCtx(t, adminID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError: %+v", body)
	}
	body := extractToolJSON(t, res)
	usersList, _ := body["users"].([]any)
	if len(usersList) == 0 {
		t.Fatalf("expected at least the seeded admin in list")
	}
}

// TestUserList_NotAdmin returns not_admin.
func TestUserList_NotAdmin(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleUserList(store)
	ctx := withCtx(t, userID, "user")

	res, err := handler(ctx, mcpgo.CallToolRequest{})
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
