package main

// public_server_wiring_test.go — rows 56 and 121: the public API server as
// main builds it (publicServer), by requests and by the fields of the
// http.Server it returns. No database: no request below reaches an
// operation that reads one.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"skein-api/internal/config"
	"skein-api/internal/issues"
)

// recordingHandler keeps every log record it receives.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// warningsNaming counts the warn records that carry an attribute whose value
// is name.
func (h *recordingHandler) warningsNaming(name string) (naming, warnings int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		warnings++
		r.Attrs(func(a slog.Attr) bool {
			if a.Value.String() == name {
				naming++
				return false
			}
			return true
		})
	}
	return naming, warnings
}

func publicConfig(enabled bool, port, url string) *config.Config {
	return &config.Config{
		Port:                  "5100",
		PublicAPIEnabled:      enabled,
		PublicAPIPort:         port,
		PublicAPIURL:          url,
		PublicAPIWriteTimeout: 30 * time.Second,
	}
}

// TestPublicServerWiring_WarningTimeoutsAndShutdown — row 56 (R-1, Changelog
// US-10, edge "port disabled", NFR "timeouts"). Enabled without
// SKEIN_PUBLIC_API_URL: exactly one warning, naming the variable (the event
// and the name are asserted, not the text). With the address: no warning.
// Disabled: no server, no warning. The server listens on
// SKEIN_PUBLIC_API_PORT, bounds reading headers, reading the request,
// writing and idling, with a write timeout past the operation deadline plus
// the read that follows a commit; started, it answers and stops on Shutdown.
func TestPublicServerWiring_WarningTimeoutsAndShutdown(t *testing.T) {
	port := freePort(t)

	t.Run("enabled without the address: one warning naming the variable", func(t *testing.T) {
		rec := &recordingHandler{}
		srv, err := publicServer(publicConfig(true, port, ""), nil, slog.New(rec))
		if err != nil || srv == nil {
			t.Fatalf("publicServer = %v, %v; want a server", srv, err)
		}
		naming, warnings := rec.warningsNaming("SKEIN_PUBLIC_API_URL")
		if warnings != 1 || naming != 1 {
			t.Errorf("%d warnings, %d naming SKEIN_PUBLIC_API_URL; want exactly one, naming it", warnings, naming)
		}
	})

	t.Run("enabled with the address: no warning", func(t *testing.T) {
		rec := &recordingHandler{}
		srv, err := publicServer(publicConfig(true, port, "https://api.skein.example"), nil, slog.New(rec))
		if err != nil || srv == nil {
			t.Fatalf("publicServer = %v, %v; want a server", srv, err)
		}
		if _, warnings := rec.warningsNaming("SKEIN_PUBLIC_API_URL"); warnings != 0 {
			t.Errorf("%d warnings with the address set, want none", warnings)
		}
	})

	t.Run("disabled: no server, no warning", func(t *testing.T) {
		rec := &recordingHandler{}
		srv, err := publicServer(publicConfig(false, port, ""), nil, slog.New(rec))
		if err != nil {
			t.Fatal(err)
		}
		if srv != nil {
			t.Fatalf("a public server on %s is built with SKEIN_PUBLIC_API_ENABLED=false", srv.Addr)
		}
		if _, warnings := rec.warningsNaming("SKEIN_PUBLIC_API_URL"); warnings != 0 {
			t.Errorf("%d warnings with the public API disabled, want none", warnings)
		}
	})

	t.Run("timeouts, port and shutdown", func(t *testing.T) {
		cfg := publicConfig(true, port, "https://api.skein.example")
		srv, err := publicServer(cfg, nil, slog.New(&recordingHandler{}))
		if err != nil || srv == nil {
			t.Fatalf("publicServer = %v, %v; want a server", srv, err)
		}
		if want := ":" + port; srv.Addr != want {
			t.Errorf("address %q, want %q (SKEIN_PUBLIC_API_PORT)", srv.Addr, want)
		}
		if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
			t.Errorf("timeouts header %v, read %v, write %v, idle %v; want all set",
				srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
		}
		if floor := cfg.PublicAPIWriteTimeout + issues.ReadBackTimeout; srv.WriteTimeout <= floor {
			t.Errorf("write timeout %v, want more than the operation deadline %v plus the read after a commit %v",
				srv.WriteTimeout, cfg.PublicAPIWriteTimeout, issues.ReadBackTimeout)
		}

		served := make(chan error, 1)
		go func() { served <- srv.ListenAndServe() }()
		client := &http.Client{Timeout: 2 * time.Second}
		var resp *http.Response
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			resp, err = client.Get("http://127.0.0.1:" + port + "/api/v1/openapi.json")
			if err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("the public port does not answer: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /api/v1/openapi.json on the public port = %d, want 200", resp.StatusCode)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		select {
		case err := <-served:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("ListenAndServe after Shutdown = %v, want http.ErrServerClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the public server did not stop within the shutdown window")
		}
	})
}

// TestPublicServerWiring_HandlerIsThePublicRouter — row 121 (F-02, INV-10,
// SC-018). The handler of the server main builds is the public router: the
// internal API and /mcp are 404 there, the document of the API is 200. Red
// when the wiring hangs the internal router or an empty handler on it.
func TestPublicServerWiring_HandlerIsThePublicRouter(t *testing.T) {
	srv, err := publicServer(publicConfig(true, freePort(t), ""), nil, slog.New(&recordingHandler{}))
	if err != nil || srv == nil {
		t.Fatalf("publicServer = %v, %v; want a server", srv, err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/ui/v1/issues", http.StatusNotFound},
		{http.MethodGet, "/ui/v1/me", http.StatusNotFound},
		{http.MethodPost, "/mcp", http.StatusNotFound},
		{http.MethodGet, "/api/v1/openapi.json", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
}
