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

	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/dependencies"
	"skein-api/internal/issues"
	"skein-api/internal/monitorcreds"
	"skein-api/internal/monitorregistry"
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
	return newWiringStackWith(t, "http://skein-wiring.test", nil)
}

// newWiringStackWith is newWiringStack with the public agent URL the mint
// bakes into its commands and, when registryDoc is not nil, the monitor
// registry the served runner carries, validated by buildServingRouter as at
// startup (the seam of buildRouterWithRegistry).
func newWiringStackWith(t *testing.T, publicURL string, registryDoc []byte) *wiringStack {
	t.Helper()
	pool := testsupport.NewTestPool(t)

	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", publicURL)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	owner, _ := testsupport.SeedUser(t, pool, "user")
	project, prefix := testsupport.SeedProject(t, pool, owner, "mcw")
	root := testsupport.SeedIssue(t, pool, project, owner, prefix+"-1", "wiring", "open", 2)

	plain, keyPrefix, hash, err := auth.GenerateAPIKey(auth.MinBcryptCost)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	keyID, _, err := testsupport.CreateOwnerKey(context.Background(), pool, owner, "wiring test", keyPrefix, hash)
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
	build := func(doc []byte) (chi.Router, func()) {
		return buildRouterWithRegistry(doc,
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
	}
	var router chi.Router
	if registryDoc == nil {
		router, _ = build(nil)
	} else {
		var err error
		router, _, err = buildServingRouter(registryDoc, func(_ *monitorregistry.Registry, doc []byte) (chi.Router, func()) {
			return build(doc)
		})
		if err != nil {
			t.Fatalf("buildServingRouter: %v", err)
		}
	}
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

// wiringChannelID is the channel of the fixture registry of the client-path
// test: an own delivery whose program refuses before sending.
const wiringChannelID = "wiring-queue"

// wiringClientRegistry is a valid registry document with one agent record
// ("alpha") whose method is the delivery by wiringChannelID; the channel's
// program (on PATH) answers exit code 3, a refusal before send.
func wiringClientRegistry(t *testing.T) []byte {
	t.Helper()
	exit0, exit3 := 0, 3
	reg := monitorregistry.Registry{
		SchemaVersion: 1,
		General:       monitorregistry.General{About: "general record", Source: "fixture", Warnings: []string{"not found"}},
		Agents: []monitorregistry.Agent{{
			ID: "alpha", About: "fixture agent", Source: "fixture",
			Process: monitorregistry.ProcessSign{Executables: []string{"alpha"}},
			Methods: []monitorregistry.Method{
				{Kind: monitorregistry.MethodDelivery, Channel: wiringChannelID, Instruction: "run detached"},
			},
		}},
		Channels: []monitorregistry.Channel{{
			ID: wiringChannelID, About: "own delivery", Source: "fixture",
			Step: monitorregistry.StepOwnDelivery, Order: 1, Kind: monitorregistry.KindAddressed,
			Detect: monitorregistry.Conditions{Env: []monitorregistry.EnvCondition{{Name: "WIRING_QUEUE_ID", Pattern: "[a-z0-9-]+"}}},
			Executor: monitorregistry.Executor{Type: monitorregistry.ExecutorArgv,
				Argv: []string{"wiringq", "{env:WIRING_QUEUE_ID}"}, Feed: monitorregistry.FeedStdin, TimeoutSec: 30},
			Idle: monitorregistry.Idle{Type: monitorregistry.IdleNone},
			Outcomes: []monitorregistry.OutcomeRow{
				{Match: monitorregistry.Match{ExitCode: &exit0}, Outcome: monitorregistry.OutcomeQueued},
				{Match: monitorregistry.Match{ExitCode: &exit3}, Outcome: monitorregistry.OutcomeRefused},
			},
			ProbeUndefined: true,
			Addressee:      monitorregistry.AddresseeCheck{Process: monitorregistry.ProcessNone},
			Binaries:       map[string][]string{monitorregistry.PlatformLinux: {"wiringq"}},
		}},
		Shells:       monitorregistry.Shells{Source: "fixture", Names: []string{"sh", "bash"}},
		ZavodEngines: map[string]string{"alpha-engine": "alpha"},
	}
	doc, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	return doc
}

// wiringShell runs command with sh in env; returns its stdout lines as JSON
// objects.
func wiringShell(t *testing.T, command string, env []string) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := osexec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\nstdout:\n%s\nstderr:\n%s", command[:min(len(command), 60)], err, stdout.String(), stderr.String())
	}
	var lines []map[string]any
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			t.Fatalf("stdout line is not JSON: %q", sc.Text())
		}
		lines = append(lines, obj)
	}
	return lines
}

// wiringJournal reads every journal line under the state home.
func wiringJournal(stateHome string) []map[string]any {
	paths, _ := filepath.Glob(filepath.Join(stateHome, "skein-monitor", "journal", "*"))
	var out []map[string]any
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(raw), "\n") {
			var obj map[string]any
			if json.Unmarshal([]byte(l), &obj) == nil {
				out = append(out, obj)
			}
		}
	}
	return out
}

