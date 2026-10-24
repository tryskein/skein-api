package push

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"

	"skein-api/internal/logging"
)

// fakePruner records DeleteByEndpointAny calls so the Send tests can assert
// the gone-endpoint prune fired without a real DB.
type fakePruner struct {
	calls     atomic.Int32
	lastArg   string
	returnErr error
}

func (f *fakePruner) DeleteByEndpointAny(_ context.Context, endpoint string) error {
	f.calls.Add(1)
	f.lastArg = endpoint
	return f.returnErr
}

// realVAPIDPublic / realVAPIDPrivate are a keypair generated fresh for every
// test run via webpush.GenerateVAPIDKeys(), solely so the VAPID JWT signing
// inside SendNotificationWithContext succeeds (an invalid key fails before
// the HTTP round-trip and we never reach the status handling we want to
// test). No key material is committed.
var realVAPIDPublic, realVAPIDPrivate = func() (string, string) {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		panic(err)
	}
	return pub, priv
}()

// testSubscription builds a Subscription whose Endpoint points at the given
// test-server URL. The p256dh / auth are valid base64url ECDH material (a
// real browser key) so payload encryption succeeds and the request actually
// reaches the test server, where we control the status code.
func testSubscription(endpoint string) Subscription {
	return Subscription{
		ID:       uuid.New(),
		UserID:   uuid.New(),
		Endpoint: endpoint,
		// A real, well-formed P-256 public key + 16-byte auth secret
		// (base64url). Values from the webpush-go example fixtures.
		P256dh: "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
		Auth:   "AAAAAAAAAAAAAAAAAAAAAA",
	}
}

func TestSend_GonePrunesSubscription(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			pr := &fakePruner{}
			s := NewSender(pr, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
			// The httptest server is loopback http — the real ValidateEndpoint
			// (correctly) rejects that, so relax the guard to exercise the
			// status-handling path we're actually testing here.
			s.validate = func(string) error { return nil }
			sub := testSubscription(srv.URL)

			err := s.Send(context.Background(), sub, []byte(`{"hello":"world"}`))
			if !errors.Is(err, ErrGone) {
				t.Fatalf("expected ErrGone for status %d, got %v", status, err)
			}
			if got := pr.calls.Load(); got != 1 {
				t.Fatalf("expected exactly 1 prune call, got %d", got)
			}
			if pr.lastArg != sub.Endpoint {
				t.Fatalf("prune called with %q, want %q", pr.lastArg, sub.Endpoint)
			}
		})
	}
}

func TestSend_GonePruneErrorStillReturnsErrGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	pr := &fakePruner{returnErr: errors.New("db down")}
	s := NewSender(pr, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	s.validate = func(string) error { return nil }

	err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`))
	if !errors.Is(err, ErrGone) {
		t.Fatalf("expected ErrGone even when prune fails, got %v", err)
	}
	if pr.calls.Load() != 1 {
		t.Fatalf("expected prune attempted once, got %d", pr.calls.Load())
	}
}

func TestSend_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	pr := &fakePruner{}
	s := NewSender(pr, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	s.validate = func(string) error { return nil }

	if err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`)); err != nil {
		t.Fatalf("expected nil on 2xx, got %v", err)
	}
	if pr.calls.Load() != 0 {
		t.Fatalf("expected no prune on success, got %d", pr.calls.Load())
	}
}

func TestSend_NonGoneErrorDoesNotPrune(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	pr := &fakePruner{}
	s := NewSender(pr, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	s.validate = func(string) error { return nil }

	err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error on 500")
	}
	if errors.Is(err, ErrGone) {
		t.Fatalf("500 should not map to ErrGone, got %v", err)
	}
	if pr.calls.Load() != 0 {
		t.Fatalf("expected no prune on 500, got %d", pr.calls.Load())
	}
}

func TestSend_DisabledWhenNoKeys(t *testing.T) {
	pr := &fakePruner{}
	s := NewSender(pr, "", "", "mailto:test@skein.local")
	if s.Enabled() {
		t.Fatal("expected Enabled()=false with no keys")
	}
	err := s.Send(context.Background(), testSubscription("http://unused"), []byte(`{}`))
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if pr.calls.Load() != 0 {
		t.Fatalf("expected no prune when disabled, got %d", pr.calls.Load())
	}
}

