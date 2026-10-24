package handler

// middleware_secret_log_test.go — (K-11: a raw link token
// is stored nowhere). RequireInternalSecret logs a refused request; the
// token-bearing routes (/auth/password-reset/{token},
// /auth/confirm-email/{token}) carry the raw token in the path, so the
// refusal must not write it to the log.

import (
	"bytes"
	"context"
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
