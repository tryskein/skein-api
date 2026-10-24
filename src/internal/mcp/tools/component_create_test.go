package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

// TestComponentCreate_HappyPath — creates a component, asserts wire shape
// + persisted row.
func TestComponentCreate_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ccch")

	store := projects.NewStore(pool)
	handler := handleComponentCreate(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":     pid.String(),
		"name":        "auth-service",
		"description": "OAuth/SAML/Local auth",
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
	c, ok := body["component"].(map[string]any)
	if !ok {
		t.Fatalf("component missing or wrong type: %T", body["component"])
	}
	if c["name"] != "auth-service" {
		t.Fatalf("name = %v, want auth-service", c["name"])
	}
	if c["description"] != "OAuth/SAML/Local auth" {
		t.Fatalf("description mismatch: %v", c["description"])
	}
	cid, _ := c["id"].(string)
	if cid == "" {
		t.Fatalf("id missing")
	}

	var dbName string
	if err := pool.QueryRow(context.Background(),
		`SELECT name FROM components WHERE id = $1`, cid,
	).Scan(&dbName); err != nil {
		t.Fatalf("check db: %v", err)
	}
	if dbName != "auth-service" {
		t.Fatalf("db name = %q, want auth-service", dbName)
	}
}

// TestComponentCreate_DuplicateName — second create with the same name
// returns id_taken.
func TestComponentCreate_DuplicateName(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cccdup")
	// Seed the first component manually.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES (gen_random_uuid(), $1, 'taken', '', '[]'::jsonb, NULL, $2, now(), now())`,
		pid, userID); err != nil {
		t.Fatalf("seed first component: %v", err)
	}

	store := projects.NewStore(pool)
	handler := handleComponentCreate(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"name":    "taken",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for duplicate name")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIDTaken {
		t.Fatalf("error = %v, want %s", got, ErrCodeIDTaken)
	}
}

// TestComponentCreate_NonMember — caller cannot see project →
// project_not_found (not forbidden, to avoid leaking existence).
func TestComponentCreate_NonMember(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "cccnm")

	store := projects.NewStore(pool)
	handler := handleComponentCreate(pool, store)
	ctx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"name":    "should-not-create",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-member")
	}
	body := extractToolJSON(t, res)
	got := body["error"]
	// Resolution path will fail first with project_not_found because the
	// project handle isn't visible to the stranger.
	if got != ErrCodeProjectNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeProjectNotFound)
	}
}

// TestComponentCreate_MemberNotOwner — member (not owner) gets forbidden.
func TestComponentCreate_MemberNotOwner(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	member, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "cccmo")
	// Add member as a plain member (not owner).
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
	handler := handleComponentCreate(pool, store)
	ctx := withCtx(t, member, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"name":    "member-attempt",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-owner")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", got, ErrCodeForbidden)
	}
}

// TestComponentCreate_EmptyName — whitespace-only name → validation_error.
func TestComponentCreate_EmptyName(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "ccceb")

	store := projects.NewStore(pool)
	handler := handleComponentCreate(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"name":    "   ",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for empty name")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}
