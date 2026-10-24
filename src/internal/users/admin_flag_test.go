package users_test

// admin_flag_test.go — sk-674.18 T037, matrix row 116 (INV-20, SC-006).
// Reference — INV-20 of the spec, written before the code: while
// SKEIN_INSTANCE_ADMIN_ENABLED=false (instance_settings.admin_enabled) no
// path gives the admin role — POST /users, the user edit, MCP skein_user
// create/update, the SSO provisioning (a default role of admin and the
// bootstrap e-mail alike) — in every login mode. Each path is asked with
// the flag off (no admin appears; the REST and MCP refusals carry
// admin_role_disabled) and, as the control, with the flag on (the same call
// gives the admin role), so a path that does not check is red.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
	mcptools "skein-api/internal/mcp/tools"
	"skein-api/internal/projects"
	"skein-api/internal/spaces"
	"skein-api/internal/testsupport"
	"skein-api/internal/users"
)

// setAdminEnabled sets instance_settings.admin_enabled for the test and
// restores the previous value at cleanup.
func setAdminEnabled(t *testing.T, pool *pgxpool.Pool, on bool) {
	t.Helper()
	ctx := context.Background()
	var prev *bool
	_ = pool.QueryRow(ctx, `SELECT admin_enabled FROM instance_settings WHERE id = 1`).Scan(&prev)
	if _, err := pool.Exec(ctx, `
INSERT INTO instance_settings (id, admin_enabled, common_space_enabled, updated_at)
VALUES (1, $1, false, now())
ON CONFLICT (id) DO UPDATE SET admin_enabled = EXCLUDED.admin_enabled, updated_at = now()`, on); err != nil {
		t.Fatalf("set admin_enabled: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if prev == nil {
			_, _ = pool.Exec(c, `DELETE FROM instance_settings WHERE id = 1`)
			return
		}
		_, _ = pool.Exec(c, `UPDATE instance_settings SET admin_enabled = $1 WHERE id = 1`, *prev)
	})
}

