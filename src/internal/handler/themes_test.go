package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/auth"
	"skein-api/internal/mcp/tools"
	"skein-api/internal/themes"
)

// ---------------- helpers ----------------

// newThemesTestPool mirrors newMeTestPool — skips when TEST_DATABASE_URL
// is unset so the package test suite still runs in plain `go test ./...`
// without the local stack.
func newThemesTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseURL)
	if url == "" {
		t.Skipf("set %s to run DB-backed tests", testDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedThemesUser inserts a minimal users row. Each test gets its own
// row so the DELETE in t.Cleanup is sufficient — no need to scrub
// themes (the test's deletes cascade through the FKs, and any leftover
// rows still get caught by the per-test name suffix).
func seedThemesUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	uname := "th" + strings.ReplaceAll(id.String()[:18], "-", "")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, email, username, name, role, password_hash, created_at, metadata)
VALUES ($1, $2, $3, $4, 'user', 'unused', now(), '{}'::jsonb)`,
		id, uname+"@test.local", uname, uname); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// withCaller threads a uuid into the request context the way the real
// middleware stack would. Used for the happy-path tests; the auth-test
// passes uuid.Nil so the context comes back empty and the handler 401s.
func withCaller(req *http.Request, userID uuid.UUID) *http.Request {
	if userID == uuid.Nil {
		return req
	}
	ctx := req.Context()
	ctx = auth.WithUserID(ctx, userID)
	ctx = auth.WithUsername(ctx, "tester")
	ctx = auth.WithUserRole(ctx, "user")
	return req.WithContext(ctx)
}

// uniqName returns a probabilistically-unique theme name so two
// tests running back-to-back can't collide on the UNIQUE constraint.
// Combines a static suffix with a fresh uuid prefix.
func uniqName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// makeThemeRouter builds the chi router for the themes-handler-only
// subset we exercise in tests. We don't drag in the full main.go
// stack because we want to exercise only the handler in isolation.
func makeThemeRouter(themesH *ThemesHandler) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/themes", themesH.List)
	r.Post("/api/v1/themes", themesH.Create)
	r.Get("/api/v1/themes/{id}", themesH.Get)
	r.Patch("/api/v1/themes/{id}", themesH.Patch)
	r.Delete("/api/v1/themes/{id}", themesH.Delete)
	return r
}

func makeMeThemeRouter(h *MeThemeHandler) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/me/theme", h.GetTheme)
	r.Put("/api/v1/me/theme", h.SetTheme)
	r.Post("/api/v1/me/theme/inspect", h.Inspect)
	return r
}

// doRequest sends `req` through `handler` and returns the response
// + parsed JSON body. Caller is responsible for closing res.Body
// via t.Cleanup if they need the response itself.
func doRequest(t *testing.T, h http.Handler, req *http.Request) (*http.Response, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	t.Cleanup(func() { res.Body.Close() })
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	res.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	return res, body
}

// postJSON builds a POST/PUT/PATCH request with a JSON body and the
// configured caller context.
func jsonRequest(t *testing.T, method, path string, payload any, userID uuid.UUID) *http.Request {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	return withCaller(req, userID)
}

// createTheme is a convenience wrapper for tests that need an existing
// theme to read / update / delete. Takes the pool explicitly so cleanup
// reuses the test's already-open pool instead of opening (and leaking)
// a brand-new one per created theme.
func createTheme(t *testing.T, pool *pgxpool.Pool, h http.Handler, userID uuid.UUID, name string) themes.ThemeDTO {
	t.Helper()
	payload := map[string]any{
		"name": name,
		"base": "light",
		"variables": map[string]string{
			"--bg-base": "#ffffff",
			"--fg-base": "#000000",
		},
	}
	req := jsonRequest(t, http.MethodPost, "/api/v1/themes", payload, userID)
	res, _ := doRequest(t, h, req)
	if res.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("createTheme: status = %d, body = %s", res.StatusCode, raw)
	}
	var dto themes.ThemeDTO
	if err := json.NewDecoder(res.Body).Decode(&dto); err != nil {
		t.Fatalf("decode theme: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort cleanup so the UNIQUE name constraint doesn't
		// fail subsequent runs against the same DB. Reuses the test's
		// pool — if the parent test already tore it down by the time
		// cleanup fires, Exec returns an error which we surface via
		// t.Logf so a UNIQUE-flake on the next run isn't silent.
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM themes WHERE id = $1`, dto.ID); err != nil {
			t.Logf("themes cleanup: id=%s err=%v", dto.ID, err)
		}
	})
	return dto
}

