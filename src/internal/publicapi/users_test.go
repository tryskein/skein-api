package publicapi_test

// users_test.go — the user operations of the public API (sk-878.3.1 T028,
// row 99; INV-1, INV-12, SC-016, OST-2-9). The reference is the spec: «all
// the operations on users — admin; a key with admin has scope all and may
// have no level (then the operations on users only, and the ones without a
// right)»; «through a key the password of an existing user is never changed,
// the instance admin role is neither given nor taken, an admin is not
// deactivated; deactivating an ordinary user revokes his keys»; «the
// initial password of a created user is allowed»; INV-1 «a key never gives
// more than its owner can now». The reference of the parity is the MCP
// channel itself.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/publicapi"
	"skein-api/internal/testsupport"
)

// adminKey is a key with the admin permission only (no level) of owner.
func adminKey(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID) string {
	t.Helper()
	return rigKey(t, pool, owner, apikeys.Grant{Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
}

// createdUsers removes the users created through the APIs whose email
// carries sfx, with their personal spaces, after the test.
func createdUsers(t *testing.T, pool *pgxpool.Pool, sfx string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		like := "%" + sfx + "%"
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE user_id IN (SELECT id FROM users WHERE email LIKE $1)`, like)
		_, _ = pool.Exec(ctx, `DELETE FROM space_members WHERE space_id IN (SELECT s.id FROM spaces s JOIN users u ON u.id = s.owner_id WHERE u.email LIKE $1)`, like)
		_, _ = pool.Exec(ctx, `DELETE FROM spaces WHERE owner_id IN (SELECT id FROM users WHERE email LIKE $1)`, like)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE email LIKE $1`, like)
	})
}