func TestSend_InvalidEndpointSkippedNoPrune(t *testing.T) {
	// A subscription whose endpoint targets loopback (SSRF) must never be
	// dispatched. With the REAL validator (NewSender's default), Send returns
	// an ErrInvalidEndpoint-wrapped error and does NOT prune the row.
	pr := &fakePruner{}
	s := NewSender(pr, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	sub := testSubscription("https://127.0.0.1/internal")

	err := s.Send(context.Background(), sub, []byte(`{}`))
	if !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("expected ErrInvalidEndpoint, got %v", err)
	}
	if errors.Is(err, ErrGone) {
		t.Fatalf("invalid endpoint must not map to ErrGone, got %v", err)
	}
	if pr.calls.Load() != 0 {
		t.Fatalf("expected no prune on invalid endpoint, got %d", pr.calls.Load())
	}
}

// vapidSubOf extracts the `sub` claim from the VAPID Authorization header
// webpush-go put on a real request ("vapid t=<jwt>, k=<public key>"). The
// signature is not checked: the subject is what the push service reads, and
// what is under test is the exact string we hand it.
func vapidSubOf(t *testing.T, authorization string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(authorization, "vapid t=")
	if !ok {
		t.Fatalf("Authorization = %q, want the \"vapid t=...\" scheme", authorization)
	}
	jwt, _, ok := strings.Cut(rest, ",")
	if !ok {
		t.Fatalf("Authorization = %q, want \"t=<jwt>, k=<key>\"", authorization)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3: %q", len(parts), jwt)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding JWT claims: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parsing JWT claims %s: %v", raw, err)
	}
	return claims.Sub
}

// TestSend_VAPIDSubjectIsSentVerbatim — sk-627 regression. The configured
// subject must reach the push service exactly as configured. webpush-go
// prepends "mailto:" to every subscriber that does not start with "https:",
// so a configured "mailto:admin@example.com" used to leave as
// "mailto:mailto:admin@example.com" — and Apple's push service refused every
// such request with 403.
func TestSend_VAPIDSubjectIsSentVerbatim(t *testing.T) {
	for _, subject := range []string{"mailto:admin@example.com", "https://example.com"} {
		t.Run(subject, func(t *testing.T) {
			var got atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.Store(r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusCreated)
			}))
			defer srv.Close()

			s := NewSender(&fakePruner{}, realVAPIDPublic, realVAPIDPrivate, subject)
			s.validate = func(string) error { return nil }
			if err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`)); err != nil {
				t.Fatalf("Send: %v", err)
			}
			auth, _ := got.Load().(string)
			if sub := vapidSubOf(t, auth); sub != subject {
				t.Errorf("VAPID sub = %q, want %q", sub, subject)
			}
		})
	}
}

// TestSend_ForbiddenIsNotGoneAndKeepsTheSubscription — sk-627. Apple answers
// 403 when it rejects the VAPID JWT: a configuration error on our side, not a
// dead device. The subscription must survive, and the answer must be
// classifiable as ErrForbidden so the caller does not retry it.
func TestSend_ForbiddenIsNotGoneAndKeepsTheSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"reason":"BadJwtToken"}`))
	}))
	defer srv.Close()

	pr := &fakePruner{}
	s := NewSender(pr, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	s.validate = func(string) error { return nil }

	err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("Send on 403 = %v, want ErrForbidden", err)
	}
	if errors.Is(err, ErrGone) {
		t.Fatalf("403 must not map to ErrGone, got %v", err)
	}
	if got := pr.calls.Load(); got != 0 {
		t.Fatalf("prune calls on 403 = %d, want 0 — the subscription is not dead", got)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Body != `{"reason":"BadJwtToken"}` {
		t.Fatalf("Send on 403: StatusError = %+v, want the provider's reason body", se)
	}
}