// ---------------- /themes happy-path ----------------

// TestThemes_CRUD covers create → get → list → patch → delete in one
// long flow. The intermediate assertions catch shape regressions
// (createdBy / updatedBy populated, variables round-trip, timestamps
// monotonic across the patch).
func TestThemes_CRUD(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	themesH := NewThemesHandler(pool)
	router := makeThemeRouter(themesH)

	name := uniqName("solar")
	dto := createTheme(t, pool, router, user, name)
	if dto.Name != name {
		t.Fatalf("name = %q, want %q", dto.Name, name)
	}
	if dto.Base != "light" {
		t.Fatalf("base = %q, want light", dto.Base)
	}
	if got, want := dto.Variables["--bg-base"], "#ffffff"; got != want {
		t.Fatalf("variables[--bg-base] = %q, want %q", got, want)
	}
	if dto.CreatedBy.ID != user {
		t.Fatalf("createdBy.id = %s, want %s", dto.CreatedBy.ID, user)
	}
	if dto.CreatedBy.Username == "" {
		t.Fatalf("createdBy.username is empty — JOIN to users failed")
	}
	if dto.UpdatedBy.ID != user {
		t.Fatalf("updatedBy.id = %s, want %s", dto.UpdatedBy.ID, user)
	}

	// GET by id
	req := jsonRequest(t, http.MethodGet, "/api/v1/themes/"+dto.ID.String(), nil, user)
	res, _ := doRequest(t, router, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", res.StatusCode)
	}

	// LIST
	req = jsonRequest(t, http.MethodGet, "/api/v1/themes", nil, user)
	res, _ = doRequest(t, router, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("LIST status = %d", res.StatusCode)
	}
	var list []themes.ThemeDTO
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, item := range list {
		if item.ID == dto.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created theme missing from list response")
	}

	// PATCH the variables — replace, not merge.
	patch := map[string]any{
		"name": uniqName("renamed"),
		"variables": map[string]string{
			"--bg-base": "#fafafa",
		},
	}
	req = jsonRequest(t, http.MethodPatch, "/api/v1/themes/"+dto.ID.String(), patch, user)
	res, _ = doRequest(t, router, req)
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("PATCH status = %d, body = %s", res.StatusCode, raw)
	}
	var updated themes.ThemeDTO
	if err := json.NewDecoder(res.Body).Decode(&updated); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	if updated.Variables["--bg-base"] != "#fafafa" {
		t.Fatalf("patched bg-base = %q, want #fafafa", updated.Variables["--bg-base"])
	}
	if _, has := updated.Variables["--fg-base"]; has {
		t.Fatalf("PATCH must replace variables, not merge — --fg-base still present")
	}
	if !updated.UpdatedAt.After(dto.UpdatedAt) {
		t.Fatalf("updatedAt did not advance: %v vs %v", updated.UpdatedAt, dto.UpdatedAt)
	}

	// DELETE
	req = jsonRequest(t, http.MethodDelete, "/api/v1/themes/"+dto.ID.String(), nil, user)
	res, _ = doRequest(t, router, req)
	if res.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("DELETE status = %d, body = %s", res.StatusCode, raw)
	}

	// GET after DELETE → 404
	req = jsonRequest(t, http.MethodGet, "/api/v1/themes/"+dto.ID.String(), nil, user)
	res, body := doRequest(t, router, req)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete status = %d, want 404; body=%v", res.StatusCode, body)
	}
}

// ---------------- name_taken ----------------

