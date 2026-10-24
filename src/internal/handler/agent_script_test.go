package handler

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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/auth"
	"skein-api/internal/monitorcreds"
)

// scriptTestDBURL returns TEST_DATABASE_URL or skips the test — the same
// convention as the rest of the handler DB-backed suite.
func scriptTestDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed tests")
	}
	return url
}

// TestAgentScript_Body is a pure-unit test (no DB): it exercises the handler
// directly and asserts the served runner is well-formed Python with the
// cadence placeholders substituted by the configured seconds. This is the
// load-bearing contract — the Monitor executes this body verbatim.
func TestAgentScript_Body(t *testing.T) {
	h := NewAgentScriptHandler(2, 10*time.Second, 15*time.Second, 3*time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil)
	rec := httptest.NewRecorder()
	h.Script(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-python") {
		t.Fatalf("Content-Type = %q, want text/x-python prefix", ct)
	}

	body := rec.Body.String()

	// Placeholder must be gone, replaced by the configured integer seconds.
	if strings.Contains(body, "__SKEIN_POLL_INTERVAL_SEC__") {
		t.Fatalf("served body still contains placeholder %q", "__SKEIN_POLL_INTERVAL_SEC__")
	}
	// Placeholder sits inside int("...") so the raw asset stays valid Python;
	// after substitution the served body reads int("2").
	if !strings.Contains(body, `POLL_INTERVAL_SEC = int("2")`) {
		t.Fatalf(`served body missing substituted POLL_INTERVAL_SEC = int("2")`)
	}

	// Expected runner markers (defensive: catch an empty/wrong embed).
	for _, marker := range []string{
		"X-Skein-Api-Key",
		"/agent/v1/notifications/poll",
		"SKEIN_TOKEN_FILE",
		"monitor_started",
		"def run(",
		// sk-164 FR-2 (T085/F4): the served runner reads SKEIN_EXCLUDE_SESSION
		// and passes it as the exclude_session poll query-param. The bootstrap
		// (get_monitor_script.go) bakes the env var; the runner body must
		// actually consume it, or the rebind channel is dead end-to-end.
		"SKEIN_EXCLUDE_SESSION",
		"exclude_session",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("served body missing expected marker %q", marker)
		}
	}
}

// TestAgentScript_CadenceFloor proves a sub-second configured cadence never
// templates a `0` (busy-loop) into the runner — it is floored at 1.
func TestAgentScript_CadenceFloor(t *testing.T) {
	h := NewAgentScriptHandler(0, 10*time.Second, 15*time.Second, 3*time.Minute)
	rec := httptest.NewRecorder()
	h.Script(rec, httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `POLL_INTERVAL_SEC = int("1")`) {
		t.Fatalf("sub-second poll interval not floored to 1")
	}
}

