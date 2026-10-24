package accounts_test

// ratelimit_test.go — sk-674.18, test matrix row 154 (US-8 "Limits";
// plan K-17, T038/T039): the api-side rate limit of signup and of the
// password-reset request, driven through the real Service methods; and (T040)
// of the anonymous invitation link — its view and signup
// by invitation, whose subject is the link token (its hash) instead of an
// email.
//
// Reference: the plan — the key is the action + the email + the client IP
// from X-Skein-Client-Ip, one bucket per email and one per IP, N attempts
// pass, the N+1-th is rate_limited, after the window it passes again. Each
// series changes exactly one axis and keeps the others; body fields outside
// the key (username, password) change on every attempt and lift nothing.
//
// Everything past the limiter is a fake: an admitted signup succeeds on the
// fake users store; an admitted reset request reaches its transaction,
// which the fake database refuses with errAdmitted; an admitted invitation
// request reaches the invitation, whose fake link is invalid (the view
// answers status invalid, signup invitation_invalid).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"skein-api/internal/accounts"
	"skein-api/internal/auth"
	"skein-api/internal/handler"
)

var errAdmitted = errors.New("admitted past the limiter")

// fakeDB refuses every transaction: a reset that reaches it was admitted.
type fakeDB struct{}

func (fakeDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return nil, errAdmitted }
func (fakeDB) QueryRow(context.Context, string, ...any) pgx.Row       { return nil }

// fakeUsers admits every password and creates every account.
type fakeUsers struct{}

func (fakeUsers) CheckNewPassword(context.Context, string, string) error { return nil }
func (fakeUsers) CreateSelfService(_ context.Context, a accounts.NewAccount) (accounts.AuthResponse, error) {
	return accounts.AuthResponse{ID: uuid.NewString(), Email: a.Email, Username: a.Username}, nil
}
func (fakeUsers) HashPassword(string) (string, error) { return "hash", nil }
func (fakeUsers) CompletePasswordResetInTx(context.Context, pgx.Tx, uuid.UUID, string) (accounts.AuthResponse, error) {
	return accounts.AuthResponse{}, nil
}

// freeIdentities: nothing is taken.
type freeIdentities struct{}