func TestThemes_NameTakenOnCreate(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	themesH := NewThemesHandler(pool)
	router := makeThemeRouter(themesH)

	name := uniqName("dup")
	_ = createTheme(t, pool, router, user, name)

	// Second create with same name → 422 name_taken
	payload := map[string]any{
		"name":      name,
		"base":      "dark",
		"variables": map[string]string{"--bg-base": "#000"},
	}
	req := jsonRequest(t, http.MethodPost, "/api/v1/themes", payload, user)
	res, body := doRequest(t, router, req)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%v", res.StatusCode, body)
	}
	if body["error"] != "name_taken" {
		t.Fatalf(`error = %v, want "name_taken"`, body["error"])
	}
}

func TestThemes_NameTakenOnPatch(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	themesH := NewThemesHandler(pool)
	router := makeThemeRouter(themesH)

	first := createTheme(t, pool, router, user, uniqName("a"))
	second := createTheme(t, pool, router, user, uniqName("b"))

	patch := map[string]any{"name": first.Name}
	req := jsonRequest(t, http.MethodPatch, "/api/v1/themes/"+second.ID.String(), patch, user)
	res, body := doRequest(t, router, req)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%v", res.StatusCode, body)
	}
	if body["error"] != "name_taken" {
		t.Fatalf(`error = %v, want "name_taken"`, body["error"])
	}
}

// ---------------- 404 paths ----------------

func TestThemes_NotFound(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	router := makeThemeRouter(NewThemesHandler(pool))
	bogus := uuid.NewString()

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/themes/" + bogus, nil},
		{http.MethodPatch, "/api/v1/themes/" + bogus, map[string]any{"name": uniqName("patch-missing")}},
		{http.MethodDelete, "/api/v1/themes/" + bogus, nil},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			req := jsonRequest(t, c.method, c.path, c.body, user)
			res, body := doRequest(t, router, req)
			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("%s status = %d, want 404; body=%v", c.method, res.StatusCode, body)
			}
		})
	}
}

// ---------------- invalid_variables ----------------

func TestThemes_InvalidVariables(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	router := makeThemeRouter(NewThemesHandler(pool))

	cases := []struct {
		name string
		vars map[string]string
	}{
		{"empty value", map[string]string{"--bg-base": ""}},
		{"too long", map[string]string{"--bg-base": strings.Repeat("x", 300)}},
		{"contains ;", map[string]string{"--bg-base": "red; expression(alert(1))"}},
		{"contains <", map[string]string{"--bg-base": "<script>"}},
		{"contains >", map[string]string{"--bg-base": ">"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := map[string]any{
				"name":      uniqName("inv"),
				"base":      "light",
				"variables": c.vars,
			}
			req := jsonRequest(t, http.MethodPost, "/api/v1/themes", payload, user)
			res, body := doRequest(t, router, req)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("%s status = %d, want 422; body=%v", c.name, res.StatusCode, body)
			}
			if body["error"] != "invalid_variables" {
				t.Fatalf(`%s error = %v, want "invalid_variables"`, c.name, body["error"])
			}
		})
	}
}

// ---------------- 401 ----------------