// TestSend_StatusErrorCarriesHostAndBoundedBody — sk-627. A non-2xx answer is
// reported with its status, the push-service HOST (never the endpoint, which
// embeds the subscription token) and at most maxErrorBodyBytes of the body.
func TestSend_StatusErrorCarriesHostAndBoundedBody(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantBody      string
		wantTruncated bool
		wantForbidden bool
	}{
		{"short body kept whole", http.StatusInternalServerError, "upstream overloaded",
			"upstream overloaded", false, false},
		{"long body cut at the bound", http.StatusBadRequest, strings.Repeat("x", maxErrorBodyBytes+500),
			strings.Repeat("x", maxErrorBodyBytes), true, false},
		{"body exactly at the bound is not truncated", http.StatusForbidden, strings.Repeat("y", maxErrorBodyBytes),
			strings.Repeat("y", maxErrorBodyBytes), false, true},
		{"empty body", http.StatusTooManyRequests, "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			s := NewSender(&fakePruner{}, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
			s.validate = func(string) error { return nil }
			sub := testSubscription(srv.URL + "/push/v1/secret-subscription-token")

			err := s.Send(context.Background(), sub, []byte(`{}`))
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("Send on %d = %v, want a *StatusError", tc.status, err)
			}
			if se.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", se.StatusCode, tc.status)
			}
			if want := strings.TrimPrefix(srv.URL, "http://"); se.Host != want {
				t.Errorf("Host = %q, want %q", se.Host, want)
			}
			if se.Body != tc.wantBody || se.BodyTruncated != tc.wantTruncated {
				t.Errorf("Body = %d bytes truncated=%v, want %d bytes truncated=%v",
					len(se.Body), se.BodyTruncated, len(tc.wantBody), tc.wantTruncated)
			}
			if errors.Is(err, ErrForbidden) != tc.wantForbidden {
				t.Errorf("errors.Is(err, ErrForbidden) = %v, want %v", !tc.wantForbidden, tc.wantForbidden)
			}
			if strings.Contains(err.Error(), "secret-subscription-token") {
				t.Errorf("error text %q carries the endpoint path", err.Error())
			}
		})
	}
}

// sk627Token is a token-shaped secret: the last endpoint segment is what
// lets anyone push to the device. It contains ':' (as FCM tokens do) so the
// percent-encoded form differs from the raw one.
const sk627Token = "SK627SECRETtok:APA91b-aaaaaaaaaaaaaaaaaaa"

// TestSend_ResponseBodyNeverCarriesTheSubscriptionSecret — sk-627 review No. 1.
// A provider may echo the endpoint back in its error body, and JSON allows
// escaping '/', so a literal-URL redaction is not enough. Whatever form the
// echo takes, neither the token nor a prefix of it may reach the log line —
// checked on the line the project's DualHandler actually writes.
func TestSend_ResponseBodyNeverCarriesTheSubscriptionSecret(t *testing.T) {
	secretPrefix := sk627Token[:8]
	cases := []struct {
		name string
		body func(endpoint string) []byte
	}{
		{"plain endpoint echo", func(e string) []byte {
			return []byte(`{"reason":"BadJwtToken","endpoint":"` + e + `"}`)
		}},
		{"JSON-escaped slashes", func(e string) []byte {
			return []byte(`{"endpoint":"` + strings.ReplaceAll(e, "/", `\/`) + `"}`)
		}},
		{"percent-encoded endpoint", func(e string) []byte {
			return []byte("subscription=" + url.QueryEscape(e))
		}},
		{"bare token", func(string) []byte {
			return []byte(`{"reason":"BadDeviceToken","token":"` + sk627Token + `"}`)
		}},
		{"token straddling the shown-body cap", func(string) []byte {
			return []byte(strings.Repeat("a", maxErrorBodyBytes-12) + sk627Token)
		}},
		{"token straddling the read limit behind collapsing bytes", func(string) []byte {
			// Each 0xFF run collapses to one U+FFFD on UTF-8 repair, so text
			// read near the read limit ends up inside the shown 1 KiB.
			junk := bytes.Repeat([]byte{0xFF}, maxErrorBodyReadBytes-4)
			return append(junk, sk627Token...)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var endpoint string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write(tc.body(endpoint))
			}))
			defer srv.Close()
			endpoint = srv.URL + "/3/device/" + sk627Token

			s := NewSender(&fakePruner{}, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
			s.validate = func(string) error { return nil }
			err := s.Send(context.Background(), testSubscription(endpoint), []byte(`{}`))
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("Send = %v, want a *StatusError", err)
			}

			var buf bytes.Buffer
			slog.New(logging.NewDualHandler(&buf, slog.LevelDebug)).Warn("push send failed",
				"status", se.StatusCode, "pushHost", se.Host, "responseBody", se.Body)
			line := buf.String()
			if strings.Contains(line, secretPrefix) {
				t.Errorf("the log line carries the subscription secret (or its prefix %q): %s", secretPrefix, line)
			}
			if !strings.Contains(line, "responseBody=") || strings.Contains(line, "***") {
				t.Errorf("the body field is missing or masked: %s", line)
			}
		})
	}
}

