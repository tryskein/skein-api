package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// pushSendTimeout bounds a single web-push delivery. The dispatcher sends
// pushes synchronously after the bell-commit; without a per-delivery deadline
// one hung push-service connection would stall the whole dispatcher loop until
// shutdown. webpush-go falls back to a no-timeout &http.Client{} unless we pass
// our own, so we inject a bounded client. Kept independent of the long-lived
// dispatcher Run context (which only cancels on shutdown).
const pushSendTimeout = 10 * time.Second

// defaultTTL is the seconds a push service should retain an undelivered
// message. 30s keeps it short — Skein push payloads are real-time nudges
// (a comment, a mention); a message that couldn't be delivered for half a
// minute is stale, the user will see the change in-app instead.
const defaultTTL = 30

// maxErrorBodyBytes bounds how much of a non-2xx push-service response body
// is kept for diagnostics. The body is where a push service says WHY it
// refused (Apple answers with a small JSON object naming the reason, e.g. a
// bad VAPID JWT), and without it a 403 is undiagnosable. The reasons are
// short, so 1 KiB holds them whole; the bound exists because the body ends up
// in one log line, and a misbehaving or hostile endpoint must not be able to
// make that line arbitrarily large.
const maxErrorBodyBytes = 1024

// maxErrorBodyReadBytes bounds how much of a non-2xx body is READ before it is
// redacted and cut to maxErrorBodyBytes. It is larger than the shown body on
// purpose: a subscription secret echoed across the 1 KiB boundary must be
// read whole to be recognised and redacted, rather than cut in half and
// leaked as a prefix. 16 KiB is far above any provider error body while still
// a fixed, small allocation per refused send.
const maxErrorBodyReadBytes = 16 << 10

// ErrDisabled is returned by Send when VAPID keys are not configured. It is
// a benign, expected condition (push is optional infra) — callers should log
// at debug/info and move on, not treat it as a failure.
var ErrDisabled = errors.New("push disabled: VAPID keys not configured")

// ErrGone is returned (wrapped) by Send when the push service reported the
// subscription endpoint is dead (404/410). Send has already pruned the row;
// the sentinel lets callers distinguish "the device unsubscribed" from a
// transient delivery failure so they don't retry.
var ErrGone = errors.New("push subscription gone (pruned)")

// ErrForbidden is returned (wrapped, via *StatusError) by Send when the push
// service answered 403. A 403 is not a transient failure and not a dead
// subscription: Apple's push service, for one, answers 403 when it rejects
// the VAPID JWT — a configuration error on our side that a retry cannot fix
// and that says nothing about the device. Callers must not prune the
// subscription and must not retry on its account (a row may still be retried
// for ANOTHER device's transient failure — see outbox classifySendOutcome).
var ErrForbidden = errors.New("push service refused the request (403)")

// StatusError describes a non-2xx answer of a push service other than the
// gone statuses (404/410, reported as ErrGone). It carries what is needed to
// diagnose the refusal without the endpoint itself: the endpoint URL embeds
// the subscription's token, so only its host is kept.
type StatusError struct {
	// StatusCode is the HTTP status the push service answered with.
	StatusCode int
	// Host is the push service host (e.g. "web.push.apple.com") — never the
	// full endpoint.
	Host string
	// Body is at most maxErrorBodyBytes of the response body, valid UTF-8,
	// with every form of the subscription secret replaced (see
	// newStatusError). It is the push service's own text and may name the
	// reason.
	Body string
	// BodyTruncated reports that the body was longer than what Body holds.
	BodyTruncated bool
	// BodyReadError is set when reading the body failed part-way; Body then
	// holds what arrived before the failure.
	BodyReadError string
}

// Error keeps the historical wording so log searches keep matching.
func (e *StatusError) Error() string {
	return fmt.Sprintf("push endpoint returned status %d", e.StatusCode)
}

// Unwrap lets errors.Is(err, ErrForbidden) classify a 403.
func (e *StatusError) Unwrap() error {
	if e.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	return nil
}

