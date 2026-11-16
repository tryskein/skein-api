package handler

// middleware_service_call_test.go — sk-998 US-2: RequireServiceCall lets a
// request reach the handler only with the service-call marker, exactly one
// X-Skein-Service-Call value equal to "1"; any other form is answered 403
// service_call_required and the handler does not run. The refusal is logged at
// Warn without the secret, the link token of the path or the client IP claim.
//
// The contract literals (header name, value, code) are written out here rather
// than taken from the product constants: the contract with skein-web is the
// literal wire text.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const serviceCallHeader = "X-Skein-Service-Call"

// spyHandler records whether it ran and answers 200 "spied".
type spyHandler struct{ called bool }

func (s *spyHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.called = true
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("spied"))
}

func TestRequireServiceCall_OnlyTheSingleLiteralOneIsTheMarker(t *testing.T) {
	quietServiceCallLog(t)

	for _, tc := range []struct {
		name   string
		values []string // header lines; nil = no header
	}{
		{"no header", nil},
		{"empty", []string{""}},
		{"zero", []string{"0"}},
		{"true", []string{"true"}},
		{"leading zero", []string{"01"}},
		{"trailing space", []string{"1 "}},
		{"leading space", []string{" 1"}},
		{"two lines of 1", []string{"1", "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyHandler{}
			req := httptest.NewRequest(http.MethodPost, "/ui/v1/internal/instance-config", nil)
			for _, v := range tc.values {
				req.Header.Add(serviceCallHeader, v)
			}
			rec := httptest.NewRecorder()
			RequireServiceCall(spy).ServeHTTP(rec, req)

			if spy.called {
				t.Errorf("the handler ran for %q", tc.values)
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
			}
			if len(body) != 1 || body["error"] != "service_call_required" {
				t.Errorf(`body %s, want {"error":"service_call_required"}`, rec.Body.String())
			}
		})
	}

	t.Run("exactly one 1", func(t *testing.T) {
		spy := &spyHandler{}
		req := httptest.NewRequest(http.MethodPost, "/ui/v1/internal/instance-config", nil)
		req.Header.Add(serviceCallHeader, "1")
		rec := httptest.NewRecorder()
		RequireServiceCall(spy).ServeHTTP(rec, req)

		if !spy.called {
			t.Fatal("the handler did not run with the marker")
		}
		if rec.Code != http.StatusOK || rec.Body.String() != "spied" {
			t.Errorf("answer %d %q, want the handler's 200 \"spied\"", rec.Code, rec.Body.String())
		}
	})
}

// The refusal is one Warn record that names the code and the transport peer,
// and carries neither the secret, nor the link token of the path, nor the
// X-Skein-Client-Ip claim.
func TestRequireServiceCall_RefusalLogsWarnWithoutSecrets(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A link token of the issued shape (43 characters of base64url).
	const token = "LEAKCANARY_" + "abcdefghijklmnopqrstuvwxyzABCDEF"
	const secret = "SECRETCANARY-internal-secret"
	const clientIP = "203.0.113.77"
	const peer = "198.51.100.7:40123"

	req := httptest.NewRequest(http.MethodPost, "/ui/v1/auth/password-reset/"+token, nil)
	req.RemoteAddr = peer
	req.Header.Set("X-Skein-Internal-Secret", secret)
	req.Header.Set("X-Skein-Client-Ip", clientIP)
	spy := &spyHandler{}
	rec := httptest.NewRecorder()
	RequireServiceCall(spy).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || spy.called {
		t.Fatalf("status %d, handler ran %v; want 403 and not run", rec.Code, spy.called)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want exactly one log record, got %d: %s", len(lines), buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("log record %q is not JSON: %v", lines[0], err)
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
	if !hasValue(entry, "service_call_required") {
		t.Errorf("the record does not name service_call_required: %s", lines[0])
	}
	if !hasValue(entry, peer) {
		t.Errorf("the record does not name the transport peer %s: %s", peer, lines[0])
	}
	for _, leak := range []string{token, secret, clientIP} {
		if strings.Contains(lines[0], leak) {
			t.Errorf("the record carries %q: %s", leak, lines[0])
		}
	}
}

// hasValue reports whether one of the record's attributes equals want.
func hasValue(entry map[string]any, want string) bool {
	for _, v := range entry {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

// quietServiceCallLog keeps the refusal records of a table test out of the
// test output.
func quietServiceCallLog(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}
