package tools

// whoami_test.go — sk-674.18 T024, matrix row 99 (US-12). Reference — US-12
// of the spec, written before the code: skein_whoami gives the user's
// spaces; an admin sees the non-personal spaces as the admin (without the
// right to create projects there) and never another user's "Personal". The
// short id of each entry is the `space` of skein_project_create.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/agents"
	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

type whoamiSpace struct {
	role      string
	canCreate bool
}

// whoamiSpaces calls skein_whoami as user and returns its spaces by short id.
func whoamiSpaces(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, role string) map[string]whoamiSpace {
	t.Helper()
	res, err := handleWhoami(pool)(withCtx(t, user, role), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	body := extractToolJSON(t, res)
	if res.IsError {
		t.Fatalf("whoami refused: %v", body)
	}
	list, ok := body["spaces"].([]any)
	if !ok {
		t.Fatalf("whoami has no spaces list: %v", body)
	}
	out := map[string]whoamiSpace{}
	for _, raw := range list {
		s, _ := raw.(map[string]any)
		id, _ := s["id"].(string)
		r, _ := s["role"].(string)
		c, _ := s["canCreateProject"].(bool)
		out[id] = whoamiSpace{role: r, canCreate: c}
	}
	return out
}

func TestWhoami_SpacesOfTheUser(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	short := func(space uuid.UUID) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&s); err != nil {
			t.Fatal(err)
		}
		// The column order is personal, common, then by name: a name that
		// sorts first keeps the space inside whoami's cap on a busy database.
		if _, err := pool.Exec(ctx, `UPDATE spaces SET name = '0 row99 ' || short_id WHERE id = $1 AND kind = 'regular'`, space); err != nil {
			t.Fatal(err)
		}
		return s
	}
	user, _ := seedUser(t, pool, "user")
	other, _ := seedUser(t, pool, "user")
	admin, _ := seedUser(t, pool, "admin")
	owned := short(testsupport.SeedSpace(t, pool, user))
	joinedID := testsupport.SeedSpace(t, pool, other)
	testsupport.AddSpaceMember(t, pool, joinedID, user)
	joined := short(joinedID)
	foreign := short(testsupport.SeedSpace(t, pool, other))
	personal := short(testsupport.SeedSpaceOfKind(t, pool, user, "personal"))
	othersPersonal := short(testsupport.SeedSpaceOfKind(t, pool, other, "personal"))
	adminsPersonal := short(testsupport.SeedSpaceOfKind(t, pool, admin, "personal"))

	t.Run("a user", func(t *testing.T) {
		got := whoamiSpaces(t, pool, user, "user")
		for id, want := range map[string]whoamiSpace{
			owned:    {"owner", true},
			joined:   {"member", true},
			personal: {"owner", true},
		} {
			if g, ok := got[id]; !ok || g != want {
				t.Errorf("space %s: %+v (listed %v), want %+v", id, g, ok, want)
			}
		}
		for _, id := range []string{foreign, othersPersonal, adminsPersonal} {
			if _, ok := got[id]; ok {
				t.Errorf("space %s of others is listed", id)
			}
		}
	})

	t.Run("an admin", func(t *testing.T) {
		got := whoamiSpaces(t, pool, admin, "admin")
		for _, id := range []string{owned, joined, foreign} {
			if g, ok := got[id]; !ok || g != (whoamiSpace{"admin", false}) {
				t.Errorf("space %s: %+v (listed %v), want admin access without project creation", id, g, ok)
			}
		}
		if g, ok := got[adminsPersonal]; !ok || g != (whoamiSpace{"owner", true}) {
			t.Errorf("the admin's own Personal: %+v (listed %v)", g, ok)
		}
		for _, id := range []string{personal, othersPersonal} {
			if _, ok := got[id]; ok {
				t.Errorf("another user's Personal %s is listed to the admin", id)
			}
		}
	})
}

// TestWhoami_RESTExecutorRowIsTheOwnersSession — matrix row 47 (D-6, class
// of agents readers). Reference — the plan's decision D-6: skein_whoami
// treats the agents row of a REST executor as a session of its owner — the
// row is read under the owner's id and its stamps are the owner's session
// stamps; the same session string under another user is not that row.
func TestWhoami_RESTExecutorRowIsTheOwnersSession(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	owner, _ := seedUser(t, pool, "user")
	stranger, _ := seedUser(t, pool, "user")
	k, err := apikeys.New(pool).Create(ctx, apikeys.NewKey{
		UserID: owner, Label: "rest executor",
		Prefix: "ska_t016" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8], Hash: "fake-bcrypt-hash-for-test",
		Grant: apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}},
	}, testsupport.KeyTTLPolicy)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	holder := agents.RESTHolder(k.ID, "worker-1")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE session_id = $1`, holder)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, k.ID)
	})
	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `
INSERT INTO agents (user_id, session_id, first_seen_at, last_seen_at, api_key_id, worker_id)
VALUES ($1, $2, now() - interval '1 hour', now() - interval '5 minutes', $3, 'worker-1')
RETURNING last_seen_at`, owner, holder, k.ID).Scan(&lastSeen); err != nil {
		t.Fatalf("seed the REST executor row: %v", err)
	}

	agentOf := func(user uuid.UUID) map[string]any {
		t.Helper()
		res, err := handleWhoami(pool)(auth.WithSessionID(withCtx(t, user, "user"), holder), mcpgo.CallToolRequest{})
		if err != nil {
			t.Fatalf("whoami: %v", err)
		}
		body := extractToolJSON(t, res)
		if res.IsError {
			t.Fatalf("whoami refused: %v", body)
		}
		a, _ := body["agent"].(map[string]any)
		return a
	}

	a := agentOf(owner)
	if a["sessionId"] != holder {
		t.Errorf("owner's agent.sessionId = %v, want %s", a["sessionId"], holder)
	}
	if got, _ := a["lastSeenAt"].(string); got == "" || !strings.HasPrefix(got, lastSeen.UTC().Format("2006-01-02T15:04:05")) {
		t.Errorf("owner's agent.lastSeenAt = %v, want the row's %s", a["lastSeenAt"], lastSeen.UTC())
	}

	b := agentOf(stranger)
	if got, _ := b["lastSeenAt"].(string); got != "" && strings.HasPrefix(got, lastSeen.UTC().Format("2006-01-02T15:04:05")) {
		t.Errorf("another user sees the owner's executor stamps: %v", b)
	}
}
