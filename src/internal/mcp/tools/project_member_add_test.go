package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

func TestProjectMemberAdd_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pma")
	target, _ := seedUser(t, pool, "user")
	// INV-5 (T024): a project member is a participant of its space.
	testsupport.JoinProjectSpace(t, pool, pid, target)

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleProjectMemberAdd(pool, pStore, uStore)
	ctx := withCtx(t, ownerID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"userId":  target.String(),
		"role":    "member",
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

func TestProjectMemberAdd_ByUsername(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmau")
	target, targetUname := seedUser(t, pool, "user")
	// INV-5 (T024): a project member is a participant of its space.
	testsupport.JoinProjectSpace(t, pool, pid, target)
	_ = target

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleProjectMemberAdd(pool, pStore, uStore)
	ctx := withCtx(t, ownerID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"username": targetUname,
		"role":     "owner",
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

func TestProjectMemberAdd_NotOwner(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmano")
	// otherID is a plain (non-owner) member.
	otherID, _ := seedUser(t, pool, "user")
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, otherID, ownerID); err != nil {
		t.Fatalf("seed second member: %v", err)
	}
	target, _ := seedUser(t, pool, "user")

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleProjectMemberAdd(pool, pStore, uStore)
	ctx := withCtx(t, otherID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"userId":  target.String(),
		"role":    "member",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected forbidden for plain member adding")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeForbidden)
	}
}

func TestProjectMemberAdd_UnknownUsername(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pmaun")

	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	handler := handleProjectMemberAdd(pool, pStore, uStore)
	ctx := withCtx(t, ownerID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"username": "no-such-username-12345",
		"role":     "member",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected user_not_found")
	}
	body := extractToolJSON(t, res)
	if body["error"] != ErrCodeUserNotFound {
		t.Fatalf("error = %v, want %s", body["error"], ErrCodeUserNotFound)
	}
}

// memberAddCall runs skein_project_members(action=add) as owner and returns
// the decoded answer and whether it is an error.
func memberAddCall(t *testing.T, pool *pgxpool.Pool, owner, pid, target uuid.UUID, role string) (map[string]any, bool) {
	t.Helper()
	pStore := projects.NewStore(pool)
	uStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	h := handleProjectMemberAdd(pool, pStore, uStore)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": pid.String(), "userId": target.String(), "role": role}
	res, err := h(withCtx(t, owner, "user"), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return extractToolJSON(t, res), res.IsError
}

func setExternal(t *testing.T, pool *pgxpool.Pool, u uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE users SET is_external = true WHERE id = $1`, u); err != nil {
		t.Fatal(err)
	}
}

// TestProjectMemberAdd_INV5 — sk-674.18 T024, matrix row 96 (the MCP half;
// REST — handler/projects_spaces_test.go). Reference — INV-5 and US-12 of
// the spec: a member of a project of a non-personal space is a participant
// of that space (any role) or an external user (member only); anyone else is
// refused with the answer of a user who does not exist — the two
// indistinguishable.
func TestProjectMemberAdd_INV5(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, owner)
	pid, _ := testsupport.SeedProjectInSpace(t, pool, owner, space, "pmi")
	participant, _ := seedUser(t, pool, "user")
	other, _ := seedUser(t, pool, "user")
	testsupport.AddSpaceMember(t, pool, space, participant)
	testsupport.AddSpaceMember(t, pool, space, other)
	ext, _ := seedUser(t, pool, "user")
	setExternal(t, pool, ext)
	outsider, _ := seedUser(t, pool, "user")

	for _, c := range []struct {
		name string
		who  uuid.UUID
		role string
	}{
		{"a participant as member", participant, "member"},
		{"a participant as owner", other, "owner"},
		{"an external user as member", ext, "member"},
	} {
		if body, isErr := memberAddCall(t, pool, owner, pid, c.who, c.role); isErr {
			t.Errorf("%s: refused %v", c.name, body)
		}
	}
	if body, isErr := memberAddCall(t, pool, owner, pid, ext, "owner"); !isErr {
		t.Errorf("an external user as owner: accepted %v", body)
	}
	stranger, sErr := memberAddCall(t, pool, owner, pid, outsider, "member")
	missing, mErr := memberAddCall(t, pool, owner, pid, uuid.New(), "member")
	if !sErr || !mErr {
		t.Fatalf("a non-participant or a nonexistent user was accepted: %v %v", stranger, missing)
	}
	delete(stranger, "userId")
	delete(missing, "userId")
	if fmt.Sprint(stranger) != fmt.Sprint(missing) {
		t.Errorf("non-participant %v differs from nonexistent %v", stranger, missing)
	}
}

// TestProjectMemberAdd_PersonalSpaceTakesNobody — sk-674.18 T024, matrix row
// 97 (INV-8, the special actor of US-1). Reference — INV-8: a project of a
// A "Personal space" has no member but its owner, external users included; refused in
// REST and in MCP.
func TestProjectMemberAdd_PersonalSpaceTakesNobody(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	personal := testsupport.SeedSpaceOfKind(t, pool, owner, "personal")
	pid, _ := testsupport.SeedProjectInSpace(t, pool, owner, personal, "pmp")
	internal, _ := seedUser(t, pool, "user")
	ext, _ := seedUser(t, pool, "user")
	setExternal(t, pool, ext)

	ph := handler.NewProjectsHandler(pool, nil)
	r := chi.NewRouter()
	r.With(handler.ParseUserHeaders, handler.RequireUser).Put("/projects/{projectID}/members/{userID}", ph.UpsertMember)
	for _, who := range []uuid.UUID{internal, ext} {
		if body, isErr := memberAddCall(t, pool, owner, pid, who, "member"); !isErr {
			t.Errorf("MCP: %s added to a personal project: %v", who, body)
		}
		req := httptest.NewRequest(http.MethodPut, "/projects/"+pid.String()+"/members/"+who.String(),
			strings.NewReader(`{"role":"member"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.HeaderUserID, owner.String())
		req.Header.Set(auth.HeaderUsername, ownerName)
		req.Header.Set(auth.HeaderUserRole, "user")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("REST: %s added to a personal project: %d %s", who, rec.Code, rec.Body.String())
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM project_members WHERE project_id = $1`, pid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the personal project has %d members, want its owner alone", n)
	}
}
