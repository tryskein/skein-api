package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

// TestComponentArchive_HappyPath — default archived=true sets archived_at.
func TestComponentArchive_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cah")
	cid := seedComponent(t, pool, pid, userID, "to-archive", "")

	store := projects.NewStore(pool)
	handler := handleComponentArchive(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"id":      cid.String(),
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
	if c["archivedAt"] == nil || c["archivedAt"] == "" {
		t.Fatalf("archivedAt should be set, got %v", c["archivedAt"])
	}

	// DB row reflects archived state.
	var archivedAt *string
	if err := pool.QueryRow(context.Background(),
		`SELECT archived_at::text FROM components WHERE id = $1`, cid,
	).Scan(&archivedAt); err != nil {
		t.Fatalf("check db: %v", err)
	}
	if archivedAt == nil {
		t.Fatalf("db archived_at is null after archive")
	}
}

// TestComponentArchive_Unarchive — archived=false on an archived comp
// clears archived_at.
func TestComponentArchive_Unarchive(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "caun")
	cid := seedComponent(t, pool, pid, userID, "comp", "")
	// Pre-archive directly.
	if _, err := pool.Exec(context.Background(),
		`UPDATE components SET archived_at = now() WHERE id = $1`, cid,
	); err != nil {
		t.Fatalf("pre-archive: %v", err)
	}

	store := projects.NewStore(pool)
	handler := handleComponentArchive(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"id":       cid.String(),
		"archived": false,
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
	if c["archivedAt"] != nil {
		t.Fatalf("archivedAt should be null after unarchive, got %v", c["archivedAt"])
	}
}

// TestComponentArchive_AlreadyArchivedIdempotent — second archive call
// on an already-archived component is a no-op (returns post-state, no
// error).
func TestComponentArchive_AlreadyArchivedIdempotent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "caai")
	cid := seedComponent(t, pool, pid, userID, "comp", "")
	if _, err := pool.Exec(context.Background(),
		`UPDATE components SET archived_at = now() WHERE id = $1`, cid,
	); err != nil {
		t.Fatalf("pre-archive: %v", err)
	}

	store := projects.NewStore(pool)
	handler := handleComponentArchive(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"id":       cid.String(),
		"archived": true,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError on idempotent archive: %+v", body)
	}
	body := extractToolJSON(t, res)
	c, _ := body["component"].(map[string]any)
	if c["archivedAt"] == nil {
		t.Fatalf("archivedAt should still be set, got nil")
	}
}

// TestComponentArchive_NonOwner — member (not owner) is rejected with
// forbidden.
func TestComponentArchive_NonOwner(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	member, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, owner, "canm")
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
	handler := handleComponentArchive(pool, store)
	ctx := withCtx(t, member, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"id":      cid.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for non-owner archive")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", got, ErrCodeForbidden)
	}
}
