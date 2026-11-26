package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/auth"
	mcptools "skein-api/internal/mcp/tools"
	"skein-api/internal/monitorcreds"
)

// The internal secret of the router under test.
const loaderTestSecret = "loader-test-internal-secret"

// A monitor token of the right shape. No request here may reach the token
// store with it: the store has no pool, so a lookup would panic.
var loaderTestToken = auth.MonitorTokenTag + strings.Repeat("a", auth.MonitorTokenLen-len(auth.MonitorTokenTag))

// loaderTestRouter mounts the /agent/v1 group the way the server does
// (MountAgentRoutes), with the real internal-secret and monitor-token gates.
// The monitor-token gate refuses a request without a token by its shape,
// before the store, so the store needs no database.
func loaderTestRouter(t *testing.T, loader *AgentLoaderHandler) http.Handler {
	t.Helper()
	store := monitorcreds.New(nil, 6*time.Hour)
	unreachable := func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("%s reached its handler without a monitor token", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}
	r := chi.NewRouter()
	MountAgentRoutes(r, loaderTestSecret, auth.RequireMonitorToken(store, "notifications:poll"),
		unreachable, unreachable, loader.Loader)
	return r
}

func loaderGet(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Matrix row #142 (R-10, INV-16): the body served is the loader, and the
// digest of the bytes actually written is the one the mint bakes into every
// start command.
func TestAgentLoader_ServesTheLoaderWithItsDigest(t *testing.T) {
	h := NewAgentLoaderHandler(mcptools.ServedLoader())
	rec := loaderGet(http.HandlerFunc(h.Loader), "/agent/v1/monitor-loader", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-python") {
		t.Errorf("Content-Type = %q, want text/x-python", ct)
	}
	body := rec.Body.Bytes()
	if string(body) != mcptools.ServedLoader() {
		t.Fatalf("served body (%d bytes) is not the loader (%d bytes)", len(body), len(mcptools.ServedLoader()))
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != h.SHA256() {
		t.Fatalf("SHA-256 of the served bytes = %s, handler SHA256() = %s", got, h.SHA256())
	}
}

// Matrix row #142: two different callers — other user headers, with and
// without a monitor token — get the same bytes through the assembled group.
func TestAgentLoader_SameBytesForEveryCaller(t *testing.T) {
	h := NewAgentLoaderHandler(mcptools.ServedLoader())
	r := loaderTestRouter(t, h)
	first := loaderGet(r, "/agent/v1/monitor-loader", map[string]string{
		auth.HeaderInternalSecret: loaderTestSecret,
		auth.HeaderUserID:         "00000000-0000-0000-0000-000000000001",
	})
	second := loaderGet(r, "/agent/v1/monitor-loader", map[string]string{
		auth.HeaderInternalSecret: loaderTestSecret,
		auth.HeaderUserID:         "00000000-0000-0000-0000-000000000002",
		auth.HeaderAPIKey:         loaderTestToken,
	})
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d, want 200, 200", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("two callers got different bodies (%d and %d bytes)", first.Body.Len(), second.Body.Len())
	}
}

// Matrix row #142: through the assembled group the loader route sits under the
// internal secret and outside the monitor token; the other routes keep the
// token. It turns red if the loader falls under RequireMonitorToken or leaves
// RequireInternalSecret.
func TestAgentLoader_Gates(t *testing.T) {
	r := loaderTestRouter(t, NewAgentLoaderHandler(mcptools.ServedLoader()))
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"loader without the internal secret", "/agent/v1/monitor-loader", nil, http.StatusUnauthorized},
		{"loader with a wrong internal secret", "/agent/v1/monitor-loader",
			map[string]string{auth.HeaderInternalSecret: "wrong-" + loaderTestSecret}, http.StatusUnauthorized},
		{"loader with a monitor token but no internal secret", "/agent/v1/monitor-loader",
			map[string]string{auth.HeaderAPIKey: loaderTestToken}, http.StatusUnauthorized},
		{"loader with the internal secret and no monitor token", "/agent/v1/monitor-loader",
			map[string]string{auth.HeaderInternalSecret: loaderTestSecret}, http.StatusOK},
		{"poll with the internal secret and no monitor token", "/agent/v1/notifications/poll",
			map[string]string{auth.HeaderInternalSecret: loaderTestSecret}, http.StatusUnauthorized},
		{"monitor-script with the internal secret and no monitor token", "/agent/v1/monitor-script",
			map[string]string{auth.HeaderInternalSecret: loaderTestSecret}, http.StatusUnauthorized},
		{"poll without the internal secret", "/agent/v1/notifications/poll", nil, http.StatusUnauthorized},
		{"monitor-script without the internal secret", "/agent/v1/monitor-script", nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := loaderGet(r, tc.path, tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