func (freeIdentities) EmailTaken(context.Context, string) (bool, error) { return false, nil }
func (freeIdentities) TakenUsernames(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// fakeInvitations answers every link as invalid: an invitation request
// that gets past the limiter ends there.
type fakeInvitations struct{}

func (fakeInvitations) Preview(context.Context, string) (accounts.InvitationPreview, error) {
	return accounts.InvitationPreview{Status: accounts.InvitationInvalid}, nil
}
func (fakeInvitations) SignupStep(string, string) accounts.InvitationStep { return nil }

type noCaptcha struct{}

func (noCaptcha) Enabled() bool                                { return false }
func (noCaptcha) Verify(context.Context, string, string) error { return nil }

type clock struct{ now time.Time }

func (c *clock) read() time.Time         { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

// limited is one action under test: how to make one attempt through the
// service, and the action's thresholds.
type limited struct {
	action accounts.Action
	// attempt makes one request; n varies the body fields outside the key.
	attempt func(svc *accounts.Service, email, ip string, n int) error
}

var limitedActions = []limited{
	{accounts.ActionSignup, func(svc *accounts.Service, email, ip string, n int) error {
		_, err := svc.Signup(context.Background(), accounts.SignupRequest{
			Email: email, Username: fmt.Sprintf("user%d", n), Password: fmt.Sprintf("password number %d", n),
			TermsAccepted: true, Language: accounts.LanguageEN,
		}, ip)
		return err
	}},
	{accounts.ActionPasswordReset, func(svc *accounts.Service, email, ip string, _ int) error {
		return svc.RequestPasswordReset(context.Background(), accounts.PasswordResetRequest{
			Email: email, Language: accounts.LanguageEN,
		}, ip)
	}},
	// The invitation link: the subject is the token (the «email» argument
	// stands for it).
	{accounts.ActionInvitationPreview, func(svc *accounts.Service, token, ip string, _ int) error {
		_, err := svc.InvitationPreview(context.Background(), token, ip)
		return err
	}},
	{accounts.ActionInvitationSignup, func(svc *accounts.Service, token, ip string, n int) error {
		_, err := svc.InvitationSignup(context.Background(), token, accounts.InvitationSignupRequest{
			Username: fmt.Sprintf("user%d", n), Password: fmt.Sprintf("password number %d", n), TermsAccepted: true,
		}, ip)
		return err
	}},
}

func newLimitedService(limiter *accounts.RateLimiter) *accounts.Service {
	svc := accounts.NewService(accounts.ServiceConfig{
		SignupEnabled: true, EmailEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local",
	}, fakeDB{}, fakeUsers{}, freeIdentities{}, noCaptcha{}, limiter)
	svc.UseInvitations(fakeInvitations{})
	return svc
}

// admitted reports whether err is the outcome of a request the limiter let
// through, and fails on anything else but rate_limited.
func admitted(t *testing.T, err error) bool {
	t.Helper()
	var ref *accounts.Refusal
	switch {
	case err == nil || errors.Is(err, errAdmitted):
		return true
	case errors.As(err, &ref) && ref.Code == "invitation_invalid":
		return true // reached the (fake) invitation
	case errors.As(err, &ref) && ref.Code == "rate_limited":
		if ref.Status() != 429 {
			t.Errorf("rate_limited status = %d, want 429", ref.Status())
		}
		return false
	default:
		t.Fatalf("unexpected outcome %v", err)
		return false
	}
}

func TestRateLimit_KeyIsActionEmailAndIP(t *testing.T) {
	for _, la := range limitedActions {
		a := la.action
		if a.PerEmail < 1 || a.PerIP < 1 {
			t.Fatalf("%s: thresholds %d/%d, want both buckets with a positive threshold", a.Name, a.PerEmail, a.PerIP)
		}
		t.Run(a.Name, func(t *testing.T) {
			n := 0
			try := func(svc *accounts.Service, email, ip string) bool {
				n++
				return admitted(t, la.attempt(svc, email, ip, n))
			}

			t.Run("email fixed, IP changes: the email bucket", func(t *testing.T) {
				c := &clock{now: time.Unix(1_700_000_000, 0)}
				svc := newLimitedService(accounts.NewRateLimiterWithClock(c.read))
				email := "one@limit.test"
				lastIP := ""
				for i := 0; i < a.PerEmail; i++ {
					lastIP = fmt.Sprintf("198.51.100.%d", i+1)
					if !try(svc, email, lastIP) {
						t.Fatalf("attempt %d of %d refused", i+1, a.PerEmail)
					}
				}
				if try(svc, email, "198.51.100.250") {
					t.Fatalf("attempt %d with a fresh IP admitted, want rate_limited by the email", a.PerEmail+1)
				}
				// (1b) the last IP of the series is not exhausted: another
				// email from it passes.
				if !try(svc, "other@limit.test", lastIP) {
					t.Error("another email from the series' last IP refused, want admitted")
				}
				c.advance(a.Window + time.Second)
				if !try(svc, email, "198.51.100.251") {
					t.Error("after the window the email is still refused")
				}
			})

			t.Run("IP fixed, email changes: the IP bucket", func(t *testing.T) {
				c := &clock{now: time.Unix(1_700_000_000, 0)}
				svc := newLimitedService(accounts.NewRateLimiterWithClock(c.read))
				ip := "203.0.113.9"
				lastEmail := ""
				for i := 0; i < a.PerIP; i++ {
					lastEmail = fmt.Sprintf("u%d@limit.test", i)
					if !try(svc, lastEmail, ip) {
						t.Fatalf("attempt %d of %d refused", i+1, a.PerIP)
					}
				}
				if try(svc, "fresh@limit.test", ip) {
					t.Fatalf("attempt %d with a fresh email admitted, want rate_limited by the IP", a.PerIP+1)
				}
				// (2b) the last email of the series is not exhausted: from
				// another IP it passes.
				if !try(svc, lastEmail, "203.0.113.10") {
					t.Error("the series' last email from another IP refused, want admitted")
				}
				c.advance(a.Window + time.Second)
				if !try(svc, "fresh@limit.test", ip) {
					t.Error("after the window the IP is still refused")
				}
			})
		})
	}

	t.Run("email and IP fixed, the action changes", func(t *testing.T) {
		for _, pair := range [][2]int{{0, 1}, {2, 3}} {
			c := &clock{now: time.Unix(1_700_000_000, 0)}
			svc := newLimitedService(accounts.NewRateLimiterWithClock(c.read))
			email, ip := "both@limit.test", "192.0.2.1"
			first, second := limitedActions[pair[0]], limitedActions[pair[1]]
			exhaustThenOther(t, svc, first, second, email, ip)
		}
	})
}

// exhaustThenOther exhausts first's subject bucket and checks second, with
// the same subject and IP, is still admitted: the key includes the action.
func exhaustThenOther(t *testing.T, svc *accounts.Service, first, second limited, email, ip string) {
	t.Helper()
	{
		for i := 0; i < first.action.PerEmail; i++ {
			if !admitted(t, first.attempt(svc, email, ip, i)) {
				t.Fatalf("%s attempt %d refused", first.action.Name, i+1)
			}
		}
		if admitted(t, first.attempt(svc, email, ip, 99)) {
			t.Fatalf("%s not exhausted after %d attempts", first.action.Name, first.action.PerEmail)
		}
		if !admitted(t, second.attempt(svc, email, ip, 100)) {
			t.Errorf("%s refused after %s was exhausted: the key must include the action", second.action.Name, first.action.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Row 154 through the HTTP handlers: the same axes, with
// the client address in X-Skein-Client-Ip as skein-web stamps it, on the
// handlers buildRouter mounts. An admitted signup is 200 (fake users store),
// an admitted reset request is 204 (fake database: no account), a refused
// one is 429 rate_limited.
// ---------------------------------------------------------------------------

// noAccountTx is a transaction in which no account exists.
type noAccountTx struct{ pgx.Tx }

func (noAccountTx) QueryRow(context.Context, string, ...any) pgx.Row { return noRow{} }
func (noAccountTx) Commit(context.Context) error                     { return nil }
func (noAccountTx) Rollback(context.Context) error                   { return nil }

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

type noAccountDB struct{}

func (noAccountDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return noAccountTx{}, nil }
func (noAccountDB) QueryRow(context.Context, string, ...any) pgx.Row       { return noRow{} }

const httpLimitSecret = "ratelimit-http-secret"

func limitedRouter(c *clock) http.Handler {
	svc := accounts.NewService(accounts.ServiceConfig{
		SignupEnabled: true, EmailEnabled: true, EmailDeliveryTTL: time.Hour, AuthMode: "local",
	}, noAccountDB{}, fakeUsers{}, freeIdentities{}, noCaptcha{}, accounts.NewRateLimiterWithClock(c.read))
	svc.UseInvitations(fakeInvitations{})
	h := handler.NewAccountsHandler(svc)
	r := chi.NewRouter()
	r.With(handler.RequireInternalSecret(httpLimitSecret, "")).Post("/api/v1/auth/signup", h.Signup)
	r.With(handler.RequireInternalSecret(httpLimitSecret, "")).Post("/api/v1/auth/password-reset", h.PasswordReset)
	r.With(handler.RequireInternalSecret(httpLimitSecret, "")).Get("/api/v1/auth/invitations/{token}", h.InvitationPreview)
	r.With(handler.RequireInternalSecret(httpLimitSecret, "")).Post("/api/v1/auth/invitations/{token}/signup", h.InvitationSignup)
	return r
}

// httpLimited is one limited route: its method, its path for a subject (the
// email routes ignore it — the email is in the body; the invitation routes
// carry the token, their subject, in the path), the status of an admitted
// request and its body.
type httpLimited struct {
	action accounts.Action
	method string
	path   func(subject string) string
	ok     int
	body   func(subject string, n int) map[string]any
}

func fixedPath(p string) func(string) string { return func(string) string { return p } }

var httpLimitedActions = []httpLimited{
	{accounts.ActionSignup, http.MethodPost, fixedPath("/api/v1/auth/signup"), http.StatusOK, func(email string, n int) map[string]any {
		return map[string]any{"email": email, "username": fmt.Sprintf("user%d", n),
			"password": fmt.Sprintf("password number %d", n), "termsAccepted": true, "language": "en"}
	}},
	{accounts.ActionPasswordReset, http.MethodPost, fixedPath("/api/v1/auth/password-reset"), http.StatusNoContent, func(email string, _ int) map[string]any {
		return map[string]any{"email": email, "language": "en"}
	}},
	// An admitted view reaches the (fake, invalid) link: 200.
	{accounts.ActionInvitationPreview, http.MethodGet, func(token string) string { return "/api/v1/auth/invitations/" + token },
		http.StatusOK, func(string, int) map[string]any { return nil }},
	// An admitted signup reaches the (fake, invalid) link: 422 invitation_invalid.
	{accounts.ActionInvitationSignup, http.MethodPost, func(token string) string { return "/api/v1/auth/invitations/" + token + "/signup" },
		http.StatusUnprocessableEntity, func(_ string, n int) map[string]any {
			return map[string]any{"username": fmt.Sprintf("user%d", n), "password": fmt.Sprintf("password number %d", n), "termsAccepted": true}
		}},
}

// send sends body with the client address in X-Skein-Client-Ip (none when
// ip is empty) plus extra headers, and reports the status and the code.
func send(t *testing.T, r http.Handler, method, path string, body map[string]any, ip string, extra map[string]string) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set(auth.HeaderInternalSecret, httpLimitSecret)
	if ip != "" {
		req.Header.Set(auth.HeaderClientIP, ip)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Error
}

func TestRateLimit_ThroughHandlers(t *testing.T) {
	for _, la := range httpLimitedActions {
		a := la.action
		t.Run(a.Name, func(t *testing.T) {
			n := 0
			admittedHTTP := func(r http.Handler, email, ip string) bool {
				t.Helper()
				n++
				code, errCode := send(t, r, la.method, la.path(email), la.body(email, n), ip, nil)
				switch {
				case code == la.ok:
					return true
				case code == http.StatusTooManyRequests && errCode == "rate_limited":
					return false
				}
				t.Fatalf("unexpected answer %d %q", code, errCode)
				return false
			}

			t.Run("email fixed, X-Skein-Client-Ip changes", func(t *testing.T) {
				c := &clock{now: time.Unix(1_700_000_000, 0)}
				r := limitedRouter(c)
				lastIP := ""
				for i := 0; i < a.PerEmail; i++ {
					lastIP = fmt.Sprintf("198.51.100.%d", i+1)
					if !admittedHTTP(r, "one@limit.test", lastIP) {
						t.Fatalf("attempt %d of %d refused", i+1, a.PerEmail)
					}
				}
				if admittedHTTP(r, "one@limit.test", "198.51.100.250") {
					t.Fatalf("attempt %d admitted, want 429 by the email", a.PerEmail+1)
				}
				if !admittedHTTP(r, "other@limit.test", lastIP) {
					t.Error("another email from the series' last IP refused")
				}
				c.advance(a.Window + time.Second)
				if !admittedHTTP(r, "one@limit.test", "198.51.100.251") {
					t.Error("after the window the email is still refused")
				}
			})

			t.Run("X-Skein-Client-Ip fixed, email changes", func(t *testing.T) {
				c := &clock{now: time.Unix(1_700_000_000, 0)}
				r := limitedRouter(c)
				lastEmail := ""
				for i := 0; i < a.PerIP; i++ {
					lastEmail = fmt.Sprintf("u%d@limit.test", i)
					if !admittedHTTP(r, lastEmail, "203.0.113.9") {
						t.Fatalf("attempt %d of %d refused", i+1, a.PerIP)
					}
				}
				if admittedHTTP(r, "fresh@limit.test", "203.0.113.9") {
					t.Fatalf("attempt %d admitted, want 429 by the IP", a.PerIP+1)
				}
				if !admittedHTTP(r, lastEmail, "203.0.113.10") {
					t.Error("the series' last email from another IP refused")
				}
				c.advance(a.Window + time.Second)
				if !admittedHTTP(r, "fresh@limit.test", "203.0.113.9") {
					t.Error("after the window the IP is still refused")
				}
			})

			t.Run("no X-Skein-Client-Ip: X-Forwarded-For and the peer address key no bucket", func(t *testing.T) {
				c := &clock{now: time.Unix(1_700_000_000, 0)}
				r := limitedRouter(c)
				// More attempts than the IP threshold, all from one forwarded
				// address and one peer, each with its own email: an IP bucket
				// built from either would refuse the last ones.
				for i := 0; i <= a.PerIP; i++ {
					n++
					subject := fmt.Sprintf("x%d@limit.test", i)
					code, errCode := send(t, r, la.method, la.path(subject), la.body(subject, n), "",
						map[string]string{"X-Forwarded-For": "192.0.2.77", "X-Real-Ip": "192.0.2.77"})
					if code != la.ok {
						t.Fatalf("attempt %d without X-Skein-Client-Ip: %d %q, want %d", i+1, code, errCode, la.ok)
					}
				}
			})
		})
	}

	t.Run("subject and X-Skein-Client-Ip fixed, the action changes", func(t *testing.T) {
		for _, pair := range [][2]int{{0, 1}, {2, 3}} {
			c := &clock{now: time.Unix(1_700_000_000, 0)}
			r := limitedRouter(c)
			first, second := httpLimitedActions[pair[0]], httpLimitedActions[pair[1]]
			const subject = "both@limit.test"
			for i := 0; i < first.action.PerEmail; i++ {
				if code, _ := send(t, r, first.method, first.path(subject), first.body(subject, i), "192.0.2.1", nil); code != first.ok {
					t.Fatalf("%s attempt %d: %d", first.action.Name, i+1, code)
				}
			}
			if code, errCode := send(t, r, first.method, first.path(subject), first.body(subject, 99), "192.0.2.1", nil); code != http.StatusTooManyRequests || errCode != "rate_limited" {
				t.Fatalf("%s over the limit: %d %q, want 429 rate_limited", first.action.Name, code, errCode)
			}
			if code, errCode := send(t, r, second.method, second.path(subject), second.body(subject, 100), "192.0.2.1", nil); code != second.ok {
				t.Errorf("%s after %s was exhausted: %d %q, want %d — the key includes the action", second.action.Name, first.action.Name, code, errCode, second.ok)
			}
		}
	})
}
