package tools

// project_create_race_test.go — sk-674.18.
// Reference — the contract of spaces.CodeNotSpaceMember (spaces/contract.go:
// «Carries spaceId») and SC-012: REST and MCP answer one code with one body.
// The path is the race inside CreateProject: the space was chosen while the
// caller was its participant, and he left it before the creating
// transaction took the space's lock. Staged: another transaction holds the
// space's row, the creation waits for it, the holder removes the caller's
// participation and commits.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/projects"
	"skein-api/internal/testsupport"
)

func TestProjectCreate_LeftTheSpaceMeanwhileNamesTheSpace(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner, _ := seedUser(t, pool, "user")
	me, _ := seedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, owner)
	testsupport.AddSpaceMember(t, pool, space, me)
	var short string
	if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&short); err != nil {
		t.Fatal(err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	var pid int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM spaces WHERE id = $1 FOR UPDATE`, space); err != nil {
		t.Fatal(err)
	}

	prefix := "race" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	done := make(chan *mcpgo.CallToolResult, 1)
	go func() {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"name": prefix, "idPrefix": prefix, "space": short}
		res, err := handleProjectCreate(pool, projects.NewStore(pool), "members")(withCtx(t, me, "user"), req)
		if err != nil {
			t.Errorf("handler: %v", err)
		}
		done <- res
	}()

	waiting := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE wait_event_type = 'Lock' AND $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			waiting = true
			break
		}
	}
	if !waiting {
		t.Fatal("the creation did not wait for the space's row")
	}
	// The caller leaves the space; the space's row is touched, as a
	// membership operation under K-7 holds it.
	if _, err := holder.Exec(ctx, `DELETE FROM space_members WHERE space_id = $1 AND user_id = $2`, space, me); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `UPDATE spaces SET updated_at = now() WHERE id = $1`, space); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var res *mcpgo.CallToolResult
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the creation did not return")
	}
	if res == nil {
		t.Fatal("no answer")
	}
	body := extractToolJSON(t, res)
	if !res.IsError || body["error"] != "not_space_member" {
		if !res.IsError {
			_, _ = pool.Exec(ctx, `DELETE FROM project_members WHERE project_id IN (SELECT id FROM projects WHERE id_prefix = $1)`, prefix)
			_, _ = pool.Exec(ctx, `DELETE FROM projects WHERE id_prefix = $1`, prefix)
		}
		t.Fatalf("a creator who left the space meanwhile: %v, want not_space_member", body)
	}
	if body["spaceId"] != short {
		t.Errorf("not_space_member %v does not name the space %s, as REST does", body, short)
	}
}
