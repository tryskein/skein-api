package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"skein-api/internal/auth"
)

// Matrix row #151 (SC-008, R-10, INV-16) on the production wiring: the
// router buildRouter assembles serves the loader and the runner, and the
// skein_get_monitor_script of the same router names their digests in its
// start commands. A digest wired to the wrong handler (or a stale constant)
// in main.go or mcp_tools.go turns this red; the integration test of the
// handler package composes both sides itself and cannot see that.
func TestAgentLoaderWiring_ProductDigests(t *testing.T) {
	s := newWiringStack(t)
	session := s.initialize()

	digestOf := func(name, command string) string {
		t.Helper()
		m := regexp.MustCompile(name + `='([0-9a-f]{64})'`).FindStringSubmatch(command)
		if m == nil {
			t.Fatalf("the command carries no %s:\n%s", name, command)
		}
		return m[1]
	}
	served := func(path, token string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(auth.HeaderInternalSecret, wiringInternalSecret)
		if token != "" {
			req.Header.Set(auth.HeaderAPIKey, token)
		}
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		sum := sha256.Sum256(rec.Body.Bytes())
		return hex.EncodeToString(sum[:])
	}

	var plain, named struct {
		Token     string `json:"token"`
		Bootstrap string `json:"bootstrap"`
		Preflight string `json:"preflight"`
	}
	if err := json.Unmarshal([]byte(s.callTool(session, "skein_get_monitor_script",
		map[string]any{"id": s.root})[0]), &plain); err != nil {
		t.Fatalf("response without client: %v", err)
	}
	if err := json.Unmarshal([]byte(s.callTool(session, "skein_get_monitor_script",
		map[string]any{"id": s.root, "client": "wiring-agent"})[0]), &named); err != nil {
		t.Fatalf("response with client: %v", err)
	}

	loader := served("/agent/v1/monitor-loader", "")
	runner := served("/agent/v1/monitor-script", plain.Token)
	for _, c := range []struct{ name, command string }{
		{"bootstrap", plain.Bootstrap}, {"preflight", named.Preflight},
	} {
		if got := digestOf("SKEIN_LOADER_SHA256", c.command); got != loader {
			t.Errorf("%s names loader %s, the router serves %s", c.name, got, loader)
		}
		if got := digestOf("SKEIN_RUNNER_SHA256", c.command); got != runner {
			t.Errorf("%s names runner %s, the router serves %s", c.name, got, runner)
		}
	}
}
