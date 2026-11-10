package main

// public_chain_wiring_test.go — row 120 (F-01, «configuration → runtime»):
// the public server built by the wiring of main (publicAPIServer over
// config.Load, not a test builder) carries on its product routes the values
// of the configuration: the request limit of a key, the limit of failed
// authentications of an address, the trusted proxies, the operation
// deadline. Every value is small and unlike its default. No stopwatch: the
// outcome and the state are asserted; the test has only a safety bound.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/apikeys"
	"skein-api/internal/auth"
	"skein-api/internal/config"
	"skein-api/internal/testsupport"
)

// chainWiringEnv sets the base environment of config.Load and the public
// API's values of the row.
func chainWiringEnv(t *testing.T, writeTimeout string) *config.Config {
	t.Helper()
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("SKEIN_INTERNAL_SECRET", wiringInternalSecret)
	t.Setenv("SKEIN_BCRYPT_COST", "4")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("SKEIN_AGENT_PUBLIC_URL", "http://skein-chain.test")
	t.Setenv("SKEIN_PUBLIC_API_RATE_LIMIT_PER_MINUTE", "3")
	t.Setenv("SKEIN_PUBLIC_API_AUTH_FAILURES_PER_MINUTE", "2")
	t.Setenv("SKEIN_PUBLIC_API_TRUSTED_PROXIES", "10.9.0.0/16")
	if writeTimeout != "" {
		t.Setenv("SKEIN_PUBLIC_API_WRITE_TIMEOUT", writeTimeout)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestPublicChainWiring_ConfigReachesTheProductRoutes — row 120.
func TestPublicChainWiring_ConfigReachesTheProductRoutes(t *testing.T) {
	pool := testsupport.NewTestPool(t)
	owner, _ := testsupport.SeedUser(t, pool, "")
	project, _ := testsupport.SeedProject(t, pool, owner, "chainw")
	mint := func() string {
		w := &classWorld{t: t, pool: pool}
		return w.key(owner, levelKey(apikeys.LevelManage))
	}
	server := func(t *testing.T, writeTimeout string) http.Handler {
		t.Helper()
		srv, err := publicAPIServer(chainWiringEnv(t, writeTimeout), pool, classDeps(pool), slog.New(&recordingHandler{}))
		if err != nil || srv == nil {
			t.Fatalf("publicAPIServer: %v %v", srv, err)
		}
		return srv.Handler
	}
	getMe := func(h http.Handler, key, peer, forwarded string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.RemoteAddr = peer
		if key != "" {
			req.Header.Set(auth.HeaderAPIKey, key)
		}
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const badKey = "ska_not_a_key_of_this_instance_000000000000"

	t.Run("the request limit of a key", func(t *testing.T) {
		h := server(t, "")
		key := mint()
		for i := 1; i <= 3; i++ {
			if code := getMe(h, key, "192.0.2.1:4000", ""); code != http.StatusOK {
				t.Fatalf("request %d: %d, want 200", i, code)
			}
		}
		if code := getMe(h, key, "192.0.2.1:4000", ""); code != http.StatusTooManyRequests {
			t.Errorf("request 4 of a limit of 3: %d, want 429", code)
		}
	})

	t.Run("the failed-authentication limit of an address", func(t *testing.T) {
		h := server(t, "")
		for i := 1; i <= 2; i++ {
			if code := getMe(h, badKey, "192.0.2.2:4000", ""); code != http.StatusUnauthorized {
				t.Fatalf("failure %d: %d, want 401", i, code)
			}
		}
		if code := getMe(h, mint(), "192.0.2.2:4000", ""); code != http.StatusTooManyRequests {
			t.Errorf("a cold valid key past the limit of 2: %d, want 429", code)
		}
		if code := getMe(h, mint(), "192.0.2.3:4000", ""); code != http.StatusOK {
			t.Errorf("a cold valid key from another address: %d, want 200", code)
		}
	})

	t.Run("the trusted proxies", func(t *testing.T) {
		h := server(t, "")
		// Through a trusted proxy the failures count for the client.
		for i := 0; i < 2; i++ {
			getMe(h, badKey, "10.9.0.1:4000", "198.51.100.1")
		}
		if code := getMe(h, mint(), "10.9.0.1:4000", "198.51.100.1"); code != http.StatusTooManyRequests {
			t.Errorf("the client over the limit through a trusted proxy: %d, want 429", code)
		}
		if code := getMe(h, mint(), "10.9.0.1:4000", "198.51.100.2"); code != http.StatusOK {
			t.Errorf("another client through the same trusted proxy: %d, want 200", code)
		}
		// From an untrusted peer the forwarded address is ignored: the
		// failures count for the peer.
		for i := 0; i < 2; i++ {
			getMe(h, badKey, "192.0.2.9:4000", "198.51.100.3")
		}
		if code := getMe(h, mint(), "192.0.2.9:4000", "198.51.100.4"); code != http.StatusTooManyRequests {
			t.Errorf("an untrusted peer over the limit, another forwarded client: %d, want 429", code)
		}
	})

	t.Run("the operation deadline on updateProject", func(t *testing.T) {
		description := func() string {
			var d string
			if err := pool.QueryRow(context.Background(), `SELECT description FROM projects WHERE id = $1`, project).Scan(&d); err != nil {
				t.Fatalf("read description: %v", err)
			}
			return d
		}
		patch := func(h http.Handler, key, desc string) int {
			body, _ := json.Marshal(map[string]any{"description": desc})
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/"+project.String(), strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.HeaderAPIKey, key)
			req.RemoteAddr = "192.0.2.20:4000"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code
		}
		lock := func(t *testing.T) (pgx.Tx, int) {
			t.Helper()
			tx, err := pool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.Exec(context.Background(), `SELECT 1 FROM projects WHERE id = $1 FOR UPDATE`, project); err != nil {
				t.Fatalf("lock the project row: %v", err)
			}
			var pid int
			if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatalf("read the locking backend: %v", err)
			}
			return tx, pid
		}

		// A small deadline from the configuration: the answer comes while
		// the lock is still held, and it is not a success; nothing changed.
		was := description()
		h := server(t, "400ms")
		tx, _ := lock(t)
		code := patch(h, mint(), "deadline "+uuid.NewString())
		_ = tx.Rollback(context.Background())
		if code >= 200 && code < 300 {
			t.Errorf("under a held lock with a 400ms deadline: %d, want a failure", code)
		}
		if got := description(); got != was {
			t.Errorf("the write past the deadline changed the description: %q → %q", was, got)
		}

		// The default deadline: the same write waits for the lock and passes.
		h = server(t, "")
		tx, pid := lock(t)
		want := "waited " + uuid.NewString()
		done := make(chan int, 1)
		key := mint()
		go func() { done <- patch(h, key, want) }()
		waiting := false
		for bound := time.Now().Add(20 * time.Second); time.Now().Before(bound); {
			var n int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
				t.Fatalf("read the waiting backends: %v", err)
			}
			if n > 0 {
				waiting = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = tx.Rollback(context.Background())
		if !waiting {
			t.Fatalf("the write never waited on the lock")
		}
		if code := <-done; code != http.StatusOK {
			t.Errorf("with the default deadline: %d, want 200", code)
		}
		if got := description(); got != want {
			t.Errorf("the description after the lock: %q, want %q", got, want)
		}
	})
}
