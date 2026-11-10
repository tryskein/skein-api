package tools

// source_api_test.go — test matrix row 20 (OST-2-8, the exact filter in MCP).
// Reference — the spec, written before the code: skein_list human-driven and
// skein_search include api; source=api and commentAuthorKinds=[api] are
// accepted, not refused as unknown; kind=directed / autonomous keep giving
// agent_directed / agent_autonomous.

import (
	"context"
	"slices"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/search"
)

func callSourceAPITool(t *testing.T, ctx context.Context, h server.ToolHandlerFunc, args map[string]any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("args %v refused: %v", args, body)
	}
	return body
}

func TestSourceAPI_MCPFilters(t *testing.T) {
	pool := newTestPool(t)
	owner, ownerName := seedUser(t, pool, "user")
	pid, prefix := seedProject(t, pool, owner, "lsapi")
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-1", "human", ownerName, nil)
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-2", "api", ownerName, nil)
	seedListIssueWithFields(t, pool, pid, owner, prefix+"-3", "agent_autonomous", ownerName, nil)
	ctx := withCtx(t, owner, "user")
	list := handleList(pool, issues.NewStore(pool), projects.NewStore(pool), nil)

	t.Run("skein_list human-driven includes api", func(t *testing.T) {
		ids := listItemIDs(t, callSourceAPITool(t, ctx, list, map[string]any{"project": pid.String(), "origin": "human-driven"}))
		slices.Sort(ids)
		if !slices.Equal(ids, []string{prefix + "-1", prefix + "-2"}) {
			t.Errorf("human-driven ids %v, want %s-1 and %s-2", ids, prefix, prefix)
		}
	})
	t.Run("skein_list source=api returns only api", func(t *testing.T) {
		ids := listItemIDs(t, callSourceAPITool(t, ctx, list, map[string]any{"project": pid.String(), "source": "api"}))
		if !slices.Equal(ids, []string{prefix + "-2"}) {
			t.Errorf("source=api ids %v, want only %s-2", ids, prefix)
		}
	})

	t.Run("skein_search accepts source=api and commentAuthorKinds=[api]", func(t *testing.T) {
		svc, err := search.NewService(search.NewStore(pool), nil, 60*time.Second)
		if err != nil {
			t.Fatalf("search service: %v", err)
		}
		h := handleSearch(pool, svc, projects.NewStore(pool))
		body := callSourceAPITool(t, ctx, h, map[string]any{"query": prefix, "project": pid.String(), "source": "api"})
		items, _ := body["items"].([]any)
		for _, raw := range items {
			row := raw.(map[string]any)
			if row["kind"] == "issue" && row["id"] != prefix+"-2" {
				t.Errorf("source=api returned issue %v, want only %s-2", row["id"], prefix)
			}
		}
		callSourceAPITool(t, ctx, h, map[string]any{"query": prefix, "commentAuthorKinds": []any{"api"}})
	})

	t.Run("skein_create kind keeps its values", func(t *testing.T) {
		// A project of its own: the rows seeded above bypass the id counter,
		// so a create in pid would collide with them.
		createPID, _ := seedProject(t, pool, owner, "lsapc")
		create := handleCreate(pool, issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool))
		for kind, want := range map[string]string{"directed": issues.SourceAgentDirected, "autonomous": issues.SourceAgentAutonomous} {
			body := callSourceAPITool(t, ctx, create, map[string]any{
				"project": createPID.String(), "title": "kind " + kind, "type": "task", "kind": kind,
			})
			issue, _ := body["issue"].(map[string]any)
			id, _ := issue["id"].(string)
			var source string
			if err := pool.QueryRow(context.Background(), `SELECT source FROM issues WHERE id = $1`, id).Scan(&source); err != nil {
				t.Fatalf("read source of %q (%v): %v", id, body, err)
			}
			if source != want {
				t.Errorf("kind=%s gave source %q, want %q", kind, source, want)
			}
		}
	})
}
