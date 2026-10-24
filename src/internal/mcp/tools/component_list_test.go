package tools

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

func TestComponentList_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cmp")

	// Seed two components directly via SQL — the projects.Store path
	// also writes events, which is fine but heavier than we need here.
	cmpID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, 'comp-a', 'desc', '[]'::jsonb, NULL, $3, now(), now())`,
		cmpID, pid, userID); err != nil {
		t.Fatalf("seed component: %v", err)
	}

	pStore := projects.NewStore(pool)
	handler := handleComponentList(pool, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": pid.String()}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected at least one component, got %v", body)
	}
}

func TestComponentList_ProjectNotFound(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	pStore := projects.NewStore(pool)
	handler := handleComponentList(pool, pStore)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": "no-such-project-handle"}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeProjectNotFound {
		t.Fatalf("error code = %v, want %s", got, ErrCodeProjectNotFound)
	}
}

func TestComponentList_ValidationRejectsBadArchived(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cmpv")

	pStore := projects.NewStore(pool)
	handler := handleComponentList(pool, pStore)
	ctx := withCtx(t, userID, "admin")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project":        pid.String(),
		"archivedFilter": "maybe",
	}
	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError=true for invalid archivedFilter value")
	}
	body := extractToolJSON(t, res)
	if got := body["error"]; got != ErrCodeValidationError {
		t.Fatalf("error code = %v, want %s", got, ErrCodeValidationError)
	}
}

// callComponentList runs the list handler for one project and returns the
// decoded success body, failing the test on any error result.
func callComponentList(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, project string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"project": project}
	for k, v := range extra {
		args[k] = v
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args

	res, err := handleComponentList(pool, projects.NewStore(pool))(withCtx(t, userID, "admin"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	return extractToolJSON(t, res)
}

// seedComponentArchived inserts one component row directly, archived or not,
// and returns its id. The neighbouring seedComponent helper always inserts an
// active row, which is exactly the distinction these tests are about.
func seedComponentArchived(t *testing.T, pool *pgxpool.Pool, projectID, userID uuid.UUID, name string, archived bool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	archivedAt := "NULL"
	if archived {
		archivedAt = "now()"
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO components (id, project_id, name, description, paths, archived_at, created_by, created_at, updated_at)
VALUES ($1, $2, $3, '', '[]'::jsonb, `+archivedAt+`, $4, now(), now())`,
		id, projectID, name, userID); err != nil {
		t.Fatalf("seed component %s: %v", name, err)
	}
	return id
}

// TestComponentList_ArchivedCount_EmptyProject — sk-359 criterion 2, first
// half: a project with no components at all answers archivedCount=0.
func TestComponentList_ArchivedCount_EmptyProject(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cmpac0")

	body := callComponentList(t, pool, userID, pid.String(), nil)
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("items = %v, want empty", body["items"])
	}
	if got := body["archivedCount"]; got != float64(0) {
		t.Fatalf("archivedCount = %v, want 0", got)
	}
}

// TestComponentList_ArchivedCount_AllArchived — sk-359 criterion 2, second
// half: every component archived → empty items, archivedCount = their number.
// This is the pair the whole issue exists for: identical `items`, different
// answer.
func TestComponentList_ArchivedCount_AllArchived(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cmpac1")
	seedComponentArchived(t, pool, pid, userID, "gone-a", true)
	seedComponentArchived(t, pool, pid, userID, "gone-b", true)

	body := callComponentList(t, pool, userID, pid.String(), nil)
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("items = %v, want empty under the default filter", body["items"])
	}
	if got := body["archivedCount"]; got != float64(2) {
		t.Fatalf("archivedCount = %v, want 2", got)
	}
}

// TestComponentList_ArchivedCount_WithItems — sk-359 criterion 3: the count
// rides along on a non-empty listing too, so "is anything hidden" is
// answerable without an empty-list precondition.
func TestComponentList_ArchivedCount_WithItems(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cmpac2")
	seedComponentArchived(t, pool, pid, userID, "live-a", false)
	seedComponentArchived(t, pool, pid, userID, "gone-a", true)

	body := callComponentList(t, pool, userID, pid.String(), nil)
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Fatalf("items = %v, want exactly the active one", body["items"])
	}
	if got := body["archivedCount"]; got != float64(1) {
		t.Fatalf("archivedCount = %v, want 1", got)
	}
}

// TestComponentList_ArchivedCount_FilterIndependent — the count describes the
// project's component set, not the leftovers of the current filter, so it
// reads the same under every archivedFilter value.
func TestComponentList_ArchivedCount_FilterIndependent(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "admin")
	pid, _ := seedProject(t, pool, userID, "cmpac3")
	seedComponentArchived(t, pool, pid, userID, "live-a", false)
	seedComponentArchived(t, pool, pid, userID, "gone-a", true)
	seedComponentArchived(t, pool, pid, userID, "gone-b", true)

	for _, filter := range []string{"false", "true", "all"} {
		body := callComponentList(t, pool, userID, pid.String(),
			map[string]any{"archivedFilter": filter})
		if got := body["archivedCount"]; got != float64(2) {
			t.Errorf("archivedFilter=%q: archivedCount = %v, want 2", filter, got)
		}
	}
}
