package accounts_test

// turnstile_test.go — sk-674.18, test matrix row 153 (US-8: Turnstile only
// when its key is set).
//
// Reference: the spec US-8 ("+ Turnstile if the key is set") and Cloudflare's
// siteverify contract (POST, form fields `secret` and `response`, JSON
// `success`). Without the secret siteverify is never called and every
// request passes; with it, siteverify is called with the secret and the
// client's token, and only its `success` admits the request. siteverify is
// an HTTP stub.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"skein-api/internal/accounts"
)

type siteverifyCall struct {
	secret, response, remoteIP string
}

// siteverifyStub records every call and answers success for good.
func siteverifyStub(t *testing.T, good string) (string, func() []siteverifyCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []siteverifyCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("siteverify called with %s, want POST", r.Method)
		}
		_ = r.ParseForm()
		c := siteverifyCall{r.PostForm.Get("secret"), r.PostForm.Get("response"), r.PostForm.Get("remoteip")}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"success": c.response == good})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []siteverifyCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]siteverifyCall(nil), calls...)
	}
}

func TestTurnstile_WithoutKeyNoCall(t *testing.T) {
	url, calls := siteverifyStub(t, "good")
	ts := accounts.NewTurnstile("", url)
	if ts.Enabled() {
		t.Fatal("Enabled() = true without a secret, want false")
	}
	for _, token := range []string{"", "anything"} {
		if err := ts.Verify(context.Background(), token, "203.0.113.7"); err != nil {
			t.Errorf("Verify(%q) without a key = %v, want nil", token, err)
		}
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("siteverify called %d times without a key, want 0: %+v", len(got), got)
	}
}

func TestTurnstile_WithKeyCallsSiteverify(t *testing.T) {
	const secret = "0x4AAAAAAA-secret"
	url, calls := siteverifyStub(t, "good-token")
	ts := accounts.NewTurnstile(secret, url)
	if !ts.Enabled() {
		t.Fatal("Enabled() = false with a secret, want true")
	}

	if err := ts.Verify(context.Background(), "good-token", "203.0.113.7"); err != nil {
		t.Fatalf("Verify(good) = %v, want nil", err)
	}
	if err := ts.Verify(context.Background(), "bad-token", "203.0.113.8"); !errors.Is(err, accounts.ErrCaptchaFailed) {
		t.Errorf("Verify(bad) = %v, want ErrCaptchaFailed", err)
	}
	got := calls()
	want := []siteverifyCall{
		{secret, "good-token", "203.0.113.7"},
		{secret, "bad-token", "203.0.113.8"},
	}
	if len(got) != len(want) {
		t.Fatalf("siteverify calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A missing token is refused without spending a call.
	if err := ts.Verify(context.Background(), "", ""); !errors.Is(err, accounts.ErrCaptchaFailed) {
		t.Errorf("Verify(empty) = %v, want ErrCaptchaFailed", err)
	}
	if n := len(calls()); n != len(want) {
		t.Errorf("an empty token reached siteverify (%d calls, want %d)", n, len(want))
	}
}
