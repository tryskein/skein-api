package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	mcptools "skein-api/internal/mcp/tools"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/monitorregistry"
	"skein-api/internal/notifications"
	"skein-api/internal/testsupport"
)

// Matrix row #151 (SC-008, integration API without skein-web, real schema):
// GET /agent/v1/monitor-loader under the /agent/v1 group as the server
// mounts it (MountAgentRoutes), next to the MCP tool built with the digest of
// the same loader handler, the way buildRouter wires them.

const loaderIntegSecret = "loader-integ-internal-secret"

// loaderIntegCaller is one user who can mint a monitor: an issue of their
// own project and an api key.
type loaderIntegCaller struct {
	user, key uuid.UUID
	issue     string
}

type loaderIntegFixture struct {
	pool    *pgxpool.Pool
	mcp     *server.MCPServer
	router  http.Handler
	loader  *AgentLoaderHandler
	callers [2]loaderIntegCaller
}

func newLoaderIntegFixture(t *testing.T) *loaderIntegFixture {
	t.Helper()
	url := scriptTestDBURL(t) // skips without TEST_DATABASE_URL
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	f := &loaderIntegFixture{pool: pool, loader: NewAgentLoaderHandler(mcptools.ServedLoader())}
	for i := range f.callers {
		user, _ := seedDashboardUser(t, pool, "user")
		project, prefix := seedDashboardProject(t, pool, user)
		f.callers[i] = loaderIntegCaller{
			user:  user,
			key:   seedScriptAPIKey(t, pool, user),
			issue: testsupport.SeedIssue(t, pool, project, user, prefix+"-1", "loader route", "open", 2),
		}
	}
	store := monitorcreds.New(pool, 6*time.Hour)
	script := NewAgentScriptHandler(2, 10*time.Second, 15*time.Second, 3*time.Minute, 6*time.Hour)
	f.mcp = server.NewMCPServer("monitor-loader", "1", server.WithToolCapabilities(true))
	mcptools.RegisterGetMonitorScript(f.mcp, pool, notifications.NewStore(pool), store, monitorregistry.MustEmbedded(),
		"http://skein-loader.test", script.SHA256(), f.loader.SHA256(), 6*time.Hour)
	r := chi.NewRouter()
	MountAgentRoutes(r, loaderIntegSecret, auth.RequireMonitorToken(store, "notifications:poll"),
		NewAgentPollHandler(pool, time.Second).Poll, script.Script, f.loader.Loader)
	f.router = r
	return f
}

// mint calls skein_get_monitor_script as the caller and returns its response.
func (f *loaderIntegFixture) mint(t *testing.T, c loaderIntegCaller, args map[string]any) map[string]any {
	t.Helper()
	ctx := auth.WithUserID(context.Background(), c.user)
	ctx = auth.WithUserRole(ctx, "user")
	ctx = auth.WithSessionID(ctx, "loader-integ")
	ctx = auth.WithAPIKeyID(ctx, c.key)
	ctx = auth.WithKeyGrant(ctx, apikeys.Grant{Level: apikeys.LevelManage, Scope: apikeys.Scope{Mode: apikeys.ScopeAll}})
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "skein_get_monitor_script", "arguments": args}})
	raw, _ := json.Marshal(f.mcp.HandleMessage(ctx, msg))
	var r struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Result.Content) == 0 {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if r.Result.IsError {
		t.Fatalf("skein_get_monitor_script refused: %s", r.Result.Content[0].Text)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &body); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	return body
}

var loaderDigestInCommand = regexp.MustCompile(`SKEIN_LOADER_SHA256='([0-9a-f]{64})'`)

// loaderDigestOf returns the SKEIN_LOADER_SHA256 value of the start command
// in field of the response.
func loaderDigestOf(t *testing.T, body map[string]any, field string) string {
	t.Helper()
	command, _ := body[field].(string)
	m := loaderDigestInCommand.FindStringSubmatch(command)
	if m == nil {
		t.Fatalf("the %s command carries no SKEIN_LOADER_SHA256:\n%s", field, command)
	}
	return m[1]
}