// userRow is the stored role, deactivation and password hash of a user.
func userRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (role string, deactivated bool, hash string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT role, deactivated_at IS NOT NULL, coalesce(password_hash, '') FROM users WHERE id = $1`, id).Scan(&role, &deactivated, &hash); err != nil {
		t.Fatalf("read user %s: %v", id, err)
	}
	return role, deactivated, hash
}

// TestUsers_ParityWithMCP — row 99 (INV-4). The user operations asked with
// the same key on REST and on MCP give the same code for the success and for
// the refusals; a created user with an initial password is created. Red when
// a code differs from MCP.
func TestUsers_ParityWithMCP(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	admin, _ := testsupport.SeedUser(t, pool, "admin")
	key := adminKey(t, pool, admin)
	sfx := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	createdUsers(t, pool, sfx)
	_, short := seedOwnSpace(t, pool, admin)
	email := func(who string) string { return who + sfx + "@public-users.test" }

	created := rg.rest(key, http.MethodPost, "/api/v1/users", map[string]any{"email": email("r"), "name": "R", "password": "initial-pass", "role": "user"}, nil)
	wantOK(t, "createUser with an initial password", created, http.StatusCreated)
	ref := rg.tool(key, "skein_user", map[string]any{"action": "create", "email": email("m"), "name": "M", "password": "initial-pass", "role": "user"})
	if ref.code != "" {
		t.Fatalf("skein_user create: %s", ref.text)
	}
	rID, mID := created.json(t)["id"].(string), ref.json(t)["user"].(map[string]any)["id"].(string)

	for _, c := range []struct {
		name string
		rest func() restCall
		mcp  func() toolAnswer
		want string
	}{
		{"createUser with a taken email", func() restCall {
			return rg.rest(key, http.MethodPost, "/api/v1/users", map[string]any{"email": email("r"), "name": "X", "password": "initial-pass", "role": "user"}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_user", map[string]any{"action": "create", "email": email("m"), "name": "X", "password": "initial-pass", "role": "user"})
		}, "email_taken"},
		{"updateUser name", func() restCall {
			return rg.rest(key, http.MethodPatch, "/api/v1/users/"+rID, map[string]any{"name": "Renamed"}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_user", map[string]any{"action": "update", "userId": mID, "name": "Renamed"})
		}, ""},
		{"updateUser of a missing user", func() restCall {
			return rg.rest(key, http.MethodPatch, "/api/v1/users/"+uuid.NewString(), map[string]any{"name": "X"}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_user", map[string]any{"action": "update", "userId": uuid.NewString(), "name": "X"})
		}, "user_not_found"},
		{"updateUser to the admin role", func() restCall {
			return rg.rest(key, http.MethodPatch, "/api/v1/users/"+rID, map[string]any{"role": "admin"}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_user", map[string]any{"action": "update", "userId": mID, "role": "admin"})
		}, "forbidden"},
		{"addSpaceMember", func() restCall {
			return rg.rest(key, http.MethodPost, "/api/v1/spaces/"+short+"/members", map[string]any{"userId": rID}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_user", map[string]any{"action": "space_add", "space": short, "userId": mID})
		}, ""},
		{"addSpaceMember again", func() restCall {
			return rg.rest(key, http.MethodPost, "/api/v1/spaces/"+short+"/members", map[string]any{"userId": rID}, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_user", map[string]any{"action": "space_add", "space": short, "userId": mID})
		}, "already_member"},
		{"listUsers", func() restCall {
			return rg.rest(key, http.MethodGet, "/api/v1/users?search="+sfx, nil, nil)
		}, func() toolAnswer {
			return rg.tool(key, "skein_user", map[string]any{"action": "list", "search": sfx})
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rest, mcp := c.rest(), c.mcp()
			parity(t, c.name, rest, mcp, c.want)
			if c.want == "" && (rest.status < 200 || rest.status > 299) {
				t.Errorf("REST answered %d %s", rest.status, rest.body)
			}
		})
	}
	if role, _, _ := userRow(t, pool, uuid.MustParse(rID)); role != "user" {
		t.Errorf("the refused promotion left the role %q", role)
	}
}

// TestUsers_KeyWithAdminOnly — row 99 (SC-016, [SPEC-CONCERN] admin-only by
// the rights table). A key with only admin reaches the operations on users
// and the operations without a right (getMe, getOpenApi); every other
// operation of the route table answers 403 insufficient_scope. Red when an
// admin-only key passes an operation that needs a level.
func TestUsers_KeyWithAdminOnly(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	admin, _ := testsupport.SeedUser(t, pool, "admin")
	key := adminKey(t, pool, admin)

	if c := rg.rest(key, http.MethodGet, "/api/v1/me", nil, nil); c.status != http.StatusOK {
		t.Errorf("getMe: %d %s", c.status, c.body)
	}
	if c := rg.rest("", http.MethodGet, "/api/v1/openapi.json", nil, nil); c.status != http.StatusOK {
		t.Errorf("getOpenApi: %d", c.status)
	}
	if c := rg.rest(key, http.MethodGet, "/api/v1/users", nil, nil); c.status != http.StatusOK {
		t.Errorf("listUsers: %d %s", c.status, c.body)
	}
	fill := strings.NewReplacer("{project}", uuid.NewString(), "{issue}", "nope-1", "{comment}", uuid.NewString(),
		"{attachment}", uuid.NewString(), "{notification}", "1", "{filter}", uuid.NewString(), "{reminder}", uuid.NewString(),
		"{user}", uuid.NewString(), "{component}", uuid.NewString(), "{hash}", "abcdef1", "{toIssue}", "nope-2",
		"{type}", "related", "{space}", "aaaaaaaa")
	for _, rt := range publicapi.ProductRoutes(publicapi.Deps{}) {
		switch rt.Key.Right.String() {
		case "none", "admin":
			continue
		}
		t.Run(rt.OperationID, func(t *testing.T) {
			var body any
			if rt.Method != http.MethodGet {
				body = map[string]any{}
			}
			c := rg.rest(key, rt.Method, fill.Replace(rt.Path), body, nil)
			if c.status != http.StatusForbidden || c.code() != "insufficient_scope" {
				t.Errorf("%s %s with an admin-only key: %d %s, want 403 insufficient_scope", rt.Method, rt.Path, c.status, c.body)
			}
		})
	}
}

// TestUsers_WhatAKeyCannotChange — row 99 (INV-12, OST-2-9, INV-1). Through
// the key: updateUser with password is 400 (no such field) and the hash
// stays; the admin role is neither given nor taken and an admin is not
// deactivated (refused, the row unchanged); deactivating an ordinary user
// revokes his keys; the admin permission of a key whose owner is no longer
// an admin passes nothing (403). Red when a key changes a password or the
// admin role.
func TestUsers_WhatAKeyCannotChange(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	rg := newResourceRig(t, pool)
	admin, _ := testsupport.SeedUser(t, pool, "admin")
	otherAdmin, _ := testsupport.SeedUser(t, pool, "admin")
	plain, _ := testsupport.SeedUser(t, pool, "")
	key := adminKey(t, pool, admin)
	path := func(id uuid.UUID) string { return "/api/v1/users/" + id.String() }

	_, _, hashBefore := userRow(t, pool, plain)
	if c := rg.rest(key, http.MethodPatch, path(plain), map[string]any{"password": "a-new-password"}, nil); c.status != http.StatusBadRequest || c.code() != "validation_error" {
		t.Errorf("updateUser with password: %d %s, want 400 validation_error", c.status, c.body)
	}
	if _, _, hash := userRow(t, pool, plain); hash != hashBefore {
		t.Errorf("the password hash changed through the key")
	}

	for _, c := range []struct {
		name string
		id   uuid.UUID
		body map[string]any
	}{
		{"give the admin role", plain, map[string]any{"role": "admin"}},
		{"take the admin role", otherAdmin, map[string]any{"role": "user"}},
		{"deactivate an admin", otherAdmin, map[string]any{"deactivated": true}},
	} {
		roleBefore, deactBefore, _ := userRow(t, pool, c.id)
		got := rg.rest(key, http.MethodPatch, path(c.id), c.body, nil)
		if got.status != http.StatusForbidden || got.code() != "forbidden" {
			t.Errorf("%s: %d %s, want 403 forbidden", c.name, got.status, got.body)
		}
		if role, deact, _ := userRow(t, pool, c.id); role != roleBefore || deact != deactBefore {
			t.Errorf("%s: the user is now %s/deactivated=%v, was %s/%v", c.name, role, deact, roleBefore, deactBefore)
		}
	}

	victimKey := rigKey(t, pool, plain, apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
	if c := rg.rest(victimKey, http.MethodGet, "/api/v1/me", nil, nil); c.status != http.StatusOK {
		t.Fatalf("the ordinary user's key before: %d %s", c.status, c.body)
	}
	if c := rg.rest(key, http.MethodPatch, path(plain), map[string]any{"deactivated": true}, nil); c.status != http.StatusOK {
		t.Fatalf("deactivate an ordinary user: %d %s", c.status, c.body)
	}
	var live int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`, plain).Scan(&live); err != nil || live != 0 {
		t.Errorf("unrevoked keys of the deactivated user: %d (%v), want 0", live, err)
	}
	if c := rg.rest(victimKey, http.MethodGet, "/api/v1/me", nil, nil); c.status != http.StatusUnauthorized {
		t.Errorf("the deactivated user's key after: %d, want 401", c.status)
	}

	// INV-1: the owner of an admin key who is no longer an admin.
	demoted, _ := testsupport.SeedUser(t, pool, "admin")
	demotedKey := adminKey(t, pool, demoted)
	if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'user' WHERE id = $1`, demoted); err != nil {
		t.Fatal(err)
	}
	if c := rg.rest(demotedKey, http.MethodGet, "/api/v1/users", nil, nil); c.status != http.StatusForbidden {
		t.Errorf("listUsers by the admin key of a demoted owner: %d %s, want 403", c.status, c.body)
	}
}