// pruner is the subset of *Store that Send needs to drop dead endpoints.
// Declared as an interface so Send is unit-testable with a fake.
type pruner interface {
	DeleteByEndpointAny(ctx context.Context, endpoint string) error
}

// Sender signs payloads with the configured VAPID keypair and POSTs them to
// browser push services. It prunes subscriptions the push service reports as
// gone (404/410) via the store. Build one at startup and share it — methods
// are goroutine-safe (no mutable state beyond the immutable config + pool).
type Sender struct {
	store        pruner
	vapidPublic  string
	vapidPrivate string
	vapidSubject string
	// httpClient is the bounded client handed to webpush-go so a hung push
	// service cannot stall the dispatcher. Shared + goroutine-safe.
	httpClient *http.Client
	// validate is the send-time endpoint guard. Defaults to ValidateEndpoint;
	// it is a field (not a direct call) only so tests can relax it to reach a
	// loopback httptest server — production always uses the real validator.
	validate func(string) error
}

// NewSender wires a Sender. When publicKey or privateKey is empty the Sender
// is "disabled": Send returns ErrDisabled without attempting a request.
//
// vapidSubject is the configured contact URI ("mailto:…" or "https:…", as the
// config validation requires). webpush-go v1.4.0 (vapid.go,
// getVAPIDAuthorizationHeader) prepends "mailto:" to every subscriber that
// does not start with "https:", so a "mailto:" subject handed over as-is
// would leave as "mailto:mailto:…" — which Apple's push service rejects with
// 403 (sk-627). The prefix is therefore removed here and restored by the
// library; an "https:" subject passes through untouched.
func NewSender(store pruner, vapidPublic, vapidPrivate, vapidSubject string) *Sender {
	return &Sender{
		store:        store,
		vapidPublic:  vapidPublic,
		vapidPrivate: vapidPrivate,
		vapidSubject: strings.TrimPrefix(vapidSubject, "mailto:"),
		httpClient:   &http.Client{Timeout: pushSendTimeout},
		validate:     ValidateEndpoint,
	}
}

// Enabled reports whether VAPID keys are configured. Callers can short-
// circuit fan-out when push is off rather than calling Send per-subscription
// just to get ErrDisabled each time.
func (s *Sender) Enabled() bool {
	return s.vapidPublic != "" && s.vapidPrivate != ""
}

// Send delivers payload to one subscription's push endpoint.
//
// Outcomes:
//   - VAPID keys unset → ErrDisabled (no request made).
//   - 2xx → nil (delivered / accepted by the push service).
//   - 404 / 410 → the endpoint is dead; the row is pruned (best-effort) and
//     the wrapped ErrGone is returned so the caller can skip a retry without
//     treating it as a hard failure.
//   - 403 → a *StatusError wrapping ErrForbidden. The row is NOT pruned and
//     the caller must not retry: see ErrForbidden.
//   - any other non-2xx → a *StatusError (status, push-service host, bounded
//     response body) — a transient failure the caller may retry.
//
// The response body is always drained + closed. Endpoint / key material is
// never logged in full — only a short prefix for correlation.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte) error {
	if !s.Enabled() {
		return ErrDisabled
	}

	// SSRF guard for stale rows: an endpoint that was valid at subscribe-time
	// could in principle be a row predating this validation, so we re-check
	// before turning it into an outbound POST. An invalid endpoint is treated
	// like a dead subscription — we log at info (with the masked prefix, never
	// the full token) and do NOT dispatch. We do not prune here: the row is not
	// "gone" per a push service, just unsendable, and pruning on a validation
	// quirk would silently drop a registration.
	if err := s.validate(sub.Endpoint); err != nil {
		slog.InfoContext(ctx, "push: skipping unsendable subscription (invalid endpoint)",
			"subscriptionId", sub.ID.String(),
			"endpointPrefix", maskEndpoint(sub.Endpoint),
			"error", err.Error())
		return fmt.Errorf("send web push: %w", err)
	}

	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.P256dh,
			Auth:   sub.Auth,
		},
	}, &webpush.Options{
		Subscriber:      s.vapidSubject,
		VAPIDPublicKey:  s.vapidPublic,
		VAPIDPrivateKey: s.vapidPrivate,
		TTL:             defaultTTL,
		HTTPClient:      s.httpClient,
	})
	if err != nil {
		return fmt.Errorf("sending web push: %w", err)
	}
	// Always drain + close so the underlying transport connection is reusable.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// The browser unsubscribed; the endpoint will never accept again.
		// Prune the row best-effort — failing to prune must not fail the
		// caller, so we log and still return ErrGone.
		if perr := s.store.DeleteByEndpointAny(ctx, sub.Endpoint); perr != nil {
			slog.InfoContext(ctx, "push: prune gone subscription failed",
				"subscriptionId", sub.ID.String(),
				"endpointPrefix", maskEndpoint(sub.Endpoint),
				"status", resp.StatusCode, "error", perr.Error())
		} else {
			slog.InfoContext(ctx, "push: pruned gone subscription",
				"subscriptionId", sub.ID.String(),
				"endpointPrefix", maskEndpoint(sub.Endpoint),
				"status", resp.StatusCode)
		}
		return fmt.Errorf("push endpoint returned %d: %w", resp.StatusCode, ErrGone)
	default:
		return newStatusError(resp, sub.Endpoint)
	}
}

