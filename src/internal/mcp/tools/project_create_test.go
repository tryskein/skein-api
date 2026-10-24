package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

// TestProjectCreate_HappyPath creates a project with name + idPrefix +
// description; verifies the response carries the new project and the
// caller is bootstrapped as owner.
func TestProjectCreate_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	// T024: the creator's one space takes the project (US-12).
	testsupport.SeedSpace(t, pool, userID)

	store := projects.NewStore(pool)
	handler := handleProjectCreate(pool, store, "members")
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":        "Demo Test Project",
		"idPrefix":    "demotst1",
		"description": "happy path",
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
	project, ok := body["project"].(map[string]any)
	if !ok {
		t.Fatalf("project missing or wrong type: %T", body["project"])
	}
	if project["name"] != "Demo Test Project" {
		t.Fatalf("name = %v, want Demo Test Project", project["name"])
	}
	if project["idPrefix"] != "demotst1" {
		t.Fatalf("idPrefix = %v, want demotst1", project["idPrefix"])
	}
	if project["defaultIdMode"] != "counter" {
		t.Fatalf("defaultIdMode = %v, want counter", project["defaultIdMode"])
	}
	pid, _ := project["id"].(string)
	if pid == "" {
		t.Fatalf("project id missing")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
	})

	// Verify owner row exists.
	var role string
	if err := pool.QueryRow(context.Background(), `
SELECT role FROM project_members WHERE project_id = $1 AND user_id = $2`,
		pid, userID).Scan(&role); err != nil {
		t.Fatalf("check membership: %v", err)
	}
	if role != "owner" {
		t.Fatalf("creator role = %q, want owner", role)
	}
}

// TestProjectCreate_DuplicatePrefix — second project with same prefix
// fails with the neutral prefix_taken (sk-674.18 T025: the code of the
// spaces registry, as REST).
func TestProjectCreate_DuplicatePrefix(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	// Seed an existing project with the prefix we'll try to reuse.
	pidExisting, prefix := seedProject(t, pool, userID, "pcdup")
	_ = pidExisting

	store := projects.NewStore(pool)
	handler := handleProjectCreate(pool, store, "members")
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":     "Different Display",
		"idPrefix": prefix,
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for duplicate prefix")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodePrefixTaken {
		t.Fatalf("error = %v, want %s", got, ErrCodePrefixTaken)
	}
}

// TestProjectCreate_BadName — empty / whitespace name → validation_error.
func TestProjectCreate_BadName(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := projects.NewStore(pool)
	handler := handleProjectCreate(pool, store, "members")
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name": "   \t  ",
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

// TestProjectCreate_BadGitRepo — gitRepos entry without url → validation_error.
func TestProjectCreate_BadGitRepo(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	store := projects.NewStore(pool)
	handler := handleProjectCreate(pool, store, "members")
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":     "Test Repos",
		"idPrefix": "trepos1",
		"gitRepos": []any{
			map[string]any{"name": "api"}, // missing url
		},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing repo url")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestProjectCreate_AliasCollision — alias already taken by another
// project → id_taken (constraint name carries project_aliases).
func TestProjectCreate_AliasCollision(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	// Seed a project + alias that we will collide against.
	pid, _ := seedProject(t, pool, userID, "pcal")
	taken := "alias-taken-" + pid.String()[:6]
	if _, err := pool.Exec(context.Background(), `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, 'human', now())`, taken, pid, userID); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	store := projects.NewStore(pool)
	handler := handleProjectCreate(pool, store, "members")
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":     "Aliased Newcomer",
		"idPrefix": "alnew1",
		"aliases":  []any{taken},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for alias collision")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIDTaken {
		t.Fatalf("error = %v, want %s", got, ErrCodeIDTaken)
	}
}

// TestProjectCreate_EventActorTypeAgent — verifies the project.created
// event row is attributed to the agent path, not to a human. Regression
// guard for R2.2c W1: previously the MCP tool resolved a Role='agent'
// actor but only used its Username and the store fell back to
// HumanActor, so every MCP-created project showed up in the events log
// as actor_type='human' / actor_session_id=NULL.
func TestProjectCreate_EventActorTypeAgent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	// T024: the creator's one space takes the project (US-12).
	testsupport.SeedSpace(t, pool, userID)

	store := projects.NewStore(pool)
	handler := handleProjectCreate(pool, store, "members")
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":     "Agent Audit Project",
		"idPrefix": "agtaud1",
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
	project, _ := body["project"].(map[string]any)
	pid, _ := project["id"].(string)
	if pid == "" {
		t.Fatalf("project id missing")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
	})

	var actorType, actorSession string
	if err := pool.QueryRow(context.Background(), `
SELECT actor_type, COALESCE(actor_session_id, '')
  FROM events
 WHERE entity_type = 'project'
   AND entity_id   = $1
   AND type        = 'project.created'
 LIMIT 1`, pid).Scan(&actorType, &actorSession); err != nil {
		t.Fatalf("verify event: %v", err)
	}
	if actorType != "agent" {
		t.Fatalf("event actor_type = %q, want %q", actorType, "agent")
	}
	if actorSession == "" {
		t.Fatalf("event actor_session_id should be populated from ctx (got empty)")
	}
}

