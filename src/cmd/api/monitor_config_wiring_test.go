package main

// monitor_config_wiring_test.go — sk-579 test matrix row 4 (SC-002, INV-1,
// INV-6, US-2, US-5): the monitor knobs read by config.Load reach the
// behaviour of the PRODUCTION router. The router is buildRouter over a real
// database and a cfg that comes from config.Load() with legal non-default
// values in the environment — no duration literal is handed to a constructor
// past Load. Every value is observed through what a client sees: the tool
// description, the stored token lifetimes, and the waits of the runner body
// the router serves, executed by python3. The threshold's second consumer,
// the monitorStalled hint, is not registered since sk-669; its subtests are
// skipped (see skipStallHint), and the threshold stays observed through the
// runner's degraded line.
//
// Time is moved by backdating stamps in the database, and for the runner by
// the virtual clock of internal/handler/testdata/runner_site/sitecustomize.py
// — never by waiting.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/notifications"
	"skein-api/internal/projects"
	"skein-api/internal/search"
	"skein-api/internal/testsupport"
)

const wiringInternalSecret = "wiring-test-internal-secret"

// wiringSessionEpoch is the X-Skein-Session-Epoch the wiring tests send with
// their user headers (sk-674.18 K-16, handler.RequireSessionEpoch): a seeded
// user has the column default, epoch 0, and nothing in these tests revokes it.
const wiringSessionEpoch = "0"

// wiringStack is the production router built from config.Load, plus the
// identity a client of it uses.
type wiringStack struct {
	t       *testing.T
	pool    *pgxpool.Pool
	cfg     *config.Config
	router  chi.Router
	owner   uuid.UUID
	project uuid.UUID
	root    string
	keyID   uuid.UUID
	key     string // plaintext ska_ key
}

// newWiringStack loads cfg from the environment the caller set and builds
// the router with buildRouter. Only the dependencies the exercised doors
// reach are real; the rest are nil, as in newRouterWithOAuthServers.
func newWiringStack(t *testing.T) *wiringStack {
	t.Helper()
	pool := testsupport.NewTestPool(t)

	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-wiring.test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	owner, _ := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "mcw")
	root := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "wiring", "open", 2)

	plain, keyPrefix, hash, err := auth.GenerateAPIKey(cfg.BcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	keyID, _, err := apikeys.New(pool).Create(context.Background(), owner, "wiring test", keyPrefix, hash)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	// Registered after the user and the project, so it runs first: the
	// credentials the key minted go with it (ON DELETE CASCADE).
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, keyID)
		_, _ = pool.Exec(ctx, `DELETE FROM agents WHERE user_id = $1`, owner)
	})

	searchSvc, err := search.NewService(nil, nil, 0)
	if err != nil {
		t.Fatalf("search.NewService: %v", err)
	}
	router, _ := buildRouter(
		pool, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // REST handlers authH…dashboardH
		nil,                                         // usersStore
		issues.NewStore(pool),                       // issuesStore
		dependencies.NewStore(pool),                 // depsStore
		projects.NewStore(pool),                     // projectsStore
		notifications.NewStore(pool),                // notifStore — the monitor tool's auto-watch
		nil,                                         // remindersStore
		searchSvc,                                   // searchSvc
		nil,                                         // s3Client
		nil, nil, nil, nil, nil, nil, nil, nil, nil, // attachmentsH…pushH
	)
	return &wiringStack{t: t, pool: pool, cfg: cfg, router: router, owner: owner,
		project: project, root: root, keyID: keyID, key: plain}
}

func (s *wiringStack) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("exec %q: %v", sql, err)
	}
}

// rpc posts one JSON-RPC message to /mcp and returns the response and the
// session id the server answered with.
func (s *wiringStack) rpc(session, method string, params any) (map[string]any, string) {
	s.t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		s.t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	req.Header.Set(auth.HeaderAPIKey, s.key)
	// The BFF stamps the trusted surface; an internal key is refused without it.
	req.Header.Set(auth.HeaderSurface, auth.SurfaceInternal)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		s.t.Fatalf("POST /mcp %s = %d: %s", method, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		s.t.Fatalf("%s response is not JSON: %v\n%s", method, err, rec.Body.String())
	}
	if out["error"] != nil {
		s.t.Fatalf("%s JSON-RPC error: %v", method, out["error"])
	}
	return out, rec.Header().Get("Mcp-Session-Id")
}