// TestAgentScript_Unauthorized wires the real RequireMonitorToken middleware
// over a CLOSED pool so any store/DB hit would surface as 5xx. A 401 on a
// missing / malformed / ska_-shaped token therefore proves the shape gate
// short-circuited before the store — no DB needed. Mirrors the auth-package
// TestRequireMonitorTokenAPIKeyShapeRejected pattern.
func TestAgentScript_Unauthorized(t *testing.T) {
	url := scriptTestDBURL(t) // skips without TEST_DATABASE_URL
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	pool.Close() // any store call now errors → would be 5xx, not 401
	store := monitorcreds.New(pool, 6*time.Hour)

	h := auth.RequireMonitorToken(store, "notifications:poll")(
		http.HandlerFunc(NewAgentScriptHandler(2, 10*time.Second, 15*time.Second, 3*time.Minute).Script))

	cases := []struct {
		name  string
		token string
	}{
		{"no token", ""},
		{"malformed skm_", "skm_short"},
		{"garbage", "not-a-token"},
		{"ska_ at monitor door", "ska_" + strings.Repeat("a", 22)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil)
			if tc.token != "" {
				req.Header.Set(auth.HeaderAPIKey, tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAgentScript_ValidToken is the DB-integration happy path: a freshly minted
// skm_ token passes RequireMonitorToken and the runner body is served.
func TestAgentScript_ValidToken(t *testing.T) {
	url := scriptTestDBURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	userID, _ := seedDashboardUser(t, pool, "user")
	projectID, _ := seedDashboardProject(t, pool, userID)
	apiKeyID := seedScriptAPIKey(t, pool, userID)

	store := monitorcreds.New(pool, 6*time.Hour)
	plaintext, _, err := store.Mint(ctx, monitorcreds.MintParams{
		UserID:           userID,
		MintedByAPIKeyID: apiKeyID,
		Audience:         "notifications:poll",
		Capability:       "read",
		ProjectID:        projectID,
		ScopeIssueID:     "as-1",
		ScopeSubtree:     true,
		EventTypes:       []string{"comment.created"},
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	h := auth.RequireMonitorToken(store, "notifications:poll")(
		http.HandlerFunc(NewAgentScriptHandler(2, 10*time.Second, 15*time.Second, 3*time.Minute).Script))

	req := httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil)
	req.Header.Set(auth.HeaderAPIKey, plaintext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-python") {
		t.Fatalf("Content-Type = %q, want text/x-python", ct)
	}
	if !strings.Contains(rec.Body.String(), "/agent/v1/notifications/poll") {
		t.Fatalf("served body does not look like the runner")
	}
}

// seedScriptAPIKey inserts an active api_keys row owned by userID. The
// monitor_credentials FK is ON DELETE CASCADE, so dropping this key removes any
// credential minted by it.
func seedScriptAPIKey(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewRandom())
	prefix := "ska_" + strings.ReplaceAll(id.String()[:12], "-", "")
	_, err := pool.Exec(context.Background(), `
INSERT INTO api_keys (id, user_id, key_hash, prefix, label, created_at)
VALUES ($1, $2, 'fake-hash-for-test', $3, 'agent-script test', now())`,
		id, userID, prefix)
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
	})
	return id
}

// Matrix row 5 (sk-579): the body Script serves APPLIES the timings it was
// built with — observed by executing it, not by reading its text. The handler
// gets values the runner's own formula max(5·P, 10) cannot produce (cadence
// 4s, ceiling 13s, request limit 7s, stall threshold 4m); a body that kept a
// constant or recomputed the ceiling would sleep 20s or 10s instead of 13s.
//
// The runner runs under testdata/runner_site/sitecustomize.py: a virtual
// clock (sleeps are logged, never waited), random.random pinned to 1.0 (so a
// jittered sleep equals its base) and, for the limit case, a resolver that
// blocks forever. Without python3 the test is skipped.

// runnerRun is the observable outcome of one runner process.
type runnerRun struct {
	lines  []map[string]any // stdout, one JSON object per line
	sleeps []float64        // runner waits, in order (virtual seconds)
	probe  string           // "alive" / "dead" for the first executor, if probed
}

func (r runnerRun) events(name string) []map[string]any {
	var out []map[string]any
	for _, l := range r.lines {
		if l["event"] == name {
			out = append(out, l)
		}
	}
	return out
}

// runServedRunner serves a body built by NewAgentScriptHandler(4, 13s, 7s, 4m)
// through Script, then executes it against serverURL until the virtual budget
// runs out. extraEnv adds sitecustomize switches.
func runServedRunner(t *testing.T, serverURL string, budget time.Duration, extraEnv ...string) runnerRun {
	t.Helper()
	py, err := osexec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	site, err := filepath.Abs(filepath.Join("testdata", "runner_site"))
	if err != nil {
		t.Fatalf("site dir: %v", err)
	}

	h := NewAgentScriptHandler(4, 13*time.Second, 7*time.Second, 4*time.Minute)
	rec := httptest.NewRecorder()
	h.Script(rec, httptest.NewRequest(http.MethodGet, "/agent/v1/monitor-script", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Script status = %d", rec.Code)
	}

	dir := t.TempDir()
	runnerPath := filepath.Join(dir, "runner.py")
	tokenPath := filepath.Join(dir, "tok")
	sleepLog := filepath.Join(dir, "sleeps")
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(runnerPath, rec.Body.Bytes(), 0o600); err != nil {
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

	var run runnerRun
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			t.Fatalf("stdout line is not JSON: %q", sc.Text())
		}
		run.lines = append(run.lines, obj)
	}
	if raw, err := os.ReadFile(sleepLog); err == nil {
		for _, f := range strings.Fields(string(raw)) {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				t.Fatalf("sleep log entry %q: %v", f, err)
			}
			run.sleeps = append(run.sleeps, v)
		}
	}
	if raw, err := os.ReadFile(probe); err == nil {
		run.probe = string(raw)
	}
	return run
}

func writePoll(w http.ResponseWriter, notifications []map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"notifications": notifications,
		"cursor":        map[string]any{"ts": "2026-01-01T00:00:00Z", "id": 1},
	})
}

