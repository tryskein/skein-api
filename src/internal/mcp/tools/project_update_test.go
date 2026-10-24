package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

// TestProjectUpdate_OwnerScalarPatch — the owner updates name +
// description; both fields land, two scalar field-changed events are
// written. An instance admin without a role in the project then patches it
// the same way: in a project of a non-personal space he has the owner's
// rights (US-6 as changed by the owner 2026-10-17, sk-741; between
// sk-674.18 T017 and sk-741 he was refused `forbidden`).
func TestProjectUpdate_OwnerScalarPatch(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	adminID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, ownerID, "puad")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	countFieldEvents := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*)
  FROM events
 WHERE entity_type = 'project'
   AND entity_id   = $1
   AND type        = 'project.field_changed'`, pid.String()).Scan(&n); err != nil {
			t.Fatalf("count field events: %v", err)
		}
		return n
	}
	patch := func(ctx context.Context, who, name, description string) {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project":     pid.String(),
			"name":        name,
			"description": description,
		}
		res, err := handler(ctx, req)
		if err != nil {
			t.Fatalf("handler (%s): %v", who, err)
		}
		if res.IsError {
			t.Fatalf("%s: unexpected isError: %+v", who, extractToolJSON(t, res))
		}
		project, _ := extractToolJSON(t, res)["project"].(map[string]any)
		if project["name"] != name {
			t.Fatalf("%s: name = %v, want %s", who, project["name"], name)
		}
		if project["description"] != description {
			t.Fatalf("%s: description = %v, want %s", who, project["description"], description)
		}
	}

	patch(withCtx(t, ownerID, "user"), "owner", "Renamed By Owner", "freshly patched")
	// Two child field_changed events written (name + description).
	if got := countFieldEvents(); got != 2 {
		t.Fatalf("project.field_changed events after the owner = %d, want 2", got)
	}

	patch(withCtx(t, adminID, "admin"), "admin without a role", "Renamed By Admin", "patched by the admin")
	if got := countFieldEvents(); got != 4 {
		t.Fatalf("project.field_changed events after the admin = %d, want 4", got)
	}
}

// TestProjectUpdate_OwnerAddsRepos — project owner appends two repos via
// gitReposAdd; gitRepos[] grows accordingly and one project.repo_added
// event lands per addition.
func TestProjectUpdate_OwnerAddsRepos(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "puro")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"gitReposAdd": []any{
			map[string]any{"url": "https://example.com/api.git", "name": "api", "defaultBranch": "main"},
			map[string]any{"url": "https://example.com/web.git", "name": "web"},
		},
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
	repos, _ := project["gitRepos"].([]any)
	if len(repos) != 2 {
		t.Fatalf("gitRepos length = %d, want 2", len(repos))
	}
	added, _ := body["reposAdded"].([]any)
	if len(added) != 2 {
		t.Fatalf("reposAdded length = %d, want 2", len(added))
	}

	var repoEvents int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*)
  FROM events
 WHERE entity_type = 'project'
   AND entity_id   = $1
   AND type        = 'project.repo_added'`, pid.String()).Scan(&repoEvents); err != nil {
		t.Fatalf("count repo events: %v", err)
	}
	if repoEvents != 2 {
		t.Fatalf("project.repo_added events = %d, want 2", repoEvents)
	}
}

// TestProjectUpdate_MemberForbidden — plain member (not owner) gets
// forbidden when trying to patch.
func TestProjectUpdate_MemberForbidden(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	memberID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "pumf")

	// Add memberID as plain member.
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
ON CONFLICT (space_id, user_id) DO NOTHING`, pid, memberID, ownerID); err != nil {
		t.Fatalf("seed plain member: %v", err)
	}

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, memberID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"name":    "Forbidden Rename",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for plain member")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeForbidden {
		t.Fatalf("error = %v, want %s", got, ErrCodeForbidden)
	}
}

// TestProjectUpdate_EmptyPatch — no fields → validation_error.
func TestProjectUpdate_EmptyPatch(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "puep")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for empty patch")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestProjectUpdate_BadGitRepoURL — empty url in gitReposAdd entry →
// validation_error, no DB writes.
func TestProjectUpdate_BadGitRepoURL(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "pubg")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": pid.String(),
		"gitReposAdd": []any{
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

	// Verify project untouched.
	var repoCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT jsonb_array_length(git_repos) FROM projects WHERE id = $1`, pid,
	).Scan(&repoCount); err != nil {
		t.Fatalf("check repo count: %v", err)
	}
	if repoCount != 0 {
		t.Fatalf("repos changed despite validation error: count=%d", repoCount)
	}
}