// initialize opens a fresh MCP session and returns its id.
func (s *wiringStack) initialize() string {
	s.t.Helper()
	_, sid := s.rpc("", "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "wiring-test", "version": "1"},
	})
	if sid == "" {
		s.t.Fatalf("initialize returned no Mcp-Session-Id")
	}
	return sid
}

// callTool runs one tool and returns the text of every content block.
func (s *wiringStack) callTool(session, name string, args map[string]any) []string {
	s.t.Helper()
	out, _ := s.rpc(session, "tools/call", map[string]any{"name": name, "arguments": args})
	result, _ := out["result"].(map[string]any)
	if result == nil {
		s.t.Fatalf("tools/call %s: no result: %v", name, out)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		s.t.Fatalf("tools/call %s: isError: %v", name, result["content"])
	}
	content, _ := result["content"].([]any)
	var texts []string
	for _, c := range content {
		block, _ := c.(map[string]any)
		if text, ok := block["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

// skipStallHint skips a subtest that observes the monitorStalled hint: the
// owner disabled the layer (sk-669) and mcpRegisterToolsFn no longer
// registers it, so the production router never shows the hint.
// TestMCPRegisterTools_NoMonitorStalled asserts that absence.
func skipStallHint(t *testing.T) {
	t.Helper()
	t.Skip("sk-669: the monitorStalled layer is not registered on the production MCP server")
}

// stalledItem returns the monitorStalled item for monitorID in a tool
// response, wherever the hint was placed (inside a JSON object or as an
// extra block).
func stalledItem(t *testing.T, texts []string, monitorID uuid.UUID) (map[string]any, bool) {
	t.Helper()
	for _, text := range texts {
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) != nil {
			continue
		}
		items, _ := obj["monitorStalled"].([]any)
		for _, it := range items {
			item, _ := it.(map[string]any)
			if item["monitorId"] == monitorID.String() {
				return item, true
			}
		}
	}
	return nil, false
}

// seconds reads an interval expression over one monitor_credentials row.
func (s *wiringStack) seconds(expr string, monitorID uuid.UUID) float64 {
	s.t.Helper()
	var v float64
	err := s.pool.QueryRow(context.Background(),
		`SELECT EXTRACT(EPOCH FROM `+expr+`)::float8 FROM monitor_credentials WHERE id = $1`, monitorID).Scan(&v)
	if err != nil {
		s.t.Fatalf("read %s: %v", expr, err)
	}
	return v
}

func TestMonitorConfigWiring_LoadReachesBehaviour(t *testing.T) {
	t.Setenv("SKEIN_MONITOR_TOKEN_IDLE_TTL", "7h")
	t.Setenv("SKEIN_MONITOR_STALL_THRESHOLD", "4m")
	t.Setenv("SKEIN_MONITOR_RUNNER_POLL_INTERVAL", "3s")
	s := newWiringStack(t)

	issuing := s.initialize()

	// --- the renewal window (SC-002, INV-1, US-2) ---------------------------
	listed, _ := s.rpc(issuing, "tools/list", map[string]any{})
	var description string
	toolsList, _ := listed["result"].(map[string]any)["tools"].([]any)
	for _, raw := range toolsList {
		tool, _ := raw.(map[string]any)
		if tool["name"] == "skein_get_monitor_script" {
			description, _ = tool["description"].(string)
		}
	}
	if description == "" {
		t.Fatalf("skein_get_monitor_script not in tools/list")
	}
	if !strings.Contains(description, "7h") {
		t.Errorf("description does not quote the configured 7h window:\n%s", description)
	}
	for _, stale := range []string{"10m", "10-minute"} {
		if strings.Contains(description, stale) {
			t.Errorf("description still quotes %q:\n%s", stale, description)
		}
	}

	minted := s.callTool(issuing, "skein_get_monitor_script", map[string]any{"id": s.root})
	var mint struct {
		MonitorID string `json:"monitorId"`
		Token     string `json:"token"`
		Bootstrap string `json:"bootstrap"`
	}
	if err := json.Unmarshal([]byte(minted[0]), &mint); err != nil {
		t.Fatalf("mint response: %v\n%s", err, minted[0])
	}
	monitorID := uuid.MustParse(mint.MonitorID)
	if got := s.seconds("expires_at - created_at", monitorID); got != (7 * time.Hour).Seconds() {
		t.Errorf("minted expires_at - created_at = %vs, want 7h", got)
	}

	poll := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil)
	poll.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	poll.Header.Set(auth.HeaderAPIKey, mint.Token)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, poll)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll = %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.seconds("expires_at - last_used_at", monitorID); got != (7 * time.Hour).Seconds() {
		t.Errorf("polled expires_at - last_used_at = %vs, want 7h", got)
	}

	// --- the stall threshold (INV-6, US-5): 3m30s is between the 3m
	// default and the configured 4m; 5m is past both ----------------------
	// sk-669: the hint layer is off, so these three are skipped; they stay
	// for the day the owner turns it back on.
	t.Run("issuing session: last success", func(t *testing.T) {
		skipStallHint(t)
		s.exec(`UPDATE monitor_credentials SET last_poll_succeeded_at = now() - interval '3 minutes 30 seconds' WHERE id = $1`, monitorID)
		if _, ok := stalledItem(t, s.callTool(issuing, "skein_get_instructions", map[string]any{}), monitorID); ok {
			t.Errorf("success 3m30s ago (threshold 4m): hint present")
		}
		s.exec(`UPDATE monitor_credentials SET last_poll_succeeded_at = now() - interval '5 minutes' WHERE id = $1`, monitorID)
		item, ok := stalledItem(t, s.callTool(issuing, "skein_get_instructions", map[string]any{}), monitorID)
		if !ok {
			t.Fatalf("success 5m ago (threshold 4m): hint absent")
		}
		if item["mine"] != true {
			t.Errorf("issuing session's item mine = %v, want true", item["mine"])
		}
	})

	t.Run("other session: issuing session silence", func(t *testing.T) {
		skipStallHint(t)
		other := s.initialize()
		// 4m30s: past the 3m default + 1m granularity, inside the configured
		// 4m + 1m — only a threshold taken from Load hides the item.
		s.exec(`UPDATE agents SET last_seen_at = now() - interval '4 minutes 30 seconds' WHERE user_id = $1 AND session_id = $2`, s.owner, issuing)
		if _, ok := stalledItem(t, s.callTool(other, "skein_get_instructions", map[string]any{}), monitorID); ok {
			t.Errorf("issuing session silent 4m30s (threshold 4m + granularity 1m): other session sees the item")
		}
		// Past threshold + the 60s activity-clock granularity (INV-8).
		s.exec(`UPDATE agents SET last_seen_at = now() - interval '5 minutes 30 seconds' WHERE user_id = $1 AND session_id = $2`, s.owner, issuing)
		item, ok := stalledItem(t, s.callTool(other, "skein_get_instructions", map[string]any{}), monitorID)
		if !ok {
			t.Fatalf("issuing session silent 5m30s (threshold 4m + granularity 1m): other session does not see the item")
		}
		if item["mine"] != false {
			t.Errorf("other session's item mine = %v, want false", item["mine"])
		}
	})

	t.Run("other session: empty issuing session, sessionless clock", func(t *testing.T) {
		skipStallHint(t)
		// A monitor minted by a client without Mcp-Session-Id: /mcp cannot
		// carry such a call (mcp-go rejects a non-initialize request without
		// a session), so it is minted through the store directly.
		_, cred, err := monitorcreds.New(s.pool, s.cfg.MonitorTokenIdleTTL).Mint(context.Background(), monitorcreds.MintParams{
			UserID: s.owner, MintedByAPIKeyID: s.keyID, Audience: "notifications:poll", Capability: "read",
			ProjectID: s.project, ScopeIssueID: s.root, EventTypes: []string{"comment.created"},
			IssuedSessionID: "", TokenFilePath: "/tmp/skein-mon-wiring.token",
		})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		s.exec(`UPDATE monitor_credentials SET created_at = now() - interval '10 minutes' WHERE id = $1`, cred.ID)
		other := s.initialize()
		s.exec(`UPDATE api_keys SET sessionless_seen_at = now() - interval '4 minutes 30 seconds' WHERE id = $1`, s.keyID)
		if _, ok := stalledItem(t, s.callTool(other, "skein_get_instructions", map[string]any{}), cred.ID); ok {
			t.Errorf("sessionless activity 4m30s ago (threshold 4m + granularity 1m): item visible")
		}
		s.exec(`UPDATE api_keys SET sessionless_seen_at = now() - interval '5 minutes 30 seconds' WHERE id = $1`, s.keyID)
		item, ok := stalledItem(t, s.callTool(other, "skein_get_instructions", map[string]any{}), cred.ID)
		if !ok {
			t.Fatalf("sessionless activity 5m30s ago (threshold 4m + granularity 1m): item not visible")
		}
		if item["mine"] != false {
			t.Errorf("empty-session monitor shown to a session: mine = %v, want false", item["mine"])
		}
	})

	// --- the runner timings (INV-6, US-1): the body this router serves ----
	script := httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil)
	script.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
	script.Header.Set(auth.HeaderAPIKey, mint.Token)
	rec = httptest.NewRecorder()
	s.router.ServeHTTP(rec, script)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agent/v1/monitor-script = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	limit := config.RunnerTimings(s.cfg.MonitorRunnerPollInterval).RequestLimit

	t.Run("runner: cadence after a delivery on the accumulated backoff", func(t *testing.T) {
		sleepLog := filepath.Join(t.TempDir(), "sleeps")
		var delivered atomic.Int32 // number of sleeps logged when the delivery went out; 0 = not yet
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Deliver once the logged sleeps reached the 15s ceiling.
			if delivered.Load() == 0 {
				sleeps := readSleeps(sleepLog)
				if n := len(sleeps); n > 0 && sleeps[n-1] == 15 {
					delivered.Store(int32(n))
					wiringWritePoll(w, []map[string]any{{"id": 1, "type": "comment.created"}})
					return
				}
			}
			wiringWritePoll(w, []map[string]any{})
		}))
		defer srv.Close()

		run := runWiringRunner(t, body, srv.URL, 3*time.Minute, sleepLog)
		k := int(delivered.Load())
		if k == 0 {
			t.Fatalf("the empty-poll sleeps never reached the 15s ceiling: %v", run.sleeps)
		}
		if len(run.sleeps) <= k {
			t.Fatalf("no sleep after the delivery: %v", run.sleeps)
		}
		for i := 1; i < k; i++ {
			if run.sleeps[i] < run.sleeps[i-1] {
				t.Errorf("empty-poll sleeps decrease: %v", run.sleeps[:k])
			}
		}
		if run.sleeps[k] != 3 {
			t.Errorf("sleep right after the delivery = %v, want the 3s cadence (sleeps %v)", run.sleeps[k], run.sleeps)
		}
	})

	t.Run("runner: failures saturate at the ceiling", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		run := runWiringRunner(t, body, srv.URL, 150*time.Second, "")
		if len(run.sleeps) < 4 {
			t.Fatalf("want several sleeps, got %v", run.sleeps)
		}
		if last := run.sleeps[len(run.sleeps)-1]; last != 15 {
			t.Errorf("saturated 503 sleep = %v, want 15 = max(5·3, 10) (sleeps %v)", last, run.sleeps)
		}
	})

	t.Run("runner: degraded follows the configured threshold", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		if got := runWiringRunner(t, body, srv.URL, 3*time.Minute+30*time.Second, "").events("degraded"); len(got) != 0 {
			t.Errorf("degraded within 3m30s of failures (threshold 4m): %v", got)
		}
		if got := runWiringRunner(t, body, srv.URL, 4*time.Minute+30*time.Second, "").events("degraded"); len(got) != 1 {
			t.Errorf("want exactly one degraded after 4m30s of failures, got %v", got)
		}
	})

	t.Run("runner: a request hung in name resolution is cut at the limit", func(t *testing.T) {
		url := "http://127.0.0.1:9" // never contacted: resolution blocks forever
		alive := runWiringRunner(t, body, url, limit-time.Second, "",
			"SKEIN_TEST_DNS_LOG="+filepath.Join(t.TempDir(), "dns")).probe
		if alive != "alive" {
			t.Errorf("first executor at L-1s virtual (L=%s) = %q, want alive", limit, alive)
		}
		dead := runWiringRunner(t, body, url, limit+time.Second, "",
			"SKEIN_TEST_DNS_LOG="+filepath.Join(t.TempDir(), "dns")).probe
		if dead != "dead" {
			t.Errorf("first executor at L+1s virtual (L=%s) = %q, want dead", limit, dead)
		}
	})
}