func TestThemes_RequiresAuth(t *testing.T) {
	pool := newThemesTestPool(t)
	router := makeThemeRouter(NewThemesHandler(pool))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/themes", nil)
	res, _ := doRequest(t, router, req)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// ---------------- /me/theme ----------------

func TestMeTheme_DefaultsToBuiltinLight(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	router := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	req := jsonRequest(t, http.MethodGet, "/api/v1/me/theme", nil, user)
	res, body := doRequest(t, router, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%v", res.StatusCode, body)
	}
	if body["mode"] != "builtin" {
		t.Fatalf("mode = %v, want builtin", body["mode"])
	}
	if body["builtinTheme"] != "light" {
		t.Fatalf("builtinTheme = %v, want light", body["builtinTheme"])
	}
	if body["customThemeId"] != nil {
		t.Fatalf("customThemeId = %v, want nil", body["customThemeId"])
	}
}

func TestMeTheme_SwitchToCustom(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	themeRouter := makeThemeRouter(NewThemesHandler(pool))
	meRouter := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	dto := createTheme(t, pool, themeRouter, user, uniqName("active"))

	payload := map[string]any{
		"mode":          "custom",
		"builtinTheme":  "dark",
		"customThemeId": dto.ID.String(),
	}
	req := jsonRequest(t, http.MethodPut, "/api/v1/me/theme", payload, user)
	res, body := doRequest(t, meRouter, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%v", res.StatusCode, body)
	}
	if body["mode"] != "custom" {
		t.Fatalf("mode = %v, want custom", body["mode"])
	}
	if body["builtinTheme"] != "dark" {
		t.Fatalf("builtinTheme = %v, want dark", body["builtinTheme"])
	}
	if body["customThemeId"] != dto.ID.String() {
		t.Fatalf("customThemeId = %v, want %s", body["customThemeId"], dto.ID)
	}

	// Echo through GET — must match the PUT response.
	req = jsonRequest(t, http.MethodGet, "/api/v1/me/theme", nil, user)
	res, body = doRequest(t, meRouter, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", res.StatusCode)
	}
	if body["customThemeId"] != dto.ID.String() {
		t.Fatalf("GET customThemeId = %v, want %s", body["customThemeId"], dto.ID)
	}
}

func TestMeTheme_BuiltinClearsCustomID(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	themeRouter := makeThemeRouter(NewThemesHandler(pool))
	meRouter := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	dto := createTheme(t, pool, themeRouter, user, uniqName("transient"))

	// First switch to custom so we have a non-null state to clear.
	payload := map[string]any{
		"mode":          "custom",
		"builtinTheme":  "light",
		"customThemeId": dto.ID.String(),
	}
	req := jsonRequest(t, http.MethodPut, "/api/v1/me/theme", payload, user)
	doRequest(t, meRouter, req)

	// Now switch back to builtin — customThemeId must be NULLed even if
	// the client sent the same id (we enforce canonical state).
	payload = map[string]any{
		"mode":          "builtin",
		"builtinTheme":  "dark",
		"customThemeId": dto.ID.String(),
	}
	req = jsonRequest(t, http.MethodPut, "/api/v1/me/theme", payload, user)
	res, body := doRequest(t, meRouter, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%v", res.StatusCode, body)
	}
	if body["mode"] != "builtin" {
		t.Fatalf("mode = %v, want builtin", body["mode"])
	}
	if body["customThemeId"] != nil {
		t.Fatalf("customThemeId = %v, want nil after switching to builtin", body["customThemeId"])
	}
}

func TestMeTheme_NonExistentCustomID(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	meRouter := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	payload := map[string]any{
		"mode":          "custom",
		"builtinTheme":  "light",
		"customThemeId": uuid.NewString(),
	}
	req := jsonRequest(t, http.MethodPut, "/api/v1/me/theme", payload, user)
	res, body := doRequest(t, meRouter, req)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%v", res.StatusCode, body)
	}
	if body["error"] != "theme_not_found" {
		t.Fatalf(`error = %v, want "theme_not_found"`, body["error"])
	}
}

func TestMeTheme_CustomRequiresID(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	meRouter := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	payload := map[string]any{
		"mode":         "custom",
		"builtinTheme": "light",
		// no customThemeId
	}
	req := jsonRequest(t, http.MethodPut, "/api/v1/me/theme", payload, user)
	res, body := doRequest(t, meRouter, req)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", res.StatusCode, body)
	}
	if body["error"] != "customThemeId_required" {
		t.Fatalf(`error = %v, want "customThemeId_required"`, body["error"])
	}
}

func TestMeTheme_RequiresAuth(t *testing.T) {
	pool := newThemesTestPool(t)
	router := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/theme", nil)
	res, _ := doRequest(t, router, req)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET status = %d, want 401", res.StatusCode)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/me/theme",
		strings.NewReader(`{"mode":"builtin","builtinTheme":"light"}`))
	req.Header.Set("Content-Type", "application/json")
	res, _ = doRequest(t, router, req)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PUT status = %d, want 401", res.StatusCode)
	}
}

// ---------------- /me/theme/inspect (sk-10.3) ----------------

