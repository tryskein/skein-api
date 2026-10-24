package tools

// project_create_prefix_test.go — sk-674.18 T025, matrix row 106, the MCP
// half (REST — handler/projects_prefix_test.go). Reference — US-15 and E-17
// of the spec: a prefix taken anywhere — another user's "Personal space" included —
// is one neutral prefix_taken with 2–3 free variants, the same body as REST
// (error, message, suggestions), never the constraint, the space or the
// owner.

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

func TestProjectCreate_PrefixTakenIsNeutral(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	me, _ := seedUser(t, pool, "user")
	mine := testsupport.SeedSpace(t, pool, me)
	var mineShort string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, mine).Scan(&mineShort); err != nil {
		t.Fatal(err)
	}
	stranger, strangerName := seedUser(t, pool, "user")
	personal := testsupport.SeedSpaceOfKind(t, pool, stranger, "personal")
	var personalShort, personalName string
	if err := pool.QueryRow(ctx, `SELECT short_id, name FROM spaces WHERE id = $1`, personal).Scan(&personalShort, &personalName); err != nil {
		t.Fatal(err)
	}
	held, heldPrefix := testsupport.SeedProjectInSpace(t, pool, stranger, personal, "e17m")
	var heldName string
	if err := pool.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, held).Scan(&heldName); err != nil {
		t.Fatal(err)
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "Neutral " + heldPrefix, "idPrefix": heldPrefix, "space": mineShort}
	res, err := handleProjectCreate(pool, projects.NewStore(pool), "members")(withCtx(t, me, "user"), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body := extractToolJSON(t, res)
	if !res.IsError || body["error"] != ErrCodePrefixTaken {
		t.Fatalf("taken in another's personal space: %v, want %s", body, ErrCodePrefixTaken)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !slices.Equal(keys, []string{"error", "message", "suggestions"}) {
		t.Errorf("body keys %v, want the contract's error, message, suggestions", keys)
	}
	sugg, _ := body["suggestions"].([]any)
	if len(sugg) < 2 || len(sugg) > 3 || slices.Contains(sugg, any(heldPrefix)) {
		t.Errorf("suggestions %v, want 2–3 free variants", sugg)
	}
	raw, _ := json.Marshal(body)
	for _, leak := range []string{personalShort, personalName, strangerName, stranger.String(),
		held.String(), heldName, "constraint", "id_prefix", "unique", "duplicate", "personal"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(leak)) {
			t.Errorf("the refusal tells %q: %s", leak, raw)
		}
	}
}