// newStatusError builds the *StatusError of a non-2xx answer: the status,
// the endpoint's host (never the endpoint) and a redacted, bounded body.
//
// The order of the steps is what makes the bounds hold:
//
//  1. read at most maxErrorBodyReadBytes (+1 to know whether there was more);
//     the caller's deferred drain consumes the rest;
//  2. redact every form of the subscription secret (redactSubscription)
//     while the text is still whole — a secret is recognised only whole, so
//     this happens before any cut, and the read window is wider than the
//     shown body so an echo across the 1 KiB boundary is read whole;
//  3. if the read was cut, drop a trailing partial secret the window split;
//  4. repair UTF-8, which can GROW the text (one U+FFFD per invalid run);
//  5. only then cut to maxErrorBodyBytes, on a rune boundary.
//
// BodyTruncated is true exactly when received text is missing from Body —
// cut by the read window or by the final bound. A failed read is reported in
// BodyReadError, a field of its own, so no bound can cut the report away.
func newStatusError(resp *http.Response, endpoint string) *StatusError {
	se := &StatusError{StatusCode: resp.StatusCode}
	if u, err := url.Parse(endpoint); err == nil {
		se.Host = u.Host
	}
	forms := secretForms(endpoint)

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyReadBytes+1))
	if err != nil {
		// The body is diagnostics only: keep what arrived, report the failure
		// beside it, and let the status — the part the caller acts on — stand.
		se.BodyReadError = redactSubscription(err.Error(), forms)
	}
	readCut := len(raw) > maxErrorBodyReadBytes
	if readCut {
		raw = raw[:maxErrorBodyReadBytes]
	}

	body := redactSubscription(string(raw), forms)
	if readCut {
		body = trimPartialSecret(body, forms)
	}
	body = strings.ToValidUTF8(body, "\uFFFD")

	cut := false
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes]
		for !utf8.ValidString(body) {
			body = body[:len(body)-1]
		}
		cut = true
	}
	se.Body = body
	se.BodyTruncated = readCut || cut
	return se
}

// redactedSecret replaces every form of the subscription secret found in a
// provider's response body.
const redactedSecret = "<redacted>"