// TestMeThemeInspect_PostHappy posts a selection and verifies (a) the
// handler returns 204 No Content and (b) the inspect cache contains the
// snapshot afterwards. We construct the cache inline so we can read
// back into it directly — going via skein_theme(action="describe")
// would couple
// this test to the MCP plumbing.
func TestMeThemeInspect_PostHappy(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	cache := themes.NewInspectCache(0)
	router := makeMeThemeRouter(NewMeThemeHandler(pool, cache))

	payload := map[string]any{
		"selector": ".task-card-title-text",
		"parents":  []string{".task-card", ".col-tasks"},
		"tokens":   []string{"--fg-emphasis", "--font-weight-semibold"},
		"computedStyle": map[string]any{
			"color":      "rgb(58, 74, 82)",
			"fontFamily": "JetBrains Mono",
			"fontSize":   "14px",
			"fontWeight": "600",
		},
	}
	req := jsonRequest(t, http.MethodPost, "/api/v1/me/theme/inspect", payload, user)
	res, body := doRequest(t, router, req)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%v", res.StatusCode, body)
	}
	entries, ok := cache.Get(user)
	if !ok {
		t.Fatalf("cache.Get returned false; inspect payload not stored")
	}
	if len(entries) != 1 {
		t.Fatalf("cache.Get returned %d entries, want 1", len(entries))
	}
	sel := entries[0]
	if sel.Selector != ".task-card-title-text" {
		t.Fatalf("selector = %q, want .task-card-title-text", sel.Selector)
	}
	if len(sel.Parents) != 2 || sel.Parents[0] != ".task-card" {
		t.Fatalf("parents = %v, want [.task-card, .col-tasks]", sel.Parents)
	}
	if len(sel.Tokens) != 2 || sel.Tokens[0] != "--fg-emphasis" {
		t.Fatalf("tokens = %v", sel.Tokens)
	}
	if sel.ComputedStyle["color"] != "rgb(58, 74, 82)" {
		t.Fatalf("computedStyle.color = %v", sel.ComputedStyle["color"])
	}
	if sel.CapturedAt.IsZero() {
		t.Fatalf("capturedAt is zero — handler did not stamp the snapshot")
	}
}

// TestMeThemeInspect_RequiresAuth ensures the inspect handler enforces
// the same auth gate as the rest of /me/theme. Without a withCaller
// call the request context has no user id and the handler returns 401.
func TestMeThemeInspect_RequiresAuth(t *testing.T) {
	pool := newThemesTestPool(t)
	router := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/theme/inspect",
		strings.NewReader(`{"selector":".x"}`))
	req.Header.Set("Content-Type", "application/json")
	res, _ := doRequest(t, router, req)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
}

// TestMeThemeInspect_RejectsEmptySelector confirms the trim-and-check
// guard surfaces invalid_selector for whitespace-only input.
func TestMeThemeInspect_RejectsEmptySelector(t *testing.T) {
	pool := newThemesTestPool(t)
	user := seedThemesUser(t, pool)
	router := makeMeThemeRouter(NewMeThemeHandler(pool, themes.NewInspectCache(0)))

	for _, sel := range []string{"", "   ", "\t\n"} {
		payload := map[string]any{"selector": sel}
		req := jsonRequest(t, http.MethodPost, "/api/v1/me/theme/inspect", payload, user)
		res, body := doRequest(t, router, req)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("selector=%q: status = %d, want 400; body=%v", sel, res.StatusCode, body)
		}
		if body["error"] != "invalid_selector" {
			t.Fatalf(`selector=%q: error = %v, want "invalid_selector"`, sel, body["error"])
		}
	}
}

// ---------------- sk-674.18 rows 147, 148, 148a (T035, US-21) ----------------
//
// Reference — US-21 and INV-3 as the spec words them, written before the
// code: a theme is its author's. Another author's theme is absent from the
// list (REST and MCP), and reading, editing, deleting or selecting it answers
// exactly as a nonexistent id; a name is unique only among one author's
// themes (create and rename alike); the user selects the built-ins and his
// own themes, and a refused selection leaves his appearance as it was.

