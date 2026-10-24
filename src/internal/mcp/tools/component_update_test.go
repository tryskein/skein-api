package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

// seedComponent inserts a components row directly and returns its id.
// Lives in the update-tests file so the same helper covers archive tests
// without exporting it from a non-_test file.
func seedComponent(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID, creator uuid.UUID, name, description string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	_, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, '[]'::jsonb, NULL, $5, now(), now())`,
		id, projectID, name, description, creator)
	if err != nil {
		t.Fatalf("seed component: %v", err)
	}
	return id
}

// TestComponentUpdate_PatchName updates only the name; description is
// left untouched.
func TestComponentUpdate_PatchName(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cup")
	cid := seedComponent(t, pool, pid, userID, "old-name", "untouched")

	store := projects.NewStore(pool)
	handler := handleComponentUpdate(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"id":      cid.String(),
		"name":    "new-name",
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
	c, _ := body["component"].(map[string]any)
	if c["name"] != "new-name" {
		t.Fatalf("name = %v, want new-name", c["name"])
	}
	if c["description"] != "untouched" {
		t.Fatalf("description = %v, want untouched", c["description"])
	}
}

// TestComponentUpdate_ClearDescription — passing description=""
// explicitly clears the field.
func TestComponentUpdate_ClearDescription(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cucd")
	cid := seedComponent(t, pool, pid, userID, "comp", "old description")

	store := projects.NewStore(pool)
	handler := handleComponentUpdate(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":     pid.String(),
		"id":          cid.String(),
		"description": "",
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
	c, _ := body["component"].(map[string]any)
	if c["description"] != "" {
		t.Fatalf("description = %v, want empty", c["description"])
	}
}

// TestComponentUpdate_NonOwner — caller is a member but not an owner →
// forbidden.
func TestComponentUpdate_NonOwner(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	member, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "cumb")
	cid := seedComponent(t, pool, pid, owner, "comp", "")
	if _, err := pool.Exec(context.Background(), `
WITH pm_ins AS (
INSERT INTO project_members (project_id, user_id, role, added_at, added_by)
VALUES ($1, $2, 'member', now(), $3)
RETURNING project_id, user_id)
-- sk-674.18 T017: a member of a project of a space is a participant
-- of that space (INV-5, INV-1b); external users stay outside (E-16).
INSERT INTO space_members (space_id, user_id, role)
SELECT p.space_id, pm_ins.user_id, 'member'
  FROM pm_ins JOIN projects p ON p.id = pm_ins.project_id JOIN users u ON u.id = pm_ins.user_id
 WHERE p.space_id IS NOT NULL AND NOT u.is_external
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, member, owner); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	store := projects.NewStore(pool)
	handler := handleComponentUpdate(pool, store)
	ctx := withCtx(t, member, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"id":      cid.String(),
		"name":    "renamed-by-member",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-owner update")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", got, ErrCodeForbidden)
	}
}