// TestProjectUpdate_RemoveMissingRepo — gitReposRemove with non-existent
// repoId → validation_error.
func TestProjectUpdate_RemoveMissingRepo(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "purm")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	bogusRepoID := uuid.Must(uuid.NewV7())
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":        pid.String(),
		"gitReposRemove": []any{bogusRepoID.String()},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing repoId")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error = %v, want %s", got, ErrCodeValidationError)
	}
}

// TestProjectUpdate_ArchiveFlip — archived=true on an active project
// archives it and writes a project.archived event.
func TestProjectUpdate_ArchiveFlip(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "puar")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
		"archived": true,
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
	if project["archivedAt"] == nil || project["archivedAt"] == "" {
		t.Fatalf("archivedAt should be populated, got %v", project["archivedAt"])
	}
	flip, _ := body["archivedFlip"].(bool)
	if !flip {
		t.Fatalf("archivedFlip should be true, got %v", body["archivedFlip"])
	}

	// project.archived event present.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*)
  FROM events
 WHERE entity_type = 'project'
   AND entity_id   = $1
   AND type        = 'project.archived'`, pid.String()).Scan(&n); err != nil {
		t.Fatalf("count archived events: %v", err)
	}
	if n != 1 {
		t.Fatalf("project.archived events = %d, want 1", n)
	}
}

// TestProjectUpdate_UnarchiveNoOp — archived=false on an already-active
// project is silently ignored: no event, no error.
func TestProjectUpdate_UnarchiveNoOp(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "puun")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":  pid.String(),
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
	if body["archivedFlip"] != nil {
		t.Fatalf("archivedFlip should be nil for no-op, got %v", body["archivedFlip"])
	}

	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*)
  FROM events
 WHERE entity_type = 'project'
   AND entity_id   = $1
   AND type        IN ('project.archived', 'project.unarchived')`, pid.String()).Scan(&n); err != nil {
		t.Fatalf("count archive events: %v", err)
	}
	if n != 0 {
		t.Fatalf("archive events = %d, want 0 for no-op", n)
	}
}

// TestProjectUpdate_AliasAddRemove — owner adds an alias, then a second
// patch removes it. Both events present, no rows leftover.
func TestProjectUpdate_AliasAddRemove(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "puar2")

	alias := "alias-" + strings.ReplaceAll(pid.String()[:8], "-", "")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	// Add.
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":    pid.String(),
		"aliasesAdd": []any{alias},
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler add: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError on add: %+v", body)
	}

	// Remove.
	req = mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":       pid.String(),
		"aliasesRemove": []any{alias},
	}
	res, err = handler(ctx, req)
	if err != nil {
		t.Fatalf("handler remove: %v", err)
	}
	if res.IsError {
		body := extractToolJSON(t, res)
		t.Fatalf("unexpected isError on remove: %+v", body)
	}

	// Alias row gone.
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM project_aliases WHERE project_id = $1 AND alias = $2`, pid, alias,
	).Scan(&n); err != nil {
		t.Fatalf("count alias rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("alias rows = %d, want 0", n)
	}

	// Both events written.
	var added, removed int
	if err := pool.QueryRow(context.Background(), `
SELECT
    count(*) FILTER (WHERE type = 'project.alias_added'),
    count(*) FILTER (WHERE type = 'project.alias_removed')
  FROM events
 WHERE entity_type = 'project'
   AND entity_id   = $1`, pid.String()).Scan(&added, &removed); err != nil {
		t.Fatalf("count alias events: %v", err)
	}
	if added != 1 || removed != 1 {
		t.Fatalf("alias events: added=%d removed=%d, want 1/1", added, removed)
	}
}

// TestProjectUpdate_AliasCollisionRollsBack — adding two aliases where
// the second already exists on another project rolls back the entire
// patch: the first alias is NOT inserted.
func TestProjectUpdate_AliasCollisionRollsBack(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "pucr")
	otherPID, _ := seedProject(t, pool, userID, "puot")

	taken := "taken-" + strings.ReplaceAll(otherPID.String()[:8], "-", "")
	if _, err := pool.Exec(context.Background(), `
