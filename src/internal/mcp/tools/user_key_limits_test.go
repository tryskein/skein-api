package tools

// user_key_limits_test.go — matrix row 34 (INV-12, INV-3, SC-016, OST-2-9).
// Reference — the spec, written before the code ("MCP changes" (2), INV-12,
// INV-3): with an API key, skein_user update never changes an existing
// user's password, never assigns or removes the instance admin role and
// never deactivates or reactivates an admin; skein_user create never
// creates an admin, and
// the initial password of a new user is allowed; deactivating an ordinary
// user revokes his keys, and reactivating him does not bring them back.
// Each refusal is asserted together with the unchanged stored state.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

type userRow struct {
	role        string
	hash        string
	deactivated bool
}

func readUserRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) userRow {
	t.Helper()
	var r userRow
	var hash *string
	if err := pool.QueryRow(context.Background(),
		`SELECT role, password_hash, deactivated_at IS NOT NULL FROM users WHERE id = $1`, id).
		Scan(&r.role, &hash, &r.deactivated); err != nil {
		t.Fatalf("read user %s: %v", id, err)
	}
	if hash != nil {
		r.hash = *hash
	}
	return r
}

func callUser(t *testing.T, h func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error),
	ctx context.Context, args map[string]any) (bool, map[string]any) {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return res.IsError, extractToolJSON(t, res)
}

func TestUserKeyLimits(t *testing.T) {
	pool := newTestPool(t)
	adminID, _ := seedUser(t, pool, "admin")
	store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	update := handleUserUpdate(store)
	create := handleUserCreate(store)
	ctx := withCtx(t, adminID, "admin") // a call by API key

	refused := func(t *testing.T, isErr bool, body map[string]any) {
		t.Helper()
		if !isErr || body["error"] != ErrCodeForbidden {
			t.Errorf("answer = %v, want %s", body, ErrCodeForbidden)
		}
	}

	t.Run("update does not change an existing user's password", func(t *testing.T) {
		target, _ := seedUser(t, pool, "user")
		before := readUserRow(t, pool, target)
		isErr, body := callUser(t, update, ctx, map[string]any{"userId": target.String(), "password": "a-new-long-password-1"})
		refused(t, isErr, body)
		if after := readUserRow(t, pool, target); after.hash != before.hash {
			t.Errorf("password hash changed after the refusal")
		}
	})

	t.Run("update does not assign the admin role", func(t *testing.T) {
		target, _ := seedUser(t, pool, "user")
		isErr, body := callUser(t, update, ctx, map[string]any{"userId": target.String(), "role": "admin"})
		refused(t, isErr, body)
		if got := readUserRow(t, pool, target).role; got != "user" {
			t.Errorf("role = %q after the refusal, want user", got)
		}
	})

	t.Run("update does not remove the admin role", func(t *testing.T) {
		target, _ := seedUser(t, pool, "admin")
		isErr, body := callUser(t, update, ctx, map[string]any{"userId": target.String(), "role": "user"})
		refused(t, isErr, body)
		if got := readUserRow(t, pool, target).role; got != "admin" {
			t.Errorf("role = %q after the refusal, want admin", got)
		}
	})

	t.Run("update does not deactivate an admin", func(t *testing.T) {
		target, _ := seedUser(t, pool, "admin")
		isErr, body := callUser(t, update, ctx, map[string]any{"userId": target.String(), "deactivated": true})
		refused(t, isErr, body)
		if readUserRow(t, pool, target).deactivated {
			t.Errorf("the admin is deactivated after the refusal")
		}
	})

	t.Run("update does not reactivate an admin", func(t *testing.T) {
		target, _ := seedUser(t, pool, "admin")
		if _, err := pool.Exec(context.Background(), `UPDATE users SET deactivated_at = now() WHERE id = $1`, target); err != nil {
			t.Fatalf("deactivate the admin in the database: %v", err)
		}
		isErr, body := callUser(t, update, ctx, map[string]any{"userId": target.String(), "deactivated": false})
		refused(t, isErr, body)
		if !readUserRow(t, pool, target).deactivated {
			t.Errorf("the admin is active again after the refusal")
		}
	})

	t.Run("create does not create an admin", func(t *testing.T) {
		email := "keyadm" + uuid.NewString()[:8] + "@keylimits.local"
		isErr, body := callUser(t, create, ctx, map[string]any{
			"email": email, "name": "Key Admin", "password": "passw0rd!long", "role": "admin"})
		refused(t, isErr, body)
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE lower(email) = lower($1)`, email).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("%d accounts written for a refused admin, want 0", n)
		}
	})

	t.Run("create sets the initial password of a new user", func(t *testing.T) {
		email := "keyusr" + uuid.NewString()[:8] + "@keylimits.local"
		t.Cleanup(func() { forgetCreatedUser(pool, email) })
		isErr, body := callUser(t, create, ctx, map[string]any{
			"email": email, "name": "Key User", "password": "passw0rd!long", "role": "user"})
		if isErr {
			t.Fatalf("create of an ordinary user with an initial password: %v", body)
		}
		var id uuid.UUID
		if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE lower(email) = lower($1)`, email).Scan(&id); err != nil {
			t.Fatalf("read created user: %v", err)
		}
		row := readUserRow(t, pool, id)
		if row.role != "user" || row.hash == "" {
			t.Errorf("created user role %q, password set %v; want user with a password", row.role, row.hash != "")
		}
	})

	t.Run("deactivating an ordinary user revokes his keys; reactivating does not restore them", func(t *testing.T) {
		target, _ := seedUser(t, pool, "user")
		keys := apikeys.New(pool)
		_, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		k, err := keys.Create(context.Background(), apikeys.NewKey{
			UserID: target, Label: "key limits", Prefix: prefix, Hash: hash,
			Grant: apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}},
		}, testsupport.KeyTTLPolicy)
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, k.ID) })

		if isErr, body := callUser(t, update, ctx, map[string]any{"userId": target.String(), "deactivated": true}); isErr {
			t.Fatalf("deactivate an ordinary user: %v", body)
		}
		st, err := keys.Status(context.Background(), k.ID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !st.Revoked() {
			t.Fatalf("the key of a deactivated user is not revoked")
		}
		if isErr, body := callUser(t, update, ctx, map[string]any{"userId": target.String(), "deactivated": false}); isErr {
			t.Fatalf("reactivate: %v", body)
		}
		st, err = keys.Status(context.Background(), k.ID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !st.Revoked() {
			t.Errorf("the key is not revoked after the reactivation")
		}
	})
}

// forgetCreatedUser removes an account the create subtest wrote, with its
// "Personal" space.
func forgetCreatedUser(pool *pgxpool.Pool, email string) {
	c := context.Background()
	for _, q := range []string{
		`DELETE FROM space_members WHERE space_id IN (SELECT s.id FROM spaces s JOIN users u ON u.id = s.owner_id WHERE lower(u.email) = lower($1))`,
		`DELETE FROM space_members WHERE user_id IN (SELECT id FROM users WHERE lower(email) = lower($1))`,
		`DELETE FROM spaces WHERE owner_id IN (SELECT id FROM users WHERE lower(email) = lower($1))`,
		`DELETE FROM users WHERE lower(email) = lower($1)`,
	} {
		_, _ = pool.Exec(c, q, email)
	}
}
