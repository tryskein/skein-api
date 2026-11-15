package main

// router_remote_addr_test.go — the internal router does not rewrite
// RemoteAddr from client-controlled forwarding headers. The reference is the
// defect report: with chi's RealIP installed, a request carrying
// True-Client-IP / X-Real-IP / X-Forwarded-For set the "remote" field of the
// internal-secret refusal log to whatever the caller wrote.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"skein-api/internal/auth"
)

func TestBuildRouter_ForwardingHeadersDoNotRewriteRemoteAddr(t *testing.T) {
	r := newRouterUnderTest(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const peer = "198.51.100.7:40123"
	for _, header := range []string{"True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		t.Run(header, func(t *testing.T) {
			buf.Reset()
			// /agent/v1 is gated by RequireInternalSecret; no secret → refusal
			// that logs the peer.
			req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil)
			req.RemoteAddr = peer
			req.Header.Set(header, "203.0.113.66")
			req.Header.Set(auth.HeaderClientIP, "192.0.2.10")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET /agent/v1/notifications/poll without secret = %d, want 401", rec.Code)
			}

			entry := refusalRecord(t, buf.String())
			if got := entry["remote"]; got != peer {
				t.Errorf("%s: logged remote = %v, want the transport peer %q", header, got, peer)
			}
			if got := entry["client_ip"]; got != "192.0.2.10" {
				t.Errorf("%s: logged client_ip = %v, want %q", header, got, "192.0.2.10")
			}
		})
	}
}

// refusalRecord returns the one "internal secret check failed" record of a
// JSON log.
func refusalRecord(t *testing.T, log string) map[string]any {
	t.Helper()
	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unparseable log line %q: %v", line, err)
		}
		if entry["msg"] == "internal secret check failed" {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d internal-secret refusal records, want 1; log:\n%s", len(found), log)
	}
	return found[0]
}
