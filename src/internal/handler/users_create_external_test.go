package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/projects"
	"skein-api/internal/users"
)

// sk-163 S8 — POST /api/v1/users accepts an is_external flag (admin-only),
// the created/list UserDTO carries isExternal, and an external+admin
// request is rejected with a clean 422 external_cannot_be_admin.

// callCreateUser drives h.Create with an admin context and a JSON body.
func callCreateUser(t *testing.T, h *UsersHandler, adminID uuid.UUID, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewReader(raw))
	ctx := auth.WithUserID(req.Context(), adminID)
	ctx = auth.WithUsername(ctx, "admin")
	ctx = auth.WithUserRole(ctx, "admin")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	res := rec.Result()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestCreateUser_External_Flagged(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	admin := seedUserRow(t, pool, "admin")
	email := "hext" + admin.String()[:6] + "@customer.com"

	res, out := callCreateUser(t, h, admin, map[string]any{
		"email":      email,
		"name":       "External via REST",
		"password":   "passw0rd!",
		"role":       "user",
		"isExternal": true,
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%+v)", res.StatusCode, out)
	}
	uid, _ := out["id"].(string)
	if uid == "" {
		t.Fatalf("created id missing (body=%+v)", out)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})
	if ext, _ := out["isExternal"].(bool); !ext {
		t.Fatalf("isExternal = %v, want true (body=%+v)", out["isExternal"], out)
	}
	if role, _ := out["role"].(string); role != "user" {
		t.Fatalf("role = %q, want user", role)
	}
	// The DTO returned by the LIST query must also carry isExternal=true.
	lr, err := usersStore.ListUsers(context.Background(), users.ListUsersInput{Search: email, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, u := range lr.Items {
		if u.ID.String() == uid {
			found = true
			if !u.IsExternal {
				t.Fatalf("list DTO IsExternal = false, want true")
			}
		}
	}
	if !found {
		t.Fatalf("created external user not returned by ListUsers")
	}
}

func TestCreateUser_ExternalAdmin_Rejected(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	admin := seedUserRow(t, pool, "admin")
	email := "hextadm" + admin.String()[:6] + "@customer.com"

	res, out := callCreateUser(t, h, admin, map[string]any{
		"email":      email,
		"name":       "External Admin Attempt",
		"password":   "passw0rd!",
		"role":       "admin",
		"isExternal": true,
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%+v)", res.StatusCode, out)
	}
	if out["error"] != "external_cannot_be_admin" {
		t.Fatalf("error = %v, want external_cannot_be_admin", out["error"])
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE lower(email) = lower($1)`, email).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows for rejected external admin, got %d", n)
	}
}

// callListUsers drives h.List with an admin context and an optional
// ?external= query value (empty = omit).
func callListUsers(t *testing.T, h *UsersHandler, adminID uuid.UUID, external string) (*http.Response, users.ListUsersResult) {
	t.Helper()
	url := "/api/v1/users?limit=100"
	if external != "" {
		url += "&external=" + external
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx := auth.WithUserID(req.Context(), adminID)
	ctx = auth.WithUsername(ctx, "admin")
	ctx = auth.WithUserRole(ctx, "admin")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	res := rec.Result()
	var out users.ListUsersResult
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// TestListUsers_ExternalQueryParam — sk-163 S8. GET /users?external=true
// returns only external users; omitting the param returns both (no
// regression). Scoped to two handler-created users.
func TestListUsers_ExternalQueryParam(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	admin := seedUserRow(t, pool, "admin")
	tok := "hqp" + admin.String()[:6]

	_, intOut := callCreateUser(t, h, admin, map[string]any{
		"email": tok + "int@test.local", "name": "Int " + tok, "password": "passw0rd!", "role": "user",
	})
	intID, _ := intOut["id"].(string)
	_, extOut := callCreateUser(t, h, admin, map[string]any{
		"email": tok + "ext@customer.com", "name": "Ext " + tok, "password": "passw0rd!",
		"role": "user", "isExternal": true,
	})
	extID, _ := extOut["id"].(string)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`,
			[]string{intID, extID})
	})

	has := func(items []users.UserDTO, id string) bool {
		for _, u := range items {
			if u.ID.String() == id {
				return true
			}
		}
		return false
	}

	// ?external=true → only the external user.
	res, out := callListUsers(t, h, admin, "true")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if !has(out.Items, extID) || has(out.Items, intID) {
		t.Fatalf("external=true returned wrong set (ext=%v int=%v)",
			has(out.Items, extID), has(out.Items, intID))
	}

	// no param → both classes present.
	res, out = callListUsers(t, h, admin, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if !has(out.Items, extID) || !has(out.Items, intID) {
		t.Fatalf("no filter missing a class (ext=%v int=%v)",
			has(out.Items, extID), has(out.Items, intID))
	}

	// sk-163 S8 — an invalid ?external value is a hard 400 invalid_external,
	// NOT a silent fall-through to "all". A typo must never widen the result
	// set (which on the admin surface would over-expose).
	res, _ = callListUsers(t, h, admin, "banana")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("external=banana status = %d, want 400", res.StatusCode)
	}
}

// TestCreateUser_InternalDefault — no isExternal in the body creates an
// internal user (isExternal=false), unchanged behaviour.
func TestCreateUser_InternalDefault(t *testing.T) {
	pool := newUsersTestPool(t)
	apikeysStore := apikeys.New(pool)
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeysStore, "local", "", nil)
	projectsStore := projects.NewStore(pool)
	h := NewUsersHandler(pool, usersStore, projectsStore)

	admin := seedUserRow(t, pool, "admin")
	email := "hint" + admin.String()[:6] + "@test.local"

	res, out := callCreateUser(t, h, admin, map[string]any{
		"email":    email,
		"name":     "Internal Default",
		"password": "passw0rd!",
		"role":     "user",
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%+v)", res.StatusCode, out)
	}
	uid, _ := out["id"].(string)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})
	if ext, _ := out["isExternal"].(bool); ext {
		t.Fatalf("isExternal = true, want false for default internal create")
	}
}
