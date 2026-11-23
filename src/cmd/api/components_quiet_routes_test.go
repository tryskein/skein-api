package main

// components_quiet_routes_test.go — the routes of the project components and
// of the caller's time zone and quiet hours, on the production router
// (buildRouter) with its whole middleware chain. The handlers' contracts are
// tested in internal/handler (components_rest_contract_test.go,
// quiet_hours_rest_contract_test.go) on the same paths; this test pins that
// buildRouter binds each method and path to the handler that answers it: every
// request gets an answer only that route's handler gives.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/handler"
	"skein-api/internal/issues"
	"skein-api/internal/projects"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

func componentsQuietRouter(t *testing.T, pool *pgxpool.Pool) chi.Router {
	t.Helper()
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil,                                   // authH
		handler.NewProjectsHandler(pool, nil), // projectsH
		handler.NewComponentsHandler(pool),    // componentsH
		nil, nil, nil, nil,                    // issuesH, depsH, readyH, searchH
		handler.NewNotificationsHandler(pool), // notifH
		nil, nil, nil, nil, nil, nil, nil,     // bulkH … dashboardH
		users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil),
		issues.NewStore(pool), dependencies.NewStore(pool), projects.NewStore(pool),
		nil, nil, searchSvc, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil,
	)
	return router
}

func TestComponentsAndQuietHoursRoutes_BoundInBuildRouter(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	router := componentsQuietRouter(t, pool)
	owner, name := testsupport.SeedUser(t, pool, "user")
	pid, _ := testsupport.SeedProject(t, pool, owner, "wirecomp")
	active := testsupport.SeedComponent(t, pool, pid, owner, "active", "")
	archived := testsupport.SeedComponent(t, pool, pid, owner, "archived", "")
	if _, err := pool.Exec(context.Background(), `UPDATE components SET archived_at = now() WHERE id = $1`, archived); err != nil {
		t.Fatal(err)
	}
	missing := uuid.Must(uuid.NewV7()).String()

	do := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var rd *bytes.Reader
		if body == nil {
			rd = bytes.NewReader(nil)
		} else {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		req.Header.Set(auth.HeaderUserID, owner.String())
		req.Header.Set(auth.HeaderSessionEpoch, wiringSessionEpoch)
		req.Header.Set(auth.HeaderUsername, name)
		req.Header.Set(auth.HeaderUserRole, "user")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	code := func(raw []byte) string {
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s, _ := body["error"].(string)
		return s
	}

	base := "/ui/v1/projects/" + pid.String() + "/components"
	cases := []struct {
		name, method, path string
		body               any
		want               int
		wantCode           string
	}{
		// Each answer belongs to one handler: list → an array, create → its
		// own validation, get/patch → component_not_found, archive and the
		// DELETE alias → already_archived on an archived component,
		// unarchive → not_archived on an active one.
		{"list", http.MethodGet, base, nil, http.StatusOK, ""},
		{"create", http.MethodPost, base, map[string]any{"description": "no name"}, http.StatusBadRequest, "invalid_name"},
		{"get", http.MethodGet, base + "/" + missing, nil, http.StatusNotFound, "component_not_found"},
		{"patch", http.MethodPatch, base + "/" + missing, map[string]any{"name": "x"}, http.StatusNotFound, "component_not_found"},
		{"archive", http.MethodPost, base + "/" + archived.String() + "/archive", nil, http.StatusUnprocessableEntity, "already_archived"},
		{"delete is archive", http.MethodDelete, base + "/" + archived.String(), nil, http.StatusUnprocessableEntity, "already_archived"},
		{"unarchive", http.MethodPost, base + "/" + active.String() + "/unarchive", nil, http.StatusUnprocessableEntity, "not_archived"},
		{"get timezone", http.MethodGet, "/ui/v1/me/timezone", nil, http.StatusOK, ""},
		{"put timezone", http.MethodPut, "/ui/v1/me/timezone", map[string]any{"timezone": "Europe/Moskva"}, http.StatusUnprocessableEntity, "invalid_timezone"},
		{"get quiet hours", http.MethodGet, "/ui/v1/me/quiet-hours", nil, http.StatusOK, ""},
		{"put quiet hours", http.MethodPut, "/ui/v1/me/quiet-hours", map[string]any{"start": "22:00"}, http.StatusUnprocessableEntity, "quiet_hours_half_set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := do(c.method, c.path, c.body)
			if status != c.want || code(raw) != c.wantCode {
				t.Errorf("%s %s = %d %s, want %d %q", c.method, c.path, status, raw, c.want, c.wantCode)
			}
		})
	}

	t.Run("the read routes answer their own shapes", func(t *testing.T) {
		_, raw := do(http.MethodGet, base, nil)
		var list []map[string]any
		if err := json.Unmarshal(raw, &list); err != nil || len(list) != 1 || list[0]["id"] != active.String() {
			t.Errorf("GET %s = %s, want an array with the active component", base, raw)
		}
		for path, keys := range map[string][]string{
			"/ui/v1/me/timezone":    {"timezone", "source"},
			"/ui/v1/me/quiet-hours": {"start", "end", "timezone"},
		} {
			_, raw := do(http.MethodGet, path, nil)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil || len(body) != len(keys) {
				t.Errorf("GET %s = %s, want exactly the keys %v", path, raw, keys)
				continue
			}
			for _, k := range keys {
				if _, ok := body[k]; !ok {
					t.Errorf("GET %s = %s, lacks %q", path, raw, k)
				}
			}
		}
	})
}
