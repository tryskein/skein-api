package handler

// apikeys_test.go — test matrix rows 12, 13, 14, 15 and 135 (US-10, SC-010,
// SC-023, the cabinet contract of /ui/v1/me/api-keys*). Reference — the
// spec, written before the code:
//
//   - a key is created with permissions, scope and expiry (201); its secret
//     is in that response only, never in the list; a list row carries the
//     prefix, creation time, expiry, last use, status with its reason,
//     permissions and scope;
//   - a page loaded before grants existed sends only a label: 400
//     validation_error with a machine marker of the stale client, and no key
//     is created (the text is not asserted — TEST §5);
//   - PATCH narrows, a widening is refused and changes nothing; reissue gives
//     a new key and the old one stays active; a revoked key is not
//     reissued; a key with admin whose owner stopped being an admin shows the
//     difference "granted by the key / unavailable to the owner";
//   - settings tell apart a disabled public API, an enabled one without an
//     address and one with an address, and give the lifetime policy;
//   - DELETE revokes the caller's own active key (204, then inactive /
//     revoked); a repeat, another user's key and an unknown id are 404, and
//     no other key changes.
//
// A narrowing to a project the owner does not see answers the same as one to
// a missing project, and leaves the key unchanged (review C2-01).
//
// Spaces of a key scope are given and returned by their short id, as on the
// rest of the surface; projects by UUID (spec Changelog 2026-11-10, US-10).
// A short id of a space the owner is not in, an unknown one and a space UUID
// in its place are one refusal, on create and on narrowing alike.
//
// The handler is mounted on the same paths as in buildRouter, behind a stand-in
// for the BFF identity middleware; that the routes exist only behind the BFF
// identity is row 16 (cmd/api/apikeys_routes_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/testsupport"
)

const keysTestPublicURL = "https://api.skein.test"

func keysTestHandler(pool *pgxpool.Pool, enabled bool, url string) *APIKeysHandler {
	return NewAPIKeysHandler(pool, auth.MinBcryptCost, APIKeysSettings{
		TTL:              testsupport.KeyTTLPolicy,
		PublicAPIEnabled: enabled,
		PublicAPIURL:     url,
	})
}

// keysCaller serves h on the buildRouter paths for one signed-in user.
type keysCaller struct {
	t      *testing.T
	router chi.Router
}

func newKeysCaller(t *testing.T, h *APIKeysHandler, user uuid.UUID, role string) keysCaller {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.WithUserID(req.Context(), user)
			ctx = auth.WithUserRole(ctx, role)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/ui/v1/me/api-keys", func(r chi.Router) {
		r.Get("/", h.List)
		r.Post("/", h.Create)
		r.Get("/settings", h.Settings)
		r.Patch("/{id}", h.Narrow)
		r.Delete("/{id}", h.Revoke)
		r.Post("/{id}/reissue", h.Reissue)
	})
	return keysCaller{t: t, router: r}
}