func TestAgentLoaderInteg_BodyDigestIsTheStartCommandDigest(t *testing.T) {
	f := newLoaderIntegFixture(t)
	withoutClient := loaderDigestOf(t, f.mint(t, f.callers[0], map[string]any{"id": f.callers[0].issue}), "bootstrap")
	withClient := loaderDigestOf(t, f.mint(t, f.callers[1],
		map[string]any{"id": f.callers[1].issue, "client": "loader-integ-agent"}), "preflight")

	var bodies [2]string
	for i, c := range f.callers {
		rec := loaderGet(f.router, "/agent/v1/monitor-loader", map[string]string{
			auth.HeaderInternalSecret: loaderIntegSecret,
			auth.HeaderUserID:         c.user.String(),
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("caller %d: status = %d, want 200 (body=%q)", i, rec.Code, rec.Body.String())
		}
		bodies[i] = rec.Body.String()
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("two users got different loader bodies (%d and %d bytes)", len(bodies[0]), len(bodies[1]))
	}
	sum := sha256.Sum256([]byte(bodies[0]))
	got := hex.EncodeToString(sum[:])
	if got != withoutClient {
		t.Errorf("SHA-256 of the served loader = %s, bootstrap without client names %s", got, withoutClient)
	}
	if got != withClient {
		t.Errorf("SHA-256 of the served loader = %s, preflight with client names %s", got, withClient)
	}
}

func TestAgentLoaderInteg_ChangesNoState(t *testing.T) {
	f := newLoaderIntegFixture(t)
	c := f.callers[0]
	token, _ := f.mint(t, c, map[string]any{"id": c.issue})["token"].(string)
	var tokenID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM monitor_credentials WHERE user_id = $1`, c.user).Scan(&tokenID); err != nil {
		t.Fatalf("the minted credential: %v", err)
	}
	type snapshot struct {
		credentials, watches int
		lastUsed             *time.Time
	}
	take := func() snapshot {
		t.Helper()
		var s snapshot
		ctx := context.Background()
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM monitor_credentials WHERE user_id = $1`, c.user).Scan(&s.credentials); err != nil {
			t.Fatalf("count credentials: %v", err)
		}
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM issue_watchers WHERE user_id = $1`, c.user).Scan(&s.watches); err != nil {
			t.Fatalf("count watches: %v", err)
		}
		if err := f.pool.QueryRow(ctx, `SELECT last_used_at FROM monitor_credentials WHERE id = $1`, tokenID).Scan(&s.lastUsed); err != nil {
			t.Fatalf("last_used_at: %v", err)
		}
		return s
	}
	before := take()
	// Without the monitor token and with the live one: the route reads
	// neither, so the token is not renewed either.
	for _, headers := range []map[string]string{
		{auth.HeaderInternalSecret: loaderIntegSecret},
		{auth.HeaderInternalSecret: loaderIntegSecret, auth.HeaderAPIKey: token},
	} {
		if rec := loaderGet(f.router, "/agent/v1/monitor-loader", headers); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	after := take()
	if before.credentials != after.credentials || before.watches != after.watches {
		t.Errorf("rows changed: credentials %d -> %d, watches %d -> %d",
			before.credentials, after.credentials, before.watches, after.watches)
	}
	if (before.lastUsed == nil) != (after.lastUsed == nil) ||
		(before.lastUsed != nil && !before.lastUsed.Equal(*after.lastUsed)) {
		t.Errorf("last_used_at changed: %v -> %v", before.lastUsed, after.lastUsed)
	}
}

func TestAgentLoaderInteg_TokenRoutesStillNeedTheToken(t *testing.T) {
	f := newLoaderIntegFixture(t)
	for _, p := range []string{"/agent/v1/notifications/poll", "/agent/v1/monitor-script"} {
		rec := loaderGet(f.router, p, map[string]string{auth.HeaderInternalSecret: loaderIntegSecret})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a monitor token: status = %d, want 401", p, rec.Code)
		}
	}
}

func TestAgentLoaderInteg_NoInternalSecretIs401OnEveryRoute(t *testing.T) {
	f := newLoaderIntegFixture(t)
	c := f.callers[0]
	// A live monitor token: only the missing secret can refuse.
	token, _ := f.mint(t, c, map[string]any{"id": c.issue})["token"].(string)
	for _, p := range []string{"/agent/v1/monitor-loader", "/agent/v1/notifications/poll", "/agent/v1/monitor-script"} {
		rec := loaderGet(f.router, p, map[string]string{auth.HeaderAPIKey: token})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without the internal secret: status = %d, want 401", p, rec.Code)
		}
	}
}
