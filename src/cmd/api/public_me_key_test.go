package main

// public_me_key_test.go — GET /api/v1/me describes the API key of the
// request: the rights it holds (by the names insufficient_scope requires),
// its scope with the projects' ids and prefixes or the spaces' short ids,
// and its lifetime — never its secret or hash. The reference is the key
// model: read ⊂ write ⊂ manage, admin an independent flag that applies only
// to an internal instance admin, admin only with scope all.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

// meKeyOf reads the key member of GET /api/v1/me with key.
func (w *classWorld) meKeyOf(t *testing.T, key string) map[string]any {
	t.Helper()
	a := w.do(key, opCall{http.MethodGet, "/api/v1/me", nil, nil}, "", nil)
	if a.status != http.StatusOK {
		t.Fatalf("GET /me: %d %s", a.status, a.body)
	}
	k, ok := a.object(t)["key"].(map[string]any)
	if !ok {
		t.Fatalf("GET /me has no key object: %s", a.body)
	}
	return k
}

// keyIDOf is the id of the key minted as plain.
func (w *classWorld) keyIDOf(t *testing.T, plain string) string {
	t.Helper()
	var id string
	if err := w.pool.QueryRow(context.Background(), `SELECT id::text FROM api_keys WHERE prefix = $1`, auth.ExtractPrefix(plain)).Scan(&id); err != nil {
		t.Fatalf("read key id: %v", err)
	}
	return id
}

func strs(t *testing.T, v any) []string {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not an array", v)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			t.Fatalf("%v is not a string", it)
		}
		out = append(out, s)
	}
	return out
}

func TestPublicMe_Key(t *testing.T) {
	w := newClassWorld(t)

	t.Run("read key over listed projects", func(t *testing.T) {
		plain := w.key(w.owner, apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeProjects, Projects: []uuid.UUID{w.p}}})
		k := w.meKeyOf(t, plain)
		if got := strs(t, k["rights"]); !slices.Equal(got, []string{"read"}) {
			t.Errorf("rights %v, want [read]", got)
		}
		if k["id"] != w.keyIDOf(t, plain) {
			t.Errorf("id %v, want the id of the key", k["id"])
		}
		if k["prefix"] != auth.ExtractPrefix(plain) {
			t.Errorf("prefix %v, want %s", k["prefix"], auth.ExtractPrefix(plain))
		}
		if k["expiresAt"] == nil || k["createdAt"] == nil {
			t.Errorf("createdAt %v expiresAt %v, want both", k["createdAt"], k["expiresAt"])
		}
		scope, _ := k["scope"].(map[string]any)
		if scope["mode"] != "projects" {
			t.Fatalf("scope %v, want mode projects", scope)
		}
		projects, _ := scope["projects"].([]any)
		if len(projects) != 1 {
			t.Fatalf("scope projects %v, want exactly %s", projects, w.p)
		}
		p, _ := projects[0].(map[string]any)
		if p["id"] != w.p.String() || p["prefix"] != w.pPrefix {
			t.Errorf("scope project %v, want id %s prefix %s", p, w.p, w.pPrefix)
		}
		if spaces, _ := scope["spaces"].([]any); len(spaces) != 0 {
			t.Errorf("scope spaces %v, want none", spaces)
		}
		// The key object never carries the secret or its hash: its members
		// are exactly the documented ones.
		var members []string
		for m := range k {
			members = append(members, m)
		}
		sort.Strings(members)
		want := []string{"createdAt", "expiresAt", "id", "label", "lastUsedAt", "prefix", "rights", "scope", "unavailableRights"}
		if !slices.Equal(members, want) {
			t.Errorf("key members %v, want %v", members, want)
		}
		raw, err := json.Marshal(k)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), plain) {
			t.Errorf("the key object carries the plain key")
		}
	})

	t.Run("write key over a space", func(t *testing.T) {
		plain := w.key(w.owner, apikeys.Grant{Level: apikeys.LevelWrite, Scope: apikeys.Scope{Mode: apikeys.ScopeSpaces, Spaces: []uuid.UUID{w.space}}})
		k := w.meKeyOf(t, plain)
		if got := strs(t, k["rights"]); !slices.Equal(got, []string{"read", "write"}) {
			t.Errorf("rights %v, want [read write]", got)
		}
		scope, _ := k["scope"].(map[string]any)
		spaces, _ := scope["spaces"].([]any)
		if scope["mode"] != "spaces" || len(spaces) != 1 {
			t.Fatalf("scope %v, want mode spaces with one space", scope)
		}
		if s, _ := spaces[0].(map[string]any); s["id"] != w.spaceShort {
			t.Errorf("scope space %v, want short id %s", s, w.spaceShort)
		}
	})

	t.Run("admin key of an instance admin", func(t *testing.T) {
		plain := w.key(w.admin, apikeys.Grant{Level: apikeys.LevelManage, Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
		k := w.meKeyOf(t, plain)
		if got := strs(t, k["rights"]); !slices.Equal(got, []string{"read", "write", "manage", "admin"}) {
			t.Errorf("rights %v, want [read write manage admin]", got)
		}
		if got := strs(t, k["unavailableRights"]); len(got) != 0 {
			t.Errorf("unavailableRights %v, want none", got)
		}
		if scope, _ := k["scope"].(map[string]any); scope["mode"] != "all" {
			t.Errorf("scope %v, want mode all", scope)
		}
	})

	t.Run("admin key of an owner who is no longer an instance admin", func(t *testing.T) {
		plain := w.key(w.admin, apikeys.Grant{Level: apikeys.LevelRead, Admin: true, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
		w.exec(`UPDATE users SET role = 'user' WHERE id = $1`, w.admin)
		t.Cleanup(func() { w.exec(`UPDATE users SET role = 'admin' WHERE id = $1`, w.admin) })
		k := w.meKeyOf(t, plain)
		if got := strs(t, k["rights"]); !slices.Equal(got, []string{"read"}) {
			t.Errorf("rights %v, want [read]", got)
		}
		if got := strs(t, k["unavailableRights"]); !slices.Equal(got, []string{"admin"}) {
			t.Errorf("unavailableRights %v, want [admin]", got)
		}
	})
}

// TestPublicMe_DeletedKeyIsUnauthenticated — a key deleted after the request
// authenticated with it is answered 401 unauthenticated, not 500: the
// request no longer has a key. The request chain is left out so the request
// can carry the id of a key that does not exist, as it would after the
// deletion.
func TestPublicMe_DeletedKeyIsUnauthenticated(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "")
	deps := classDeps(pool)
	deps.Pool = pool
	srv, err := publicServer(publicConfig(true, freePort(t), ""), nil, deps, slog.New(&recordingHandler{}))
	if err != nil || srv == nil {
		t.Fatalf("publicServer = %v, %v", srv, err)
	}
	ctx := auth.WithUserID(context.Background(), owner)
	ctx = auth.WithAPIKeyID(ctx, uuid.New())
	ctx = auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelRead, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(ctx))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	if rec.Code != http.StatusUnauthorized || body["code"] != "unauthenticated" {
		t.Errorf("answered %d %s, want 401 unauthenticated", rec.Code, rec.Body.String())
	}
}