// do sends the request and returns the status and the raw body.
func (c keysCaller) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/ui/v1/me/api-keys"+path, rd)
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func decodeKeys[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func (c keysCaller) create(body map[string]any) createAPIKeyResponse {
	c.t.Helper()
	code, raw := c.do(http.MethodPost, "", body)
	if code != http.StatusCreated {
		c.t.Fatalf("POST: %d %s, want 201", code, raw)
	}
	return decodeKeys[createAPIKeyResponse](c.t, raw)
}

func (c keysCaller) list() []apiKeyView {
	c.t.Helper()
	code, raw := c.do(http.MethodGet, "", nil)
	if code != http.StatusOK {
		c.t.Fatalf("GET list: %d %s", code, raw)
	}
	return decodeKeys[[]apiKeyView](c.t, raw)
}

func (c keysCaller) row(id string) apiKeyView {
	c.t.Helper()
	for _, v := range c.list() {
		if v.ID == id {
			return v
		}
	}
	c.t.Fatalf("key %s is not in the list", id)
	return apiKeyView{}
}

func keyBody(label, level string, admin bool, scope map[string]any, expires time.Time) map[string]any {
	var lvl any
	if level != "" {
		lvl = level
	}
	return map[string]any{
		"label":       label,
		"permissions": map[string]any{"level": lvl, "admin": admin},
		"scope":       scope,
		"expiresAt":   expires.Format(time.RFC3339),
	}
}

var scopeAll = map[string]any{"mode": "all"}

func problemCode(t *testing.T, raw []byte) (string, map[string]any) {
	t.Helper()
	body := decodeKeys[map[string]any](t, raw)
	code, _ := body["error"].(string)
	details, _ := body["details"].(map[string]any)
	return code, details
}

func keyCount(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAPIKeysCabinet_CreateAndList(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	project, _ := testsupport.SeedProject(t, pool, user, "akc")
	c := newKeysCaller(t, keysTestHandler(pool, true, keysTestPublicURL), user, "user")

	expires := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	created := c.create(keyBody("ci bot", "write", false,
		map[string]any{"mode": "projects", "projects": []string{project.String()}}, expires))
	if created.PlainKey == "" || !auth.IsAPIKeyShape(created.PlainKey) {
		t.Fatalf("plainKey %q, want a key", created.PlainKey)
	}

	code, raw := c.do(http.MethodGet, "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET list: %d", code)
	}
	if bytes.Contains(raw, []byte(created.PlainKey)) || bytes.Contains(raw, []byte(`"plainKey"`)) {
		t.Fatalf("the list carries the secret: %s", raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("list %s, want an array of one row", raw)
	}
	for _, f := range []string{"prefix", "createdAt", "expiresAt", "lastUsedAt", "status", "statusReason", "permissions", "scope"} {
		if _, ok := rows[0][f]; !ok {
			t.Errorf("list row has no %s: %v", f, rows[0])
		}
	}
	row := c.row(created.ID)
	if row.Prefix == "" || created.PlainKey[:len(row.Prefix)] != row.Prefix {
		t.Errorf("prefix %q, want the start of the secret", row.Prefix)
	}
	if row.ExpiresAt == nil || !row.ExpiresAt.Equal(expires) {
		t.Errorf("expiresAt %v, want %v", row.ExpiresAt, expires)
	}
	if row.LastUsedAt != nil {
		t.Errorf("lastUsedAt %v, want null for an unused key", row.LastUsedAt)
	}
	if row.Status != "active" || row.StatusReason != nil {
		t.Errorf("status %q reason %v, want active without reason", row.Status, row.StatusReason)
	}
	if row.Permissions.Level == nil || *row.Permissions.Level != "write" || row.Permissions.Admin {
		t.Errorf("permissions %+v, want write without admin", row.Permissions)
	}
	if row.Scope.Mode != "projects" || len(row.Scope.Projects) != 1 || row.Scope.Projects[0].ID != project.String() || row.Scope.Projects[0].Name == "" {
		t.Errorf("scope %+v, want projects {%s} with its name", row.Scope, project)
	}
}

// TestAPIKeysCabinet_CreateWithoutExpiry — review C2-03: expiresAt is
// optional; without it a new key lives the policy default (90 days in the
// test policy), with it the chosen expiry (TestAPIKeysCabinet_CreateAndList).
func TestAPIKeysCabinet_CreateWithoutExpiry(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	c := newKeysCaller(t, keysTestHandler(pool, true, keysTestPublicURL), user, "user")

	created := c.create(map[string]any{
		"label":       "default lifetime",
		"permissions": map[string]any{"level": "read", "admin": false},
		"scope":       scopeAll,
	})
	want := created.CreatedAt.Add(90 * 24 * time.Hour)
	if created.ExpiresAt == nil || created.ExpiresAt.Sub(want).Abs() > time.Second {
		t.Errorf("expiresAt %v, want createdAt %v + 90 days", created.ExpiresAt, created.CreatedAt)
	}
}

func TestAPIKeysCabinet_StaleClientBody(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	c := newKeysCaller(t, keysTestHandler(pool, true, keysTestPublicURL), user, "user")

	code, raw := c.do(http.MethodPost, "", map[string]any{"label": "old tab"})
	errCode, details := problemCode(t, raw)
	if code != http.StatusBadRequest || errCode != "validation_error" || details["staleClient"] != true {
		t.Errorf("old body: %d %s, want 400 validation_error with details.staleClient", code, raw)
	}
	if n := keyCount(t, pool, user); n != 0 {
		t.Errorf("keys after the old body = %d, want 0", n)
	}
}

func TestAPIKeysCabinet_NarrowReissueAndOwnerDemotion(t *testing.T) {
	pool := newMeTestPool(t)
	ctx := context.Background()
	user, _ := testsupport.SeedUser(t, pool, "user")
	c := newKeysCaller(t, keysTestHandler(pool, true, keysTestPublicURL), user, "user")
	expires := time.Now().UTC().Add(60 * 24 * time.Hour).Truncate(time.Second)

	t.Run("PATCH narrows; a widening is refused without changes", func(t *testing.T) {
		k := c.create(keyBody("narrow", "write", false, scopeAll, expires))
		code, raw := c.do(http.MethodPatch, "/"+k.ID, map[string]any{
			"permissions": map[string]any{"level": "read", "admin": false}, "scope": scopeAll,
		})
		if code != http.StatusOK {
			t.Fatalf("narrow to read: %d %s", code, raw)
		}
		if got := c.row(k.ID); got.Permissions.Level == nil || *got.Permissions.Level != "read" || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
			t.Errorf("after narrowing %+v expires %v, want read and the expiry kept", got.Permissions, got.ExpiresAt)
		}
		before := c.row(k.ID)
		code, raw = c.do(http.MethodPatch, "/"+k.ID, map[string]any{
			"permissions": map[string]any{"level": "manage", "admin": false}, "scope": scopeAll,
		})
		errCode, details := problemCode(t, raw)
		if code != http.StatusUnprocessableEntity || errCode != "invalid_grant" || details["rule"] != "not_narrowing" {
			t.Errorf("widening to manage: %d %s, want 422 invalid_grant not_narrowing", code, raw)
		}
		if after := c.row(k.ID); *after.Permissions.Level != *before.Permissions.Level || !after.ExpiresAt.Equal(*before.ExpiresAt) || after.Scope.Mode != before.Scope.Mode {
			t.Errorf("after a refused widening %+v, want unchanged %+v", after, before)
		}
	})

	t.Run("PATCH to a hidden or missing project is one refusal and changes nothing", func(t *testing.T) {
		stranger, _ := testsupport.SeedUser(t, pool, "user")
		hidden, _ := testsupport.SeedProject(t, pool, stranger, "akh")
		k := c.create(keyBody("narrow hidden", "write", false, scopeAll, expires))
		before := c.row(k.ID)
		for name, project := range map[string]string{"hidden project": hidden.String(), "missing project": uuid.NewString()} {
			code, raw := c.do(http.MethodPatch, "/"+k.ID, map[string]any{
				"permissions": map[string]any{"level": "write", "admin": false},
				"scope":       map[string]any{"mode": "projects", "projects": []string{project}},
			})
			errCode, details := problemCode(t, raw)
			if code != http.StatusUnprocessableEntity || errCode != "invalid_grant" || details["rule"] != "scope_unavailable" {
				t.Errorf("%s: %d %s, want 422 invalid_grant scope_unavailable", name, code, raw)
			}
		}
		if after := c.row(k.ID); after.Scope.Mode != before.Scope.Mode || len(after.Scope.Projects) != 0 {
			t.Errorf("after refused narrowing scope %+v, want unchanged %+v", after.Scope, before.Scope)
		}
	})

	t.Run("reissue gives a new key; the old one stays active", func(t *testing.T) {
		k := c.create(keyBody("reissue", "write", false, scopeAll, expires))
		code, raw := c.do(http.MethodPost, "/"+k.ID+"/reissue", nil)
		if code != http.StatusCreated {
			t.Fatalf("reissue: %d %s", code, raw)
		}
		fresh := decodeKeys[createAPIKeyResponse](t, raw)
		if fresh.ID == k.ID || fresh.PlainKey == "" || fresh.PlainKey == k.PlainKey {
			t.Errorf("reissue gave id %s key %q, want a new id and a new secret", fresh.ID, fresh.PlainKey)
		}
		if fresh.Label != "reissue" || fresh.Permissions.Level == nil || *fresh.Permissions.Level != "write" || fresh.Scope.Mode != "all" {
			t.Errorf("reissued %+v, want the old label, permissions and scope", fresh.apiKeyView)
		}
		if old := c.row(k.ID); old.Status != "active" {
			t.Errorf("old key status %q after reissue, want active", old.Status)
		}
	})

	t.Run("a revoked key is not reissued", func(t *testing.T) {
		k := c.create(keyBody("revoked", "read", false, scopeAll, expires))
		if code, raw := c.do(http.MethodDelete, "/"+k.ID, nil); code != http.StatusNoContent {
			t.Fatalf("revoke: %d %s", code, raw)
		}
		before := keyCount(t, pool, user)
		code, raw := c.do(http.MethodPost, "/"+k.ID+"/reissue", nil)
		if errCode, _ := problemCode(t, raw); code != http.StatusUnprocessableEntity || errCode != "key_revoked" {
			t.Errorf("reissue of a revoked key: %d %s, want 422 key_revoked", code, raw)
		}
		if after := keyCount(t, pool, user); after != before {
			t.Errorf("keys %d → %d, want no new key", before, after)
		}
	})

	t.Run("an admin key of a demoted owner shows the admin permission as unavailable", func(t *testing.T) {
		admin, _ := testsupport.SeedUser(t, pool, "admin")
		ac := newKeysCaller(t, keysTestHandler(pool, true, keysTestPublicURL), admin, "admin")
		k := ac.create(keyBody("admin key", "", true, scopeAll, expires))
		if got := ac.row(k.ID); !got.Permissions.Admin || len(got.UnavailablePermissions) != 0 {
			t.Fatalf("before demotion %+v unavailable %v, want admin and nothing unavailable", got.Permissions, got.UnavailablePermissions)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET role = 'user' WHERE id = $1`, admin); err != nil {
			t.Fatal(err)
		}
		demoted := newKeysCaller(t, keysTestHandler(pool, true, keysTestPublicURL), admin, "user")
		got := demoted.row(k.ID)
		if !got.Permissions.Admin || !slices.Equal(got.UnavailablePermissions, []string{"admin"}) {
			t.Errorf("after demotion %+v unavailable %v, want admin granted by the key and unavailable to the owner", got.Permissions, got.UnavailablePermissions)
		}
	})
}

func TestAPIKeysCabinet_Settings(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	cases := []struct {
		name    string
		enabled bool
		url     string
		wantURL *string
		wantDoc *string
	}{
		{"public API disabled", false, keysTestPublicURL, nil, nil},
		{"enabled, address not set", true, "", nil, nil},
		{"enabled with an address", true, keysTestPublicURL, ptr(keysTestPublicURL), ptr(keysTestPublicURL + "/api/v1/openapi.json")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newKeysCaller(t, keysTestHandler(pool, tc.enabled, tc.url), user, "user")
			code, raw := c.do(http.MethodGet, "/settings", nil)
			if code != http.StatusOK {
				t.Fatalf("settings: %d %s", code, raw)
			}
			got := decodeKeys[map[string]any](t, raw)
			if got["publicApiEnabled"] != tc.enabled {
				t.Errorf("publicApiEnabled %v, want %v", got["publicApiEnabled"], tc.enabled)
			}
			for field, want := range map[string]*string{"publicApiUrl": tc.wantURL, "openApiUrl": tc.wantDoc} {
				v, present := got[field]
				switch {
				case !present:
					t.Errorf("%s is missing, want it present (null or a string)", field)
				case want == nil && v != nil:
					t.Errorf("%s = %v, want null", field, v)
				case want != nil && v != *want:
					t.Errorf("%s = %v, want %q", field, v, *want)
				}
			}
			if got["defaultTtlSeconds"] != float64(90*24*3600) || got["maxTtlSeconds"] != float64(365*24*3600) {
				t.Errorf("ttl %v / %v, want 90 and 365 days in seconds", got["defaultTtlSeconds"], got["maxTtlSeconds"])
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestAPIKeysCabinet_Revoke(t *testing.T) {
	pool := newMeTestPool(t)
	ctx := context.Background()
	user, _ := testsupport.SeedUser(t, pool, "user")
	stranger, _ := testsupport.SeedUser(t, pool, "user")
	h := keysTestHandler(pool, true, keysTestPublicURL)
	c := newKeysCaller(t, h, user, "user")
	sc := newKeysCaller(t, h, stranger, "user")
	expires := time.Now().UTC().Add(30 * 24 * time.Hour)

	target := c.create(keyBody("to revoke", "read", false, scopeAll, expires))
	kept := c.create(keyBody("kept", "read", false, scopeAll, expires))
	foreign := sc.create(keyBody("foreign", "read", false, scopeAll, expires))

	if code, raw := c.do(http.MethodDelete, "/"+target.ID, nil); code != http.StatusNoContent {
		t.Fatalf("revoke own active key: %d %s, want 204", code, raw)
	}
	if row := c.row(target.ID); row.Status != "inactive" || row.StatusReason == nil || *row.StatusReason != "revoked" {
		t.Errorf("revoked row status %q reason %v, want inactive / revoked", row.Status, row.StatusReason)
	}
	st, err := apikeys.New(pool).Status(ctx, uuid.MustParse(target.ID))
	if err != nil || !st.Revoked() {
		t.Errorf("Status after revoke %+v err %v, want revoked", st, err)
	}

	t.Run("a repeat, another user's key and an unknown id are 404 and change nothing", func(t *testing.T) {
		for name, id := range map[string]string{"repeat": target.ID, "another user's key": foreign.ID, "unknown id": uuid.NewString()} {
			if code, raw := c.do(http.MethodDelete, "/"+id, nil); code != http.StatusNotFound {
				t.Errorf("%s: %d %s, want 404", name, code, raw)
			}
		}
		if row := c.row(kept.ID); row.Status != "active" {
			t.Errorf("the caller's other key is %q, want active", row.Status)
		}
		if row := sc.row(foreign.ID); row.Status != "active" {
			t.Errorf("the other user's key is %q, want active", row.Status)
		}
	})
}

func spaceShortID(t *testing.T, pool *pgxpool.Pool, space uuid.UUID) string {
	t.Helper()
	var short string
	if err := pool.QueryRow(context.Background(), `SELECT short_id FROM spaces WHERE id = $1`, space).Scan(&short); err != nil {
		t.Fatal(err)
	}
	return short
}

func TestAPIKeysCabinet_ScopeSpacesByShortID(t *testing.T) {
	pool := newMeTestPool(t)
	user, _ := testsupport.SeedUser(t, pool, "user")
	stranger, _ := testsupport.SeedUser(t, pool, "user")
	own := testsupport.SeedSpace(t, pool, user)
	foreign := testsupport.SeedSpace(t, pool, stranger)
	ownShort, foreignShort := spaceShortID(t, pool, own), spaceShortID(t, pool, foreign)
	c := newKeysCaller(t, keysTestHandler(pool, true, keysTestPublicURL), user, "user")
	expires := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	spacesScope := func(ids ...string) map[string]any { return map[string]any{"mode": "spaces", "spaces": ids} }
	refusals := map[string]string{
		"a space the owner is not in": foreignShort,
		"an unknown short id":         "zzzzzzzz",
		"the space UUID":              own.String(),
	}

	t.Run("create takes and the list returns the short id", func(t *testing.T) {
		k := c.create(keyBody("by space", "read", false, spacesScope(ownShort), expires))
		row := c.row(k.ID)
		if row.Scope.Mode != "spaces" || len(row.Scope.Spaces) != 1 || row.Scope.Spaces[0].ID != ownShort || row.Scope.Spaces[0].Name != "ts space" {
			t.Errorf("scope %+v, want spaces {%s ts space}", row.Scope, ownShort)
		}
		var stored uuid.UUID
		if err := pool.QueryRow(context.Background(),
			`SELECT space_id FROM api_key_scope_spaces WHERE api_key_id = $1`, k.ID).Scan(&stored); err != nil || stored != own {
			t.Errorf("stored scope space %v (%v), want %v", stored, err, own)
		}
	})

	t.Run("create refuses a member out of reach and creates nothing", func(t *testing.T) {
		before := keyCount(t, pool, user)
		for name, id := range refusals {
			code, raw := c.do(http.MethodPost, "", keyBody(name, "read", false, spacesScope(id), expires))
			errCode, details := problemCode(t, raw)
			if code != http.StatusUnprocessableEntity || errCode != "invalid_grant" || details["rule"] != "scope_unavailable" {
				t.Errorf("%s: %d %s, want 422 invalid_grant scope_unavailable", name, code, raw)
			}
		}
		if n := keyCount(t, pool, user); n != before {
			t.Errorf("keys after the refusals = %d, want %d", n, before)
		}
	})

	t.Run("PATCH narrows to a space by short id and refuses one out of reach", func(t *testing.T) {
		k := c.create(keyBody("narrow to space", "read", false, scopeAll, expires))
		code, raw := c.do(http.MethodPatch, "/"+k.ID, map[string]any{
			"permissions": map[string]any{"level": "read", "admin": false}, "scope": spacesScope(ownShort),
		})
		if code != http.StatusOK {
			t.Fatalf("narrow to the own space: %d %s", code, raw)
		}
		got := decodeKeys[apiKeyView](t, raw)
		if got.Scope.Mode != "spaces" || len(got.Scope.Spaces) != 1 || got.Scope.Spaces[0].ID != ownShort {
			t.Errorf("narrowed scope %+v, want spaces {%s}", got.Scope, ownShort)
		}
		for name, id := range refusals {
			code, raw := c.do(http.MethodPatch, "/"+k.ID, map[string]any{
				"permissions": map[string]any{"level": "read", "admin": false}, "scope": spacesScope(id),
			})
			errCode, details := problemCode(t, raw)
			if code != http.StatusUnprocessableEntity || errCode != "invalid_grant" || details["rule"] != "scope_unavailable" {
				t.Errorf("narrow to %s: %d %s, want 422 invalid_grant scope_unavailable", name, code, raw)
			}
		}
		if after := c.row(k.ID); len(after.Scope.Spaces) != 1 || after.Scope.Spaces[0].ID != ownShort {
			t.Errorf("after refused narrowing scope %+v, want spaces {%s}", after.Scope, ownShort)
		}
	})
}
