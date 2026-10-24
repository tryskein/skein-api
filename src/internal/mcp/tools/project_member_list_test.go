package tools

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

func TestProjectMemberList_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pml")

	pStore := projects.NewStore(pool)
	handler := handleProjectMemberList(pool, pStore)
	ctx := withCtx(t, ownerID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
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
	members, _ := body["members"].([]any)
	if len(members) == 0 {
		t.Fatalf("expected at least the owner in member list")
	}
}

func TestProjectMemberList_NotVisible(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	otherID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmlnv")

	pStore := projects.NewStore(pool)
	handler := handleProjectMemberList(pool, pStore)
	// otherID is not a member.
	ctx := withCtx(t, otherID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError for non-visible project")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeProjectNotFound {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeProjectNotFound)
	}
}
