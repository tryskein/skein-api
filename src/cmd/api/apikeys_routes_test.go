package main

// apikeys_routes_test.go — test matrix row 16 (INV-3, SC-016), on the
// production router. Reference — the spec, written before the code: no
// operation by an API key creates, reissues or revokes a key, so the API
// keys routes answer only the BFF identity. A request carrying a valid
// X-Skein-Api-Key and nothing else is refused with 401 on every
// /ui/v1/me/api-keys* route (and the transitional /api/v1 prefix), and the
// api_keys rows do not change.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
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

func apiKeysRouteRouter(t *testing.T, pool *pgxpool.Pool) chi.Router {
	t.Helper()
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	apiKeysH := handler.NewAPIKeysHandler(pool, auth.MinBcryptCost, handler.APIKeysSettings{
		TTL:              apikeys.TTLPolicy{Default: cfg.APIKeyDefaultTTL, Max: cfg.APIKeyMaxTTL},
		PublicAPIEnabled: cfg.PublicAPIEnabled,
		PublicAPIURL:     cfg.PublicAPIURL,
	})
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	projectsStore := projects.NewStore(pool)
	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, // authH…bulkH
		apiKeysH,
		nil,                // usersH
		nil, nil, nil, nil, // issuesMeH…dashboardH
		usersStore, issues.NewStore(pool), dependencies.NewStore(pool), projectsStore,
		nil, nil, searchSvc, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil,
	)
	return router
}

// apiKeysSnapshot is what must not change: every key row of the user.
func apiKeysSnapshot(t *testing.T, pool *pgxpool.Pool, user string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `
SELECT coalesce(string_agg(id::text || '/' || coalesce(level, '-') || '/' || scope_mode || '/' ||
                           coalesce(revoked_at::text, '-') || '/' || coalesce(expires_at::text, '-'), ',' ORDER BY id), '')
  FROM api_keys WHERE user_id = $1::uuid`, user).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAPIKeysRoutes_RefuseAKeyWithoutBFFIdentity(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	router := apiKeysRouteRouter(t, pool)

	user, _ := testsupport.SeedUser(t, pool, "user")
	plain, prefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	keyID, _, err := testsupport.CreateOwnerKey(ctx, pool, user, "route probe", prefix, hash)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	before := apiKeysSnapshot(t, pool, user.String())

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/me/api-keys", ""},
		{http.MethodPost, "/me/api-keys", `{"label":"by key","permissions":{"level":"manage","admin":false},"scope":{"mode":"all"},"expiresAt":"2099-01-01T00:00:00Z"}`},
		{http.MethodGet, "/me/api-keys/settings", ""},
		{http.MethodPatch, "/me/api-keys/" + keyID.String(), `{"permissions":{"level":"read","admin":false},"scope":{"mode":"all"}}`},
		{http.MethodPost, "/me/api-keys/" + keyID.String() + "/reissue", ""},
		{http.MethodDelete, "/me/api-keys/" + keyID.String(), ""},
	}
	for _, prefixPath := range []string{"/ui/v1", "/api/v1"} {
		for _, rt := range routes {
			t.Run(rt.method+" "+prefixPath+rt.path, func(t *testing.T) {
				req := httptest.NewRequest(rt.method, prefixPath+rt.path, bytes.NewBufferString(rt.body))
				req.Header.Set(auth.HeaderAPIKey, plain)
				if rt.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status %d %s, want 401", rec.Code, rec.Body.String())
				}
			})
		}
	}
	if after := apiKeysSnapshot(t, pool, user.String()); after != before {
		t.Errorf("api_keys rows changed:\nbefore %s\nafter  %s", before, after)
	}
}