// TestProjectCreate_DefaultIdModeHashPersisted — explicit defaultIdMode
// is honored (and ends up in the row).
func TestProjectCreate_DefaultIdModeHashPersisted(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	// T024: the creator's one space takes the project (US-12).
	testsupport.SeedSpace(t, pool, userID)

	store := projects.NewStore(pool)
	handler := handleProjectCreate(pool, store, "members")
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":          "Hash Mode Project",
		"idPrefix":      "hmodep1",
		"defaultIdMode": "hash",
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
	project, _ := body["project"].(map[string]any)
	if project["defaultIdMode"] != "hash" {
		t.Fatalf("defaultIdMode = %v, want hash", project["defaultIdMode"])
	}
	pid, _ := project["id"].(string)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, pid)
	})
	var dbMode string
	if err := pool.QueryRow(context.Background(),
		`SELECT default_id_mode FROM projects WHERE id = $1`, pid,
	).Scan(&dbMode); err != nil {
		t.Fatalf("check db mode: %v", err)
	}
	if dbMode != "hash" {
		t.Fatalf("db default_id_mode = %q, want hash", dbMode)
	}
}

// TestProjectCreate_AgentChoosesTheSpace — sk-674.18 T024, matrix row 94
// (US-12). Reference — the contract «Choosing the space of a new project»
// (spaces/contract.go, T013): exactly one space where the caller may create
// projects → the project goes there; several and none named → refused with
// space_required listing every one of them; one named → that one.
func TestProjectCreate_AgentChoosesTheSpace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID, _ := seedUser(t, pool, "user")
	handler := handleProjectCreate(pool, projects.NewStore(pool), "members")
	call := func(args map[string]any) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := handler(withCtx(t, userID, "user"), req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		body := extractToolJSON(t, res)
		if !res.IsError {
			p := body["project"].(map[string]any)
			pid := p["id"].(string)
			t.Cleanup(func() {
				c := context.Background()
				for _, q := range []string{
					`DELETE FROM events WHERE project_id = $1`,
					`DELETE FROM project_members WHERE project_id = $1`,
					`DELETE FROM project_counters WHERE project_id = $1`,
					`DELETE FROM projects WHERE id = $1`,
				} {
					_, _ = pool.Exec(c, q, pid)
				}
			})
		}
		return body
	}
	shortOf := func(id uuid.UUID) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := shortOf(testsupport.SeedSpace(t, pool, userID))

	// One space: no need to name it.
	prefix := "pac" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	body := call(map[string]any{"name": prefix, "idPrefix": prefix})
	if p, ok := body["project"].(map[string]any); !ok || p["spaceId"] != first {
		t.Fatalf("with one space: %v, want the project in %s", body, first)
	}

	// Two spaces, none named: space_required with both.
	second := shortOf(testsupport.SeedSpace(t, pool, userID))
	prefix = "pac" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	body = call(map[string]any{"name": prefix, "idPrefix": prefix})
	if body["error"] != ErrCodeSpaceRequired {
		t.Fatalf("with two spaces and none named: %v, want space_required", body)
	}
	listed := map[string]bool{}
	for _, s := range body["spaces"].([]any) {
		listed[s.(map[string]any)["id"].(string)] = true
	}
	if len(listed) != 2 || !listed[first] || !listed[second] {
		t.Errorf("space_required lists %v, want exactly %s and %s", listed, first, second)
	}

	// Named: there.
	prefix = "pac" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	body = call(map[string]any{"name": prefix, "idPrefix": prefix, "space": second})
	if p, ok := body["project"].(map[string]any); !ok || p["spaceId"] != second {
		t.Errorf("with the space named: %v, want the project in %s", body, second)
	}
}