// secretForms lists the strings that would disclose the subscription if they
// reached a log: the endpoint itself and its token-bearing path segments and
// query values, each as written, percent-encoded (query and path rules) and
// with JSON-escaped slashes. A segment counts as token-bearing when it is the
// last one or is at least minSecretSegmentLen long — the static parts of the
// push services' paths ("3", "device", "fcm", "send", "wpush", "v2") are
// shorter, device tokens are far longer. Longest first, so a whole endpoint is
// replaced before its parts.
func secretForms(endpoint string) []string {
	const minSecretSegmentLen = 8
	secrets := []string{endpoint}
	if u, err := url.Parse(endpoint); err == nil {
		segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
		for i, seg := range segs {
			if seg == "" {
				continue
			}
			if i == len(segs)-1 || len(seg) >= minSecretSegmentLen {
				secrets = append(secrets, seg)
				if dec, derr := url.PathUnescape(seg); derr == nil {
					secrets = append(secrets, dec)
				}
			}
		}
		for _, vals := range u.Query() {
			for _, v := range vals {
				if v != "" {
					secrets = append(secrets, v)
				}
			}
		}
	}
	seen := map[string]bool{}
	var forms []string
	for _, sec := range secrets {
		for _, f := range []string{
			sec,
			url.QueryEscape(sec),
			url.PathEscape(sec),
			strings.ReplaceAll(sec, "/", `\/`),
		} {
			if f != "" && !seen[f] {
				seen[f] = true
				forms = append(forms, f)
			}
		}
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// redactSubscription replaces every secret form in s.
func redactSubscription(s string, forms []string) string {
	for _, f := range forms {
		s = strings.ReplaceAll(s, f, redactedSecret)
	}
	return s
}

// trimPartialSecret drops a suffix of s that is the beginning of a secret
// form: a window that cut the body may have split a secret, and a whole-form
// replacement cannot see half of one.
func trimPartialSecret(s string, forms []string) string {
	for _, f := range forms {
		for k := min(len(f)-1, len(s)); k > 0; k-- {
			if strings.HasSuffix(s, f[:k]) {
				s = s[:len(s)-k]
				break
			}
		}
	}
	return s
}

// maskEndpoint returns a short, non-identifying prefix of the endpoint URL
// for correlation in logs. The full endpoint embeds a per-device token we do
// not want in log aggregation.
func maskEndpoint(endpoint string) string {
	const n = 32
	if len(endpoint) <= n {
		return endpoint
	}
	return endpoint[:n] + "…"
}

// ---------------------------------------------------------------------------
// Payload size ceiling — sk-303.14, task T015.
// ---------------------------------------------------------------------------
//
// Knowledge of the record size is transport knowledge, so it lives here,
// with the sender, not with whoever assembles the text. The digest
// assembler needs ONE number: how many bytes of JSON it is allowed to
// hand over so the send does not turn into a provider refusal. Without
// it, overflow looks like "the transport died" and pulls the incident
// the opposite way — exactly the risk the plan named.
//
// The number is not invented, it is DERIVED from what webpush-go does
// when encrypting (aes128gcm, RFC 8188/8291), and so it must move with
// the library, not with someone's idea of "about four kilobytes":
//
//	record             = webpush.MaxRecordSize            = 4096
//	content-coding hdr = salt(16) + rs(4) + idlen(1) + pub(65) = 86
//	GCM tag            =                                        16
//	padding delimiter  =                                        1
//	plaintext          = 4096 − 86 − 16 − 1                    = 3993
//
// Exceeding the plaintext gives webpush.ErrMaxPadExceeded BEFORE a
// network call: the message does not leave at all.
const (
	// vapidRecordHeaderBytes — the aes128gcm content-coding header: salt,
	// declared record size, key-id length, and the uncompressed P-256
	// public key.
	vapidRecordHeaderBytes = 16 + 4 + 1 + 65
	// gcmTagBytes — the AES-GCM authenticity tag appended to the
	// ciphertext.
	gcmTagBytes = 16
	// paddingDelimiterBytes — the mandatory padding delimiter byte
	// appended to the plaintext before encryption.
	paddingDelimiterBytes = 1

	// MaxPayloadBytes — the maximum length of one push's JSON payload,
	// in bytes. This is the transport CEILING, not the sender's budget:
	// whoever assembles the text must leave itself headroom, because the
	// ceiling is reached at exactly the moment the message stops leaving.
	MaxPayloadBytes = int(webpush.MaxRecordSize) -
		vapidRecordHeaderBytes - gcmTagBytes - paddingDelimiterBytes
)