// mcpThemeCall sends one skein_theme call through a real MCP server and
// returns the tool's JSON text and whether it is an error.
func mcpThemeCall(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, args map[string]any) (map[string]any, bool) {
	t.Helper()
	s := server.NewMCPServer("t", "0", server.WithToolCapabilities(true))
	tools.RegisterTheme(s, themes.NewStore(pool), themes.NewRecordingPublisher(), nil)
	ctx := auth.WithUserID(context.Background(), user)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithAPIKeyID(ctx, uuid.New())
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "skein_theme", "arguments": args},
	})
	out, _ := json.Marshal(s.HandleMessage(ctx, msg))
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil || len(env.Result.Content) == 0 {
		t.Fatalf("skein_theme envelope %s: %v", out, err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(env.Result.Content[0].Text), &body)
	return body, env.Result.IsError
}

// TestThemes_AnotherAuthorsThemeIsNonexistent — row 147.
func TestThemes_AnotherAuthorsThemeIsNonexistent(t *testing.T) {
	pool := newThemesTestPool(t)
	a, b := seedThemesUser(t, pool), seedThemesUser(t, pool)
	router := makeThemeRouter(NewThemesHandler(pool))
	theirs := createTheme(t, pool, router, b, uniqName("theirs"))
	missing := uuid.NewString()

	t.Run("REST list", func(t *testing.T) {
		res, _ := doRequest(t, router, jsonRequest(t, http.MethodGet, "/api/v1/themes", nil, a))
		raw, _ := io.ReadAll(res.Body)
		if bytes.Contains(raw, []byte(theirs.ID.String())) || bytes.Contains(raw, []byte(theirs.Name)) {
			t.Errorf("A's theme list carries B's theme: %s", raw)
		}
	})
	for _, c := range []struct {
		method string
		body   any
	}{{http.MethodGet, nil}, {http.MethodPatch, map[string]any{"name": uniqName("hijack")}}, {http.MethodDelete, nil}} {
		t.Run("REST "+c.method, func(t *testing.T) {
			got, gotBody := doRequest(t, router, jsonRequest(t, c.method, "/api/v1/themes/"+theirs.ID.String(), c.body, a))
			want, wantBody := doRequest(t, router, jsonRequest(t, c.method, "/api/v1/themes/"+missing, c.body, a))
			if got.StatusCode != want.StatusCode || gotBody["error"] != wantBody["error"] || gotBody["message"] != wantBody["message"] {
				t.Errorf("B's theme: %d %v, a nonexistent id: %d %v", got.StatusCode, gotBody, want.StatusCode, wantBody)
			}
		})
	}
	t.Run("MCP describe", func(t *testing.T) {
		body, isErr := mcpThemeCall(t, pool, a, map[string]any{"action": "describe"})
		if isErr {
			t.Fatalf("describe: %v", body)
		}
		raw, _ := json.Marshal(body)
		if bytes.Contains(raw, []byte(theirs.ID.String())) || bytes.Contains(raw, []byte(theirs.Name)) {
			t.Errorf("A's describe carries B's theme")
		}
	})
	for _, args := range []map[string]any{
		{"action": "set_variables", "variables": map[string]string{"--bg-base": "#000000"}},
		{"action": "fork", "name": uniqName("forked")},
	} {
		t.Run("MCP "+args["action"].(string), func(t *testing.T) {
			withID := func(id string) map[string]any {
				m := map[string]any{}
				for k, v := range args {
					m[k] = v
				}
				if args["action"] == "fork" {
					m["source"] = id
				} else {
					m["themeId"] = id
				}
				return m
			}
			got, gotErr := mcpThemeCall(t, pool, a, withID(theirs.ID.String()))
			want, wantErr := mcpThemeCall(t, pool, a, withID(missing))
			if !gotErr || gotErr != wantErr || got["error"] != want["error"] || got["message"] != want["message"] {
				t.Errorf("B's theme: %v (error %v), a nonexistent id: %v (error %v)", got, gotErr, want, wantErr)
			}
		})
	}
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT name FROM themes WHERE id = $1`, theirs.ID).Scan(&name); err != nil || name != theirs.Name {
		t.Errorf("B's theme after A's attempts: name %q, err %v — want it untouched", name, err)
	}
}

// TestThemes_NameUniquePerAuthor — row 148.
func TestThemes_NameUniquePerAuthor(t *testing.T) {
	pool := newThemesTestPool(t)
	a, b := seedThemesUser(t, pool), seedThemesUser(t, pool)
	router := makeThemeRouter(NewThemesHandler(pool))
	shared := uniqName("shared")
	createTheme(t, pool, router, b, shared)
	mine := createTheme(t, pool, router, a, shared) // the same name — another author
	second := createTheme(t, pool, router, a, uniqName("second"))

	res, body := doRequest(t, router, jsonRequest(t, http.MethodPost, "/api/v1/themes",
		map[string]any{"name": strings.ToUpper(shared), "base": "light", "variables": map[string]string{}}, a))
	if res.StatusCode != http.StatusUnprocessableEntity || body["error"] != "name_taken" {
		t.Errorf("A's second theme of his own name: %d %v, want 422 name_taken", res.StatusCode, body)
	}
	// Rename: onto B's other name — free for A; onto A's own name — taken.
	bOther := uniqName("b-other")
	createTheme(t, pool, router, b, bOther)
	res, body = doRequest(t, router, jsonRequest(t, http.MethodPatch, "/api/v1/themes/"+second.ID.String(),
		map[string]any{"name": bOther}, a))
	if res.StatusCode != http.StatusOK {
		t.Errorf("rename onto a name only B has: %d %v, want 200", res.StatusCode, body)
	}
	res, body = doRequest(t, router, jsonRequest(t, http.MethodPatch, "/api/v1/themes/"+second.ID.String(),
		map[string]any{"name": mine.Name}, a))
	if res.StatusCode != http.StatusUnprocessableEntity || body["error"] != "name_taken" {
		t.Errorf("rename onto A's own name: %d %v, want 422 name_taken", res.StatusCode, body)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM themes WHERE created_by = ANY($1)`, []uuid.UUID{a, b})
	})
}