// TestSend_StatusErrorBodyHonoursItsBound — sk-627 review No. 3. The 1 KiB bound
// holds on the body as finally stored (after UTF-8 repair), the body is valid
// UTF-8, and BodyTruncated says exactly whether received text was cut.
func TestSend_StatusErrorBodyHonoursItsBound(t *testing.T) {
	alternating := make([]byte, 0, maxErrorBodyBytes)
	for len(alternating) < maxErrorBodyBytes {
		alternating = append(alternating, 0xFF, 'A')
	}
	cases := []struct {
		name          string
		body          []byte
		wantTruncated bool
	}{
		{"invalid bytes expand on repair", alternating, true},
		{"multibyte rune at the bound", append(bytes.Repeat([]byte("a"), maxErrorBodyBytes-1), "ж"...), true},
		{"multibyte text that fits", []byte(strings.Repeat("ж", maxErrorBodyBytes/2)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()

			s := NewSender(&fakePruner{}, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
			s.validate = func(string) error { return nil }
			var se *StatusError
			if err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`)); !errors.As(err, &se) {
				t.Fatalf("Send = %v, want a *StatusError", err)
			}
			if len(se.Body) > maxErrorBodyBytes {
				t.Errorf("len(Body) = %d, want ≤ %d", len(se.Body), maxErrorBodyBytes)
			}
			if !utf8.ValidString(se.Body) {
				t.Errorf("Body is not valid UTF-8")
			}
			if se.BodyTruncated != tc.wantTruncated {
				t.Errorf("BodyTruncated = %v, want %v", se.BodyTruncated, tc.wantTruncated)
			}
		})
	}
}

// TestSend_BodyReadFailureIsReportedNotCut — sk-627 review No. 3. When the body
// breaks off mid-read, what arrived is kept, the failure is reported in a
// field of its own (so no cap can cut it away), and the body is not claimed
// to be truncated by us.
func TestSend_BodyReadFailureIsReportedNotCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise more than is written: the client sees an unexpected EOF.
		w.Header().Set("Content-Length", strconv.Itoa(maxErrorBodyBytes*2))
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxErrorBodyBytes))
	}))
	defer srv.Close()

	s := NewSender(&fakePruner{}, realVAPIDPublic, realVAPIDPrivate, "mailto:test@skein.local")
	s.validate = func(string) error { return nil }
	var se *StatusError
	if err := s.Send(context.Background(), testSubscription(srv.URL), []byte(`{}`)); !errors.As(err, &se) {
		t.Fatalf("Send = %v, want a *StatusError", err)
	}
	if se.BodyReadError == "" {
		t.Error("BodyReadError is empty — the broken read is not reported")
	}
	if se.Body != strings.Repeat("x", maxErrorBodyBytes) {
		t.Errorf("Body = %d bytes, want the %d bytes that arrived", len(se.Body), maxErrorBodyBytes)
	}
	if se.BodyTruncated {
		t.Error("BodyTruncated = true, but nothing that arrived was cut")
	}
}
