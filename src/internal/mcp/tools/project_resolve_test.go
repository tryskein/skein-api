package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

func TestProjectResolve_ExactName(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "prv")

	// Look up the actual project name (seedProject builds it as
	// "<prefix>_<8 hex>" so we read the row instead of reconstructing).
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT name FROM projects WHERE id = $1`, pid).Scan(&name); err != nil {
		t.Fatalf("loading project name: %v", err)
	}

	pStore := projects.NewStore(pool)
	handler := handleProjectResolve(pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"q": name}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	match, _ := body["match"].(map[string]any)
	if match == nil {
		t.Fatalf("expected match for exact name, got nil; body=%v", body)
	}
	if got := match["name"].(string); got != name {
		t.Fatalf("match.name = %q, want %q", got, name)
	}
}

func TestProjectResolve_NoMatch(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	pStore := projects.NewStore(pool)
	handler := handleProjectResolve(pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"q": "wxyzzz-no-such-project-anywhere"}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	if body["match"] != nil {
		t.Fatalf("expected match=nil for no hits")
	}
	cands, ok := body["candidates"].([]any)
	if !ok {
		t.Fatalf("candidates must always be an array (possibly empty), got %T", body["candidates"])
	}
	if len(cands) != 0 {
		t.Fatalf("expected zero candidates, got %d", len(cands))
	}
}