// TestMeTheme_SelectOnlyBuiltinOrOwn — row 148a.
func TestMeTheme_SelectOnlyBuiltinOrOwn(t *testing.T) {
	pool := newThemesTestPool(t)
	a, b := seedThemesUser(t, pool), seedThemesUser(t, pool)
	themesRouter := makeThemeRouter(NewThemesHandler(pool))
	me := makeMeThemeRouter(NewMeThemeHandler(pool, nil))
	own := createTheme(t, pool, themesRouter, a, uniqName("own"))
	theirs := createTheme(t, pool, themesRouter, b, uniqName("theirs"))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE users SET custom_theme_id = NULL WHERE id = ANY($1)`, []uuid.UUID{a, b})
	})
	pick := func(mode string, custom *uuid.UUID) (*http.Response, map[string]any) {
		payload := map[string]any{"mode": mode, "builtinTheme": "dark"}
		if custom != nil {
			payload["customThemeId"] = custom.String()
		}
		return doRequest(t, me, jsonRequest(t, http.MethodPut, "/api/v1/me/theme", payload, a))
	}
	current := func() (string, any) {
		_, body := doRequest(t, me, jsonRequest(t, http.MethodGet, "/api/v1/me/theme", nil, a))
		return fmt.Sprint(body["mode"]), body["customThemeId"]
	}

	if res, body := pick("custom", &own.ID); res.StatusCode != http.StatusOK {
		t.Fatalf("selecting his own theme: %d %v", res.StatusCode, body)
	}
	missing := uuid.New()
	got, gotBody := pick("custom", &theirs.ID)
	want, wantBody := pick("custom", &missing)
	if got.StatusCode == http.StatusOK || got.StatusCode != want.StatusCode || gotBody["error"] != wantBody["error"] {
		t.Errorf("selecting B's theme: %d %v; a nonexistent id: %d %v — want the same refusal", got.StatusCode, gotBody, want.StatusCode, wantBody)
	}
	if mode, custom := current(); mode != "custom" || custom != own.ID.String() {
		t.Errorf("after the refused selection: mode %s, custom %v — want his own theme still", mode, custom)
	}
	if res, body := pick("builtin", nil); res.StatusCode != http.StatusOK {
		t.Errorf("selecting a built-in: %d %v", res.StatusCode, body)
	}
	if res, body := pick("custom", &own.ID); res.StatusCode != http.StatusOK {
		t.Errorf("back to his own theme: %d %v", res.StatusCode, body)
	}
}