func TestAgentScript_ServedBodyAppliesTimings(t *testing.T) {
	t.Run("cadence after a delivery and growth to the ceiling", func(t *testing.T) {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Empty polls grow the sleep 8 → 12 → 13 → 13; the fifth poll
			// delivers on that accumulated backoff.
			if n.Add(1) == 5 {
				writePoll(w, []map[string]any{{"id": 1, "type": "comment.created"}})
				return
			}
			writePoll(w, []map[string]any{})
		}))
		defer srv.Close()

		run := runServedRunner(t, srv.URL, 70*time.Second)
		if len(run.events("notification")) != 1 {
			t.Fatalf("want one delivered notification, got lines %v", run.lines)
		}
		if len(run.sleeps) < 5 {
			t.Fatalf("want at least 5 runner sleeps, got %v", run.sleeps)
		}
		if run.sleeps[3] != 13 {
			t.Errorf("sleep before the delivery = %v, want the 13s ceiling (sleeps %v)", run.sleeps[3], run.sleeps)
		}
		if run.sleeps[4] != 4 {
			t.Errorf("sleep right after the delivery = %v, want the 4s cadence (sleeps %v)", run.sleeps[4], run.sleeps)
		}
	})

	t.Run("failures saturate at the substituted ceiling", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		run := runServedRunner(t, srv.URL, 150*time.Second)
		if len(run.sleeps) < 4 {
			t.Fatalf("want several sleeps, got %v", run.sleeps)
		}
		for _, s := range run.sleeps {
			if s > 13 {
				t.Fatalf("a sleep exceeded the 13s ceiling: %v", run.sleeps)
			}
		}
		if last := run.sleeps[len(run.sleeps)-1]; last != 13 {
			t.Errorf("saturated 503 sleep = %v, want 13 (sleeps %v)", last, run.sleeps)
		}
	})

	t.Run("degraded follows the substituted stall threshold", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		// Failing polls land at ≈0, 8 and then every 13s (8+13k): …177, 190,
		// 203, …229, 242. A body keeping the 3m default would print degraded
		// at 190 — inside a 3m30s budget — while the substituted 4m prints it
		// only at 242, inside 4m30s.
		if got := runServedRunner(t, srv.URL, 3*time.Minute+30*time.Second).events("degraded"); len(got) != 0 {
			t.Errorf("degraded after 3m30s of failures (threshold 4m): %v", got)
		}
		if got := runServedRunner(t, srv.URL, 4*time.Minute+30*time.Second).events("degraded"); len(got) != 1 {
			t.Errorf("want exactly one degraded after 4m30s of failures, got %v", got)
		}
	})

	t.Run("a request hung in name resolution is cut at the substituted limit", func(t *testing.T) {
		dnsLog := filepath.Join(t.TempDir(), "dns")
		// The address is never contacted: resolution blocks forever.
		url := "http://127.0.0.1:9"
		if got := runServedRunner(t, url, 6*time.Second, "SKEIN_TEST_DNS_LOG="+dnsLog).probe; got != "alive" {
			t.Errorf("first executor at 6s virtual (limit 7s) = %q, want alive", got)
		}
		dnsLog2 := filepath.Join(t.TempDir(), "dns")
		if got := runServedRunner(t, url, 8*time.Second, "SKEIN_TEST_DNS_LOG="+dnsLog2).probe; got != "dead" {
			t.Errorf("first executor at 8s virtual (limit 7s) = %q, want dead", got)
		}
	})
}
