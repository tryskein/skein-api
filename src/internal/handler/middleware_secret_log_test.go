package handler

// middleware_secret_log_test.go — (K-11: a raw link token
// is stored nowhere). RequireInternalSecret logs a refused request; the
// token-bearing routes (/auth/password-reset/{token},
// /auth/confirm-email/{token}) carry the raw token in the path, so the
// refusal must not write it to the log.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"skein-api/internal/auth"
	"skein-api/internal/logging"
)

func TestRequireInternalSecret_RefusalLogsNoLinkToken(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A token of the issued shape (43 characters of base64url), recognisable.
	const token = "LEAKCANARY_" + "abcdefghijklmnopqrstuvwxyzABCDEF"
	gate := RequireInternalSecret("the-right-internal-secret", "")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, path := range []string{
		"/api/v1/auth/password-reset/" + token,
		"/api/v1/auth/confirm-email/" + token,
	} {
		buf.Reset()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set(auth.HeaderInternalSecret, "a-wrong-internal-secret!!")
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", path, rec.Code)
		}
		if buf.Len() == 0 {
			t.Fatalf("%s: the refusal wrote no log record", path)
		}
		if strings.Contains(buf.String(), token) {
			t.Errorf("%s: the refusal log carries the link token: %s", path, buf.String())
		}
	}
}

// A refused request logs the transport peer as "remote" and the claimed
// X-Skein-Client-Ip as "client_ip", the latter cut to 64 bytes so an
// oversized header cannot bloat the record.
func TestRequireInternalSecret_RefusalLogsPeerAndClientIP(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	gate := RequireInternalSecret("the-right-internal-secret", "")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	long := strings.Repeat("a", 200)
	for _, tc := range []struct {
		name, header, want string
	}{
		{"address", "2001:db8::1", "2001:db8::1"},
		{"absent", "", ""},
		{"oversized", long, strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			req := httptest.NewRequest(http.MethodGet, "/agent/v1/notifications/poll", nil)
			req.RemoteAddr = "198.51.100.7:40123"
			if tc.header != "" {
				req.Header.Set(auth.HeaderClientIP, tc.header)
			}
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", rec.Code)
			}
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
				t.Fatalf("refusal log %q is not one JSON record: %v", buf.String(), err)
			}
			if got := entry["remote"]; got != "198.51.100.7:40123" {
				t.Errorf("remote = %v, want 198.51.100.7:40123", got)
			}
			if got := entry["client_ip"]; got != tc.want {
				t.Errorf("client_ip for header %q = %v, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// K-11: the logged-in invitation link
// /api/v1/invitations/{token} sits behind ParseUserHeaders and
// RequireSessionEpoch; their log records — a session refused with an
// outdated epoch, a malformed user-id header — must not carry the live link
// token, and the refusal does not spend it. Logged through the production
// handler (logging.NewDualHandler), whose sanitizer masks keys, not values.
func TestSessionGuards_LogNoInvitationToken(t *testing.T) {
	e := newInvTokenEnv(t)
	u, email := e.addressee()
	_, link := e.invite(email)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := NewInvitationsHandler(e.svc)
	r := chi.NewRouter()
	r.Route("/api/v1/invitations/{token}", func(r chi.Router) {
		r.Use(ParseUserHeaders, RequireSessionEpoch(e.pool), RequireUser)
		r.Get("/", h.View)
	})
	path := "/api/v1/invitations/" + link

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(auth.HeaderUserID, u.id.String())
	req.Header.Set(auth.HeaderUsername, u.name)
	req.Header.Set(auth.HeaderUserRole, "user")
	req.Header.Set(auth.HeaderSessionEpoch, "999")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"session_revoked"`) {
		t.Fatalf("an outdated session: %d %s, want 401 session_revoked", rec.Code, rec.Body)
	}

	bad := httptest.NewRequest(http.MethodGet, path, nil)
	bad.Header.Set(auth.HeaderUserID, "not-a-uuid")
	r.ServeHTTP(httptest.NewRecorder(), bad)

	if buf.Len() == 0 {
		t.Fatal("the guards wrote no log record")
	}
	if strings.Contains(buf.String(), link) {
		t.Errorf("the guards' log carries the link token: %s", buf.String())
	}
	if p, err := e.svc.Preview(context.Background(), link); err != nil || p.Status != "valid" {
		t.Errorf("the link after the refused session: %+v %v, want valid", p, err)
	}
}

// TestRequireInternalSecret_RefusalLogsTheLoggablePath — the "path" field of
// the refusal record, segment by segment (K-11). A segment that could be a
// link token — 20 or more characters of the base64url alphabet, not a UUID —
// is written as {redacted}; a UUID, an id, a word and anything with a
// character outside that alphabet stay as they are. All values are synthetic.
func TestRequireInternalSecret_RefusalLogsTheLoggablePath(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	gate := RequireInternalSecret("the-right-internal-secret", "")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	const uuidSeg = "0b4f3c1e-8d2a-4e6f-9a7b-1c2d3e4f5a6b"
	cases := []struct {
		name, seg, want string
	}{
		{"lower-case letters", "abcdefghijklmnopqrstuvwxyz", "{redacted}"},
		{"upper-case letters", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "{redacted}"},
		{"digits", "01234567890123456789", "{redacted}"},
		{"dashes", "--------------------", "{redacted}"},
		{"underscores", "____________________", "{redacted}"},
		{"the edges of every range", "azAZ09-_azAZ09-_azAZ", "{redacted}"},
		{"a token of the issued length", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ0123456", "{redacted}"},
		{"exactly 20 characters", "aaaaaaaaaaaaaaaaaaaa", "{redacted}"},
		{"19 characters", "aaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaa"},
		{"a UUID", uuidSeg, uuidSeg},
		{"a dot inside", "aaaaaaaaaa.aaaaaaaaaa", "aaaaaaaaaa.aaaaaaaaaa"},
		{"a plus inside", "aaaaaaaaaa+aaaaaaaaaa", "aaaaaaaaaa+aaaaaaaaaa"},
		{"a tilde inside", "aaaaaaaaaa~aaaaaaaaaa", "aaaaaaaaaa~aaaaaaaaaa"},
		{"a colon inside", "aaaaaaaaaa:aaaaaaaaaa", "aaaaaaaaaa:aaaaaaaaaa"},
		{"a backtick inside", "aaaaaaaaaa`aaaaaaaaaa", "aaaaaaaaaa`aaaaaaaaaa"},
		{"an at sign inside", "aaaaaaaaaa@aaaaaaaaaa", "aaaaaaaaaa@aaaaaaaaaa"},
		{"a bracket inside", "AAAAAAAAAA[AAAAAAAAAA", "AAAAAAAAAA[AAAAAAAAAA"},
		{"a brace inside", "zzzzzzzzzz{zzzzzzzzzz", "zzzzzzzzzz{zzzzzzzzzz"},
		{"a non-ASCII letter inside", "aaaaaaaaaaéaaaaaaaaaa", "aaaaaaaaaaéaaaaaaaaaa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.URL.Path = "/api/v1/projects/" + uuidSeg + "/issues/sk-12/" + tc.seg + "/comments"
			req.Header.Set(auth.HeaderInternalSecret, "a-wrong-internal-secret!!")
			gate.ServeHTTP(httptest.NewRecorder(), req)
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
				t.Fatalf("refusal log %q is not one JSON record: %v", buf.String(), err)
			}
			want := "/api/v1/projects/" + uuidSeg + "/issues/sk-12/" + tc.want + "/comments"
			if got := entry["path"]; got != want {
				t.Errorf("path = %v, want %s", got, want)
			}
		})
	}
}
