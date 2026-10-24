package tools

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
)

func TestProjectList_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	seedProject(t, pool, userID, "pl1")
	seedProject(t, pool, userID, "pl2")

	pStore := projects.NewStore(pool)
	handler := handleProjectList(pStore, nil)
	ctx := withCtx(t, userID, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("expected items array")
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 projects (the two we seeded), got %d", len(items))
	}
}

func TestProjectList_NonMemberSeesNothing(t *testing.T) {
	pool := newTestPool(t)
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	seedProject(t, pool, owner, "plowned")

	pStore := projects.NewStore(pool)
	handler := handleProjectList(pStore, nil)
	ctx := withCtx(t, stranger, "user")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	res, err := handler(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %+v", res)
	}
	body := extractToolJSON(t, res)
	// stranger has no project_members rows, so the list is empty.
	items, _ := body["items"].([]any)
	for _, raw := range items {
		m := raw.(map[string]any)
		if name, ok := m["name"].(string); ok && len(name) > 0 && name[:7] == "plowned" {
			t.Fatalf("stranger should not see owner's plowned* projects: %v", name)
		}
	}
}

// ---------------------------------------------------------------------
// sk-282 T016 — the project-list row (test-plan rows 63 and 64). Row 65 is
// the REST side and lives with the REST handler.
// ---------------------------------------------------------------------

func callProjectList(t *testing.T, pool *pgxpool.Pool, caller uuid.UUID, args map[string]any) map[string]any {
	t.Helper()
	handler := handleProjectList(projects.NewStore(pool), nil)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := handler(withCtx(t, caller, "user"), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", extractToolJSON(t, res))
	}
	return extractToolJSON(t, res)
}

// row 63 — no page size named → at most the new default, with the ceiling
// unchanged.
func TestProjectList_DefaultPageIsFiftyCeilingStillHundred(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")
	for i := range ProjectListDefaultLimit + 5 {
		seedProject(t, pool, userID, "plpg"+strconv.Itoa(i))
	}

	body := callProjectList(t, pool, userID, map[string]any{})
	items := body["items"].([]any)
	if len(items) != ProjectListDefaultLimit {
		t.Errorf("default project page returned %d rows, want exactly %d",
			len(items), ProjectListDefaultLimit)
	}

	// The ceiling did not move with the default: asking for more than the
	// default still yields more.
	wide := callProjectList(t, pool, userID, map[string]any{"limit": ProjectListDefaultLimit + 5})
	if got := len(wide["items"].([]any)); got <= ProjectListDefaultLimit {
		t.Errorf("limit=%d returned %d rows — the ceiling is the store's 100 and T016 did not "+
			"move it; only the default changed", ProjectListDefaultLimit+5, got)
	}
}

// row 64 — the three description inputs, which are three and not two.
func TestProjectList_DescriptionTruncationThreeCases(t *testing.T) {
	pool := newTestPool(t)
	userID, _ := seedUser(t, pool, "user")

	long := strings.Repeat("д", ProjectDescriptionPreviewMaxRunes+40)
	short := "short but non-empty description"

	longID, _ := seedProject(t, pool, userID, "pldlong")
	shortID, _ := seedProject(t, pool, userID, "pldshort")
	emptyID, _ := seedProject(t, pool, userID, "pldempty")
	setProjectDescription(t, pool, longID, long)
	setProjectDescription(t, pool, shortID, short)
	// emptyID keeps the '' the seeder wrote.

	rows := map[string]map[string]any{}
	for _, raw := range callProjectList(t, pool, userID, map[string]any{"limit": 100})["items"].([]any) {
		row := raw.(map[string]any)
		rows[row["id"].(string)] = row
	}

	t.Run("long is cut and marked", func(t *testing.T) {
		row := rows[longID.String()]
		desc, _ := row["description"].(string)
		if n := len([]rune(desc)); n != ProjectDescriptionPreviewMaxRunes {
			t.Errorf("description is %d runes, want exactly %d", n, ProjectDescriptionPreviewMaxRunes)
		}
		if row["descriptionTruncated"] != true {
			t.Errorf("descriptionTruncated = %v, want true", row["descriptionTruncated"])
		}
	})

	t.Run("short and non-empty is untouched and unmarked", func(t *testing.T) {
		row := rows[shortID.String()]
		if row["description"] != short {
			t.Errorf("description = %v, want it untouched (%q)", row["description"], short)
		}
		if v, ok := row["descriptionTruncated"]; ok {
			t.Errorf("descriptionTruncated = %v on a description that fits — on short "+
				"text the marker is not set, otherwise the marker lies", v)
		}
	})

	t.Run("empty does not become truncated", func(t *testing.T) {
		row := rows[emptyID.String()]
		if row["description"] != "" {
			t.Errorf("description = %v, want the empty string", row["description"])
		}
		if v, ok := row["descriptionTruncated"]; ok {
			t.Errorf("descriptionTruncated = %v on an EMPTY description — US-8's edge case "+
				"says an empty description is not turned into a truncated one; empty and short are "+
				"different inputs and this is the one a two-case test would miss", v)
		}
	})
}

// setProjectDescription writes a project's description directly.
func setProjectDescription(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, text string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET description = $2 WHERE id = $1`, id, text); err != nil {
		t.Fatalf("set project description: %v", err)
	}
}
