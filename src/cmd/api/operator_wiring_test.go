package main

// operator_wiring_test.go — sk-674.18, the test delivery of row 182 (T042,
// US-11) and the public half of row 180, by requests (no inspection of the
// route tree — part III of the test standards), on the functions main uses: the
// operator's server built from the configuration (operatorServer) and the
// public router (buildRouter). No database: neither request below reaches
// an operation.
//
// References: US-11 — the operator's port is enabled by
// SKEIN_OPERATOR_API_ENABLED, does not answer without the key, and the
// public port does not serve the operator's API; the contract the lead
// accepted for T042 (operator/contract.go) — 401 operator_unauthorized
// before routing, routes under /operator/v1.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"skein-api/internal/config"
	"skein-api/internal/operator"
)

const wiringKey = "operator-wiring-key-0123456789"

// freePort returns a port nothing listens on at the moment of the call.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	return port
}

// TestOperatorWiring_ServerOnlyWithTheFlag — row 182. With
// SKEIN_OPERATOR_API_ENABLED=false no operator server is built, so nothing
// is started on its port; with true a server on SKEIN_OPERATOR_API_PORT is
// built, and once started a request on that port without the key gets 401
// operator_unauthorized.
func TestOperatorWiring_ServerOnlyWithTheFlag(t *testing.T) {
	port := freePort(t)

	t.Run("disabled: no server", func(t *testing.T) {
		cfg := &config.Config{Port: "5100", OperatorAPIEnabled: false, OperatorAPIPort: port, OperatorAPIKey: wiringKey}
		if srv := operatorServer(cfg, nil); srv != nil {
			t.Fatalf("an operator server on %s is built with the flag off", srv.Addr)
		}
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond); err == nil {
			_ = c.Close()
			t.Errorf("something listens on the operator's port %s with the flag off", port)
		}
	})

	t.Run("enabled: the port answers 401 without the key", func(t *testing.T) {
		cfg := &config.Config{Port: "5100", OperatorAPIEnabled: true, OperatorAPIPort: port, OperatorAPIKey: wiringKey}
		srv := operatorServer(cfg, nil)
		if srv == nil {
			t.Fatal("no operator server with the flag on")
		}
		if want := ":" + port; srv.Addr != want {
			t.Fatalf("operator server address %q, want %q (SKEIN_OPERATOR_API_PORT)", srv.Addr, want)
		}
		served := make(chan error, 1)
		go func() { served <- srv.ListenAndServe() }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
			if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("operator server: %v", err)
			}
		})

		client := &http.Client{Timeout: 2 * time.Second}
		var resp *http.Response
		var err error
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			resp, err = client.Post("http://127.0.0.1:"+port+operator.PathBlock, "application/json",
				strings.NewReader(`{"email":"someone@example.com"}`))
			if err == nil {
				break
			}
		}
		if err != nil {
			t.Fatalf("the operator's port does not answer: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != http.StatusUnauthorized || body.Error != operator.CodeUnauthorized {
			t.Errorf("POST %s without the key = %d %q, want 401 %s", operator.PathBlock, resp.StatusCode, body.Error, operator.CodeUnauthorized)
		}
	})
}

// TestOperatorWiring_PublicRouterServesNoOperatorRoute — row 180, the
// public half: a request to each of the operator's paths, with the
// operator's key, is not served by the router of the main port.
func TestOperatorWiring_PublicRouterServesNoOperatorRoute(t *testing.T) {
	r := newRouterUnderTest(t)
	for _, path := range []string{operator.PathBlock, operator.PathUnblock, operator.PathDelete} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"someone@example.com"}`))
		req.Header.Set(operator.KeyHeader, wiringKey)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s on the public router = %d, want not served (404)", path, rec.Code)
		}
	}
}
