package main

// session_role_route_test.go — sk-674.18, on the production router. Reference — T037 (m-11) and E-14 of the
// spec: the role and the external flag of a request carrying a user are the
// database's, read on every request by RequireSessionEpoch; the BFF's
// headers can be stale — a demotion does not end the session — so a header
// claiming admin for a user who is not, or claiming «not external» for an
// external user, must not open what the database closes. The router is the
// one main builds, so the middleware's mount and order are part of what is
// checked; the answer is judged by its status and body, never the headers.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

// sessionRoleRouter is the router main builds with the users routes wired
// (watchingWindowRouter leaves the users handler out).
func sessionRoleRouter(t *testing.T, pool *pgxpool.Pool) chi.Router {
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
	usersStore := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), "local", "", nil)
	projectsStore := projects.NewStore(pool)
	usersH := handler.NewUsersHandler(pool, usersStore, projectsStore)
	issuesStore := issues.NewStore(pool)
	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // authH…apiKeysH
		usersH,             // usersH
		nil, nil, nil, nil, // issuesMeH…dashboardH
		usersStore, issuesStore, dependencies.NewStore(pool), projectsStore,
		nil, nil, searchSvc, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil,
	)
	return router
}

func TestSessionRole_DatabaseOverridesStaleHeaders(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.NewTestPool(t)
	router := sessionRoleRouter(t, pool)

	request := func(method, path string, user uuid.UUID, headerRole string, headerExternal bool) (int, map[string]any) {
		t.Helper()
		var epoch int64
		if err := pool.QueryRow(ctx, `SELECT session_epoch FROM users WHERE id = $1`, user).Scan(&epoch); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		req.Header.Set(auth.HeaderUserID, user.String())
		req.Header.Set(auth.HeaderUserRole, headerRole)
		if headerExternal {
			req.Header.Set(auth.HeaderUserIsExternal, "true")
		}
		req.Header.Set(auth.HeaderSessionEpoch, strconv.FormatInt(epoch, 10))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	t.Run("a header claiming admin for a user", func(t *testing.T) {
		admin, _ := testsupport.SeedUser(t, pool, "admin")
		if code, body := request(http.MethodGet, "/api/v1/users", admin, "admin", false); code != http.StatusOK {
			t.Fatalf("control: the admin on an admin route: %d %v", code, body)
		}
		demoted, _ := testsupport.SeedUser(t, pool, "user")
		code, body := request(http.MethodGet, "/api/v1/users", demoted, "admin", false)
		if code != http.StatusForbidden || body["error"] != "not_admin" {
			t.Errorf("a user with a stale admin header on an admin route: %d %v, want 403 not_admin", code, body)
		}
	})

	t.Run("a header claiming «not external» for an external user", func(t *testing.T) {
		// The directory is narrowed for an external caller to the people he
		// shares a project with; a stranger is in nobody's project with him.
		// An internal caller sees the participants of his spaces (INV-9,
		// sk-674.18 T027): the stranger shares one with the internal user.
		stranger, strangerName := testsupport.SeedUser(t, pool, "user")
		internal, _ := testsupport.SeedUser(t, pool, "user")
		testsupport.AddSpaceMember(t, pool, testsupport.SeedSpace(t, pool, internal), stranger)
		external, _ := testsupport.SeedUser(t, pool, "user")
		if _, err := pool.Exec(ctx, `UPDATE users SET is_external = true WHERE id = $1`, external); err != nil {
			t.Fatal(err)
		}
		lists := func(user uuid.UUID) bool {
			t.Helper()
			code, body := request(http.MethodGet, "/api/v1/users/directory?q="+strangerName, user, "user", false)
			if code != http.StatusOK {
				t.Fatalf("directory: %d %v", code, body)
			}
			items, _ := body["items"].([]any)
			for _, it := range items {
				if m, _ := it.(map[string]any); m["username"] == strangerName {
					return true
				}
			}
			return false
		}
		if !lists(internal) {
			t.Fatal("control: an internal user's directory does not list the stranger")
		}
		if lists(external) {
			t.Error("an external user with a stale «not external» header sees the whole directory")
		}
	})
}
