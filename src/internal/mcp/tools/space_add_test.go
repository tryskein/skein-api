package tools

// space_add_test.go — sk-674.18 T024, matrix row 98 (INV-17). Reference —
// INV-17 of the spec, written before the code: through MCP nobody invites to
// a space, accepts or declines an invitation; adding to a space at once is
// for the agent of an instance admin only. Here: skein_user action=space_add
// adds as the admin's agent and refuses everyone else — the space's own
// owner and an external user claiming the admin role included — leaving
// the target outside the space. The tools/list half (no invitation tool on
// the wired server) lives in cmd/api/space_add_tools_list_test.go: only the
// production wiring knows the whole tool set.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/auth"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
)

func TestSpaceAdd_OnlyTheAdminsAgentAdds(t *testing.T) {
	pool := newTestPool(t)
	h := handleUserSpaceAdd(pool, spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll}))
	owner, _ := seedUser(t, pool, "user")
	space := testsupport.SeedSpace(t, pool, owner)
	var short string
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&short); err != nil {
		t.Fatal(err)
	}
	participant, _ := seedUser(t, pool, "user")
	testsupport.AddSpaceMember(t, pool, space, participant)
	admin, _ := seedUser(t, pool, "admin")
	extAdmin, _ := seedUser(t, pool, "user")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET is_external = true WHERE id = $1`, extAdmin); err != nil {
		t.Fatal(err)
	}

	call := func(ctx context.Context, target uuid.UUID) *mcpgo.CallToolResult {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"userId": target.String(), "space": short}
		res, err := h(ctx, req)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		return res
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"the space owner", withCtx(t, owner, "user")},
		{"a participant", withCtx(t, participant, "user")},
		// The schema forbids a stored external admin (users_external_not_admin);
		// the context claims the role anyway, as a stale session could.
		{"an external user claiming the admin role", auth.WithIsExternal(withCtx(t, extAdmin, "admin"), true)},
	} {
		t.Run(c.name+" is refused", func(t *testing.T) {
			target, _ := seedUser(t, pool, "user")
			res := call(c.ctx, target)
			if !res.IsError {
				t.Fatalf("added: %v", extractToolJSON(t, res))
			}
			if body := extractToolJSON(t, res); body["error"] != ErrCodeNotAdmin {
				t.Errorf("refusal %v, want %s", body, ErrCodeNotAdmin)
			}
			if spaceMember(t, pool, space, target) {
				t.Error("the refused add left the target in the space")
			}
		})
	}

	t.Run("the admin's agent adds at once", func(t *testing.T) {
		target, _ := seedUser(t, pool, "user")
		res := call(withCtx(t, admin, "admin"), target)
		if res.IsError {
			t.Fatalf("refused: %v", extractToolJSON(t, res))
		}
		if !spaceMember(t, pool, space, target) {
			t.Error("the admin's add did not make the target a participant")
		}
		var invitations int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM space_invitations WHERE space_id = $1`, space).Scan(&invitations); err != nil {
			t.Fatal(err)
		}
		if invitations != 0 {
			t.Errorf("the add went through %d invitations, want a direct membership", invitations)
		}
	})
}

func spaceMember(t *testing.T, pool *pgxpool.Pool, space, user uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM space_members WHERE space_id = $1 AND user_id = $2)`, space, user).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}