// wiringRunnerRun is the observable outcome of one runner process.
type wiringRunnerRun struct {
	lines  []map[string]any // stdout, one JSON object per line
	sleeps []float64        // runner waits, in order (virtual seconds)
	probe  string           // "alive" / "dead" for the first executor, if probed
}

func (r wiringRunnerRun) events(name string) []map[string]any {
	var out []map[string]any
	for _, l := range r.lines {
		if l["event"] == name {
			out = append(out, l)
		}
	}
	return out
}

func readSleeps(path string) []float64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []float64
	for _, f := range strings.Fields(string(raw)) {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func wiringWritePoll(w http.ResponseWriter, notifications []map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"notifications": notifications,
		"cursor":        map[string]any{"ts": "2026-01-01T00:00:00Z", "id": 1},
	})
}

// runWiringRunner executes a served runner body with python3 under the
// test-only sitecustomize (virtual clock, random pinned to 1.0) against
// serverURL until the virtual budget runs out. sleepLog may be "" for a
// fresh one. Without python3 the calling test is skipped.
func runWiringRunner(t *testing.T, body []byte, serverURL string, budget time.Duration, sleepLog string, extraEnv ...string) wiringRunnerRun {
	t.Helper()
	py, err := osexec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	site, err := filepath.Abs(filepath.Join("..", "..", "internal", "handler", "testdata", "runner_site"))
	if err != nil {
		t.Fatalf("site dir: %v", err)
	}
	dir := t.TempDir()
	if sleepLog == "" {
		sleepLog = filepath.Join(dir, "sleeps")
	}
	runnerPath := filepath.Join(dir, "runner.py")
	tokenPath := filepath.Join(dir, "tok")
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(runnerPath, body, 0o600); err != nil {
		t.Fatalf("write runner: %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte("skm_"+strings.Repeat("a", 43)), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := osexec.CommandContext(ctx, py, runnerPath)
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+site,
		"SKEIN_TEST_SITE=1",
		"SKEIN_TEST_VCLOCK=1",
		"SKEIN_TEST_RANDOM=1.0",
		"SKEIN_TEST_BUDGET_SEC="+strconv.FormatFloat(budget.Seconds(), 'f', -1, 64),
		"SKEIN_TEST_SLEEP_LOG="+sleepLog,
		"SKEIN_TEST_PROBE_FILE="+probe,
		"SKEIN_PUBLIC_URL="+serverURL,
		"SKEIN_TOKEN_FILE="+tokenPath,
		"SKEIN_ISSUE=zz-77",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("runner: %v\nstderr:\n%s", err, stderr.String())
	}

	run := wiringRunnerRun{sleeps: readSleeps(sleepLog)}
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			t.Fatalf("stdout line is not JSON: %q", sc.Text())
		}
		run.lines = append(run.lines, obj)
	}
	if raw, err := os.ReadFile(probe); err == nil {
		run.probe = string(raw)
	}
	return run
}
