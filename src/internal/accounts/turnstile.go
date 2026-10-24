package accounts

// turnstile.go — the registration captcha (sk-674.18 T038, US-8): Cloudflare
// Turnstile, verified server-side by siteverify when SKEIN_TURNSTILE_SECRET
// is set. Without the secret there is no captcha: Verify admits every
// request and nothing leaves the process.
//
// siteverify (developers.cloudflare.com/turnstile/get-started/
// server-side-validation): POST, form-encoded `secret`, `response` (the
// widget token), optional `remoteip` and `idempotency_key`; the answer is
// JSON with `success`. A token is valid for 300 s, at most 2048 characters
// and single-use — so a retried call carries the same idempotency key and
// does not consume the token twice.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-retryablehttp"
)

// TurnstileVerifyURL is Cloudflare's siteverify endpoint.
const TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// turnstileTokenMaxLen is the longest token Turnstile issues.
const turnstileTokenMaxLen = 2048

// turnstileTimeout bounds one siteverify attempt.
const turnstileTimeout = 5 * time.Second

// ErrCaptchaFailed — the token is missing, malformed, rejected by
// siteverify, or siteverify could not be asked. Registration is refused
// with CodeCaptchaFailed: an unverifiable captcha does not admit a signup.
var ErrCaptchaFailed = errors.New("accounts: captcha failed")

// Turnstile verifies registration captcha tokens.
type Turnstile struct {
	secret    string
	verifyURL string
	client    *retryablehttp.Client
}

// NewTurnstile builds the verifier. An empty secret disables the captcha;
// verifyURL is TurnstileVerifyURL outside tests.
func NewTurnstile(secret, verifyURL string) *Turnstile {
	c := retryablehttp.NewClient()
	c.RetryMax = 2
	c.RetryWaitMin = 200 * time.Millisecond
	c.RetryWaitMax = time.Second
	c.HTTPClient.Timeout = turnstileTimeout
	c.Logger = nil
	return &Turnstile{secret: secret, verifyURL: verifyURL, client: c}
}

// Enabled reports whether a captcha is required.
func (t *Turnstile) Enabled() bool { return t != nil && t.secret != "" }

// Verify checks token for a request from remoteIP (empty = not sent). With
// the captcha disabled it returns nil without any call; otherwise nil only
// when siteverify answered success.
func (t *Turnstile) Verify(ctx context.Context, token, remoteIP string) error {
	if !t.Enabled() {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > turnstileTokenMaxLen {
		return ErrCaptchaFailed
	}
	form := url.Values{
		"secret":          {t.secret},
		"response":        {token},
		"idempotency_key": {uuid.NewString()},
	}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodPost, t.verifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: building the siteverify request: %v", ErrCaptchaFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "turnstile siteverify unavailable, refusing the captcha", "error", err.Error())
		return fmt.Errorf("%w: siteverify: %v", ErrCaptchaFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "turnstile siteverify answered an error status, refusing the captcha", "status", resp.StatusCode)
		return fmt.Errorf("%w: siteverify status %d", ErrCaptchaFailed, resp.StatusCode)
	}
	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return fmt.Errorf("%w: decoding siteverify: %v", ErrCaptchaFailed, err)
	}
	if !out.Success {
		return fmt.Errorf("%w: %s", ErrCaptchaFailed, strings.Join(out.ErrorCodes, ","))
	}
	return nil
}