// forgetUserByEmail removes, at cleanup, a user a path created (and his
// "Personal").
func forgetUserByEmail(t *testing.T, pool *pgxpool.Pool, email string) {
	t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM space_members WHERE space_id IN (SELECT s.id FROM spaces s JOIN users u ON u.id = s.owner_id WHERE lower(u.email) = lower($1))`,
			`DELETE FROM space_members WHERE user_id IN (SELECT id FROM users WHERE lower(email) = lower($1))`,
			`DELETE FROM spaces WHERE owner_id IN (SELECT id FROM users WHERE lower(email) = lower($1))`,
			`DELETE FROM users WHERE lower(email) = lower($1)`,
		} {
			_, _ = pool.Exec(c, q, email)
		}
	})
}

func roleOf(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	var role string
	if err := pool.QueryRow(context.Background(), `SELECT role FROM users WHERE lower(email) = lower($1)`, email).Scan(&role); err != nil {
		return ""
	}
	return role
}

func TestAdminFlag_NoPathGivesTheAdminRole(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	admin, adminName := testsupport.SeedUser(t, pool, "admin")
	spacesSvc := spaces.NewService(pool, spaces.Options{Creation: spaces.CreationAll})

	rest := func(store *users.Store) func(method, path string, body any) (int, map[string]any) {
		h := handler.NewUsersHandler(pool, store, projects.NewStore(pool))
		r := chi.NewRouter()
		r.With(handler.ParseUserHeaders, handler.RequireUser, handler.RequireAdmin).Post("/users", h.Create)
		r.With(handler.ParseUserHeaders, handler.RequireUser, handler.RequireAdmin).Patch("/users/{id}", h.Update)
		return func(method, path string, body any) (int, map[string]any) {
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest(method, path, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.HeaderUserID, admin.String())
			req.Header.Set(auth.HeaderUsername, adminName)
			req.Header.Set(auth.HeaderUserRole, "admin")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec.Code, out
		}
	}
	mcp := func(store *users.Store) func(args map[string]any) (bool, map[string]any) {
		s := server.NewMCPServer("admin-flag", "1", server.WithToolCapabilities(true))
		mcptools.RegisterUser(s, store, pool, spacesSvc)
		ctx := auth.WithAPIKeyID(auth.WithUserRole(auth.WithUserID(context.Background(), admin), "admin"), uuid.New())
		return func(args map[string]any) (bool, map[string]any) {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "skein_user", "arguments": args}})
			resp := s.HandleMessage(ctx, raw)
			out, _ := json.Marshal(resp)
			var msg struct {
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(out, &msg); err != nil || len(msg.Result.Content) == 0 {
				t.Fatalf("skein_user: %s", out)
			}
			var body map[string]any
			_ = json.Unmarshal([]byte(msg.Result.Content[0].Text), &body)
			return msg.Result.IsError, body
		}
	}
	newEmail := func(tag string) string {
		email := tag + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "@flag.test"
		forgetUserByEmail(t, pool, email)
		return email
	}
	username := func(email string) string { return strings.SplitN(email, "@", 2)[0] }

	for _, mode := range []string{"local", "keycloak", "hybrid"} {
		store := users.NewStore(pool, auth.MinBcryptCost, apikeys.New(pool), mode, "", nil)
		for _, on := range []bool{false, true} {
			want := "user"
			if on {
				want = "admin"
			}
			t.Run(mode+map[bool]string{false: " flag off", true: " flag on (control)"}[on], func(t *testing.T) {
				setAdminEnabled(t, pool, on)

				t.Run("POST /users", func(t *testing.T) {
					email := newEmail("rc")
					code, body := rest(store)(http.MethodPost, "/users", map[string]any{
						"email": email, "username": username(email), "name": "Flag", "password": "long-enough-pass-1", "role": "admin"})
					if !on && (code != http.StatusUnprocessableEntity || body["error"] != spaces.CodeAdminRoleDisabled) {
						t.Errorf("%d %v, want 422 %s", code, body, spaces.CodeAdminRoleDisabled)
					}
					if got := roleOf(t, pool, email); got != map[bool]string{false: "", true: "admin"}[on] {
						t.Errorf("stored role %q", got)
					}
				})

				t.Run("PATCH /users/{id}", func(t *testing.T) {
					u, _ := testsupport.SeedUser(t, pool, "user")
					code, body := rest(store)(http.MethodPatch, "/users/"+u.String(), map[string]any{"role": "admin"})
					if !on && (code != http.StatusUnprocessableEntity || body["error"] != spaces.CodeAdminRoleDisabled) {
						t.Errorf("%d %v, want 422 %s", code, body, spaces.CodeAdminRoleDisabled)
					}
					var role string
					_ = pool.QueryRow(context.Background(), `SELECT role FROM users WHERE id = $1`, u).Scan(&role)
					if role != want {
						t.Errorf("stored role %q, want %q", role, want)
					}
				})

				t.Run("skein_user create", func(t *testing.T) {
					email := newEmail("mc")
					isErr, body := mcp(store)(map[string]any{"action": "create",
						"email": email, "username": username(email), "name": "Flag", "password": "long-enough-pass-1", "role": "admin"})
					if !on && (!isErr || body["error"] != spaces.CodeAdminRoleDisabled) {
						t.Errorf("%v, want %s", body, spaces.CodeAdminRoleDisabled)
					}
					if got := roleOf(t, pool, email); got != map[bool]string{false: "", true: "admin"}[on] {
						t.Errorf("stored role %q", got)
					}
				})

				t.Run("skein_user update", func(t *testing.T) {
					u, _ := testsupport.SeedUser(t, pool, "user")
					isErr, body := mcp(store)(map[string]any{"action": "update", "userId": u.String(), "role": "admin"})
					if !on && (!isErr || body["error"] != spaces.CodeAdminRoleDisabled) {
						t.Errorf("%v, want %s", body, spaces.CodeAdminRoleDisabled)
					}
					var role string
					_ = pool.QueryRow(context.Background(), `SELECT role FROM users WHERE id = $1`, u).Scan(&role)
					if role != want {
						t.Errorf("stored role %q, want %q", role, want)
					}
				})
			})
		}
	}

	// SSO provisioning — one provisioner for every login mode; the role
	// comes from its default role or the bootstrap e-mail.
	for _, on := range []bool{false, true} {
		want := map[bool]string{false: "user", true: "admin"}[on]
		t.Run("SSO"+map[bool]string{false: " flag off", true: " flag on (control)"}[on], func(t *testing.T) {
			setAdminEnabled(t, pool, on)
			defaultAdmin := newEmail("sd")
			res, err := auth.NewProvisioner(pool, true, "admin", "").Provision(context.Background(),
				auth.ProvisionInput{Email: defaultAdmin, Username: username(defaultAdmin)})
			if err != nil {
				t.Fatalf("provision with the default role admin: %v", err)
			}
			if res.Role != want || roleOf(t, pool, defaultAdmin) != want {
				t.Errorf("the default role admin gave %q (stored %q), want %q", res.Role, roleOf(t, pool, defaultAdmin), want)
			}
			bootstrap := newEmail("sb")
			res, err = auth.NewProvisioner(pool, true, "user", bootstrap).Provision(context.Background(),
				auth.ProvisionInput{Email: bootstrap, Username: username(bootstrap)})
			if err != nil {
				t.Fatalf("provision the bootstrap e-mail: %v", err)
			}
			if res.Role != want || roleOf(t, pool, bootstrap) != want {
				t.Errorf("the bootstrap e-mail gave %q (stored %q), want %q", res.Role, roleOf(t, pool, bootstrap), want)
			}
		})
	}
}