// TestMonitorConfigWiring_IdleWindowReachesTheClientPath — test matrix row
// #11 (US-12 "liveness of the token", the named path of the idle window).
// SKEIN_MONITOR_TOKEN_IDLE_TTL comes from config.Load with a value that is
// no other timing and not the default; the router carries a fixture registry
// through its seam. The path is the one an agent takes: the MCP call with
// `client`, `prepare`, the token written 0644, `preflight`, the working
// command of its line; starter, loader, runner download and polls go through
// a local adapter that adds the internal secret, as the proxy does. The
// detached runner runs on the virtual clock.
//
// Deviation from the wording of the row: the channel refuses before send
// instead of taking the call and never answering, because the executor's
// wait on a channel call is a select with a real timeout that the virtual
// clock does not cover. The same parameter is observed: INV-8 calls a
// channel again only after a proven refusal, after 5, 30 and 120 s, and only
// within the idle window from the first call; with a window of 100 s the
// calls are at 0, 5 and 35 s and a fourth (at 155 s) is not made, while the
// default window of 6 h would allow it.
func TestMonitorConfigWiring_IdleWindowReachesTheClientPath(t *testing.T) {
	if _, err := osexec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	const window = 100 * time.Second
	t.Setenv("SKEIN_MONITOR_TOKEN_IDLE_TTL", "100s")
	t.Setenv("SKEIN_MONITOR_POLL_SAFETY_LAG", "1s")

	// The adapter is the proxy: it adds the internal secret and hands the
	// request to the router built below.
	var router atomic.Value
	adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		router.Load().(chi.Router).ServeHTTP(w, r)
	}))
	t.Cleanup(adapter.Close)
	s := newWiringStackWith(t, adapter.URL, wiringClientRegistry(t))
	router.Store(s.router)
	if s.cfg.MonitorTokenIdleTTL != window {
		t.Fatalf("config.Load idle window = %s, want %s", s.cfg.MonitorTokenIdleTTL, window)
	}

	actor, _ := testsupport.SeedUser(t, s.pool, "user")
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM events WHERE actor_user_id = $1`, actor)
	})

	issuing := s.initialize()
	minted := s.callTool(issuing, "skein_get_monitor_script", map[string]any{
		"id": s.root, "client": "alpha", "platform": "posix", "eventTypes": []string{"issue.status_changed"}})
	var mint struct {
		MonitorID     string `json:"monitorId"`
		Token         string `json:"token"`
		TokenFilePath string `json:"tokenFilePath"`
		Prepare       string `json:"prepare"`
		Preflight     string `json:"preflight"`
	}
	if err := json.Unmarshal([]byte(minted[0]), &mint); err != nil {
		t.Fatalf("mint response: %v\n%s", err, minted[0])
	}
	if mint.Prepare == "" || mint.Preflight == "" {
		t.Fatalf("a call with client has prepare and preflight: %s", minted[0])
	}
	monitorID := uuid.MustParse(mint.MonitorID)
	// The runner stops on its own once its token is gone (401), and the
	// token directory goes with it.
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM monitor_credentials WHERE id = $1`, monitorID)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Dir(mint.TokenFilePath)); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	home := t.TempDir()
	stateHome := filepath.Join(home, "state")
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("bin: %v", err)
	}
	calls := filepath.Join(home, "calls")
	fake := "#!/bin/sh\ncat >/dev/null\necho call >> '" + calls + "'\nexit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "wiringq"), []byte(fake), 0o700); err != nil {
		t.Fatalf("fake channel: %v", err)
	}
	site, err := filepath.Abs(filepath.Join("..", "..", "internal", "handler", "testdata", "runner_site"))
	if err != nil {
		t.Fatalf("site dir: %v", err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SKEIN_") && !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "XDG_STATE_HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_STATE_HOME="+stateHome, "WIRING_QUEUE_ID=thread-11")

	// prepare, then the token written like the Write tool does (0644).
	if lines := wiringShell(t, mint.Prepare, env); len(lines) != 1 || lines[0]["event"] != "prepare_ready" {
		t.Fatalf("prepare = %v", lines)
	}
	if err := os.WriteFile(mint.TokenFilePath, []byte(mint.Token), 0o644); err != nil {
		t.Fatalf("write token: %v", err)
	}
	lines := wiringShell(t, mint.Preflight, env)
	pre := lines[len(lines)-1]
	if pre["event"] != "preflight" || pre["method"] != "detached" || pre["channel"] != wiringChannelID {
		t.Fatalf("preflight line = %v", pre)
	}
	command, _ := pre["command"].(string)

	// The working command, the detached runner on the virtual clock.
	runEnv := append(append([]string(nil), env...), "PYTHONPATH="+site, "SKEIN_TEST_SITE=1",
		"SKEIN_TEST_VCLOCK=1", "SKEIN_TEST_RANDOM=1.0")
	started := wiringShell(t, command, runEnv)
	if last := started[len(started)-1]; last["event"] != "monitor_started" {
		t.Fatalf("working command = %v", started)
	}

	// An event the token sees, past the safety lag.
	s.exec(`INSERT INTO events (occurred_at, type, entity_type, entity_id, project_id,
                    actor, actor_user_id, actor_session_id, actor_type, payload)
VALUES (now(), 'issue.status_changed', 'issue', $1, $2, 'tester', $3, NULL, 'human', '{}'::jsonb)`,
		s.root, s.project, actor)

	var outcome map[string]any
	deadline := time.Now().Add(90 * time.Second)
	for outcome == nil && time.Now().Before(deadline) {
		for _, l := range wiringJournal(stateHome) {
			if l["event"] == "delivery" {
				outcome = l
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if outcome == nil {
		t.Fatalf("no delivery outcome in the journal; journal: %v", wiringJournal(stateHome))
	}
	if outcome["status"] != "refused-before-send" {
		t.Errorf("outcome = %v, want refused-before-send", outcome["status"])
	}
	raw, _ := os.ReadFile(calls)
	// Calls at 0, 5 and 35 s of the 100 s window; the one at 155 s is past it.
	if got := strings.Count(string(raw), "call"); got != 3 {
		t.Errorf("channel calls = %d, want 3 within a window of %s (INV-8: 0, 5, 35 s; not 155 s)", got, window)
	}
}
