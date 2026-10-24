package tools

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

func TestProjectMemberRemove_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmr")
	target, _ := seedUser(t, pool, "user")
	// INV-5 (T024): a project member is a participant of its space.
	testsupport.JoinProjectSpace(t, pool, pid, target)

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)

	// Add member first.
	addH := handleProjectMemberAdd(pool, pStore, uStore)
	ctx := withCtx(t, ownerID, "user")
	addReq := mcpgo.CallToolRequest{}
	addReq.Params.Arguments = map[string]any{
		"project": pid.String(),
		"userId":  target.String(),
		"role":    "member",
	}
	if _, err := addH(ctx, addReq); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Remove.
	rmH := handleProjectMemberRemove(pool, pStore, uStore)
	rmReq := mcpgo.CallToolRequest{}
	rmReq.Params.Arguments = map[string]any{
		"project": pid.String(),
		"userId":  target.String(),
	}
	res, err := rmH(ctx, rmReq)
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError: %+v", body)
	}
}

// TestProjectMemberRemove_LastOwner — the last explicit owner
// of a project of a space may be removed: the project stays under the
// implicit owner of its space (INV-4, US-17 "the last explicit owner
// leaves — no refusal"), who still sees it. Until sk-674.18 T018 the last
// owner could not be removed (last_owner); the rule of spaces replaced it.
func TestProjectMemberRemove_LastOwner(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmrls")

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleProjectMemberRemove(pool, pStore, uStore)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": pid.String(), "userId": ownerID.String()}
	res, err := handler(withCtx(t, ownerID, "user"), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("removing the last explicit owner of a space project was refused: %+v", extractToolJSON(t, res))
	}
	var spaceOwnerSees bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM projects p JOIN spaces s ON s.id = p.space_id
                 JOIN skein_project_access_set(s.owner_id) va ON va.project_id = p.id AND va.access = 'owner'
                WHERE p.id = $1)`, pid).Scan(&spaceOwnerSees); err != nil {
		t.Fatal(err)
	}
	if !spaceOwnerSees {
		t.Error("the project is left without an owner: the space owner has no owner level on it")
	}
}

// Restored (found in review): a plain member may
// remove himself without being an owner (project_member_remove.go: «caller
// must be owner of the project OR the same user as the target»), and the
// removal of a non-member answers the public member_not_found code. Both
// properties live on; a7837c5 dropped the tests without a §9 verdict.
func TestProjectMemberRemove_SelfLeave(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmrsl")
	memberID, _ := seedUser(t, pool, "user")
	if _, err := pool.Exec(t.Context(), `
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, memberID, ownerID); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleProjectMemberRemove(pool, pStore, uStore)
	ctx := withCtx(t, memberID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"userId":  memberID.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("self-leave should succeed; got %+v", body)
	}
}

func TestProjectMemberRemove_MemberNotFound(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmrmnf")
	stranger, _ := seedUser(t, pool, "user")

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleProjectMemberRemove(pool, pStore, uStore)
	ctx := withCtx(t, ownerID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"userId":  stranger.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected member_not_found")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeMemberNotFound {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeMemberNotFound)
	}
}