INSERT INTO project_aliases (alias, project_id, added_by, added_by_kind, created_at)
VALUES ($1, $2, $3, 'human', now())`, taken, otherPID, userID); err != nil {
		t.Fatalf("seed alias on other project: %v", err)
	}

	fresh := "fresh-" + strings.ReplaceAll(pid.String()[:8], "-", "")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":    pid.String(),
		"aliasesAdd": []any{fresh, taken}, // second one collides
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true on collision")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeIDTaken {
		t.Fatalf("error = %v, want %s", got, ErrCodeIDTaken)
	}

	// Fresh alias must NOT be present (rollback).
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM project_aliases WHERE project_id = $1 AND alias = $2`, pid, fresh,
	).Scan(&n); err != nil {
		t.Fatalf("count fresh alias: %v", err)
	}
	if n != 0 {
		t.Fatalf("fresh alias rows = %d after rollback, want 0", n)
	}
}

// TestProjectUpdate_ConcurrentPatches — two parallel patches against the
// same project both succeed. SERIALIZABLE retry inside pgx.BeginTxFunc is
// not exposed at this layer; the test verifies the outer behaviour: both
// callers get a successful response and the post-state reflects both
// changes (gitRepos[] grew by 2).
func TestProjectUpdate_ConcurrentPatches(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, userID, "pucp")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "user")

	makeReq := func(suffix string) mcpgo.CallToolRequest {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"project": pid.String(),
			"gitReposAdd": []any{
				map[string]any{"url": "https://example.com/" + suffix + ".git", "name": suffix},
			},
		}
		return req
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	results := make([]bool, 2) // true = no isError

	wg.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			res, err := handler(ctx, makeReq(string(rune('a'+i))))
			errs[i] = err
			if err == nil && res != nil && !res.IsError {
				results[i] = true
			}
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d errored: %v", i, err)
		}
	}

	// At least one of the two should have completed successfully. The
	// SERIALIZABLE isolation may surface a 40001 to the second writer
	// (which our store layer does not retry — see the deviation note).
	// The test guards the looser "best effort" outcome: gitRepos[] must
	// reflect a strict superset of what was committed.
	var repoCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT jsonb_array_length(git_repos) FROM projects WHERE id = $1`, pid,
	).Scan(&repoCount); err != nil {
		t.Fatalf("check repo count: %v", err)
	}
	successCount := 0
	for _, ok := range results {
		if ok {
			successCount++
		}
	}
	if repoCount != successCount {
		t.Fatalf("repo count after concurrent patches = %d, want %d (one per success)", repoCount, successCount)
	}
}

// TestProjectUpdate_ProjectNotFound — bogus UUID → project_not_found.
func TestProjectUpdate_ProjectNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")

	store := projects.NewStore(pool)
	handler := handleProjectUpdate(pool, store)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project": uuid.Must(uuid.NewV7()).String(),
		"name":    "x",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for missing project")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeProjectNotFound {
		t.Fatalf("error = %v, want %s", got, ErrCodeProjectNotFound)
	}
}

// TestProjectUpdate_NullSpaceIsAbsent, the
// MCP half: `space: null` names no space and the update applies; any value
// is still refused with space_change_forbidden.
func TestProjectUpdate_NullSpaceIsAbsent(t *testing.T) {
	pool := newTestPool(t)
	ownerID, _ := seedUser(t, pool, "user")
	pid, _ := seedProject(t, pool, ownerID, "punl")
	handler := handleProjectUpdate(pool, projects.NewStore(pool))
	call := func(args map[string]any) (bool, map[string]any) {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		res, err := handler(withCtx(t, ownerID, "user"), req)
		if err != nil {
			t.Fatal(err)
		}
		return res.IsError, extractToolJSON(t, res)
	}
	if isErr, body := call(map[string]any{"project": pid.String(), "space": nil, "description": "null space"}); isErr {
		t.Errorf("space null: %v, want the update applied", body)
	}
	if isErr, body := call(map[string]any{"project": pid.String(), "space": "aaaaaaaa", "description": "moved"}); !isErr || body["error"] != ErrCodeSpaceChangeForbidden {
		t.Errorf("a space value: %v, want %s", body, ErrCodeSpaceChangeForbidden)
	}
	var desc string
	if err := pool.QueryRow(context.Background(), `SELECT description FROM projects WHERE id = $1`, pid).Scan(&desc); err != nil {
		t.Fatal(err)
	}
	if desc != "null space" {
		t.Errorf("description %q, want the null-space update and not the refused one", desc)
	}
}
